// Package deliver turns a recorded take into Broadcast Wave files that a
// DAW can place on its timeline, and verifies them.
//
// Each contiguous transport run becomes one BWF whose bext TimeReference
// is its absolute timeline position (samples since 00:00:00:00), the
// standard field DAWs use to spot a file to its original position. The
// master itself is never modified.
package deliver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"antimusic.dev/remotesession/internal/caf"
	"antimusic.dev/remotesession/internal/pcm"
	"antimusic.dev/remotesession/internal/take"
	"antimusic.dev/remotesession/internal/timeline"
	"antimusic.dev/remotesession/internal/wav"
)

const ReportFile = "delivery.json"

// Options control rendering.
type Options struct {
	// Codec is the deliverable sample format: pcm_s24le (default),
	// pcm_s16le or pcm_f32le.
	Codec      string
	MinFrames  int64
	Originator string
	FFmpeg     string

	// Chain, if set, prints each segment through these plugins in order
	// (post-processing). Output goes to deliver/<Label>/.
	Chain []Plugin
	Label string
	// TailFrames of plugin response (decay) are kept after each segment.
	TailFrames int64
	// Renderer is the rs-render binary.
	Renderer string
}

// Plan is what a take resolves to before rendering.
type Plan struct {
	MasterFrames      int64              `json:"master_frames"`
	MasterInterrupted bool               `json:"master_interrupted"`
	SampleRate        int                `json:"sample_rate"`
	Channels          int                `json:"channels"`
	Segments          []timeline.Segment `json:"segments"`
	// UncertaintyFrames is the largest placement uncertainty declared by
	// the transport events used.
	UncertaintyFrames int64    `json:"uncertainty_frames"`
	Warnings          []string `json:"warnings,omitempty"`
}

// Deliverable is one placed file.
type Deliverable struct {
	File          string           `json:"file"`
	Segment       timeline.Segment `json:"segment"`
	TimeReference int64            `json:"time_reference"`
	// SessionOffset is the position relative to the session start.
	SessionOffset int64   `json:"session_offset"`
	MaxErrorLSB   float64 `json:"max_error_lsb,omitempty"`
	// Set for processed prints.
	Chain     *ChainReport `json:"chain,omitempty"`
	Alignment *Alignment   `json:"alignment,omitempty"`
}

// Report is written to deliver/delivery.json.
type Report struct {
	Plan
	Created      time.Time     `json:"created"`
	Session      *take.Session `json:"session,omitempty"`
	Codec        string        `json:"codec"`
	Deliverables []Deliverable `json:"deliverables"`
}

// MakePlan resolves a take's transport journal against its master.
func MakePlan(dir string, minFrames int64) (*Plan, *take.Manifest, error) {
	m, err := take.ReadManifest(dir)
	if err != nil {
		return nil, nil, err
	}
	ci, err := caf.ReadFile(filepath.Join(dir, take.MasterFile))
	if err != nil {
		return nil, nil, fmt.Errorf("master: %w", err)
	}
	p := &Plan{
		MasterFrames:      ci.Frames(),
		MasterInterrupted: ci.SizeUnknown,
		SampleRate:        int(ci.SampleRate),
		Channels:          ci.Channels,
	}
	if ci.SizeUnknown {
		p.Warnings = append(p.Warnings, "master was not closed cleanly; using the frames on disk")
	}
	if ci.TrailingBytes > 0 {
		p.Warnings = append(p.Warnings, fmt.Sprintf("ignoring %d bytes of a torn final frame", ci.TrailingBytes))
	}
	if m.Session == nil {
		return nil, nil, errors.New("take has no session information")
	}
	if m.Session.SampleRate != p.SampleRate {
		// Placement assumes one master frame per timeline sample.
		return nil, nil, fmt.Errorf("master is %d Hz but the session is %d Hz; refusing to place without sample-rate conversion",
			p.SampleRate, m.Session.SampleRate)
	}
	clog, err := timeline.ReadCaptureLog(filepath.Join(dir, take.CaptureLog), p.SampleRate)
	if err != nil {
		return nil, nil, fmt.Errorf("capture log: %w", err)
	}
	if n := clog.Frames(); n < p.MasterFrames {
		p.Warnings = append(p.Warnings, fmt.Sprintf("capture log covers %d of %d master frames", n, p.MasterFrames))
	}
	evs, err := take.ReadEvents(dir)
	if err != nil {
		return nil, nil, err
	}
	var tevs []timeline.Event
	for _, e := range evs {
		te := timeline.Event{Rolling: e.Rolling, TimelineSample: e.TimelineSample}
		switch {
		case e.Frame != nil:
			te.Frame = *e.Frame
		case e.HostMicros != 0:
			if te.Frame, err = clog.FrameAt(e.HostMicros); err != nil {
				return nil, nil, err
			}
		default:
			return nil, nil, errors.New("transport event has neither frame nor host time")
		}
		p.UncertaintyFrames = max(p.UncertaintyFrames, e.UncertaintyFrames)
		tevs = append(tevs, te)
	}
	tevs = append(tevs, reanchorDropouts(tevs, clog, m.LatencyFrames, p)...)
	p.Segments, err = timeline.Segments(tevs, p.MasterFrames, m.LatencyFrames, minFrames)
	if err != nil {
		return nil, nil, err
	}
	return p, m, nil
}

// reanchorDropouts handles input dropouts: if the capture clock jumped
// while the transport was rolling, the timeline advanced by the missing
// time but the master did not, so placement after the gap is re-anchored.
// Gaps are found in master frames; transport events are in transport
// frames, which lead the master by the capture latency.
func reanchorDropouts(evs []timeline.Event, clog *timeline.CaptureLog, latency int64, p *Plan) []timeline.Event {
	var out []timeline.Event
	tolerance := int64(1_000_000/p.SampleRate) + 1 // one sample
	for _, d := range clog.Discontinuities(tolerance) {
		p.Warnings = append(p.Warnings, fmt.Sprintf("capture clock discontinuity of %d µs at master frame %d", d.MissingMicros, d.Frame))
		// Find the transport state in force when the audio at the gap was
		// produced, including re-anchors added for earlier dropouts.
		at := d.Frame - latency
		var cur *timeline.Event
		for _, set := range [][]timeline.Event{evs, out} {
			for i := range set {
				if set[i].Frame <= at && (cur == nil || set[i].Frame >= cur.Frame) {
					cur = &set[i]
				}
			}
		}
		if cur == nil || !cur.Rolling {
			continue
		}
		missing := int64(math.Round(float64(d.MissingMicros) * float64(p.SampleRate) / 1e6))
		out = append(out, timeline.Event{
			Frame:          at,
			Rolling:        true,
			TimelineSample: cur.TimelineSample + (at - cur.Frame) + missing,
		})
	}
	return out
}

// Render writes one BWF per segment into dir/deliver and verifies each.
func Render(ctx context.Context, dir string, opts Options) (*Report, error) {
	if opts.Codec == "" {
		opts.Codec = "pcm_s24le"
	}
	if opts.FFmpeg == "" {
		opts.FFmpeg = "ffmpeg"
	}
	if opts.Originator == "" {
		opts.Originator = "AntiMusic RemoteSession"
	}
	plan, m, err := MakePlan(dir, opts.MinFrames)
	if err != nil {
		return nil, err
	}
	rel := take.DeliverDir
	if len(opts.Chain) > 0 {
		if opts.Label == "" {
			opts.Label = "processed"
		}
		if opts.Renderer == "" {
			opts.Renderer = "rs-render"
		}
		rel = filepath.Join(rel, opts.Label)
	}
	out := filepath.Join(dir, rel)
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, err
	}
	rep := &Report{Plan: *plan, Created: time.Now().UTC(), Session: m.Session, Codec: opts.Codec}
	created := m.Created.UTC()
	for i, seg := range plan.Segments {
		name := fmt.Sprintf("%s_%03d.wav", filepath.Base(filepath.Clean(dir)), i+1)
		path := filepath.Join(out, name)
		tref := seg.TimelineStart
		master := filepath.Join(dir, take.MasterFile)
		encode := []string{"-c:a", opts.Codec, "-rf64", "auto", "-write_bext", "1",
			"-metadata", "time_reference=" + strconv.FormatInt(tref, 10),
			"-metadata", "originator=" + opts.Originator,
			"-metadata", fmt.Sprintf("description=master frames %d-%d", seg.MasterStart, seg.MasterStart+seg.Frames),
			"-metadata", "origination_date=" + created.Format("2006-01-02"),
			"-metadata", "origination_time=" + created.Format("15:04:05")}
		d := Deliverable{File: filepath.Join(rel, name), Segment: seg, TimeReference: tref,
			SessionOffset: tref - m.Session.StartSample}
		if len(opts.Chain) == 0 {
			args := append([]string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y", "-i", master,
				"-af", fmt.Sprintf("atrim=start_sample=%d:end_sample=%d", seg.MasterStart, seg.MasterStart+seg.Frames)},
				append(encode, path)...)
			if b, err := exec.CommandContext(ctx, opts.FFmpeg, args...).CombinedOutput(); err != nil {
				return nil, fmt.Errorf("rendering %s: %v: %s", name, err, b)
			}
			if d.MaxErrorLSB, err = Verify(master, path, seg); err != nil {
				return nil, fmt.Errorf("verifying %s: %w", name, err)
			}
		} else {
			if d.Chain, err = renderChain(ctx, opts, master, seg, plan.Channels, plan.SampleRate, opts.TailFrames, encode, path); err != nil {
				return nil, fmt.Errorf("processing %s: %w", name, err)
			}
			if err := verifyHeader(path, tref, seg.Frames+opts.TailFrames); err != nil {
				return nil, fmt.Errorf("verifying %s: %w", name, err)
			}
			al, err := CheckAlignment(master, path, seg, 4096)
			if err != nil {
				return nil, fmt.Errorf("checking alignment of %s: %w", name, err)
			}
			d.Alignment = &al
			if al.Checked && al.LagFrames != 0 {
				return nil, fmt.Errorf("%s: processed audio is %d frames off the dry signal (correlation %.2f): a plugin misreports its latency",
					name, al.LagFrames, al.Correlation)
			}
			if !al.Checked {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: alignment could not be checked against the dry signal (correlation %.2f); relying on reported latency %d",
					name, al.Correlation, d.Chain.Latency))
			}
		}
		rep.Deliverables = append(rep.Deliverables, d)
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return nil, err
	}
	return rep, take.WriteFileAtomic(filepath.Join(out, ReportFile), append(b, '\n'))
}

func verifyHeader(path string, tref, frames int64) error {
	info, err := wav.ReadFile(path)
	if err != nil {
		return err
	}
	if !info.HasBext || int64(info.Bext.TimeReference) != tref {
		return fmt.Errorf("TimeReference %d, want %d", info.Bext.TimeReference, tref)
	}
	if info.Frames() != frames {
		return fmt.Errorf("%d frames, want %d", info.Frames(), frames)
	}
	return nil
}

// Verify checks a deliverable against the master segment it was rendered
// from, reading both files directly: BWF TimeReference, length, layout,
// and every sample. It returns the largest sample difference in units of
// the deliverable's least significant bit; quantizing float to integer
// PCM may differ by at most one.
func Verify(masterPath, wavPath string, seg timeline.Segment) (float64, error) {
	info, err := wav.ReadFile(wavPath)
	if err != nil {
		return 0, err
	}
	if !info.HasBext {
		return 0, errors.New("no bext chunk")
	}
	if int64(info.Bext.TimeReference) != seg.TimelineStart {
		return 0, fmt.Errorf("TimeReference %d, want %d", info.Bext.TimeReference, seg.TimelineStart)
	}
	if info.Frames() != seg.Frames {
		return 0, fmt.Errorf("%d frames, want %d", info.Frames(), seg.Frames)
	}
	master, err := pcm.OpenCAF(masterPath)
	if err != nil {
		return 0, err
	}
	defer master.Close()
	out, err := pcm.OpenWAV(wavPath)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	if out.SampleRate != master.SampleRate || out.Channels != master.Channels {
		return 0, fmt.Errorf("layout %d Hz/%d ch, master %d Hz/%d ch", out.SampleRate, out.Channels, master.SampleRate, master.Channels)
	}
	lsb := 1.0 / (1 << 23) // float output: errors expressed in 24-bit LSBs
	if !out.Float {
		lsb = 1 / float64(int64(1)<<(out.Bits-1))
	}
	const chunk = 8192
	a := make([]float64, chunk*master.Channels)
	b := make([]float64, chunk*master.Channels)
	var maxErr float64
	for done := int64(0); done < seg.Frames; {
		n := int(min(chunk, seg.Frames-done))
		if err := master.ReadFrames(seg.MasterStart+done, n, a); err != nil {
			return 0, err
		}
		if err := out.ReadFrames(done, n, b); err != nil {
			return 0, err
		}
		for i := 0; i < n*master.Channels; i++ {
			want := a[i]
			if !out.Float { // integer output clips at full scale
				want = math.Max(-1, math.Min(1-lsb, want))
			}
			e := math.Abs(want-b[i]) / lsb
			if e > maxErr {
				maxErr = e
			}
		}
		done += int64(n)
	}
	if !out.Float && maxErr > 1 || out.Float && maxErr > 0 {
		return maxErr, fmt.Errorf("sample mismatch of %.2f LSB", maxErr)
	}
	return maxErr, nil
}
