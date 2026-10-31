package live

import (
	"encoding/binary"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func helper(t *testing.T) string {
	p, _ := filepath.Abs("../../bin/rs-live")
	if _, err := os.Stat(p); err != nil {
		t.Skip("bin/rs-live not built (make bin/rs-live)")
	}
	return p
}

// encodeTone encodes seconds of a stereo tone and returns all packets,
// hello first.
func encodeTone(t *testing.T, h string, rate int, seconds, hz float64) []Packet {
	e, err := StartEncoder(EncoderConfig{Helper: h, Rate: rate})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	go func() {
		n := int(seconds * float64(rate))
		b := make([]byte, 0, 4096*8)
		for i := 0; i < n; i++ {
			v := math.Float32bits(float32(0.5 * math.Sin(2*math.Pi*hz*float64(i)/float64(rate))))
			b = binary.LittleEndian.AppendUint32(b, v)
			b = binary.LittleEndian.AppendUint32(b, v)
			if len(b) >= 4096*8 {
				e.In.Write(b)
				b = b[:0]
			}
		}
		e.In.Write(b)
		e.In.Close()
	}()
	var out []Packet
	for {
		p, err := e.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

type network struct {
	jitter    time.Duration // uniform extra delay per packet
	stallAt   time.Duration // packets due in [stallAt, stallAt+stall) are held until its end
	stall     time.Duration
	loss      float64 // probability a packet never arrives (UDP)
	targetMs  int     // player's starting target (default 40)
	reorder   bool    // packets may overtake each other (UDP)
	frameTime time.Duration
}

// relay feeds packets to the player as a network would deliver them.
func relay(p *Player, pk []Packet, n network) {
	rng := rand.New(rand.NewSource(7))
	p.Write(pk[0]) // hello
	type due struct {
		at time.Time
		p  Packet
	}
	start := time.Now()
	var q []due
	var last time.Time
	for i, k := range pk[1:] {
		if rng.Float64() < n.loss {
			continue
		}
		sent := time.Duration(i) * n.frameTime
		at := start.Add(sent + time.Duration(rng.Int63n(int64(n.jitter)+1)))
		if n.stall > 0 && sent >= n.stallAt && sent < n.stallAt+n.stall {
			at = start.Add(n.stallAt + n.stall)
		}
		if !n.reorder && at.Before(last) {
			at = last // in order, like TCP
		}
		last = at
		q = append(q, due{at, k})
	}
	if n.reorder {
		for i := 1; i < len(q); i++ { // deliver by arrival time
			for j := i; j > 0 && q[j].at.Before(q[j-1].at); j-- {
				q[j], q[j-1] = q[j-1], q[j]
			}
		}
	}
	for _, d := range q {
		time.Sleep(time.Until(d.at))
		p.Write(d.p)
	}
}

// toneLevels returns the tone's amplitude in each 20 ms window of a
// stereo float32 dump (left channel), via Goertzel.
func toneLevels(t *testing.T, path string, rate int, hz float64) []float64 {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	n := len(b) / 8
	x := make([]float64, n)
	for i := range x {
		x[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(b[8*i:])))
	}
	first := 0
	for first < n && x[first] == 0 {
		first++
	}
	w := rate / 50
	var out []float64
	for s := first; s+w <= n; s += w {
		c := 2 * math.Cos(2*math.Pi*hz/float64(rate))
		var s1, s2 float64
		for _, v := range x[s : s+w] {
			s1, s2 = v+c*s1-s2, s1
		}
		p := s1*s1 + s2*s2 - c*s1*s2
		out = append(out, 2*math.Sqrt(math.Max(p, 0))/float64(w))
	}
	return out
}

func run(t *testing.T, name string, ppm float64, n network) (Stats, []float64) {
	return runFor(t, name, 12, ppm, n)
}

func runFor(t *testing.T, name string, seconds, ppm float64, n network) (Stats, []float64) {
	h := helper(t)
	const rate = 48000
	pk := encodeTone(t, h, rate, seconds, 997)
	if f, ok := ParseHello(pk[0]); !ok || f.Rate != 48000 || f.Frame != 480 {
		t.Fatalf("hello %q", pk[0].Payload())
	}
	dump := filepath.Join(t.TempDir(), name+".f32")
	var stats []Stats
	target := n.targetMs
	if target == 0 {
		target = 40
	}
	p, err := StartPlayer(PlayerConfig{Helper: h, Null: true, NullPPM: ppm, Dump: dump, TargetMs: target, MinMs: 20,
		OnStats: func(s Stats) { stats = append(stats, s) }})
	if err != nil {
		t.Fatal(err)
	}
	n.frameTime = 10 * time.Millisecond
	relay(p, pk, n)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	for _, l := range p.Diag() {
		t.Log(l)
	}
	final := p.Last()
	// Judge playback by the last report before the stream ended: running
	// dry at the very end is expected.
	// The ratio is the mean of the last five reports.
	var live []Stats
	for _, s := range stats {
		if !s.Done && !s.EOF && s.Received > 0 {
			live = append(live, s)
			final.Underruns = s.Underruns
		}
	}
	var ratio float64
	tail := live[max(0, len(live)-5):]
	for _, s := range tail {
		ratio += s.RatioPPM / float64(len(tail))
	}
	lv := toneLevels(t, dump, rate, 997)
	low := 0
	for _, v := range lv[5 : len(lv)-5] {
		if v < 0.35 {
			low++
		}
	}
	t.Logf("%s: %d windows, %d low; final %+v; ratio %.0f ppm; target %v", name, len(lv), low, final, ratio,
		stats[len(stats)-2].TargetMs)
	final.RatioPPM = ratio
	return final, lv
}

func lowWindows(lv []float64) int {
	low := 0
	for _, v := range lv[5 : len(lv)-5] {
		if v < 0.35 {
			low++
		}
	}
	return low
}

// Timing tests: run on a quiet machine (make test runs packages one at a
// time); the simulated network is goroutines with sleeps, and heavy load
// makes it deliver late.
//
// A clean in-order network with a receiving clock 200 ppm fast (two
// crystals rarely differ by more than about 100): no losses, no
// concealment, the resampler takes up the drift.
func TestPlayDriftAndJitter(t *testing.T) {
	// 60 ms: the 20 ms output ring plus room for 15 ms of jitter and 10 ms
	// packet steps. (At 40 a late packet can raise the target, which is
	// the adaptive behaviour the other tests cover, not drift.)
	s, lv := runFor(t, "drift", 30, 200, network{jitter: 15 * time.Millisecond, targetMs: 60})
	if s.Underruns != 0 || s.PLC != 0 || s.Skipped != 0 || s.Late != 0 {
		t.Errorf("clean network: %+v", s)
	}
	if s.RatioPPM > -80 || s.RatioPPM < -400 {
		t.Errorf("ratio %.0f ppm: not steering against a +200 ppm clock", s.RatioPPM)
	}
	if low := lowWindows(lv); low != 0 {
		t.Errorf("%d dropouts in the tone", low)
	}
	if len(lv) < 1490 {
		t.Errorf("only %d of 1500 windows played", len(lv))
	}
}

// A 150 ms stall (TCP head-of-line blocking): a short gap is concealed,
// the burst afterwards is skipped back to the target, and playback
// carries on.
func TestPlayStall(t *testing.T) {
	s, lv := run(t, "stall", -200, network{jitter: 10 * time.Millisecond, stallAt: 6 * time.Second, stall: 150 * time.Millisecond})
	if s.Skipped == 0 && s.PLC == 0 {
		t.Errorf("stall left no trace: %+v", s)
	}
	if low := lowWindows(lv); low > 12 {
		t.Errorf("%d low windows (more than the stall itself)", low)
	}
}

// UDP-like delivery: 2 % loss and reordering. Lost packets are concealed.
func TestPlayLossAndReorder(t *testing.T) {
	s, lv := run(t, "loss", 0, network{jitter: 20 * time.Millisecond, loss: 0.02, reorder: true})
	if s.PLC == 0 {
		t.Errorf("no concealment with 2%% loss: %+v", s)
	}
	if low := lowWindows(lv); low > 10 {
		t.Errorf("%d low windows", low)
	}
}
