// Command ptslverify checks deliverable placement inside Pro Tools.
//
// It creates a throwaway session (start 01:00:00:00, 48 kHz), records a
// simulated take whose transport events are known exactly, delivers it,
// imports each deliverable through PTSL at its position, and reads back
// where Pro Tools put the clip (select all clips on the new track, then
// read the timeline selection in samples). The session is closed without
// saving.
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"antimusic.dev/remotesession/internal/deliver"
	"antimusic.dev/remotesession/internal/ptsl"
	"antimusic.dev/remotesession/internal/recorder"
	"antimusic.dev/remotesession/internal/take"
	"antimusic.dev/remotesession/internal/testsignal"
)

const rate = 48000

func main() {
	dir := flag.String("dir", "", "working directory for the session and take (required)")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "ptslverify: -dir is required")
		os.Exit(2)
	}
	if err := run(*dir); err != nil {
		fmt.Fprintln(os.Stderr, "ptslverify:", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	c, err := ptsl.Dial(ptsl.DefaultAddress)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Register(ctx, "AntiMusic", "RemoteSession verify"); err != nil {
		return err
	}
	name := "rs_verify_" + time.Now().Format("150405")
	if err := c.Call(ctx, ptsl.CmdCreateSession, map[string]any{
		"session_name": name, "session_location": dir, "file_type": "FT_WAVE",
		"sample_rate": "SR_48000", "bit_depth": "Bit24", "input_output_settings": "IO_Last",
		"is_interleaved": true, "is_cloud_project": false, "create_from_template": false, "create_from_aaf": false,
	}, nil); err != nil {
		return fmt.Errorf("creating session: %w", err)
	}
	defer func() {
		err := c.Call(context.Background(), ptsl.CmdCloseSession, map[string]any{"save_on_close": false}, nil)
		fmt.Println("closed test session without saving:", err == nil, errString(err))
	}()
	if err := c.Call(ctx, ptsl.CmdSetSessionStartTime, map[string]any{"session_start_time": "01:00:00:00"}, nil); err != nil {
		return fmt.Errorf("setting session start: %w", err)
	}
	s, err := c.Session(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("session %q: %d Hz, start %s (%s) = %d samples since midnight\n", s.Name, s.SampleRate, s.StartTimecode, s.TimecodeRate, s.StartSample)
	if s.SampleRate != rate {
		return fmt.Errorf("session rate %d", s.SampleRate)
	}

	// How quickly can transport state be observed? A PTSL-only transport
	// anchor can never be timed more precisely than one round trip.
	var rtts []time.Duration
	for i := 0; i < 200; i++ {
		t0 := time.Now()
		if _, err := c.TransportState(ctx); err != nil {
			return err
		}
		rtts = append(rtts, time.Since(t0))
	}
	slices.Sort(rtts)
	fmt.Printf("GetTransportState round trip over %d calls: min %v, median %v, p95 %v, max %v (= %d..%d samples at 48 kHz)\n",
		len(rtts), rtts[0], rtts[len(rtts)/2], rtts[len(rtts)*95/100], rtts[len(rtts)-1],
		rtts[0].Microseconds()*rate/1e6, rtts[len(rtts)-1].Microseconds()*rate/1e6)

	// Which units does PTSL use for sample positions? With a new session
	// the insertion point sits at the session start.
	sel, err := c.TimelineSelection(ctx)
	if err != nil {
		return err
	}
	absolute := sel.InSample == s.StartSample
	fmt.Printf("PTSL selection at session start reads %d samples -> sample positions are %s\n",
		sel.InSample, map[bool]string{true: "absolute (since 00:00:00:00)", false: "relative to session start"}[absolute])
	if !absolute && sel.InSample != 0 {
		return fmt.Errorf("unexpected insertion point %d", sel.InSample)
	}

	// A simulated take: 1 s rolling at session +2 s, a stop, then 0.5 s
	// at +10 s 4321 samples (deliberately not on a frame boundary).
	type span struct {
		frames  int64
		rolling bool
		at      int64
	}
	spans := []span{{4800, false, 0}, {48000, true, 2 * rate}, {9600, false, 0}, {24000, true, 10*rate + 4321}, {4800, false, 0}}
	takeDir := filepath.Join(dir, "take")
	raw := filepath.Join(dir, "capture.f32")
	var pcm []byte
	var evs []take.Event
	frame := int64(0)
	for _, sp := range spans {
		f := frame
		evs = append(evs, take.Event{Frame: &f, Rolling: sp.rolling, TimelineSample: s.StartSample + sp.at, Source: "simulated"})
		for i := int64(0); i < sp.frames; i++ {
			for ch := 0; ch < 2; ch++ {
				v := testsignal.Idle
				if sp.rolling {
					v = testsignal.Value(s.StartSample+sp.at+i, ch)
				}
				pcm = binary.LittleEndian.AppendUint32(pcm, math.Float32bits(v))
			}
		}
		frame += sp.frames
	}
	if err := os.WriteFile(raw, pcm, 0o644); err != nil {
		return err
	}
	sess := &take.Session{Source: "ptsl", Name: s.Name, SampleRate: s.SampleRate, StartTimecode: s.StartTimecode,
		TimecodeRate: s.TimecodeRate, StartSample: s.StartSample}
	r, err := recorder.Start(recorder.Config{Dir: takeDir, Session: sess,
		Input: []string{"-f", "f32le", "-ar", strconv.Itoa(rate), "-ch_layout", "stereo", "-i", raw}})
	if err != nil {
		return err
	}
	for _, e := range evs {
		r.Events.Append(e)
	}
	if err := r.Wait(); err != nil {
		return err
	}
	rep, err := deliver.Render(ctx, takeDir, deliver.Options{})
	if err != nil {
		return err
	}

	sessPathDir := filepath.Join(dir, name, "Audio Files")
	failures := 0
	for _, d := range rep.Deliverables {
		before, err := trackNames(ctx, c)
		if err != nil {
			return err
		}
		pos := d.SessionOffset
		if absolute {
			return fmt.Errorf("PTSL reported absolute sample positions; ptsl.ImportSpotted assumes relative")
		}
		abs, _ := filepath.Abs(filepath.Join(takeDir, d.File))
		if err := c.ImportSpotted(ctx, []string{abs}, sessPathDir, pos); err != nil {
			return fmt.Errorf("import %s: %w", d.File, err)
		}
		after, err := trackNames(ctx, c)
		if err != nil {
			return err
		}
		var track string
		for n := range after {
			if !before[n] {
				track = n
			}
		}
		if track == "" {
			return fmt.Errorf("import of %s created no track", d.File)
		}
		if err := c.Call(ctx, ptsl.CmdSelectAllClipsOnTrack, map[string]any{"track_name": track}, nil); err != nil {
			return err
		}
		got, err := c.TimelineSelection(ctx)
		if err != nil {
			return err
		}
		wantIn, wantOut := pos, pos+d.Segment.Frames
		ok := got.InSample == wantIn && got.OutSample == wantOut
		if !ok {
			failures++
		}
		fmt.Printf("%s -> track %q: Pro Tools clip %d..%d, expected %d..%d (session +%d samples): %s\n",
			d.File, track, got.InSample, got.OutSample, wantIn, wantOut, d.SessionOffset,
			map[bool]string{true: "EXACT", false: fmt.Sprintf("OFF BY %d/%d", got.InSample-wantIn, got.OutSample-wantOut)}[ok])
	}
	// Pro Tools' own reading of each file's BWF time stamp.
	var cl struct {
		ClipList []struct {
			ClipFullName           string `json:"clip_full_name"`
			ClipRootName           string `json:"clip_root_name"`
			OriginalTimestampPoint struct {
				Location string `json:"location"`
			} `json:"original_timestamp_point"`
		} `json:"clip_list"`
	}
	if err := c.Call(ctx, ptsl.CmdGetClipList, map[string]any{}, &cl); err != nil {
		return err
	}
	for _, d := range rep.Deliverables {
		root := strings.TrimSuffix(filepath.Base(d.File), ".wav")
		seen := 0
		for _, clip := range cl.ClipList {
			if clip.ClipRootName != root {
				continue
			}
			seen++
			ok := clip.OriginalTimestampPoint.Location == strconv.FormatInt(d.TimeReference, 10)
			if !ok {
				failures++
			}
			fmt.Printf("clip %s: Pro Tools original time stamp %s, bext TimeReference %d: %s\n",
				clip.ClipFullName, clip.OriginalTimestampPoint.Location, d.TimeReference,
				map[bool]string{true: "MATCH", false: "MISMATCH"}[ok])
		}
		if seen == 0 {
			failures++
			fmt.Printf("no clips found for %s\n", d.File)
		}
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d deliverables misplaced", failures, len(rep.Deliverables))
	}
	return nil
}

func trackNames(ctx context.Context, c *ptsl.Client) (map[string]bool, error) {
	var r struct {
		TrackList []struct {
			Name string `json:"name"`
		} `json:"track_list"`
	}
	err := c.Call(ctx, ptsl.CmdGetTrackList, map[string]any{
		"page_limit":        1000,
		"track_filter_list": []map[string]any{{"filter": "All", "is_inverted": false}},
	}, &r)
	out := map[string]bool{}
	for _, t := range r.TrackList {
		out[t.Name] = true
	}
	return out, err
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
