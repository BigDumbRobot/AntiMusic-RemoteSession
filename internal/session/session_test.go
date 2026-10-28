package session

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"antimusic.dev/remotesession/internal/deliver"
	"antimusic.dev/remotesession/internal/pair"
	"antimusic.dev/remotesession/internal/take"
	"antimusic.dev/remotesession/internal/xfer"
)

// writeWAV writes a 16-bit stereo PCM WAV with no timestamp.
func writeWAV(t *testing.T, path string, frames int) {
	t.Helper()
	data := make([]byte, frames*4)
	for i := range data {
		data[i] = byte(i * 7)
	}
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+len(data)))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(2), uint32(48000), uint32(48000 * 4), uint16(4), uint16(16)} {
		binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(len(data)))
	b.Write(data)
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeCapture stands in for rs-capture: a header on stderr, then 0.5 s of
// a known stereo float32 signal, then it idles until killed.
func fakeCapture(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-capture")
	script := `#!/usr/bin/env python3
import sys, struct, time
sys.stderr.write('{"rate":48000,"channels":2}\n'); sys.stderr.flush()
out = sys.stdout.buffer
for i in range(24000):
    out.write(struct.pack('<ff', (i % 1000) / 1000.0, -0.5))
out.flush()
time.sleep(30)
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) Close() error                { return nil }
func (s *syncBuf) Len() int                    { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Len() }
func (s *syncBuf) Bytes() []byte               { s.mu.Lock(); defer s.mu.Unlock(); return bytes.Clone(s.b.Bytes()) }

func TestAttendedSession(t *testing.T) {
	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Engineer.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	studio := pair.NewCode()
	dl := pair.ListenDirect(ctx, ln, studio, t.Logf)
	defer dl.Close()
	var hostEvents, guestEvents syncEvents
	stemsIn := make(chan string, 1)
	host := &Host{Name: "Matt's room", Events: hostEvents.add,
		Takes:   &xfer.Receiver{Root: filepath.Join(tmp, "engineer"), OnComplete: func(_ context.Context, d string) (string, error) { stemsIn <- d; return "", nil }},
		Monitor: &Monitor{Device: "fake", Channels: [2]int{1, 2}, Capture: fakeCapture(t)}}
	go host.Serve(ctx, dl)

	// Client.
	code := studio.Direct(ln.Addr().(*net.TCPAddr).AddrPort())
	dial := func(ctx context.Context) (net.Conn, error) { return pair.Connect(ctx, "", code) }
	printsIn := make(chan string, 1)
	guest := &Guest{Dial: dial, Name: "Sam", Events: guestEvents.add,
		Prints: &xfer.Receiver{Root: filepath.Join(tmp, "client"), OnComplete: func(_ context.Context, d string) (string, error) { printsIn <- d; return "", nil }}}
	go guest.Run(ctx)

	waitFor(t, "client connected", func() bool { return hostEvents.has("client", "name", "Sam") })
	waitFor(t, "client sees studio", func() bool { return guestEvents.has("studio", "name", "Matt's room") })

	// Chat both ways.
	if err := guest.Chat("more air on the vocal"); err != nil {
		t.Fatal(err)
	}
	host.Chat("on it")
	waitFor(t, "chat to engineer", func() bool { return hostEvents.has("chat", "text", "more air on the vocal") })
	waitFor(t, "chat to client", func() bool { return guestEvents.has("chat", "text", "on it") })

	// Stems in.
	stems := filepath.Join(tmp, "stems_1")
	writeWAV(t, filepath.Join(tmp, "Vox.wav"), 4800)
	if _, err := Bundle(stems, take.KindStems, []string{filepath.Join(tmp, "Vox.wav")}, -1); err != nil {
		t.Fatal(err)
	}
	if _, err := (&xfer.Sender{Dir: stems, Addr: "studio", Dial: dial, Poll: 10 * time.Millisecond}).Run(ctx); err != nil {
		t.Fatal(err)
	}
	got := <-stemsIn
	assertSameFile(t, filepath.Join(stems, "deliver", "Vox.wav"), filepath.Join(got, "deliver", "Vox.wav"))

	// Listen.
	var heard syncBuf
	lctx, lcancel := context.WithCancel(ctx)
	go guest.Listen(lctx, func(rate, ch int) (io.WriteCloser, error) {
		if rate != 48000 || ch != 2 {
			t.Errorf("stream %d Hz %d ch", rate, ch)
		}
		return &heard, nil
	})
	waitFor(t, "audio", func() bool { return heard.Len() >= 24000*6 })
	lcancel()
	b := heard.Bytes()
	for i := 0; i < 24000; i += 997 {
		l := int32(uint32(b[i*6])|uint32(b[i*6+1])<<8|uint32(b[i*6+2])<<16) << 8 >> 8
		r := int32(uint32(b[i*6+3])|uint32(b[i*6+4])<<8|uint32(b[i*6+5])<<16) << 8 >> 8
		wantL := int32(float64(i%1000) / 1000 * 8388607)
		if d := l - wantL; d < -1 || d > 1 || r != -4194304 {
			t.Fatalf("frame %d: got %d,%d want %d,-4194304", i, l, r, wantL)
		}
	}

	// Prints back.
	prints := filepath.Join(tmp, "prints_1")
	writeWAV(t, filepath.Join(tmp, "Vox_print.wav"), 9600)
	if _, err := Bundle(prints, take.KindPrints, []string{filepath.Join(tmp, "Vox_print.wav")}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := host.SendPrints(ctx, prints); err != nil {
		t.Fatal(err)
	}
	got = <-printsIn
	assertSameFile(t, filepath.Join(prints, "deliver", "Vox_print.wav"), filepath.Join(got, "deliver", "Vox_print.wav"))
	assertSameFile(t, filepath.Join(prints, "deliver", deliver.ReportFile), filepath.Join(got, "deliver", deliver.ReportFile))
	m, err := take.ReadManifest(got)
	if err != nil || m.Kind != take.KindPrints {
		t.Fatalf("prints manifest %+v %v", m, err)
	}
}

type syncEvents struct {
	mu sync.Mutex
	ev []Event
}

func (s *syncEvents) add(e Event) { s.mu.Lock(); s.ev = append(s.ev, e); s.mu.Unlock() }
func (s *syncEvents) has(typ, key string, val any) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.ev {
		if e["type"] == typ && e[key] == val {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func assertSameFile(t *testing.T, a, b string) {
	t.Helper()
	x, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(x, y) {
		t.Fatalf("%s differs from %s", filepath.Base(b), a)
	}
}
