package session

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"antimusic.dev/remotesession/internal/live"
)

// Stream wire format: one JSON header line, then frames of
// [length u32 BE][payload]. Format "opus" (when rs-live is available):
// each frame is a live.Packet (its own length prefix is the frame's), a
// hello packet first. Format "s24le": the payload is interleaved 24-bit
// little-endian PCM.
type streamHeader struct {
	Rate     int       `json:"rate,omitempty"`
	Channels int       `json:"channels,omitempty"`
	Format   string    `json:"format,omitempty"`
	UDP      *udpOffer `json:"udp,omitempty"`
	Error    string    `json:"error,omitempty"`
}

const (
	blockFrames = 480 // 10 ms at 48 kHz
	queueBlocks = 50  // per listener: at most ~0.5 s of delay builds up
)

// Monitor captures the engineer's monitor mix (e.g. Pro Tools Audio
// Bridge) once and streams it to every listening client. Capture runs only
// while someone listens.
type Monitor struct {
	Device   string // CoreAudio device name or UID
	Channels [2]int // 1-based device channels for left and right
	Capture  string // path to rs-capture
	// Live is the rs-live helper. If set, the mix is sent as Opus
	// (about 256 kbit/s instead of 2.3 Mbit/s) and played through a
	// jitter buffer with drift control; otherwise as 24-bit PCM.
	Live string

	mu      sync.Mutex
	hello   live.Packet // Opus stream format, sent to each new listener
	enc     *live.Encoder
	subs    map[chan []byte]bool
	rate    int
	running bool
	proc    *exec.Cmd
}

// Stream sends the monitor mix to c until the client goes away. With mux
// (and Opus), it offers UDP and sends by UDP while the client's hellos
// arrive, keeping c alive with the hello packet every 2 s.
func (m *Monitor) Stream(ctx context.Context, c net.Conn, mux *UDPMux) error {
	ch, rate, err := m.subscribe()
	if err != nil {
		b, _ := json.Marshal(streamHeader{Error: err.Error()})
		c.Write(append(b, '\n'))
		return err
	}
	defer m.unsubscribe(ch)
	m.mu.Lock()
	hello := m.hello
	m.mu.Unlock()
	format := "s24le"
	if hello != nil {
		format = "opus"
		if f, ok := live.ParseHello(hello); ok {
			rate = f.Rate
		}
	}
	hdrOut := streamHeader{Rate: rate, Channels: 2, Format: format}
	var peer *udpPeer
	if hello != nil && mux != nil {
		var offer udpOffer
		peer, offer = mux.register(nil)
		defer mux.unregister(peer)
		hdrOut.UDP = &offer
	}
	b, _ := json.Marshal(hdrOut)
	if _, err := c.Write(append(b, '\n')); err != nil {
		return err
	}
	if hello != nil {
		if _, err := c.Write(hello); err != nil {
			return err
		}
	}
	var hdr [4]byte
	var lastTCP time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case buf, ok := <-ch:
			if !ok {
				return errors.New("monitor capture stopped")
			}
			c.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if peer != nil && peer.usable() && mux.send(peer, buf) == nil {
				if time.Since(lastTCP) > 2*time.Second { // keep the TCP side alive
					if _, err := c.Write(hello); err != nil {
						return err
					}
					lastTCP = time.Now()
				}
				continue
			}
			lastTCP = time.Now()
			if hello != nil {
				if _, err := c.Write(buf); err != nil { // already framed
					return err
				}
				continue
			}
			binary.BigEndian.PutUint32(hdr[:], uint32(len(buf)))
			if _, err := c.Write(append(hdr[:], buf...)); err != nil {
				return err
			}
		}
	}
}

// Packets captures and encodes for one consumer (the musician's live
// audio uses this): the Opus hello, then packets until ctx ends. Live
// must be set.
func (m *Monitor) Packets(ctx context.Context) (live.Packet, <-chan []byte, error) {
	if m.Live == "" {
		return nil, nil, errors.New("rs-live is missing")
	}
	ch, _, err := m.subscribe()
	if err != nil {
		return nil, nil, err
	}
	m.mu.Lock()
	hello := m.hello
	m.mu.Unlock()
	if hello == nil {
		m.unsubscribe(ch)
		return nil, nil, errors.New("cannot encode live audio")
	}
	out := make(chan []byte, queueBlocks)
	go func() {
		defer close(out)
		defer m.unsubscribe(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case p, ok := <-ch:
				if !ok {
					return
				}
				select {
				case out <- p:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return hello, out, nil
}

func (m *Monitor) subscribe() (chan []byte, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running {
		if err := m.start(); err != nil {
			return nil, 0, err
		}
	}
	ch := make(chan []byte, queueBlocks)
	if m.subs == nil {
		m.subs = map[chan []byte]bool{}
	}
	m.subs[ch] = true
	return ch, m.rate, nil
}

func (m *Monitor) unsubscribe(ch chan []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.subs[ch] {
		return
	}
	delete(m.subs, ch)
	if len(m.subs) == 0 && m.proc != nil {
		m.proc.Process.Signal(syscall.SIGTERM)
	}
}

// start launches the capture helper. Called with m.mu held.
func (m *Monitor) start() error {
	cmd := exec.Command(m.Capture, "--device", m.Device)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting capture: %w", err)
	}
	type hdr struct {
		Rate     float64 `json:"rate"`
		Channels int     `json:"channels"`
	}
	got := make(chan hdr, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		first := true
		for sc.Scan() {
			if first {
				var h hdr
				json.Unmarshal(sc.Bytes(), &h)
				got <- h
				first = false
			}
		}
		if first {
			got <- hdr{}
		}
	}()
	var h hdr
	select {
	case h = <-got:
	case <-time.After(5 * time.Second):
	}
	if h.Rate <= 0 || h.Channels <= 0 {
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("cannot capture the listen source %q (is it connected?)", m.Device)
	}
	m.rate, m.running, m.proc = int(h.Rate), true, cmd
	m.hello, m.enc = nil, nil
	if m.Live != "" {
		enc, err := live.StartEncoder(live.EncoderConfig{Helper: m.Live, Rate: m.rate, Channels: 2})
		if err == nil {
			hello, herr := enc.Next()
			if _, ok := live.ParseHello(hello); herr == nil && ok {
				m.hello, m.enc = hello, enc
				go m.pumpPackets(enc)
			} else {
				enc.Close()
			}
		}
		// Without the helper the mix still goes out as PCM.
	}
	left, right := m.Channels[0]-1, m.Channels[1]-1
	if left < 0 || left >= h.Channels {
		left = 0
	}
	if right < 0 || right >= h.Channels {
		right = min(1, h.Channels-1)
	}
	go m.pump(cmd, stdout, h.Channels, left, right, m.enc)
	return nil
}

// pumpPackets fans the encoder's packets out to the listeners.
func (m *Monitor) pumpPackets(enc *live.Encoder) {
	for {
		p, err := enc.Next()
		if err != nil {
			return
		}
		m.fanOut(p)
	}
}

// fanOut queues buf for every listener. A slow listener loses its
// oldest audio rather than falling further behind. Called without m.mu.
func (m *Monitor) fanOut(buf []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for ch := range m.subs {
		select {
		case ch <- buf:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- buf:
			default:
			}
		}
	}
}

// pump takes the two monitor channels from the capture and sends them
// to the Opus encoder, or converts them to 24-bit PCM and fans them out.
func (m *Monitor) pump(cmd *exec.Cmd, r io.Reader, devCh, left, right int, enc *live.Encoder) {
	in := make([]byte, blockFrames*devCh*4)
	br := bufio.NewReaderSize(r, len(in)*8)
	stereo := make([]byte, blockFrames*2*4)
	for {
		if _, err := io.ReadFull(br, in); err != nil {
			break
		}
		if enc != nil {
			for f := 0; f < blockFrames; f++ {
				copy(stereo[f*8:], in[(f*devCh+left)*4:(f*devCh+left)*4+4])
				copy(stereo[f*8+4:], in[(f*devCh+right)*4:(f*devCh+right)*4+4])
			}
			if _, err := enc.In.Write(stereo); err != nil {
				break
			}
			continue
		}
		out := make([]byte, blockFrames*2*3)
		for f := 0; f < blockFrames; f++ {
			for i, c := range [2]int{left, right} {
				v := math.Float32frombits(binary.LittleEndian.Uint32(in[(f*devCh+c)*4:]))
				s := int32(math.Round(float64(max(-1, min(1, v))) * 8388607))
				o := (f*2 + i) * 3
				out[o], out[o+1], out[o+2] = byte(s), byte(s>>8), byte(s>>16)
			}
		}
		m.fanOut(out)
	}
	if enc != nil {
		enc.Close()
	}
	cmd.Wait()
	m.mu.Lock()
	for ch := range m.subs {
		close(ch)
		delete(m.subs, ch)
	}
	m.running, m.proc = false, nil
	m.mu.Unlock()
}

// readPackets copies Opus packets from br to w, one Write per packet.
func readPackets(c net.Conn, br *bufio.Reader, w io.Writer) error {
	for {
		c.SetReadDeadline(time.Now().Add(15 * time.Second))
		p, err := live.ReadPacket(br)
		if err != nil {
			return err
		}
		if _, err := w.Write(p); err != nil {
			return err
		}
	}
}

// readFrames copies stream frames from br to w.
func readFrames(c net.Conn, br *bufio.Reader, w io.Writer) error {
	var hdr [4]byte
	buf := make([]byte, 0, blockFrames*2*3*4)
	for {
		c.SetReadDeadline(time.Now().Add(15 * time.Second))
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return err
		}
		n := binary.BigEndian.Uint32(hdr[:])
		if n > 1<<20 {
			return errors.New("listen stream: oversized frame")
		}
		buf = buf[:n]
		if _, err := io.ReadFull(br, buf); err != nil {
			return err
		}
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}
}
