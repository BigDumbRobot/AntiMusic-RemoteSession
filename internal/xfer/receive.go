package xfer

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"antimusic.dev/remotesession/internal/take"
)

// OriginFile in a received take records which sender take it holds.
const OriginFile = ".rs-origin"

// CompleteFile marks a received take whose copy was verified identical.
const CompleteFile = ".rs-received"

// Receiver accepts takes from senders into Root/<take name>.
type Receiver struct {
	Root  string
	Token string // required from senders when set
	Logf  func(string, ...any)

	// OnComplete runs after a take's copy is verified (e.g. deliver and
	// import). Its summary or error is reported back to the sender.
	OnComplete func(ctx context.Context, dir string) (summary string, err error)
	// OnStart is told when a sender starts or resumes a take.
	OnStart func(dir string)

	mu     sync.Mutex
	active map[string]*conn // by take directory
}

type conn struct {
	c    net.Conn
	done chan struct{}
}

func (rv *Receiver) logf(format string, a ...any) {
	if rv.Logf != nil {
		rv.Logf(format, a...)
	}
}

// Serve accepts connections until ctx ends.
func (rv *Receiver) Serve(ctx context.Context, ln net.Listener) error {
	if err := os.MkdirAll(rv.Root, 0o755); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer c.Close()
			stop := context.AfterFunc(ctx, func() { c.Close() })
			defer stop()
			if err := rv.handle(ctx, c); err != nil && ctx.Err() == nil {
				rv.logf("receive: %s: %v", c.RemoteAddr(), err)
			}
		}()
	}
}

func (rv *Receiver) reject(w *bufio.Writer, msg string, fatal bool) error {
	writeJSON(w, msgError, errorMsg{Error: msg, Fatal: fatal})
	w.Flush()
	return errors.New(msg)
}

func (rv *Receiver) handle(ctx context.Context, c net.Conn) error {
	dc := &deadlineConn{Conn: c, timeout: 60 * time.Second}
	r := bufio.NewReaderSize(dc, 1<<16)
	w := bufio.NewWriter(dc)

	var h hello
	if err := readJSON(r, msgHello, &h); err != nil {
		return err
	}
	if h.Version != Version {
		return rv.reject(w, fmt.Sprintf("protocol version %d, receiver speaks %d; update both sides", h.Version, Version), true)
	}
	if rv.Token != "" && subtle.ConstantTimeCompare([]byte(h.Token), []byte(rv.Token)) != 1 {
		return rv.reject(w, "wrong token", true)
	}
	name := filepath.Base(h.Take)
	if name == "." || name == ".." || name == "/" || name == "" || strings.ContainsAny(h.Take, `/\`) || h.Created == "" {
		return rv.reject(w, fmt.Sprintf("bad take name %q", h.Take), true)
	}
	dirName, err := rv.claimDir(name, h.Created)
	if err != nil {
		return rv.reject(w, err.Error(), false)
	}
	dir := filepath.Join(rv.Root, dirName)

	// A reconnecting sender supersedes its own stale connection.
	me := &conn{c: c, done: make(chan struct{})}
	defer close(me.done)
	rv.mu.Lock()
	if rv.active == nil {
		rv.active = map[string]*conn{}
	}
	old := rv.active[dir]
	rv.active[dir] = me
	rv.mu.Unlock()
	if old != nil {
		old.c.Close()
		<-old.done
	}
	defer func() {
		rv.mu.Lock()
		if rv.active[dir] == me {
			delete(rv.active, dir)
		}
		rv.mu.Unlock()
	}()

	os.Remove(filepath.Join(dir, CompleteFile))
	files := map[string]*os.File{}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	open := func(name string) (*os.File, error) {
		if f := files[name]; f != nil {
			return f, nil
		}
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			return nil, err
		}
		files[name] = f
		return f, nil
	}

	hv := have{Dir: dirName, Sizes: map[string]int64{}}
	for _, f := range appendFiles {
		if st, err := os.Stat(filepath.Join(dir, f)); err == nil {
			hv.Sizes[f] = st.Size()
		}
	}
	if err := writeJSON(w, msgHave, hv); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	rv.logf("receive: %s: take %q -> %s", c.RemoteAddr(), h.Take, dir)
	if rv.OnStart != nil {
		rv.OnStart(dir)
	}

	var received int64
	lastLog := time.Now()
	for {
		t, p, err := readFrame(r)
		if err != nil {
			return err
		}
		switch t {
		case msgData, msgPut:
			name, off, b, err := decodeData(p)
			if err != nil {
				return err
			}
			if !knownFile(name) || (t == msgPut && name != take.ManifestFile) || off < 0 {
				return rv.reject(w, fmt.Sprintf("unexpected file %q", name), true)
			}
			if t == msgPut {
				if f := files[name]; f != nil {
					f.Close()
					delete(files, name)
				}
				if err := take.WriteFileAtomic(filepath.Join(dir, name), b); err != nil {
					return err
				}
				continue
			}
			f, err := open(name)
			if err != nil {
				return err
			}
			if _, err := f.WriteAt(b, off); err != nil {
				return err
			}
			received += int64(len(b))
			if time.Since(lastLog) > 10*time.Second {
				rv.logf("receive: %s: %.1f MB so far", dirName, float64(received)/1e6)
				lastLog = time.Now()
			}
		case msgFinish:
			var fin finish
			if err := json.Unmarshal(p, &fin); err != nil {
				return err
			}
			nd, err := rv.compare(dir, fin, files)
			if err != nil {
				return err
			}
			if err := writeJSON(w, msgNeed, nd); err != nil {
				return err
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if len(nd.Blocks) > 0 {
				continue
			}
			for _, f := range files {
				if err := f.Sync(); err != nil {
					return err
				}
			}
			if err := os.WriteFile(filepath.Join(dir, CompleteFile), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
				return err
			}
			rv.logf("receive: %s: complete and verified", dirName)
			d := done{Dir: dirName}
			if rv.OnComplete != nil {
				// The sender waits for this; keep the link alive meanwhile.
				dc.timeout = 30 * time.Minute
				sum, err := rv.OnComplete(ctx, dir)
				d.Summary = sum
				if err != nil {
					d.Error = err.Error()
					rv.logf("receive: %s: %v", dirName, err)
				}
			}
			if err := writeJSON(w, msgDone, d); err != nil {
				return err
			}
			return w.Flush()
		default:
			return rv.reject(w, fmt.Sprintf("unexpected message %d", t), true)
		}
	}
}

// compare truncates each file to the sender's size and lists the blocks
// whose hashes differ.
func (rv *Receiver) compare(dir string, fin finish, files map[string]*os.File) (need, error) {
	nd := need{Blocks: map[string][]int{}}
	for name, want := range fin.Files {
		if !knownFile(name) {
			return nd, fmt.Errorf("unexpected file %q", name)
		}
		path := filepath.Join(dir, name)
		if f := files[name]; f != nil {
			if err := f.Truncate(want.Size); err != nil {
				return nd, err
			}
		} else if err := os.Truncate(path, want.Size); err != nil && !os.IsNotExist(err) {
			return nd, err
		}
		have, err := sumFile(path)
		if err != nil {
			return nd, err
		}
		for i, b := range want.Blocks {
			if i >= len(have.Blocks) || have.Blocks[i] != b {
				nd.Blocks[name] = append(nd.Blocks[name], i)
			}
		}
	}
	for k, v := range nd.Blocks {
		if len(v) == 0 {
			delete(nd.Blocks, k)
		}
	}
	return nd, nil
}

// claimDir picks Root/<name>, or <name>-2, -3, ... when that name already
// holds a different take. It records the take's identity in OriginFile.
func (rv *Receiver) claimDir(name, created string) (string, error) {
	rv.mu.Lock()
	defer rv.mu.Unlock()
	for i := 1; i < 1000; i++ {
		d := name
		if i > 1 {
			d = name + "-" + strconv.Itoa(i)
		}
		path := filepath.Join(rv.Root, d)
		origin, err := os.ReadFile(filepath.Join(path, OriginFile))
		switch {
		case err == nil && strings.TrimSpace(string(origin)) == created:
			return d, nil
		case err == nil:
			continue // another take
		case !os.IsNotExist(err):
			return "", err
		}
		if _, err := os.Stat(path); err == nil {
			continue // a directory we did not create
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(path, OriginFile), []byte(created+"\n"), 0o644); err != nil {
			return "", err
		}
		return d, nil
	}
	return "", fmt.Errorf("too many takes named %q", name)
}
