package xfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"antimusic.dev/remotesession/internal/take"
)

// fakeTake writes a take the way the recorder does: a manifest, a master
// that grows, a header rewritten on close, and appended logs.
type fakeTake struct {
	dir string
	m   *take.Manifest
}

func newFakeTake(t *testing.T, dir string, created time.Time) *fakeTake {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ft := &fakeTake{dir: dir, m: &take.Manifest{Version: 1, Created: created, MasterCodec: "pcm_f32le", State: take.StateRecording}}
	if err := take.WriteManifest(dir, ft.m); err != nil {
		t.Fatal(err)
	}
	// CAF-like header with an unknown data size.
	ft.appendTo(t, take.MasterFile, bytes.Repeat([]byte{0xff}, 4096))
	return ft
}

func (ft *fakeTake) appendTo(t *testing.T, name string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(ft.dir, name), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
}

func (ft *fakeTake) record(t *testing.T, chunks int) {
	t.Helper()
	for i := 0; i < chunks; i++ {
		b := make([]byte, 300_000)
		rand.Read(b)
		ft.appendTo(t, take.MasterFile, b)
		ft.appendTo(t, take.CaptureLog, []byte("0 128 1 1/1000000 0 1/48000 0\n"))
		if i%3 == 0 {
			ft.appendTo(t, take.TransportFile, []byte(`{"frame":0,"rolling":true,"timeline_sample":0,"source":"test"}`+"\n"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// close rewrites the header in place and marks the take complete.
func (ft *fakeTake) close(t *testing.T) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(ft.dir, take.MasterFile), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{0x11}, 4096), 0); err != nil {
		t.Fatal(err)
	}
	f.Close()
	ft.m.State = take.StateComplete
	if err := take.WriteManifest(ft.dir, ft.m); err != nil {
		t.Fatal(err)
	}
}

func startReceiver(t *testing.T, rv *Receiver) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { rv.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return ln.Addr().String()
}

func sameFiles(t *testing.T, a, b string) {
	t.Helper()
	for _, f := range []string{take.ManifestFile, take.MasterFile, take.CaptureLog, take.TransportFile} {
		x, err := os.ReadFile(filepath.Join(a, f))
		if err != nil {
			t.Fatal(err)
		}
		y, err := os.ReadFile(filepath.Join(b, f))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(x, y) {
			t.Fatalf("%s differs: %d vs %d bytes", f, len(x), len(y))
		}
	}
}

func TestSendWhileRecording(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "home", "take1")
	ft := newFakeTake(t, src, time.Now())
	var completed atomic.Value
	rv := &Receiver{Root: filepath.Join(tmp, "work"), Token: "s3cret", Logf: t.Logf,
		OnComplete: func(_ context.Context, dir string) (string, error) {
			completed.Store(dir)
			return "delivered 1 file", nil
		}}
	addr := startReceiver(t, rv)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); ft.record(t, 30); ft.close(t) }()
	s := &Sender{Dir: src, Addr: addr, Token: "s3cret", Poll: 10 * time.Millisecond, Logf: t.Logf}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := s.Run(ctx)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(rv.Root, "take1")
	if res.Dir != "take1" || res.Summary != "delivered 1 file" || completed.Load() != dst {
		t.Fatalf("result %+v, completed %v", res, completed.Load())
	}
	sameFiles(t, src, dst)
	if _, err := os.Stat(filepath.Join(dst, CompleteFile)); err != nil {
		t.Fatal("no completion marker:", err)
	}
}

// cutProxy forwards to addr and kills the first connection after limit
// bytes from the sender.
func cutProxy(t *testing.T, addr string, limit int64) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var first atomic.Bool
	first.Store(true)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", addr)
			if err != nil {
				c.Close()
				continue
			}
			cut := first.Swap(false)
			go func() {
				if cut {
					io.CopyN(up, c, limit)
					c.Close()
					up.Close()
					return
				}
				io.Copy(up, c)
				up.Close()
			}()
			go func() { io.Copy(c, up); c.Close() }()
		}
	}()
	return ln.Addr().String()
}

func TestResumeAfterDroppedConnection(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "home", "take2")
	ft := newFakeTake(t, src, time.Now())
	ft.record(t, 20) // ~6 MB already on disk
	ft.close(t)
	rv := &Receiver{Root: filepath.Join(tmp, "work"), Logf: t.Logf}
	addr := cutProxy(t, startReceiver(t, rv), 2_500_000)

	s := &Sender{Dir: src, Addr: addr, Poll: 10 * time.Millisecond, Logf: t.Logf}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Dir != "take2" {
		t.Fatalf("resumed into %q, want take2", res.Dir)
	}
	sameFiles(t, src, filepath.Join(rv.Root, "take2"))
	if _, err := os.Stat(filepath.Join(rv.Root, "take2-2")); err == nil {
		t.Fatal("resume created a second directory")
	}
}

func TestSameNameDifferentTake(t *testing.T) {
	tmp := t.TempDir()
	rv := &Receiver{Root: filepath.Join(tmp, "work")}
	addr := startReceiver(t, rv)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i, want := range []string{"take", "take-2"} {
		src := filepath.Join(tmp, "home"+string(rune('a'+i)), "take")
		ft := newFakeTake(t, src, time.Now().Add(time.Duration(i)*time.Second))
		ft.record(t, 2)
		ft.close(t)
		res, err := (&Sender{Dir: src, Addr: addr, Poll: 10 * time.Millisecond}).Run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if res.Dir != want {
			t.Fatalf("take %d went to %q, want %q", i, res.Dir, want)
		}
		sameFiles(t, src, filepath.Join(rv.Root, want))
	}
}

func TestWrongTokenIsFatal(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "home", "take")
	newFakeTake(t, src, time.Now())
	addr := startReceiver(t, &Receiver{Root: filepath.Join(tmp, "work"), Token: "right"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := (&Sender{Dir: src, Addr: addr, Token: "wrong"}).Run(ctx)
	if !IsFatal(err) {
		t.Fatalf("got %v, want a fatal token error", err)
	}
}
