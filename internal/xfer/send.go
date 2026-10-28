package xfer

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"antimusic.dev/remotesession/internal/take"
)

// Sender ships one take directory to a receiver.
type Sender struct {
	Dir  string
	Addr string // host:port, or a label for Dial in logs
	// Dial, when set, replaces a direct TCP connection to Addr (e.g. a
	// pairing-code connection through a relay).
	Dial  func(ctx context.Context) (net.Conn, error)
	Token string
	Poll  time.Duration        // how often to look for new data (default 200 ms)
	Logf  func(string, ...any) // progress and retry messages (optional)

	// DoneTimeout bounds the wait for the receiver's post-processing
	// (deliver/import) after the copy is verified. Default 10 minutes.
	DoneTimeout time.Duration
}

// Result is what the receiver reported for a completed take.
type Result struct {
	Dir     string // receiver's directory name
	Summary string
	Error   string // receiver-side post-processing error, if any
}

func (s *Sender) logf(format string, a ...any) {
	if s.Logf != nil {
		s.Logf(format, a...)
	}
}

// Run sends the take, reconnecting after failures, until the receiver
// holds an identical copy of a finished take (state complete or failed)
// or ctx ends. Recording never depends on it.
func (s *Sender) Run(ctx context.Context) (*Result, error) {
	if s.Poll <= 0 {
		s.Poll = 200 * time.Millisecond
	}
	if s.DoneTimeout <= 0 {
		s.DoneTimeout = 10 * time.Minute
	}
	backoff := time.Second
	for {
		res, err := s.session(ctx)
		if err == nil {
			return res, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if IsFatal(err) {
			return nil, err
		}
		s.logf("send: %v; retrying in %v (recording is unaffected)", err, backoff)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 10*time.Second)
	}
}

// deadlineConn applies a fresh deadline to every read and write, so a
// dead link fails instead of hanging.
type deadlineConn struct {
	net.Conn
	timeout time.Duration
}

func (c *deadlineConn) Write(p []byte) (int, error) {
	c.Conn.SetWriteDeadline(time.Now().Add(c.timeout))
	return c.Conn.Write(p)
}

func (c *deadlineConn) Read(p []byte) (int, error) {
	c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
	return c.Conn.Read(p)
}

func (s *Sender) session(ctx context.Context) (*Result, error) {
	m, err := take.ReadManifest(s.Dir)
	if err != nil {
		return nil, err
	}
	var raw net.Conn
	if s.Dial != nil {
		raw, err = s.Dial(ctx)
	} else {
		d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
		raw, err = d.DialContext(ctx, "tcp", s.Addr)
	}
	if err != nil {
		return nil, err
	}
	defer raw.Close()
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	dc := &deadlineConn{Conn: raw, timeout: 30 * time.Second}
	r := bufio.NewReader(dc)
	w := bufio.NewWriterSize(dc, 1<<16)

	name := filepath.Base(filepath.Clean(s.Dir))
	if err := writeJSON(w, msgHello, hello{Version: Version, Token: s.Token, Take: name,
		Created: m.Created.UTC().Format(time.RFC3339Nano)}); err != nil {
		return nil, err
	}
	if err := w.Flush(); err != nil {
		return nil, err
	}
	var h have
	if err := readJSON(r, msgHave, &h); err != nil {
		return nil, err
	}
	s.logf("send: connected to %s, take %q -> %q", s.Addr, name, h.Dir)

	sent := map[string]int64{}
	for _, f := range appendFiles {
		sent[f] = h.Sizes[f]
	}
	var lastManifest []byte
	lastPut := time.Now()
	buf := make([]byte, chunkSize)
	for {
		if time.Since(lastPut) > 10*time.Second {
			lastManifest = nil // heartbeat: resend the small manifest
		}
		// Read the state before shipping data, so a "complete" manifest
		// is only acted on once everything written before it is sent.
		mb, err := os.ReadFile(filepath.Join(s.Dir, take.ManifestFile))
		if err != nil {
			return nil, err
		}
		finished := false
		if mm, err := take.ReadManifest(s.Dir); err == nil {
			finished = mm.State != take.StateRecording
		}
		for _, f := range appendFiles {
			if err := s.shipGrowth(w, f, sent, buf); err != nil {
				return nil, err
			}
		}
		if !bytes.Equal(mb, lastManifest) {
			if err := writeFrame(w, msgPut, encodeData(take.ManifestFile, 0, mb)); err != nil {
				return nil, err
			}
			lastManifest, lastPut = mb, time.Now()
		}
		if err := w.Flush(); err != nil {
			return nil, err
		}
		if finished {
			dc.timeout = s.DoneTimeout
			return s.finish(r, w)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(s.Poll):
		}
	}
}

// shipGrowth sends what file f has grown by since sent[f].
func (s *Sender) shipGrowth(w io.Writer, f string, sent map[string]int64, buf []byte) error {
	fh, err := os.Open(filepath.Join(s.Dir, f))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return err
	}
	if st.Size() < sent[f] {
		sent[f] = 0 // file was replaced; the final comparison fixes the rest
	}
	for sent[f] < st.Size() {
		n, err := fh.ReadAt(buf[:min(int64(len(buf)), st.Size()-sent[f])], sent[f])
		if n > 0 {
			if err := writeFrame(w, msgData, encodeData(f, sent[f], buf[:n])); err != nil {
				return err
			}
			sent[f] += int64(n)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if n == 0 {
			break
		}
	}
	return nil
}

// finish compares block hashes with the receiver and resends differing
// blocks until the copies match, then waits for the receiver's result.
func (s *Sender) finish(r io.Reader, w *bufio.Writer) (*Result, error) {
	for round := 0; ; round++ {
		if round > 5 {
			return nil, errors.New("send: copies still differ after 5 rounds; is the take still being written?")
		}
		sums, err := sumDir(s.Dir)
		if err != nil {
			return nil, err
		}
		if err := writeJSON(w, msgFinish, finish{Files: sums}); err != nil {
			return nil, err
		}
		if err := w.Flush(); err != nil {
			return nil, err
		}
		var nd need
		if err := readJSON(r, msgNeed, &nd); err != nil {
			return nil, err
		}
		if len(nd.Blocks) == 0 {
			break
		}
		resent := 0
		buf := make([]byte, blockSize)
		for f, blocks := range nd.Blocks {
			if !validName(f) {
				return nil, fmt.Errorf("send: receiver asked for unknown file %q", f)
			}
			fh, err := os.Open(filepath.Join(s.Dir, filepath.FromSlash(f)))
			if err != nil {
				return nil, err
			}
			for _, b := range blocks {
				off := int64(b) * blockSize
				n, err := fh.ReadAt(buf, off)
				if err != nil && !errors.Is(err, io.EOF) {
					fh.Close()
					return nil, err
				}
				if err := writeFrame(w, msgData, encodeData(f, off, buf[:n])); err != nil {
					fh.Close()
					return nil, err
				}
				resent++
			}
			fh.Close()
		}
		s.logf("send: sent %d block(s) to complete the copy", resent)
	}
	var d done
	if err := readJSON(r, msgDone, &d); err != nil {
		return nil, err
	}
	return &Result{Dir: d.Dir, Summary: d.Summary, Error: d.Error}, nil
}
