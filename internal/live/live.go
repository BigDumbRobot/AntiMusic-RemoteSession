// Package live runs the rs-live helper (native/live) for low-latency
// monitoring: Opus encoding on the sending side, and on the playing side
// a jitter buffer with drift control in front of CoreAudio. Go only moves
// packets between them.
//
// Packet framing (the helper's stdin/stdout, and the network):
// [u32 BE length][u32 BE sequence][Opus frame]. The stream begins with a
// hello packet (sequence HelloSeq) whose body is the JSON format.
package live

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
)

// HelloSeq marks the format packet at the start of a stream.
const HelloSeq = 0xFFFFFFFF

const maxPacket = 4000 + 4

// Packet is one framed packet, header included, ready to forward.
type Packet []byte

// Seq returns the packet's sequence number.
func (p Packet) Seq() uint32 { return binary.BigEndian.Uint32(p[4:8]) }

// Payload returns the Opus frame (or the hello JSON).
func (p Packet) Payload() []byte { return p[8:] }

// ReadPacket reads one framed packet.
func ReadPacket(r io.Reader) (Packet, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(h[:])
	if n < 4 || n > maxPacket {
		return nil, fmt.Errorf("live: bad packet length %d", n)
	}
	p := make(Packet, 4+n)
	copy(p, h[:])
	if _, err := io.ReadFull(r, p[4:]); err != nil {
		return nil, err
	}
	return p, nil
}

// MakePacket frames a payload.
func MakePacket(seq uint32, payload []byte) Packet {
	p := make(Packet, 8+len(payload))
	binary.BigEndian.PutUint32(p, uint32(4+len(payload)))
	binary.BigEndian.PutUint32(p[4:], seq)
	copy(p[8:], payload)
	return p
}

// Format is the hello packet's body.
type Format struct {
	Rate     int `json:"rate"`
	Channels int `json:"channels"`
	Frame    int `json:"frame"`
}

// ParseHello returns the format if p is a hello packet.
func ParseHello(p Packet) (Format, bool) {
	var f Format
	if p.Seq() != HelloSeq || json.Unmarshal(p.Payload(), &f) != nil {
		return f, false
	}
	return f, true
}

// FindHelper locates rs-live next to the running executable, then in
// $RS_BIN, then on $PATH.
func FindHelper() string {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "rs-live")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if d := os.Getenv("RS_BIN"); d != "" {
		return filepath.Join(d, "rs-live")
	}
	return "rs-live"
}

// Encoder turns interleaved float32 PCM written to In into packets.
type Encoder struct {
	In  io.WriteCloser
	cmd *exec.Cmd
	out *bufio.Reader
}

// EncoderConfig sets the stream. Frame is in 48 kHz samples (120, 240,
// 480 or 960; default 480 = 10 ms). Bitrate defaults to 256 kbit/s.
type EncoderConfig struct {
	Helper   string
	Rate     int
	Channels int
	Frame    int
	Bitrate  int
}

// StartEncoder launches rs-live encode.
func StartEncoder(c EncoderConfig) (*Encoder, error) {
	if c.Helper == "" {
		c.Helper = FindHelper()
	}
	if c.Channels == 0 {
		c.Channels = 2
	}
	args := []string{"encode", "--rate", strconv.Itoa(c.Rate), "--channels", strconv.Itoa(c.Channels)}
	if c.Frame > 0 {
		args = append(args, "--frame", strconv.Itoa(c.Frame))
	}
	if c.Bitrate > 0 {
		args = append(args, "--bitrate", strconv.Itoa(c.Bitrate))
	}
	cmd := exec.Command(c.Helper, args...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting rs-live: %w", err)
	}
	return &Encoder{In: in, cmd: cmd, out: bufio.NewReaderSize(out, 64<<10)}, nil
}

// Next returns the next packet; the first is the hello packet. It
// returns io.EOF after In is closed and everything is encoded.
func (e *Encoder) Next() (Packet, error) { return ReadPacket(e.out) }

// Close stops the encoder.
func (e *Encoder) Close() error {
	e.In.Close()
	e.cmd.Process.Kill()
	e.cmd.Wait()
	return nil
}

// Stats is one of the player's once-a-second reports.
type Stats struct {
	DepthMs   float64 `json:"depth_ms"`
	TargetMs  float64 `json:"target_ms"`
	RatioPPM  float64 `json:"ratio_ppm"`
	Received  int64   `json:"received"`
	PLC       int64   `json:"plc"`
	Late      int64   `json:"late"`
	Skipped   int64   `json:"skipped"`
	Underruns int64   `json:"underruns"`
	Buffering bool    `json:"buffering"`
	EOF       bool    `json:"eof"` // the stream has ended; the rest is playing out
	Done      bool    `json:"done"`
}

// PlayerConfig sets up playback. Device is a CoreAudio output name or
// UID ("" for the default output). NullPPM (with Null) replaces the
// device with a clock that runs that many ppm fast, for tests; Dump then
// receives what was played.
type PlayerConfig struct {
	Helper   string
	Device   string
	Channels [2]int // 1-based output channels, default 1,2
	TargetMs int
	MinMs    int
	Null     bool
	NullPPM  float64
	Dump     string
	OnStats  func(Stats)
}

// Player plays packets written with Write.
type Player struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	done chan struct{}
	mu   sync.Mutex
	last Stats
	diag []string
}

// StartPlayer launches rs-live play.
func StartPlayer(c PlayerConfig) (*Player, error) {
	if c.Helper == "" {
		c.Helper = FindHelper()
	}
	args := []string{"play"}
	if c.Device != "" {
		args = append(args, "--device", c.Device)
	}
	if c.Channels[0] > 0 && c.Channels[1] > 0 {
		args = append(args, "--ch", fmt.Sprintf("%d,%d", c.Channels[0], c.Channels[1]))
	}
	if c.TargetMs > 0 {
		args = append(args, "--target-ms", strconv.Itoa(c.TargetMs))
	}
	if c.MinMs > 0 {
		args = append(args, "--min-ms", strconv.Itoa(c.MinMs))
	}
	if c.Null {
		args = append(args, "--null", strconv.FormatFloat(c.NullPPM, 'f', -1, 64))
		if c.Dump != "" {
			args = append(args, "--dump", c.Dump)
		}
	}
	cmd := exec.Command(c.Helper, args...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	se, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting rs-live: %w", err)
	}
	p := &Player{cmd: cmd, in: in, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		sc := bufio.NewScanner(se)
		for sc.Scan() {
			var s Stats
			if json.Unmarshal(sc.Bytes(), &s) == nil && (s.TargetMs > 0 || s.Done) {
				p.mu.Lock()
				p.last = s
				p.mu.Unlock()
				if c.OnStats != nil {
					c.OnStats(s)
				}
				continue
			}
			p.mu.Lock()
			p.diag = append(p.diag, sc.Text())
			p.mu.Unlock()
		}
		cmd.Wait()
	}()
	return p, nil
}

// Write sends one packet to the player.
func (p *Player) Write(pk Packet) error {
	_, err := p.in.Write(pk)
	return err
}

// Close ends the stream, lets the buffered audio play out and waits.
func (p *Player) Close() error {
	p.in.Close()
	<-p.done
	return p.err()
}

// Stop ends playback at once.
func (p *Player) Stop() {
	p.in.Close()
	p.cmd.Process.Signal(os.Interrupt)
	<-p.done
}

// Done is closed when the player has exited.
func (p *Player) Done() <-chan struct{} { return p.done }

// Diag returns the helper's non-stats output lines.
func (p *Player) Diag() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.diag...)
}

// Last returns the most recent stats.
func (p *Player) Last() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

func (p *Player) err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.cmd.ProcessState; s != nil && !s.Success() {
		msg := "rs-live play failed"
		for _, l := range p.diag {
			if l != "" && l[0] != '{' {
				msg = l
			}
		}
		return errors.New(msg)
	}
	return nil
}
