package session

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"antimusic.dev/remotesession/internal/deliver"
	"antimusic.dev/remotesession/internal/live"
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
	go guest.Listen(lctx, func(f StreamFormat) (io.WriteCloser, error) {
		if f.Rate != 48000 || f.Channels != 2 || f.Format != "s24le" {
			t.Errorf("stream %+v", f)
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

// packetLog records each Write as one packet.
type packetLog struct {
	mu sync.Mutex
	p  []live.Packet
}

func (l *packetLog) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.p = append(l.p, live.Packet(bytes.Clone(b)))
	return len(b), nil
}
func (l *packetLog) Close() error { return nil }
func (l *packetLog) all() []live.Packet {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]live.Packet(nil), l.p...)
}

// With rs-live the monitor mix goes out as Opus: a hello packet, then
// one packet per 10 ms in sequence.
func TestListenOpus(t *testing.T) {
	helper, _ := filepath.Abs("../../bin/rs-live")
	if _, err := os.Stat(helper); err != nil {
		t.Skip("bin/rs-live not built")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	studio := pair.NewCode()
	dl := pair.ListenDirect(ctx, ln, studio, t.Logf)
	defer dl.Close()
	host := &Host{Name: "room", Events: func(Event) {},
		Takes:   &xfer.Receiver{Root: t.TempDir()},
		Monitor: &Monitor{Device: "fake", Channels: [2]int{1, 2}, Capture: fakeCapture(t), Live: helper}}
	go host.Serve(ctx, dl)
	code := studio.Direct(ln.Addr().(*net.TCPAddr).AddrPort())
	guest := &Guest{Dial: func(ctx context.Context) (net.Conn, error) { return pair.Connect(ctx, "", code) },
		Name: "Sam", Events: func(Event) {}, Prints: &xfer.Receiver{Root: t.TempDir()}}
	go guest.Run(ctx)

	var got packetLog
	var format StreamFormat
	lctx, lcancel := context.WithCancel(ctx)
	defer lcancel()
	go guest.Listen(lctx, func(f StreamFormat) (io.WriteCloser, error) { format = f; return &got, nil })
	waitFor(t, "packets", func() bool { return len(got.all()) >= 50 })
	lcancel()
	p := got.all()
	if format.Format != "opus" || format.Rate != 48000 {
		t.Fatalf("format %+v", format)
	}
	f, ok := live.ParseHello(p[0])
	if !ok || f.Rate != 48000 || f.Channels != 2 || f.Frame != 480 {
		t.Fatalf("first packet is not the hello: %q", p[0])
	}
	for i, k := range p[1:] {
		if k.Seq() != uint32(i) {
			t.Fatalf("packet %d has sequence %d", i, k.Seq())
		}
		if n := len(k.Payload()); n < 10 || n > 1000 {
			t.Fatalf("packet %d: %d bytes", i, n)
		}
	}
	bits := 0
	for _, k := range p[1:] {
		bits += 8 * len(k)
	}
	t.Logf("%d packets, %.0f kbit/s", len(p)-1, float64(bits)/float64(len(p)-1)/10)
}

// fakeCaptureLive plays a stereo signal in real time, 10 ms at a time.
func fakeCaptureLive(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-capture-live")
	script := `#!/usr/bin/env python3
import sys, struct, time, math
sys.stderr.write('{"rate":48000,"channels":2}\n'); sys.stderr.flush()
out = sys.stdout.buffer
start = time.time()
for b in range(6000):
    out.write(b''.join(struct.pack('<ff', 0.3*math.sin(i*0.13), 0.3*math.sin(i*0.13)) for i in range(b*480, b*480+480)))
    out.flush()
    time.sleep(max(0, start + (b+1)*0.01 - time.time()))
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// udpProxy sits between the client and the studio's UDP socket and can
// drop either direction.
type udpProxy struct {
	conn    *net.UDPConn
	studio  *net.UDPAddr
	client  atomic.Pointer[net.UDPAddr]
	dropIn  atomic.Bool // studio to client
	dropOut atomic.Bool // client to studio
}

func newUDPProxy(t *testing.T, studio *net.UDPAddr) *udpProxy {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	p := &udpProxy{conn: c, studio: studio}
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if from.Port == studio.Port {
				if a := p.client.Load(); a != nil && !p.dropIn.Load() {
					c.WriteToUDP(buf[:n], a)
				}
				continue
			}
			p.client.Store(from)
			if !p.dropOut.Load() {
				c.WriteToUDP(buf[:n], studio)
			}
		}
	}()
	t.Cleanup(func() { c.Close() })
	return p
}

type seqLog struct {
	mu    sync.Mutex
	seqs  []uint32
	paths []string
}

func (l *seqLog) Write(b []byte) (int, error) {
	p := live.Packet(b)
	if p.Seq() != live.HelloSeq {
		l.mu.Lock()
		l.seqs = append(l.seqs, p.Seq())
		l.mu.Unlock()
	}
	return len(b), nil
}
func (l *seqLog) Close() error { return nil }
func (l *seqLog) path() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.paths) == 0 {
		return ""
	}
	return l.paths[len(l.paths)-1]
}

// maxGap returns the longest run of missing sequence numbers (each
// counted once, duplicates ignored) after the first packet.
func (l *seqLog) maxGap() (gap int, n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	seen := map[uint32]bool{}
	var lo, hi uint32
	for i, s := range l.seqs {
		if i == 0 || s < lo {
			lo = s
		}
		if s > hi {
			hi = s
		}
		seen[s] = true
	}
	run := 0
	for s := lo; s <= hi; s++ {
		if seen[s] {
			run = 0
			continue
		}
		run++
		gap = max(gap, run)
	}
	return gap, len(seen)
}

// Listen audio moves to UDP, falls back to TCP when UDP stops (both ways,
// or only towards the client), and returns to UDP after an outage.
func TestListenUDP(t *testing.T) {
	helper, _ := filepath.Abs("../../bin/rs-live")
	if _, err := os.Stat(helper); err != nil {
		t.Skip("bin/rs-live not built")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	uc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	proxy := newUDPProxy(t, uc.LocalAddr().(*net.UDPAddr))
	mux := NewUDPMux(uc, proxy.conn.LocalAddr().(*net.UDPAddr).Port)
	defer mux.Close()
	studio := pair.NewCode()
	dl := pair.ListenDirect(ctx, ln, studio, t.Logf)
	defer dl.Close()
	host := &Host{Name: "room", Events: func(Event) {}, Takes: &xfer.Receiver{Root: t.TempDir()}, UDP: mux,
		Monitor: &Monitor{Device: "fake", Channels: [2]int{1, 2}, Capture: fakeCaptureLive(t), Live: helper}}
	go host.Serve(ctx, dl)
	code := studio.Direct(ln.Addr().(*net.TCPAddr).AddrPort())
	var got seqLog
	guest := &Guest{Dial: func(ctx context.Context) (net.Conn, error) { return pair.Connect(ctx, "", code) },
		Name: "Sam", Prints: &xfer.Receiver{Root: t.TempDir()}, UDPHost: netip.MustParseAddr("127.0.0.1"),
		Events: func(e Event) {
			if p, ok := e["path"].(string); ok {
				got.mu.Lock()
				got.paths = append(got.paths, p)
				got.mu.Unlock()
			}
		}}
	go guest.Run(ctx)
	lctx, lcancel := context.WithCancel(ctx)
	defer lcancel()
	go guest.Listen(lctx, func(StreamFormat) (io.WriteCloser, error) { return &got, nil })

	waitFor(t, "udp", func() bool { return got.path() == "udp" })
	time.Sleep(time.Second)
	if g, _ := got.maxGap(); g != 0 {
		t.Fatalf("gap of %d packets while switching to UDP", g)
	}

	// Both directions cut: back to TCP within about a second.
	proxy.dropIn.Store(true)
	proxy.dropOut.Store(true)
	waitFor(t, "tcp after cut", func() bool { return got.path() == "tcp" })
	time.Sleep(time.Second)
	if g, _ := got.maxGap(); g == 0 || g > 150 {
		t.Fatalf("gap of %d packets on a full cut (want about 100)", g)
	}
	// Restored: UDP again.
	proxy.dropIn.Store(false)
	proxy.dropOut.Store(false)
	waitFor(t, "udp after outage", func() bool { return got.path() == "udp" })

	// Only studio-to-client blocked: hellos still arrive but report no
	// audio, so the studio settles on TCP.
	proxy.dropIn.Store(true)
	waitFor(t, "tcp after one-way block", func() bool { return got.path() == "tcp" })
	time.Sleep(time.Second)
	n1, _ := got.maxGap()
	time.Sleep(2 * time.Second) // and it stays there: no new gaps
	g, n := got.maxGap()
	if g > 250 || g != n1 {
		t.Fatalf("one-way block: gap %d packets (was %d)", g, n1)
	}
	t.Logf("%d distinct packets, longest gap %d", n, g)
}

// The musician's live audio reaches the engineer by UDP, falls back to
// TCP when UDP is cut, and returns to UDP afterwards.
func TestPerformUDP(t *testing.T) {
	helper, _ := filepath.Abs("../../bin/rs-live")
	if _, err := os.Stat(helper); err != nil {
		t.Skip("bin/rs-live not built")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	uc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	proxy := newUDPProxy(t, uc.LocalAddr().(*net.UDPAddr))
	mux := NewUDPMux(uc, proxy.conn.LocalAddr().(*net.UDPAddr).Port)
	defer mux.Close()
	studio := pair.NewCode()
	dl := pair.ListenDirect(ctx, ln, studio, t.Logf)
	defer dl.Close()

	var got seqLog
	var format StreamFormat
	host := &Host{Name: "room", Takes: &xfer.Receiver{Root: t.TempDir()}, UDP: mux,
		Perform: func(f StreamFormat) (io.WriteCloser, error) { format = f; return &got, nil },
		Events: func(e Event) {
			if p, ok := e["path"].(string); ok && e["type"] == "perform" {
				got.mu.Lock()
				got.paths = append(got.paths, p)
				got.mu.Unlock()
			}
		}}
	go host.Serve(ctx, dl)
	code := studio.Direct(ln.Addr().(*net.TCPAddr).AddrPort())
	guest := &Guest{Dial: func(ctx context.Context) (net.Conn, error) { return pair.Connect(ctx, "", code) },
		Name: "Sam", Prints: &xfer.Receiver{Root: t.TempDir()}, UDPHost: netip.MustParseAddr("127.0.0.1"), Events: func(Event) {}}
	go guest.Run(ctx)
	src := &Monitor{Device: "fake", Channels: [2]int{1, 2}, Capture: fakeCaptureLive(t), Live: helper}
	pctx, pcancel := context.WithCancel(ctx)
	defer pcancel()
	go guest.Perform(pctx, src.Packets)

	waitFor(t, "udp", func() bool { return got.path() == "udp" })
	if format.Format != "opus" || format.Rate != 48000 {
		t.Fatalf("format %+v", format)
	}
	time.Sleep(time.Second)
	if g, _ := got.maxGap(); g != 0 {
		t.Fatalf("gap of %d packets while switching to UDP", g)
	}
	proxy.dropOut.Store(true) // client to studio is the audio direction
	waitFor(t, "tcp after cut", func() bool { return got.path() == "tcp" })
	time.Sleep(time.Second)
	g, _ := got.maxGap()
	if g == 0 || g > 200 {
		t.Fatalf("gap of %d packets on a cut", g)
	}
	proxy.dropOut.Store(false)
	waitFor(t, "udp after outage", func() bool { return got.path() == "udp" })
	time.Sleep(time.Second)
	g2, n := got.maxGap()
	if g2 != g {
		t.Fatalf("new gap of %d packets returning to UDP", g2)
	}
	t.Logf("%d distinct packets, longest gap %d (the cut)", n, g)
	pcancel()
}
