package pair

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// Relay protocol: one text line from the client, then raw bytes.
//
//	receiver control:  LISTEN <id> 1   ← OK | ERR msg;  then ← CONN <token> / PING, → PING
//	sender:            DIAL <id> 1     ← OK (paired, now raw) | NOLISTENER | ERR msg
//	receiver data:     ACCEPT <token>  ← OK (paired, now raw) | ERR msg
//
// After OK the relay copies bytes both ways. It never sees the pairing
// secret; the two ends run the encrypted handshake through it.

const protoVersion = "1"

// DefaultRelayPort is the relay's default TCP port.
const DefaultRelayPort = 7401

// Server is the relay.
type Server struct {
	Logf func(string, ...any)

	mu        sync.Mutex
	listeners map[string]*ctrl
	pending   map[string]chan net.Conn
}

type ctrl struct {
	c   net.Conn
	wmu sync.Mutex
}

func (k *ctrl) send(line string) error {
	k.wmu.Lock()
	defer k.wmu.Unlock()
	k.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := io.WriteString(k.c, line+"\n")
	return err
}

func (s *Server) logf(format string, a ...any) {
	if s.Logf != nil {
		s.Logf(format, a...)
	}
}

// Serve accepts clients until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.mu.Lock()
	s.listeners = map[string]*ctrl{}
	s.pending = map[string]chan net.Conn{}
	s.mu.Unlock()
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
		if tc, ok := c.(*net.TCPConn); ok {
			tc.SetKeepAlive(true)
			tc.SetKeepAlivePeriod(15 * time.Second)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			stop := context.AfterFunc(ctx, func() { c.Close() }) // shutting down drops everyone
			defer stop()
			s.handle(c)
		}()
	}
}

// readLine reads one short line a byte at a time, so no bytes after it
// are consumed.
func readLine(c net.Conn) (string, error) {
	var b []byte
	one := make([]byte, 1)
	for len(b) < 128 {
		if _, err := c.Read(one); err != nil {
			return "", err
		}
		if one[0] == '\n' {
			return strings.TrimSpace(string(b)), nil
		}
		b = append(b, one[0])
	}
	return "", errors.New("line too long")
}

func validID(id string) bool {
	if len(id) != idLen {
		return false
	}
	for _, r := range id {
		if !strings.ContainsRune(alphabet, r) {
			return false
		}
	}
	return true
}

func (s *Server) handle(c net.Conn) {
	handedOff := false
	defer func() {
		if !handedOff {
			c.Close()
		}
	}()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	line, err := readLine(c)
	if err != nil {
		return
	}
	f := strings.Fields(line)
	reply := func(msg string) { io.WriteString(c, msg+"\n") }
	switch {
	case len(f) == 3 && f[0] == "LISTEN" && validID(f[1]) && f[2] == protoVersion:
		s.serveListener(c, f[1])
	case len(f) == 3 && f[0] == "DIAL" && validID(f[1]) && f[2] == protoVersion:
		s.serveDial(c, f[1])
	case len(f) == 2 && f[0] == "ACCEPT":
		s.mu.Lock()
		ch := s.pending[f[1]]
		delete(s.pending, f[1])
		s.mu.Unlock()
		if ch == nil {
			reply("ERR unknown or expired connection")
			return
		}
		c.SetDeadline(time.Time{})
		reply("OK")
		ch <- c // the dialing side now owns it
		handedOff = true
	case len(f) >= 1 && (f[0] == "LISTEN" || f[0] == "DIAL") && len(f) == 3 && f[2] != protoVersion:
		reply("ERR relay speaks protocol " + protoVersion + "; update RemoteSession")
	default:
		reply("ERR bad request")
	}
}

func (s *Server) serveListener(c net.Conn, id string) {
	k := &ctrl{c: c}
	s.mu.Lock()
	old := s.listeners[id]
	s.listeners[id] = k // a reconnecting studio replaces its stale entry
	s.mu.Unlock()
	if old != nil {
		old.c.Close()
	}
	defer func() {
		s.mu.Lock()
		if s.listeners[id] == k {
			delete(s.listeners, id)
		}
		s.mu.Unlock()
	}()
	if k.send("OK") != nil {
		return
	}
	s.logf("relay: studio %s online from %s", id, c.RemoteAddr())
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if k.send("PING") != nil {
					c.Close()
					return
				}
			}
		}
	}()
	for {
		c.SetReadDeadline(time.Now().Add(60 * time.Second)) // the client pings every 20 s
		if _, err := readLine(c); err != nil {
			s.logf("relay: studio %s offline", id)
			return
		}
	}
}

func (s *Server) serveDial(c net.Conn, id string) {
	s.mu.Lock()
	k := s.listeners[id]
	s.mu.Unlock()
	if k == nil {
		io.WriteString(c, "NOLISTENER\n")
		return
	}
	tb := make([]byte, 16)
	rand.Read(tb)
	token := hex.EncodeToString(tb)
	ch := make(chan net.Conn, 1)
	s.mu.Lock()
	s.pending[token] = ch
	s.mu.Unlock()
	if err := k.send("CONN " + token); err != nil {
		s.mu.Lock()
		delete(s.pending, token)
		s.mu.Unlock()
		io.WriteString(c, "NOLISTENER\n")
		return
	}
	var peer net.Conn
	select {
	case peer = <-ch:
	case <-time.After(15 * time.Second):
		s.mu.Lock()
		delete(s.pending, token)
		s.mu.Unlock()
		select {
		case peer = <-ch: // arrived just as we gave up
		default:
			io.WriteString(c, "ERR the studio did not answer\n")
			return
		}
	}
	c.SetDeadline(time.Time{})
	if _, err := io.WriteString(c, "OK\n"); err != nil {
		peer.Close()
		return
	}
	s.logf("relay: paired a sender with studio %s", id)
	splice(c, peer)
}

// splice copies both ways until either side closes, then closes both.
func splice(a, b net.Conn) {
	var once sync.Once
	closeBoth := func() { once.Do(func() { a.Close(); b.Close() }) }
	go func() { io.Copy(a, b); closeBoth() }()
	io.Copy(b, a)
	closeBoth()
}

// Dial connects to the studio holding code through the relay and returns
// an encrypted, authenticated connection.
func Dial(ctx context.Context, relay string, code Code) (net.Conn, error) {
	c, err := relayConn(ctx, relay, "DIAL "+code.ID+" "+protoVersion)
	if err != nil {
		return nil, err
	}
	sc, err := handshake(c, code, roleSender)
	if err != nil {
		c.Close()
		return nil, err
	}
	return sc, nil
}

// relayConn opens a connection, sends one request line and waits for OK.
func relayConn(ctx context.Context, relay, req string) (net.Conn, error) {
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	c, err := d.DialContext(ctx, "tcp", relay)
	if err != nil {
		return nil, fmt.Errorf("reaching relay %s: %w", relay, err)
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	c.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(c, req+"\n"); err != nil {
		c.Close()
		return nil, err
	}
	resp, err := readLine(c)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("relay: %w", err)
	}
	c.SetDeadline(time.Time{})
	switch {
	case resp == "OK":
		return c, nil
	case resp == "NOLISTENER":
		c.Close()
		return nil, errors.New("the studio is not online for this pairing code (is rsession receive running there?)")
	default:
		c.Close()
		return nil, fmt.Errorf("relay: %s", strings.TrimPrefix(resp, "ERR "))
	}
}

// Listener is a net.Listener that accepts senders through the relay. It
// stays registered, reconnecting to the relay after network failures.
type Listener struct {
	relay string
	code  Code
	logf  func(string, ...any)
	conns chan net.Conn
	ctx   context.Context
	stop  context.CancelFunc
	addr  net.Addr
	wg    sync.WaitGroup
}

// Listen registers the studio for code on the relay. It returns once the
// relay has accepted the first registration.
func Listen(ctx context.Context, relay string, code Code, logf func(string, ...any)) (*Listener, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ctx, stop := context.WithCancel(ctx)
	l := &Listener{relay: relay, code: code, logf: logf, conns: make(chan net.Conn), ctx: ctx, stop: stop,
		addr: relayAddr(relay)}
	codeKey(code) // derive the key now, not on the first sender
	c, err := relayConn(ctx, relay, "LISTEN "+code.ID+" "+protoVersion)
	if err != nil {
		stop()
		return nil, err
	}
	l.wg.Add(1)
	go l.run(c)
	return l, nil
}

func (l *Listener) run(c net.Conn) {
	defer l.wg.Done()
	backoff := time.Second
	for {
		if c != nil {
			backoff = time.Second
			l.control(c)
			c.Close()
		}
		if l.ctx.Err() != nil {
			return
		}
		l.logf("relay connection lost; reconnecting in %v", backoff)
		select {
		case <-l.ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
		var err error
		if c, err = relayConn(l.ctx, l.relay, "LISTEN "+l.code.ID+" "+protoVersion); err != nil {
			l.logf("relay: %v", err)
			c = nil
		} else {
			l.logf("back online at relay %s", l.relay)
		}
	}
}

// control serves one control connection until it fails.
func (l *Listener) control(c net.Conn) {
	stop := context.AfterFunc(l.ctx, func() { c.Close() })
	defer stop()
	var wmu sync.Mutex
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				wmu.Lock()
				c.SetWriteDeadline(time.Now().Add(10 * time.Second))
				_, err := io.WriteString(c, "PING\n")
				wmu.Unlock()
				if err != nil {
					c.Close()
					return
				}
			}
		}
	}()
	for {
		c.SetReadDeadline(time.Now().Add(60 * time.Second))
		line, err := readLine(c)
		if err != nil {
			return
		}
		if tok, ok := strings.CutPrefix(line, "CONN "); ok {
			l.wg.Add(1)
			go l.accept(tok)
		}
	}
}

func (l *Listener) accept(token string) {
	defer l.wg.Done()
	c, err := relayConn(l.ctx, l.relay, "ACCEPT "+token)
	if err != nil {
		if l.ctx.Err() == nil {
			l.logf("accepting a sender: %v", err)
		}
		return
	}
	stop := context.AfterFunc(l.ctx, func() { c.Close() })
	sc, err := handshake(c, l.code, roleReceiver)
	stopped := stop()
	if !stopped && err == nil {
		sc.Close()
		return
	}
	if err != nil {
		c.Close()
		if l.ctx.Err() == nil {
			l.logf("rejected a sender: %v", err)
		}
		return
	}
	select {
	case l.conns <- sc:
	case <-l.ctx.Done():
		sc.Close()
	}
}

// Accept returns the next paired, encrypted sender connection.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.ctx.Done():
		return nil, net.ErrClosed
	}
}

// Close unregisters from the relay and waits for background work to end.
func (l *Listener) Close() error {
	l.stop()
	l.wg.Wait()
	return nil
}

// Addr returns the relay's address.
func (l *Listener) Addr() net.Addr { return l.addr }

type relayAddr string

func (a relayAddr) Network() string { return "relay" }
func (a relayAddr) String() string  { return "relay " + string(a) }
