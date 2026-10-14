package deliver_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"antimusic.dev/remotesession/internal/deliver"
	"antimusic.dev/remotesession/internal/pcm"
	"antimusic.dev/remotesession/internal/recorder"
	"antimusic.dev/remotesession/internal/take"
	"antimusic.dev/remotesession/internal/testsignal"
	"antimusic.dev/remotesession/internal/timeline"
	"antimusic.dev/remotesession/internal/wav"
)

const (
	rate       = 48000
	channels   = 2
	startTC    = 172_800_000 // 01:00:00:00 @ 48 kHz
	latency    = 37          // simulated capture-path delay in frames
	idleFrames = 1000
)

// span is one stretch of a simulated session: the transport is either
// stopped or rolling from a timeline position.
type span struct {
	frames  int64
	rolling bool
	from    int64 // session offset when rolling
}

// scenario: stopped, record 1 s at bar 1, stop, locate and play 0.5 s at
// +10 s, then a loop jump back to +10 s for 0.25 s, then stop.
var scenario = []span{
	{idleFrames, false, 0},
	{48000, true, 0},
	{11000, false, 0},
	{24000, true, 480_000},
	{12000, true, 480_000},
	{5000, false, 0},
}

// buildCapture writes what the input device would deliver (audio arriving
// `latency` frames after the transport position it belongs to) and returns
// the transport events as the DAW would report them.
func buildCapture(t *testing.T, path string, spans []span) []take.Event {
	t.Helper()
	var samples []float32
	var evs []take.Event
	frame := int64(0)
	for i := 0; i < latency*channels; i++ {
		samples = append(samples, testsignal.Idle)
	}
	for _, s := range spans {
		f := frame
		evs = append(evs, take.Event{Frame: &f, Rolling: s.rolling, TimelineSample: startTC + s.from, Source: "simulated"})
		for i := int64(0); i < s.frames; i++ {
			for c := 0; c < channels; c++ {
				v := testsignal.Idle
				if s.rolling {
					v = testsignal.Value(startTC+s.from+i, c)
				}
				samples = append(samples, v)
			}
		}
		frame += s.frames
	}
	b := make([]byte, 4*len(samples))
	for i, v := range samples {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(v))
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return evs
}

func record(t *testing.T, dir, raw string, realtime bool) *recorder.Recorder {
	t.Helper()
	in := []string{"-f", "f32le", "-ar", fmt.Sprint(rate), "-ch_layout", "stereo", "-i", raw}
	if realtime {
		in = append([]string{"-re"}, in...)
	}
	r, err := recorder.Start(recorder.Config{
		Dir: dir, Input: in, LatencyFrames: latency,
		Session: &take.Session{Source: "simulated", SampleRate: rate, StartTimecode: "01:00:00:00", StartSample: startTC},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// checkAgainstTimeline verifies a deliverable without reference to the
// master: every sample must equal the session signal at the timeline
// position implied by the file's own bext TimeReference.
func checkAgainstTimeline(t *testing.T, path string) (tref int64, frames int64) {
	t.Helper()
	info, err := wav.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := pcm.OpenWAV(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tref = int64(info.Bext.TimeReference)
	buf := make([]float64, int(f.Frames)*channels)
	if err := f.ReadFrames(0, int(f.Frames), buf); err != nil {
		t.Fatal(err)
	}
	for i := int64(0); i < f.Frames; i++ {
		for c := 0; c < channels; c++ {
			if want := float64(testsignal.Value(tref+i, c)); buf[int(i)*channels+c] != want {
				t.Fatalf("%s frame %d ch %d: got %v want %v (timeline %d)", filepath.Base(path), i, c, buf[int(i)*channels+c], want, tref+i)
			}
		}
	}
	return tref, f.Frames
}

func needFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
}

func TestRecordAndDeliverKnownTimeline(t *testing.T) {
	needFFmpeg(t)
	tmp := t.TempDir()
	raw := filepath.Join(tmp, "capture.f32")
	evs := buildCapture(t, raw, scenario)
	dir := filepath.Join(tmp, "take1")
	r := record(t, dir, raw, false)
	for _, e := range evs {
		if err := r.Events.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Wait(); err != nil {
		t.Fatalf("%v\n%s", err, r.Stderr())
	}
	m, _ := take.ReadManifest(dir)
	if m.State != take.StateComplete || m.SampleRate != rate || m.Channels != channels {
		t.Fatalf("manifest %+v", m)
	}

	for _, codec := range []string{"pcm_s24le", "pcm_f32le"} {
		rep, err := deliver.Render(context.Background(), dir, deliver.Options{Codec: codec})
		if err != nil {
			t.Fatal(err)
		}
		want := []struct{ tref, frames int64 }{
			{startTC, 48000},
			{startTC + 480_000, 24000},
			{startTC + 480_000, 12000},
		}
		if len(rep.Deliverables) != len(want) {
			t.Fatalf("%s: %d deliverables, want %d: %+v", codec, len(rep.Deliverables), len(want), rep.Deliverables)
		}
		for i, d := range rep.Deliverables {
			tref, frames := checkAgainstTimeline(t, filepath.Join(dir, d.File))
			if tref != want[i].tref || frames != want[i].frames {
				t.Errorf("%s deliverable %d: tref %d frames %d, want %+v", codec, i, tref, frames, want[i])
			}
			if d.SessionOffset != want[i].tref-startTC {
				t.Errorf("session offset %d", d.SessionOffset)
			}
		}
	}
}

// A recorder killed mid-take (crash, power loss of the process) must leave
// a master whose captured portion still delivers and places exactly.
func TestInterruptedTakeStillDelivers(t *testing.T) {
	needFFmpeg(t)
	tmp := t.TempDir()
	raw := filepath.Join(tmp, "capture.f32")
	evs := buildCapture(t, raw, scenario)
	dir := filepath.Join(tmp, "take2")
	r := record(t, dir, raw, true) // real-time pacing
	for _, e := range evs {
		if err := r.Events.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(1300 * time.Millisecond) // into the second rolling span
	r.Abort()

	m, _ := take.ReadManifest(dir)
	if m.State != take.StateRecording {
		t.Fatalf("state %q after crash", m.State)
	}
	if out, err := exec.Command("ffprobe", "-v", "error", filepath.Join(dir, take.MasterFile)).CombinedOutput(); err != nil {
		t.Fatalf("ffprobe rejects crashed master: %s", out)
	}
	rep, err := deliver.Render(context.Background(), dir, deliver.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.MasterInterrupted {
		t.Error("crash not detected")
	}
	if len(rep.Deliverables) < 1 || rep.Deliverables[0].Segment.Frames != 48000 {
		t.Fatalf("deliverables %+v (master frames %d)", rep.Deliverables, rep.MasterFrames)
	}
	for _, d := range rep.Deliverables {
		checkAgainstTimeline(t, filepath.Join(dir, d.File))
	}
	t.Logf("crashed master: %d frames (%.3f s) on disk, %d deliverables, warnings: %s",
		rep.MasterFrames, float64(rep.MasterFrames)/rate, len(rep.Deliverables), strings.Join(rep.Warnings, "; "))
}

// An input dropout while rolling: the device lost 480 frames, so the
// capture clock jumps 10 ms and the audio after the gap belongs 480
// samples later on the timeline.
func TestDropoutIsReanchored(t *testing.T) {
	needFFmpeg(t)
	tmp := t.TempDir()
	raw := filepath.Join(tmp, "capture.f32")
	const gapAt, lost = int64(20480), int64(480) // gap on a 1024-frame block boundary
	spans := []span{
		{idleFrames, false, 0},
		{gapAt - idleFrames - latency, true, 0},
		{48000, true, gapAt - idleFrames - latency + lost}, // content skips 480
	}
	evs := buildCapture(t, raw, spans)
	dir := filepath.Join(tmp, "take3")
	r := record(t, dir, raw, false)
	// Only the transport start is reported; the DAW never saw the dropout.
	for _, e := range evs[:2] {
		r.Events.Append(e)
	}
	if err := r.Wait(); err != nil {
		t.Fatal(err)
	}
	// Rewrite the capture log as the device would have timed it: blocks
	// at or after the gap arrive `lost` samples late.
	cl, err := timeline.ReadCaptureLog(filepath.Join(dir, take.CaptureLog), rate)
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, b := range cl.Blocks {
		pts := b.Frame
		if b.Frame >= gapAt {
			pts += lost
		}
		fmt.Fprintf(&sb, "%d %d %d 1/%d 0 1/%d\n", b.Frame, b.Samples, pts, rate, rate)
	}
	os.WriteFile(filepath.Join(dir, take.CaptureLog), []byte(sb.String()), 0o644)

	rep, err := deliver.Render(context.Background(), dir, deliver.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Deliverables) != 2 {
		t.Fatalf("deliverables %+v warnings %v", rep.Deliverables, rep.Warnings)
	}
	for _, d := range rep.Deliverables {
		checkAgainstTimeline(t, filepath.Join(dir, d.File))
	}
	if got := rep.Deliverables[1].TimeReference - rep.Deliverables[0].TimeReference; got != gapAt-latency-idleFrames+lost {
		t.Errorf("post-gap offset %d", got)
	}
}
