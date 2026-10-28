package pair

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"antimusic.dev/remotesession/internal/take"
	"antimusic.dev/remotesession/internal/xfer"
)

func TestParseCode(t *testing.T) {
	c := NewCode()
	for _, s := range []string{c.String(), strings.ToLower(c.String()), strings.ReplaceAll(c.String(), "-", " "), c.String() + "\n"} {
		got, err := ParseCode(s)
		if err != nil || got != c {
			t.Fatalf("ParseCode(%q) = %v, %v; want %v", s, got, err, c)
		}
	}
	if got, err := ParseCode("oil0-0000-0000"); err != nil || got.ID != "0110" {
		t.Fatalf("lookalikes: %v %v", got, err)
	}
	for _, bad := range []string{"", "ABC", "7KQM-4MDX-R2PU", "7KQM-4MDX-R2PAX"} {
		if _, err := ParseCode(bad); err == nil {
			t.Errorf("ParseCode(%q) accepted", bad)
		}
	}
}

func startRelay(t *testing.T) (string, context.CancelFunc) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { (&Server{Logf: t.Logf}).Serve(ctx, ln); close(done) }()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	return ln.Addr().String(), stop
}

func TestPairAndEncrypt(t *testing.T) {
	relay, _ := startRelay(t)
	code := NewCode()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	l, err := Listen(ctx, relay, code, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	msg := make([]byte, 200_000) // spans several encrypted frames
	rand.Read(msg)
	got := make(chan []byte, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			got <- nil
			return
		}
		defer c.Close()
		b := make([]byte, len(msg))
		io.ReadFull(c, b)
		c.Write([]byte("thanks"))
		got <- b
	}()
	c, err := Dial(ctx, relay, code)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 6)
	if _, err := io.ReadFull(c, reply); err != nil || string(reply) != "thanks" {
		t.Fatalf("reply %q, %v", reply, err)
	}
	if !bytes.Equal(<-got, msg) {
		t.Fatal("data changed in transit")
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) Bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return bytes.Clone(l.b.Bytes())
}

// tap records what passes through the relay, to check it is ciphertext.
func TestRelaySeesOnlyCiphertext(t *testing.T) {
	relay, _ := startRelay(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	var seen lockedBuffer
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, _ := net.Dial("tcp", relay)
			go io.Copy(c, up)
			go func() { io.Copy(io.MultiWriter(up, &seen), c); up.Close() }()
		}
	}()
	code := NewCode()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	l, err := Listen(ctx, relay, code, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err == nil {
			io.Copy(io.Discard, c)
		}
	}()
	c, err := Dial(ctx, ln.Addr().String(), code)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("VERY RECOGNISABLE AUDIO PAYLOAD")
	c.Write(bytes.Repeat(secret, 100))
	time.Sleep(200 * time.Millisecond)
	c.Close()
	if bytes.Contains(seen.Bytes(), secret) || bytes.Contains(seen.Bytes(), []byte(code.Secret)) {
		t.Fatal("plaintext or the code's secret crossed the relay")
	}
}

func TestWrongCodeIsRejected(t *testing.T) {
	relay, _ := startRelay(t)
	code := NewCode()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	l, err := Listen(ctx, relay, code, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	wrong := code
	wrong.Secret = NewCode().Secret // same studio ID, wrong secret
	_, err = Dial(ctx, relay, wrong)
	if !errors.Is(err, ErrBadCode) || !xfer.IsFatal(err) {
		t.Fatalf("got %v, want a fatal ErrBadCode", err)
	}
}

func TestStudioOffline(t *testing.T) {
	relay, _ := startRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := Dial(ctx, relay, NewCode())
	if err == nil || !strings.Contains(err.Error(), "not online") {
		t.Fatalf("got %v", err)
	}
}

func TestListenerSurvivesRelayRestart(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan struct{})
	go func() { (&Server{}).Serve(ctx1, ln); close(done1) }()

	code := NewCode()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	l, err := Listen(ctx, addr, code, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }() // echo
		}
	}()

	cancel1() // relay goes down...
	<-done1
	ln2, err := net.Listen("tcp", addr) // ...and comes back on the same address
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go (&Server{}).Serve(ctx2, ln2)

	deadline := time.Now().Add(15 * time.Second)
	for {
		c, err := Dial(ctx, addr, code)
		if err == nil {
			c.Write([]byte("hi"))
			b := make([]byte, 2)
			io.ReadFull(c, b)
			c.Close()
			if string(b) != "hi" {
				t.Fatalf("echo %q", b)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("studio never came back: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// A whole take through the relay, with the sender's link cut mid-way.
func TestTakeOverRelayWithDrop(t *testing.T) {
	relay, _ := startRelay(t)
	tmp := t.TempDir()
	src := filepath.Join(tmp, "home", "take1")
	os.MkdirAll(src, 0o755)
	if err := take.WriteManifest(src, &take.Manifest{Version: 1, Created: time.Now(), State: take.StateComplete}); err != nil {
		t.Fatal(err)
	}
	master := make([]byte, 5_000_000)
	rand.Read(master)
	os.WriteFile(filepath.Join(src, take.MasterFile), master, 0o644)
	os.WriteFile(filepath.Join(src, take.CaptureLog), []byte("0 128 1 1/1000000 0 1/48000 0\n"), 0o644)
	os.WriteFile(filepath.Join(src, take.TransportFile), []byte("{}\n"), 0o644)

	code := NewCode()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	l, err := Listen(ctx, relay, code, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	rv := &xfer.Receiver{Root: filepath.Join(tmp, "studio"), Logf: t.Logf}
	go rv.Serve(ctx, l)

	cuts := 1
	s := &xfer.Sender{Dir: src, Addr: "relay", Logf: t.Logf, Poll: 10 * time.Millisecond,
		Dial: func(ctx context.Context) (net.Conn, error) {
			c, err := Dial(ctx, relay, code)
			if err != nil || cuts == 0 {
				return c, err
			}
			cuts--
			go func() { time.Sleep(30 * time.Millisecond); c.Close() }() // link drops
			return c, nil
		}}
	res, err := s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(rv.Root, res.Dir, take.MasterFile))
	if err != nil || !bytes.Equal(got, master) {
		t.Fatalf("master differs after relay transfer (%v)", err)
	}
}

func TestDirectCodeRoundTrip(t *testing.T) {
	for _, a := range []string{"70.24.77.77:7400", "1.2.3.4:65535", "192.168.2.187:40001"} {
		c := NewCode().Direct(netip.MustParseAddrPort(a))
		s := c.String()
		if len(s) != 20 {
			t.Fatalf("%q has length %d", s, len(s))
		}
		got, err := ParseCode(strings.ToLower(s))
		if err != nil || got != c {
			t.Fatalf("ParseCode(%q) = %+v, %v; want %+v", s, got, err, c)
		}
	}
	// A typo in the address part is caught, not dialled.
	c := NewCode().Direct(netip.MustParseAddrPort("70.24.77.77:7400")).String()
	bad := []byte(c)
	bad[9] = map[bool]byte{true: 'Z', false: '0'}[bad[9] != 'Z']
	if got, err := ParseCode(string(bad)); err == nil && got.Addr.String() == "70.24.77.77:7400" {
		t.Fatal("typo produced the same address")
	}
}

func TestDirectTakeWithDropAndWrongCode(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "home", "take1")
	os.MkdirAll(src, 0o755)
	take.WriteManifest(src, &take.Manifest{Version: 1, Created: time.Now(), State: take.StateComplete})
	master := make([]byte, 4_000_000)
	rand.Read(master)
	os.WriteFile(filepath.Join(src, take.MasterFile), master, 0o644)

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	studio := NewCode()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	l := ListenDirect(ctx, ln, studio, t.Logf)
	defer l.Close()
	rv := &xfer.Receiver{Root: filepath.Join(tmp, "studio"), Logf: t.Logf}
	go rv.Serve(ctx, l)
	code := studio.Direct(ln.Addr().(*net.TCPAddr).AddrPort())

	wrong := code
	wrong.Secret = NewCode().Secret
	if _, err := Connect(ctx, "", wrong); !errors.Is(err, ErrBadCode) {
		t.Fatalf("wrong code: got %v", err)
	}

	cuts := 1
	s := &xfer.Sender{Dir: src, Addr: "studio", Logf: t.Logf, Poll: 10 * time.Millisecond,
		Dial: func(ctx context.Context) (net.Conn, error) {
			c, err := Connect(ctx, "", code)
			if err == nil && cuts > 0 {
				cuts--
				go func() { time.Sleep(30 * time.Millisecond); c.Close() }()
			}
			return c, err
		}}
	res, err := s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(rv.Root, res.Dir, take.MasterFile))
	if !bytes.Equal(got, master) {
		t.Fatal("master differs after direct transfer")
	}
}
