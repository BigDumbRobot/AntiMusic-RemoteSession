package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"antimusic.dev/remotesession/internal/xfer"
)

// Every connection is dialled by the client. Its first byte says what it
// is for. Take transfers start with an xfer frame (type 1) instead.
const (
	kindControl byte = 0x20 // chat and notices, JSON lines both ways
	kindListen  byte = 0x21 // engineer's monitor mix, engineer to client
	kindReturn  byte = 0x22 // parked for the engineer to send prints back
)

// Event is a status message for the app (one JSON line in -events mode).
type Event map[string]any

// Message is a control-channel message.
type Message struct {
	Type string    `json:"type"` // "hello", "chat", "prints"
	From string    `json:"from,omitempty"`
	Text string    `json:"text,omitempty"`
	Name string    `json:"name,omitempty"`
	Time time.Time `json:"time"`
}

// peekConn reads through a bufio.Reader that may hold bytes already peeked.
type peekConn struct {
	net.Conn
	r *bufio.Reader
}

func (p *peekConn) Read(b []byte) (int, error) { return p.r.Read(b) }

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// ---- Engineer side ----

// Host is the engineer's side of sessions.
type Host struct {
	Takes   *xfer.Receiver // stems and takes from the client
	Monitor *Monitor       // live listen source; nil disables listening
	Events  func(Event)
	Name    string

	mu       sync.Mutex
	controls map[*controlConn]bool
	returns  chan net.Conn
}

type controlConn struct {
	c   net.Conn
	wmu sync.Mutex
}

func (cc *controlConn) send(m Message) error {
	b, _ := json.Marshal(m)
	cc.wmu.Lock()
	defer cc.wmu.Unlock()
	cc.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := cc.c.Write(append(b, '\n'))
	return err
}

func (h *Host) emit(e Event) {
	if h.Events != nil {
		h.Events(e)
	}
}

// Serve accepts client connections until ctx ends.
func (h *Host) Serve(ctx context.Context, ln net.Listener) error {
	h.mu.Lock()
	h.controls = map[*controlConn]bool{}
	h.returns = make(chan net.Conn, 4)
	h.mu.Unlock()
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
			h.dispatch(ctx, c)
		}()
	}
}

func (h *Host) dispatch(ctx context.Context, c net.Conn) {
	br := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	first, err := br.Peek(1)
	c.SetReadDeadline(time.Time{})
	if err != nil {
		c.Close()
		return
	}
	pc := &peekConn{Conn: c, r: br}
	if first[0] < kindControl { // a take transfer
		defer c.Close()
		stop := context.AfterFunc(ctx, func() { c.Close() })
		defer stop()
		h.Takes.HandleConn(ctx, pc)
		return
	}
	br.ReadByte()
	switch first[0] {
	case kindControl:
		defer c.Close()
		stop := context.AfterFunc(ctx, func() { c.Close() })
		defer stop()
		h.control(pc)
	case kindListen:
		defer c.Close()
		stop := context.AfterFunc(ctx, func() { c.Close() })
		defer stop()
		if h.Monitor == nil {
			fmt.Fprintln(c, `{"error":"the engineer has not set a listen source"}`)
			return
		}
		if err := h.Monitor.Stream(ctx, c); err != nil && ctx.Err() == nil {
			h.emit(Event{"type": "log", "text": "listen stream ended: " + err.Error()})
		}
	case kindReturn:
		select {
		case h.returns <- pc: // parked until prints go back
		default:
			select { // keep the newest few
			case old := <-h.returns:
				old.Close()
			default:
			}
			h.returns <- pc
		}
	default:
		c.Close()
	}
}

func (h *Host) control(c net.Conn) {
	cc := &controlConn{c: c}
	h.mu.Lock()
	h.controls[cc] = true
	h.mu.Unlock()
	cc.send(Message{Type: "hello", From: "engineer", Name: h.Name, Time: time.Now().UTC()})
	name := ""
	defer func() {
		h.mu.Lock()
		delete(h.controls, cc)
		n := len(h.controls)
		h.mu.Unlock()
		h.emit(Event{"type": "client", "connected": n > 0, "name": name})
	}()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	for {
		c.SetReadDeadline(time.Now().Add(90 * time.Second)) // the client pings every 30 s
		if !sc.Scan() {
			return
		}
		var m Message
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m.Type {
		case "hello":
			name = m.Name
			h.emit(Event{"type": "client", "connected": true, "name": name})
		case "chat":
			h.emit(Event{"type": "chat", "from": "client", "text": m.Text, "time": m.Time})
		}
	}
}

// Chat sends a message to the connected client.
func (h *Host) Chat(text string) {
	m := Message{Type: "chat", From: "engineer", Text: text, Time: time.Now().UTC()}
	h.mu.Lock()
	cs := make([]*controlConn, 0, len(h.controls))
	for cc := range h.controls {
		cs = append(cs, cc)
	}
	h.mu.Unlock()
	for _, cc := range cs {
		cc.send(m)
	}
	h.emit(Event{"type": "chat", "from": "engineer", "text": text, "time": m.Time})
}

// SendPrints sends the take directory dir (made with Bundle) back to the
// client over a connection the client parked for it.
func (h *Host) SendPrints(ctx context.Context, dir string) (*xfer.Result, error) {
	h.mu.Lock()
	for cc := range h.controls {
		go cc.send(Message{Type: "prints", Name: dir, Time: time.Now().UTC()})
	}
	h.mu.Unlock()
	s := &xfer.Sender{Dir: dir, Addr: "client", Poll: 50 * time.Millisecond,
		Logf: func(f string, a ...any) { h.emit(Event{"type": "log", "text": fmt.Sprintf(f, a...)}) },
		Dial: func(ctx context.Context) (net.Conn, error) {
			select {
			case c := <-h.returns:
				return c, nil
			case <-time.After(20 * time.Second):
				return nil, errors.New("the client is not connected")
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}}
	return s.Run(ctx)
}

// ---- Client side ----

// Dialer opens an encrypted connection to the engineer.
type Dialer func(ctx context.Context) (net.Conn, error)

// Guest is the client's side of a session.
type Guest struct {
	Dial   Dialer
	Name   string
	Prints *xfer.Receiver // where prints from the engineer arrive
	Events func(Event)

	mu   sync.Mutex
	ctrl *controlConn
}

func (g *Guest) emit(e Event) {
	if g.Events != nil {
		g.Events(e)
	}
}

func (g *Guest) open(ctx context.Context, kind byte) (net.Conn, error) {
	c, err := g.Dial(ctx)
	if err != nil {
		return nil, err
	}
	c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte{kind}); err != nil {
		c.Close()
		return nil, err
	}
	c.SetWriteDeadline(time.Time{})
	return c, nil
}

// Run keeps the control channel and the return line up until ctx ends.
func (g *Guest) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); g.returnLine(ctx) }()
	g.controlLoop(ctx)
	wg.Wait()
	return nil
}

func backoffWait(ctx context.Context, d *time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(*d):
	}
	*d = min(*d*2, 15*time.Second)
	return true
}

func (g *Guest) controlLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		c, err := g.open(ctx, kindControl)
		if err != nil {
			g.emit(Event{"type": "studio", "connected": false, "error": err.Error()})
			if xfer.IsFatal(err) || !backoffWait(ctx, &backoff) {
				return
			}
			continue
		}
		backoff = time.Second
		cc := &controlConn{c: c}
		g.mu.Lock()
		g.ctrl = cc
		g.mu.Unlock()
		stop := context.AfterFunc(ctx, func() { c.Close() })
		done := make(chan struct{})
		go func() { // keep NATs and the engineer's read deadline happy
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-t.C:
					cc.send(Message{Type: "ping", Time: time.Now().UTC()})
				}
			}
		}()
		cc.send(Message{Type: "hello", From: "client", Name: g.Name, Time: time.Now().UTC()})
		sc := bufio.NewScanner(c)
		sc.Buffer(make([]byte, 64<<10), 64<<10)
		for sc.Scan() {
			var m Message
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			switch m.Type {
			case "hello":
				g.emit(Event{"type": "studio", "connected": true, "name": m.Name})
			case "chat":
				g.emit(Event{"type": "chat", "from": "engineer", "text": m.Text, "time": m.Time})
			case "prints":
				g.emit(Event{"type": "prints", "state": "incoming"})
			}
		}
		close(done)
		stop()
		c.Close()
		g.mu.Lock()
		g.ctrl = nil
		g.mu.Unlock()
		if ctx.Err() == nil {
			g.emit(Event{"type": "studio", "connected": false, "error": "connection lost; reconnecting"})
			backoffWait(ctx, &backoff)
		}
	}
}

// Chat sends a message to the engineer.
func (g *Guest) Chat(text string) error {
	g.mu.Lock()
	cc := g.ctrl
	g.mu.Unlock()
	if cc == nil {
		return errors.New("not connected to the studio")
	}
	m := Message{Type: "chat", From: "client", Text: text, Time: time.Now().UTC()}
	if err := cc.send(m); err != nil {
		return err
	}
	g.emit(Event{"type": "chat", "from": "client", "text": text, "time": m.Time})
	return nil
}

// returnLine keeps one connection parked at the engineer so prints can
// come back through the client's router.
func (g *Guest) returnLine(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		c, err := g.open(ctx, kindReturn)
		if err != nil {
			if xfer.IsFatal(err) || !backoffWait(ctx, &backoff) {
				return
			}
			continue
		}
		backoff = time.Second
		stop := context.AfterFunc(ctx, func() { c.Close() })
		br := bufio.NewReader(c)
		if _, err := br.Peek(1); err == nil { // the engineer started sending
			g.Prints.HandleConn(ctx, &peekConn{Conn: c, r: br})
		}
		stop()
		c.Close()
	}
}

// Listen plays the engineer's monitor mix until ctx ends, reconnecting
// after drops. open is called for each stream with its format and must
// return where the 24-bit little-endian PCM goes.
func (g *Guest) Listen(ctx context.Context, open func(rate, channels int) (io.WriteCloser, error)) error {
	backoff := time.Second
	for ctx.Err() == nil {
		err := g.listenOnce(ctx, open)
		if ctx.Err() != nil {
			return nil
		}
		g.emit(Event{"type": "listen", "state": "reconnecting", "error": errString(err)})
		if xfer.IsFatal(err) || !backoffWait(ctx, &backoff) {
			return err
		}
	}
	return nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (g *Guest) listenOnce(ctx context.Context, open func(rate, channels int) (io.WriteCloser, error)) error {
	c, err := g.open(ctx, kindListen)
	if err != nil {
		return err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	br := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(15 * time.Second))
	line, err := br.ReadBytes('\n')
	if err != nil {
		return err
	}
	var hdr streamHeader
	if err := json.Unmarshal(line, &hdr); err != nil {
		return err
	}
	if hdr.Error != "" {
		return errors.New(hdr.Error)
	}
	w, err := open(hdr.Rate, hdr.Channels)
	if err != nil {
		return err
	}
	defer w.Close()
	g.emit(Event{"type": "listen", "state": "playing", "rate": hdr.Rate})
	return readFrames(c, br, w)
}
