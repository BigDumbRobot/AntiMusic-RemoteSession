package link

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"antimusic.dev/remotesession/internal/testsignal"
)

const native = "../../native/build"

// The real plugin (VST3 build) hosted by rs-render, which drives a playing
// transport at --timeline + frame. Every published block must carry that
// timeline position and the transport flags, and the audio must be the
// input, sample for sample.
func TestPluginPublishesStampedAudio(t *testing.T) {
	render, _ := filepath.Abs(filepath.Join(native, "rs-render_artefacts/Release/rs-render"))
	plugin, _ := filepath.Abs(filepath.Join(native, "RemoteSessionLink_artefacts/Release/VST3/RemoteSession Link.vst3"))
	for _, p := range []string{render, plugin} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("%s not built (make native)", p)
		}
	}
	dir := t.TempDir()
	t.Setenv("RS_LINK_DIR", dir)

	const rate, ch, frames, start = 48000, 2, 3*48000 + 123, int64(172_800_000)
	var in bytes.Buffer
	for f := int64(0); f < frames; f++ {
		for c := 0; c < ch; c++ {
			binary.Write(&in, binary.LittleEndian, testsignal.Value(start+f, c))
		}
	}
	cmd := exec.Command(render, "--rate", "48000", "--channels", "2", "--timeline", "172800000", "--plugin", plugin)
	cmd.Stdin = bytes.NewReader(in.Bytes())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rs-render: %v", err)
	}
	if !bytes.Equal(out, in.Bytes()) {
		t.Fatal("plugin is not a bit-exact pass-through")
	}

	infos, err := List(dir)
	if err != nil || len(infos) != 1 {
		t.Fatalf("link files: %v %v", infos, err)
	}
	info := infos[0]
	if info.Channels != ch || info.SampleRate != rate || info.State != StateClosed {
		t.Fatalf("info %+v", info)
	}
	r, err := Open(info.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	blocks, lost := r.NewReader(false).Read()
	if lost != 0 {
		t.Fatalf("lost %d blocks", lost)
	}
	var next uint64
	for _, b := range blocks {
		if b.Frame != next {
			t.Fatalf("block at frame %d, want %d", b.Frame, next)
		}
		if b.Flags != Playing|HasTimeline || b.TimelineSample != start+int64(b.Frame) {
			t.Fatalf("block %d: flags %b timeline %d", b.Frame, b.Flags, b.TimelineSample)
		}
		for f := uint64(0); f < uint64(b.Frames) && b.Frame+f < frames; f++ {
			for c := 0; c < ch; c++ {
				want := testsignal.Value(b.TimelineSample+int64(f), c)
				if got := b.Audio[f*ch+uint64(c)]; math.Float32bits(got) != math.Float32bits(want) {
					t.Fatalf("frame %d ch %d: %v want %v", b.Frame+f, c, got, want)
				}
			}
		}
		next += uint64(b.Frames)
	}
	if next < frames {
		t.Fatalf("published %d frames, want >= %d", next, frames)
	}
	t.Logf("%d blocks, %d frames, every sample stamped with its timeline position", len(blocks), next)
}
