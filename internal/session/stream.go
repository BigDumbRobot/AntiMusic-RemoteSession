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
)

// Stream wire format: one JSON header line, then frames of
// [length u32 BE][interleaved 24-bit little-endian PCM].
type streamHeader struct {
	Rate     int    `json:"rate,omitempty"`
	Channels int    `json:"channels,omitempty"`
	Format   string `json:"format,omitempty"`
	Error    string `json:"error,omitempty"`
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

	mu      sync.Mutex
	subs    map[chan []byte]bool
	rate    int
	running bool
	proc    *exec.Cmd
}

// Stream sends the monitor mix to c until the client goes away.
func (m *Monitor) Stream(ctx context.Context, c net.Conn) error {
	ch, rate, err := m.subscribe()
	if err != nil {
		b, _ := json.Marshal(streamHeader{Error: err.Error()})
		c.Write(append(b, '\n'))
		return err
	}
	defer m.unsubscribe(ch)
	b, _ := json.Marshal(streamHeader{Rate: rate, Channels: 2, Format: "s24le"})
	if _, err := c.Write(append(b, '\n')); err != nil {
		return err
	}
	var hdr [4]byte
	for {
		select {
		case <-ctx.Done():
			return nil
		case buf, ok := <-ch:
			if !ok {
				return errors.New("monitor capture stopped")
			}
			binary.BigEndian.PutUint32(hdr[:], uint32(len(buf)))
			c.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := c.Write(append(hdr[:], buf...)); err != nil {
				return err
			}
		}
	}
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
	left, right := m.Channels[0]-1, m.Channels[1]-1
	if left < 0 || left >= h.Channels {
		left = 0
	}
	if right < 0 || right >= h.Channels {
		right = min(1, h.Channels-1)
	}
	go m.pump(cmd, stdout, h.Channels, left, right)
	return nil
}

// pump converts captured float32 to stereo 24-bit and fans it out. A slow
// listener loses its oldest audio rather than falling further behind.
func (m *Monitor) pump(cmd *exec.Cmd, r io.Reader, devCh, left, right int) {
	in := make([]byte, blockFrames*devCh*4)
	br := bufio.NewReaderSize(r, len(in)*8)
	for {
		if _, err := io.ReadFull(br, in); err != nil {
			break
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
		m.mu.Lock()
		for ch := range m.subs {
			select {
			case ch <- out:
			default:
				select {
				case <-ch:
				default:
				}
				select {
				case ch <- out:
				default:
				}
			}
		}
		m.mu.Unlock()
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
