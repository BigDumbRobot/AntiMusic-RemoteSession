package timeline

import (
	"reflect"
	"strings"
	"testing"
)

func TestSegmentsTransportChanges(t *testing.T) {
	// Recording starts 1000 frames into the master at 01:00:00:00 @48k,
	// stops, locates, plays again, then a linear re-anchor arrives mid-run
	// (must not split), then a loop jump (must split).
	const tc1h = 172_800_000
	evs := []Event{
		{Frame: 0, Rolling: false},
		{Frame: 1000, Rolling: true, TimelineSample: tc1h},
		{Frame: 49000, Rolling: false},
		{Frame: 60000, Rolling: true, TimelineSample: tc1h + 480_000},
		{Frame: 70000, Rolling: true, TimelineSample: tc1h + 490_000}, // linear
		{Frame: 80000, Rolling: true, TimelineSample: tc1h + 480_000}, // loop
	}
	got, err := Segments(evs, 90000, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []Segment{
		{MasterStart: 1000, Frames: 48000, TimelineStart: tc1h},
		{MasterStart: 60000, Frames: 20000, TimelineStart: tc1h + 480_000},
		{MasterStart: 80000, Frames: 10000, TimelineStart: tc1h + 480_000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestSegmentsLatencyAndClipping(t *testing.T) {
	evs := []Event{{Frame: 0, Rolling: true, TimelineSample: 5000}, {Frame: 100, Rolling: false}}
	// Positive latency shifts the master range later.
	got, _ := Segments(evs, 1000, 64, 0)
	if want := []Segment{{MasterStart: 64, Frames: 100, TimelineStart: 5000}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("latency: got %+v want %+v", got, want)
	}
	// Negative latency clips the start and advances the timeline to match.
	got, _ = Segments(evs, 1000, -10, 0)
	if want := []Segment{{MasterStart: 0, Frames: 90, TimelineStart: 5010}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("clip start: got %+v want %+v", got, want)
	}
	// A run still open when the master ends is clipped to the master.
	got, _ = Segments([]Event{{Frame: 900, Rolling: true, TimelineSample: 0}}, 1000, 64, 0)
	if want := []Segment{{MasterStart: 964, Frames: 36, TimelineStart: 0}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("clip end: got %+v want %+v", got, want)
	}
	// Short segments are dropped.
	got, _ = Segments(evs, 1000, 0, 101)
	if len(got) != 0 {
		t.Fatalf("minFrames: got %+v", got)
	}
}

func TestSegmentsDuplicateFrameLastWins(t *testing.T) {
	evs := []Event{
		{Frame: 10, Rolling: true, TimelineSample: 1},
		{Frame: 10, Rolling: false},
	}
	got, _ := Segments(evs, 100, 0, 0)
	if len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestTimecodeToSamples(t *testing.T) {
	cases := []struct {
		tc   string
		rate FrameRate
		sr   int
		want int64
	}{
		{"01:00:00:00", FPS25, 48000, 172_800_000},
		{"01:00:00:00", FPS24, 48000, 172_800_000},
		{"00:00:01:12", FPS24, 48000, 72_000},
		{"01:00:00:00", FPS2997, 48000, 172_972_800},   // 108000 frames * 1001/30000 s
		{"01:00:00;00", FPS2997DF, 48000, 172_799_827}, // 107892 frames, 3599.9964 s, rounded
		{"00:01:00;02", FPS2997DF, 48000, 2_882_880},   // first frame after the drop = frame 1800
		{"10:00:00:00", FPS25, 96000, 3_456_000_000},
		{"01:00:00:00.00", FPS25, 48000, 172_800_000}, // Pro Tools' format
		{"00:00:00:00.50", FPS25, 48000, 960},         // half a frame
	}
	for _, c := range cases {
		got, err := TimecodeToSamples(c.tc, c.rate, c.sr)
		if err != nil || got != c.want {
			t.Errorf("%s @%v/%d: got %d, %v; want %d", c.tc, c.rate, c.sr, got, err, c.want)
		}
	}
	if _, err := TimecodeToSamples("00:01:00;00", FPS2997DF, 48000); err == nil {
		t.Error("nonexistent drop-frame timecode accepted")
	}
	if _, err := TimecodeToSamples("00:00:00:25", FPS25, 48000); err == nil {
		t.Error("frame 25 at 25 fps accepted")
	}
}

func TestCaptureLog(t *testing.T) {
	// Lines as written by FFmpeg: sn samp ptsi tbi pts tb. 512 frames @ 44.1k =
	// 11609.977 µs; the third block arrives 50 ms late (driver dropout);
	// the last line was torn by a crash.
	log := "0 512 1000000000 1/1000000 0 1/44100\n512 512 1000011610 1/1000000 512 1/44100\n1024 512 1000073220 1/1000000 1024 1/44100\n1536 51"
	cl, err := ParseCaptureLog(strings.NewReader(log), 44100)
	if err != nil {
		t.Fatal(err)
	}
	if cl.Frames() != 1536 {
		t.Fatalf("frames %d", cl.Frames())
	}
	d := cl.Discontinuities(1000)
	if len(d) != 1 || d[0].Frame != 1024 {
		t.Fatalf("discontinuities %+v", d)
	}
	f, _ := cl.FrameAt(1000011610 + 5805) // half a block into block 2
	if f != 512+256 {
		t.Fatalf("FrameAt = %d", f)
	}
	f, _ = cl.FrameAt(999_999_000) // before the log
	if f != -44 {
		t.Fatalf("FrameAt before = %d", f)
	}
}

func TestCaptureLogSampleTimeBase(t *testing.T) {
	// Raw PCM input has no input PTS; the encoder PTS is used instead.
	cl, err := ParseCaptureLog(strings.NewReader("0 1024 -9223372036854775808 1/48000 0 1/48000\n1024 1024 -9223372036854775808 1/48000 1024 1/48000\n"), 48000)
	if err != nil {
		t.Fatal(err)
	}
	if cl.Blocks[1].HostMicros != 21333 {
		t.Fatalf("µs = %d", cl.Blocks[1].HostMicros)
	}
	if d := cl.Discontinuities(100); len(d) != 0 {
		t.Fatalf("unexpected discontinuities %+v", d)
	}
	if _, err := ParseCaptureLog(strings.NewReader("0 1024 x 1/48000 0 1/48000\n1024 1024 1024 1/48000 1024 1/48000\n"), 48000); err == nil {
		t.Fatal("corrupt interior line accepted")
	}
}

func TestTrimBeforeCutsPreRoll(t *testing.T) {
	const start = 172_800_000
	// A run started at the session start with 4100 samples of pre-roll,
	// one entirely in pre-roll, and one after the start.
	segs := []Segment{
		{MasterStart: 1000, Frames: 48000, TimelineStart: start - 4100},
		{MasterStart: 60000, Frames: 2000, TimelineStart: start - 3000},
		{MasterStart: 90000, Frames: 500, TimelineStart: start + 100},
	}
	got, cut := TrimBefore(segs, start, 0)
	want := []Segment{
		{MasterStart: 5100, Frames: 43900, TimelineStart: start},
		{MasterStart: 90000, Frames: 500, TimelineStart: start + 100},
	}
	if !reflect.DeepEqual(got, want) || cut != 4100+2000 {
		t.Fatalf("got %+v cut %d, want %+v cut %d", got, cut, want, 6100)
	}
}
