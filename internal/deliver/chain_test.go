package deliver_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"antimusic.dev/remotesession/internal/deliver"
	"antimusic.dev/remotesession/internal/take"
)

func renderer(t *testing.T) string {
	p, err := filepath.Abs("../../native/build/rs-render_artefacts/Release/rs-render")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Skip("rs-render not built (cmake --build native/build --target rs-render)")
	}
	return p
}

func recordScenario(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	raw := filepath.Join(tmp, "capture.f32")
	evs := buildCapture(t, raw, scenario)
	dir := filepath.Join(tmp, "take")
	r := record(t, dir, raw, false)
	for _, e := range evs {
		r.Events.Append(e)
	}
	if err := r.Wait(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// AUPeakLimiter has 96 samples of lookahead latency. The test signal stays
// below its threshold, so after compensation the print must equal the dry
// signal at exactly the same timeline positions.
func TestPluginPrintIsPlacedAndCompensated(t *testing.T) {
	needFFmpeg(t)
	rs := renderer(t)
	dir := recordScenario(t)
	rep, err := deliver.Render(context.Background(), dir, deliver.Options{
		Codec: "pcm_f32le", Renderer: rs, Label: "limiter",
		Chain: []deliver.Plugin{{ID: "AudioUnit:Effects/aufx,lmtr,appl"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Deliverables) != 3 {
		t.Fatalf("%d deliverables", len(rep.Deliverables))
	}
	for _, d := range rep.Deliverables {
		if d.Chain.Latency != 96 {
			t.Errorf("reported chain latency %d", d.Chain.Latency)
		}
		if d.Alignment == nil || !d.Alignment.Checked || d.Alignment.LagFrames != 0 {
			t.Errorf("alignment %+v", d.Alignment)
		}
		if !strings.HasPrefix(d.File, filepath.Join(take.DeliverDir, "limiter")) {
			t.Errorf("file %s", d.File)
		}
		checkAgainstTimeline(t, filepath.Join(dir, d.File))
	}
}

// TestMain lets the test binary stand in for rs-render: with
// FAKE_RENDER_DELAY=n it delays its input by n frames while reporting zero
// latency, like a plugin that misreports.
func TestMain(m *testing.M) {
	if d := os.Getenv("FAKE_RENDER_DELAY"); d != "" {
		fakeRender(d)
		return
	}
	os.Exit(m.Run())
}

func fakeRender(delayArg string) {
	delay, _ := strconv.Atoi(delayArg)
	var tail int64
	for i, a := range os.Args {
		if a == "--tail" && i+1 < len(os.Args) {
			tail, _ = strconv.ParseInt(os.Args[i+1], 10, 64)
		}
	}
	in, _ := io.ReadAll(os.Stdin)
	frame := 4 * channels
	frames := int64(len(in) / frame)
	out := make([]byte, (frames+tail)*int64(frame))
	copy(out[delay*frame:], in) // shifted later; the end is cut off
	os.Stdout.Write(out)
	fmt.Fprintf(os.Stderr, `{"latency":0,"plugins":[{"name":"liar","format":"test","latency":0}],"frames":%d}`+"\n", frames+tail)
}

// A plugin that delays audio without reporting it must be caught.
func TestMisreportedLatencyIsRejected(t *testing.T) {
	needFFmpeg(t)
	dir := recordScenario(t)
	exe, _ := os.Executable()
	t.Setenv("FAKE_RENDER_DELAY", "37")
	_, err := deliver.Render(context.Background(), dir, deliver.Options{
		Renderer: exe, Chain: []deliver.Plugin{{ID: "liar"}},
	})
	if err == nil || !strings.Contains(err.Error(), "37 frames off") {
		t.Fatalf("misreported latency not caught: %v", err)
	}
	t.Log(err)
}
