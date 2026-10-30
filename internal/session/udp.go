package session

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// Listen audio over UDP.
//
// TCP stalls every packet behind a lost segment (about 200 ms on a real
// link); over UDP a lost packet is just concealed. The listen stream
// starts on its TCP connection, which is already authenticated by the
// pairing code and encrypted, and the studio offers UDP in the stream
// header: a port, a random session id and a fresh AES-256 key. The
// client then sends hellos to that port; they open its own NAT and tell
// the studio where to send. Hellos go every 250 ms and say whether UDP
// audio is reaching the client. While they arrive the studio sends the
// audio by UDP; if they stop for udpTimeout it falls back to TCP, and if
// they report no audio 2 s after UDP started (blocked inbound), it stays
// on TCP for that stream.
//
// Datagram: [session id 8][counter u64 BE][AES-GCM ciphertext]. The
// nonce is a 4-byte direction ("S2C\x00" or "C2S\x00") plus the counter.
// Hellos must arrive with increasing counters, so a replayed hello
// cannot redirect the stream.

const (
	udpTimeout = time.Second
	udpHello   = 250 * time.Millisecond
	udpProbe   = 2 * time.Second // time for audio to reach the client
	udpMax     = 1500
)

var (
	dirS2C = [4]byte{'S', '2', 'C', 0}
	dirC2S = [4]byte{'C', '2', 'S', 0}
)

// udpOffer is sent to the client in the listen stream header.
type udpOffer struct {
	Port int    `json:"port"`
	ID   string `json:"id"`
	Key  string `json:"key"`
}

func udpNonce(dir [4]byte, n uint64) []byte {
	var b [12]byte
	copy(b[:], dir[:])
	binary.BigEndian.PutUint64(b[4:], n)
	return b[:]
}

func newAEAD(key []byte) cipher.AEAD {
	b, err := aes.NewCipher(key)
	if err != nil {
		panic(err) // 32-byte keys only
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		panic(err)
	}
	return g
}

// UDPMux is the studio's UDP socket, shared by every listener.
type UDPMux struct {
	conn *net.UDPConn
	port int // advertised to clients (the router's external port)

	mu    sync.Mutex
	peers map[[8]byte]*udpPeer
}

type udpPeer struct {
	id     [8]byte
	aead   cipher.AEAD
	key    []byte
	addr   atomic.Pointer[netip.AddrPort]
	last   atomic.Int64 // unix nanos of the last good hello
	helloN uint64       // highest hello counter seen (under mux.mu)
	sendN  atomic.Uint64
	rx     atomic.Bool // the last hello said UDP audio is arriving
	// Perform sessions (client to studio audio): datagrams are audio
	// packets for onAudio, accepted out of order within a replay window.
	onAudio   func([]byte)
	win       replayWindow // under mux.mu
	lastAudio atomic.Int64 // unix nanos
	// Stream's goroutine only:
	started time.Time // first UDP send
	failed  bool      // UDP audio never reached the client
}

// NewUDPMux serves conn; port is what clients send to (the router's
// external port when one is mapped).
func NewUDPMux(conn *net.UDPConn, port int) *UDPMux {
	m := &UDPMux{conn: conn, port: port, peers: map[[8]byte]*udpPeer{}}
	go m.run()
	return m
}

// Close stops the socket.
func (m *UDPMux) Close() error { return m.conn.Close() }

func (m *UDPMux) run() {
	buf := make([]byte, udpMax)
	for {
		n, from, err := m.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if n < 16 {
			continue
		}
		var id [8]byte
		copy(id[:], buf[:8])
		ctr := binary.BigEndian.Uint64(buf[8:16])
		m.mu.Lock()
		p := m.peers[id]
		if p != nil && p.onAudio != nil {
			var pt []byte
			if p.win.fresh(ctr) {
				if pt, err = p.aead.Open(nil, udpNonce(dirC2S, ctr), buf[16:n], id[:]); err == nil {
					p.win.mark(ctr)
				}
			}
			m.mu.Unlock()
			if pt != nil {
				p.lastAudio.Store(time.Now().UnixNano())
				p.onAudio(pt)
			}
			continue
		}
		if p == nil || ctr <= p.helloN {
			m.mu.Unlock()
			continue // unknown session, or a replay
		}
		pt, err := p.aead.Open(nil, udpNonce(dirC2S, ctr), buf[16:n], id[:])
		if err != nil || len(pt) != 1 {
			m.mu.Unlock()
			continue
		}
		p.rx.Store(pt[0] == 1)
		p.helloN = ctr
		m.mu.Unlock()
		a := netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
		p.addr.Store(&a)
		p.last.Store(time.Now().UnixNano())
	}
}

// register creates a session for one listener, or with onAudio for a
// client sending audio.
func (m *UDPMux) register(onAudio func([]byte)) (*udpPeer, udpOffer) {
	p := &udpPeer{key: make([]byte, 32), onAudio: onAudio}
	rand.Read(p.id[:])
	rand.Read(p.key)
	p.aead = newAEAD(p.key)
	m.mu.Lock()
	m.peers[p.id] = p
	m.mu.Unlock()
	return p, udpOffer{Port: m.port, ID: hex.EncodeToString(p.id[:]), Key: hex.EncodeToString(p.key)}
}

func (m *UDPMux) unregister(p *udpPeer) {
	m.mu.Lock()
	delete(m.peers, p.id)
	m.mu.Unlock()
}

// usable reports whether to send by UDP now. Called from Stream's
// goroutine only.
func (p *udpPeer) usable() bool {
	if p.failed {
		return false
	}
	if p.addr.Load() == nil || time.Since(time.Unix(0, p.last.Load())) >= udpTimeout {
		p.started = time.Time{} // an outage: probe again when hellos return
		return false
	}
	if p.started.IsZero() {
		p.started = time.Now()
	} else if time.Since(p.started) > udpProbe && !p.rx.Load() {
		p.failed = true // hellos get out, audio does not get in
		return false
	}
	return true
}

// send seals payload to the client's last hello address.
func (m *UDPMux) send(p *udpPeer, payload []byte) error {
	a := p.addr.Load()
	if a == nil {
		return errors.New("no client address yet")
	}
	n := p.sendN.Add(1)
	d := make([]byte, 16, 16+len(payload)+p.aead.Overhead())
	copy(d, p.id[:])
	binary.BigEndian.PutUint64(d[8:], n)
	d = p.aead.Seal(d, udpNonce(dirS2C, n), payload, p.id[:])
	_, err := m.conn.WriteToUDPAddrPort(d, *a)
	return err
}

// udpClient is the client's end of one offered session.
type udpClient struct {
	conn  *net.UDPConn
	id    [8]byte
	aead  cipher.AEAD
	n     uint64
	got   atomic.Int64 // unix nanos of the last audio datagram
	close sync.Once
}

func dialUDP(host netip.Addr, o udpOffer) (*udpClient, error) {
	id, err := hex.DecodeString(o.ID)
	if err != nil || len(id) != 8 {
		return nil, errors.New("bad UDP offer")
	}
	key, err := hex.DecodeString(o.Key)
	if err != nil || len(key) != 32 || o.Port <= 0 || o.Port > 65535 {
		return nil, errors.New("bad UDP offer")
	}
	c, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(host, uint16(o.Port))))
	if err != nil {
		return nil, err
	}
	u := &udpClient{conn: c, aead: newAEAD(key)}
	copy(u.id[:], id)
	return u, nil
}

func (u *udpClient) hello() error {
	u.n++
	d := make([]byte, 16, 16+1+16)
	copy(d, u.id[:])
	binary.BigEndian.PutUint64(d[8:], u.n)
	rx := byte(0)
	if u.receiving() {
		rx = 1
	}
	d = u.aead.Seal(d, udpNonce(dirC2S, u.n), []byte{rx}, u.id[:])
	_, err := u.conn.Write(d)
	return err
}

// recv returns the next audio payload.
func (u *udpClient) recv(buf []byte) ([]byte, error) {
	for {
		n, err := u.conn.Read(buf)
		if err != nil {
			return nil, err
		}
		if n < 16 || [8]byte(buf[:8]) != u.id {
			continue
		}
		ctr := binary.BigEndian.Uint64(buf[8:16])
		pt, err := u.aead.Open(nil, udpNonce(dirS2C, ctr), buf[16:n], u.id[:])
		if err != nil {
			continue
		}
		u.got.Store(time.Now().UnixNano())
		return pt, nil
	}
}

func (u *udpClient) receiving() bool {
	return time.Since(time.Unix(0, u.got.Load())) < udpTimeout
}

func (u *udpClient) Close() error {
	u.close.Do(func() { u.conn.Close() })
	return nil
}

// replayWindow accepts each counter once, out of order within 64.
type replayWindow struct {
	top  uint64
	bits uint64 // bit i: top-i seen
}

func (w *replayWindow) fresh(n uint64) bool {
	if n == 0 {
		return false
	}
	if n > w.top {
		return true
	}
	d := w.top - n
	return d < 64 && w.bits&(1<<d) == 0
}

func (w *replayWindow) mark(n uint64) {
	if n > w.top {
		shift := n - w.top
		if shift >= 64 {
			w.bits = 0
		} else {
			w.bits <<= shift
		}
		w.top = n
		w.bits |= 1
		return
	}
	w.bits |= 1 << (w.top - n)
}

// audioRecent reports UDP audio from the client within udpTimeout.
func (p *udpPeer) audioRecent() bool {
	return time.Since(time.Unix(0, p.lastAudio.Load())) < udpTimeout
}

// sendAudio seals one audio packet from the client.
func (u *udpClient) sendAudio(p []byte) error {
	u.n++
	d := make([]byte, 16, 16+len(p)+u.aead.Overhead())
	copy(d, u.id[:])
	binary.BigEndian.PutUint64(d[8:], u.n)
	d = u.aead.Seal(d, udpNonce(dirC2S, u.n), p, u.id[:])
	_, err := u.conn.Write(d)
	return err
}

// lockedWriter makes each Write (one packet) atomic between the TCP and
// UDP readers.
type lockedWriter struct {
	mu sync.Mutex
	w  interface{ Write([]byte) (int, error) }
}

func (l *lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

// udpListen sends hellos and passes UDP audio to w until ctx ends. It
// reports the path ("udp" or "tcp") whenever it changes.
func (g *Guest) udpListen(ctx context.Context, u *udpClient, w *lockedWriter) {
	stop := context.AfterFunc(ctx, func() { u.Close() })
	defer stop()
	go func() {
		buf := make([]byte, udpMax)
		for {
			p, err := u.recv(buf)
			if err != nil {
				return
			}
			if _, err := w.Write(p); err != nil {
				return
			}
		}
	}()
	path := ""
	for {
		u.hello()
		now := "tcp"
		if u.receiving() {
			now = "udp"
		}
		if now != path {
			path = now
			g.emit(Event{"type": "listen", "state": "playing", "path": path})
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(udpHello):
		}
	}
}
