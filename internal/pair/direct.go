package pair

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"
)

// Connect reaches the studio for code: directly for a direct code, or
// through relay for a relay code.
func Connect(ctx context.Context, relay string, code Code) (net.Conn, error) {
	if !code.IsDirect() {
		if relay == "" {
			return nil, errors.New("this is a relay code, but no relay is configured")
		}
		return Dial(ctx, relay, code)
	}
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	c, err := d.DialContext(ctx, "tcp4", code.Addr.String())
	if err != nil {
		return nil, errors.New("cannot reach the studio (is rsession receive running there, and is the code current?): " + err.Error())
	}
	sc, err := handshake(c, code, roleSender)
	if err != nil {
		c.Close()
		return nil, err
	}
	return sc, nil
}

// DirectListener accepts senders on a TCP listener, completing the pairing
// handshake before handing a connection over. Anyone on the internet can
// reach the port; only holders of the code get through.
type DirectListener struct {
	ln    net.Listener
	code  Code
	logf  func(string, ...any)
	conns chan net.Conn
	ctx   context.Context
	stop  context.CancelFunc
	wg    sync.WaitGroup
}

// ListenDirect serves the pairing handshake on ln for code (any direct
// code with the same secret matches).
func ListenDirect(ctx context.Context, ln net.Listener, code Code, logf func(string, ...any)) *DirectListener {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	// Keys depend only on the secret and the "direct" salt, not the address.
	code = Code{Addr: netip.MustParseAddrPort("127.0.0.1:1"), Secret: code.Secret}
	codeKey(code)
	ctx, stop := context.WithCancel(ctx)
	l := &DirectListener{ln: ln, code: code, logf: logf, conns: make(chan net.Conn), ctx: ctx, stop: stop}
	l.wg.Add(1)
	go l.run()
	return l
}

func (l *DirectListener) run() {
	defer l.wg.Done()
	stop := context.AfterFunc(l.ctx, func() { l.ln.Close() })
	defer stop()
	sem := make(chan struct{}, 8) // handshakes in flight; strangers can't pile up
	for {
		c, err := l.ln.Accept()
		if err != nil {
			if l.ctx.Err() == nil {
				l.logf("listener: %v", err)
			}
			return
		}
		select {
		case sem <- struct{}{}:
		default:
			c.Close()
			continue
		}
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			defer func() { <-sem }()
			stop := context.AfterFunc(l.ctx, func() { c.Close() })
			sc, err := handshake(c, l.code, roleReceiver)
			if !stop() {
				c.Close()
				return
			}
			if err != nil {
				c.Close()
				if errors.Is(err, ErrBadCode) {
					l.logf("refused a connection from %s with the wrong code", c.RemoteAddr())
				}
				return
			}
			select {
			case l.conns <- sc:
			case <-l.ctx.Done():
				sc.Close()
			}
		}()
	}
}

// Accept returns the next paired, encrypted sender.
func (l *DirectListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.ctx.Done():
		return nil, net.ErrClosed
	}
}

// Close stops listening and waits for handshakes in progress.
func (l *DirectListener) Close() error {
	l.stop()
	l.wg.Wait()
	return nil
}

// Addr is the local listening address.
func (l *DirectListener) Addr() net.Addr { return l.ln.Addr() }
