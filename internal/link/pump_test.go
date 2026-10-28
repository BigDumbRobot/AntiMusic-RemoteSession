package link

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"antimusic.dev/remotesession/internal/deliver"
	"antimusic.dev/remotesession/internal/pcm"
	"antimusic.dev/remotesession/internal/recorder"
	"antimusic.dev/remotesession/internal/take"
	"antimusic.dev/remotesession/internal/testsignal"
	"antimusic.dev/remotesession/internal/wav"
)

// fakeWriter writes a link file exactly as the plugin does
// (native/link/LinkShared.h), so Pump can be driven through transport
// changes a test host cannot produce.
type fakeWriter struct {
	mem        []byte
	ch         int
	capacity   uint64
	slots      uint64
	frame      uint64
	blocks     uint64
	audioStart uint64
	// sessionStart stamps the test signal with absolute positions while the
	// published position is session-relative, as Pro Tools reports it.
	sessionStart int64
}

func newFakeWriter(t *testing.T, path string, rate float64, ch int, capacity, slots uint64) *fakeWriter {
	size := headerBytes + slots*metaBytes + capacity*uint64(ch)*4
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(size)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	w := &fakeWriter{mem: make([]byte, size), ch: ch, capacity: capacity, slots: slots, audioStart: headerBytes + slots*metaBytes}
	le := binary.LittleEndian
	copy(w.mem, "RSLINK01")
	le.PutUint32(w.mem[8:], 1)
	le.PutUint32(w.mem[12:], uint32(ch))
	le.PutUint64(w.mem[16:], math.Float64bits(rate))
	le.PutUint64(w.mem[24:], capacity)
	le.PutUint64(w.mem[32:], slots)
	le.PutUint32(w.mem[40:], uint32(os.Getpid()))
	le.PutUint32(w.mem[offState:], StateActive)
	return w
}

// block publishes one block whose audio is the session signal at tl (or
// Idle when stopped).
func (w *fakeWriter) block(n int, rolling bool, tl int64) {
	le := binary.LittleEndian
	for f := 0; f < n; f++ {
		for c := 0; c < w.ch; c++ {
			v := testsignal.Idle
			if rolling {
				v = testsignal.Value(w.sessionStart+tl+int64(f), c)
			}
			off := w.audioStart + ((w.frame+uint64(f))%w.capacity)*uint64(w.ch)*4 + uint64(c)*4
			le.PutUint32(w.mem[off:], math.Float32bits(v))
		}
	}
	m := w.mem[headerBytes+(w.blocks%w.slots)*metaBytes:]
	le.PutUint64(m[0:], w.frame)
	le.PutUint64(m[8:], uint64(tl))
	// Process-call times jitter by ±2 ms, as a real host's do.
	le.PutUint64(m[16:], uint64(1_000_000_000+int64(w.frame)*1_000_000/48000+int64(w.blocks%5)*1000-2000))
	le.PutUint32(m[24:], uint32(n))
	flags := uint32(HasTimeline)
	if rolling {
		flags |= Playing | Recording
	}
	le.PutUint32(m[28:], flags)
	le.PutUint64(m[32:], math.Float64bits(48000))
	w.frame += uint64(n)
	w.blocks++
	le.PutUint64(w.mem[offWriteFrames:], w.frame)
	le.PutUint64(w.mem[offWriteBlocks:], w.blocks)
}

func (w *fakeWriter) flush(t *testing.T, path string, state uint32) {
	binary.LittleEndian.PutUint32(w.mem[offState:], state)
	if err := os.WriteFile(path, w.mem, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLinkTakeIsPlacedExactly(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	const start = int64(172_800_000)
	tmp := t.TempDir()
	path := filepath.Join(tmp, "1-1.rslink")
	// Small ring so the reader can be made to fall behind.
	w := newFakeWriter(t, path, 48000, 2, 48000, 64)
	w.sessionStart = start
	r, err := Open(func() string { w.flush(t, path, StateActive); return path }())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	takeDir := filepath.Join(tmp, "take")
	pr, pw, _ := os.Pipe()
	rec, err := recorder.Start(recorder.Config{Dir: takeDir, Stream: pr, StreamRate: 48000, StreamChannels: 2,
		Session: &take.Session{Source: "link", SampleRate: 48000, StartTimecode: "01:00:00:00", StartSample: start}})
	if err != nil {
		t.Fatal(err)
	}
	clog, _ := os.Create(filepath.Join(takeDir, take.CaptureLog))
	pump := &Pump{Reader: r.NewReader(false), PCM: pw, Log: clog, Events: rec.Events, Poll: time.Millisecond, SessionStart: start}
	var steps []func()
	steps = append(steps, func() {
		for i := 0; i < 4; i++ {
			w.block(512, false, 0)
		}
	})
	steps = append(steps, func() {
		for i := int64(0); i < 40; i++ {
			w.block(512, true, 96000+i*512)
		}
	})
	steps = append(steps, func() {
		for i := 0; i < 8; i++ {
			w.block(512, false, 96000+40*512)
		}
	})
	steps = append(steps, func() {
		for i := int64(0); i < 20; i++ {
			w.block(512, true, 480000+i*512)
		}
		for i := int64(0); i < 10; i++ { // loop jump back
			w.block(512, true, 480000+i*512)
		}
	})
	steps = append(steps, func() {
		// 100 blocks while the reader is not looking: the 64-slot ring
		// keeps only the last 64, so 36 are lost and must be filled.
		for i := int64(0); i < 100; i++ {
			w.block(512, true, 480000+(10+i)*512)
		}
	})
	steps = append(steps, func() {
		for i := 0; i < 4; i++ {
			w.block(512, false, 0)
		}
	})
	for _, s := range steps {
		s()
		w.flush(t, path, StateActive)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		err := pump.Run(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	clog.Close()
	pw.Close()
	if err := rec.Wait(); err != nil {
		t.Fatal(err)
	}
	total := pump.Stats
	if total.LostBlocks != 36 || total.FilledFrames != 36*512 {
		t.Errorf("lost %d blocks, filled %d frames", total.LostBlocks, total.FilledFrames)
	}

	rep, err := deliver.Render(context.Background(), takeDir, deliver.Options{Codec: "pcm_f32le"})
	if err != nil {
		t.Fatal(err)
	}
	// Expected placements: record run, run at +10 s, loop restart; the
	// gap splits the loop run into before/after (the after part
	// re-anchored at its true position).
	type seg struct{ tref, frames int64 }
	want := []seg{
		{start + 96000, 40 * 512},
		{start + 480000, 20 * 512},
		{start + 480000, 10 * 512},
		{start + 480000 + (10+36)*512, 64 * 512},
	}
	if len(rep.Deliverables) != len(want) {
		t.Fatalf("deliverables %+v\nwarnings %v", rep.Deliverables, rep.Warnings)
	}
	for i, d := range rep.Deliverables {
		if (seg{d.TimeReference, d.Segment.Frames}) != want[i] {
			t.Errorf("deliverable %d: %d/%d, want %+v", i, d.TimeReference, d.Segment.Frames, want[i])
		}
		checkPlaced(t, filepath.Join(takeDir, d.File))
	}
}

// checkPlaced verifies every sample against the session signal at the
// position given by the file's own TimeReference.
func checkPlaced(t *testing.T, path string) {
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
	buf := make([]float64, int(f.Frames)*2)
	f.ReadFrames(0, int(f.Frames), buf)
	tref := int64(info.Bext.TimeReference)
	for i := int64(0); i < f.Frames; i++ {
		for c := 0; c < 2; c++ {
			if want := float64(testsignal.Value(tref+i, c)); buf[int(i)*2+c] != want {
				t.Fatalf("%s frame %d: got %v want %v", filepath.Base(path), i, buf[int(i)*2+c], want)
			}
		}
	}
}
