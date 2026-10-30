// Package latency measures an audio interface's round trip with a
// loopback: single-sample clicks go out of one output, a cable (or an
// analog chain) brings them back into one input, and rs-capture plays and
// records them in the same IOProc, so both ends share one clock and the
// delay is exact to the sample.
//
// The result is compared with what CoreAudio predicts from the device's
// reported figures (2 × buffer + latencies + safety offsets). On an
// Apogee Symphony I/O (docs/ARCHITECTURE.md) the difference is set each
// time the interface locks its clock (0 to -9 frames seen) and holds,
// across device starts and buffer sizes, until it locks again, so a
// measurement holds until the next sample-rate change or clock reset.
package latency

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"
	"time"
)

// Config describes one measurement. Out and In are 1-based channel
// numbers as printed on the interface.
type Config struct {
	Device string
	Out    int
	In     int
	// Buffer asks for this IO buffer size (0 keeps the device's).
	Buffer int
	// LevelDB is the click level in dBFS (default -12, at most -6).
	LevelDB float64
	// Clicks is the number of clicks (default 8, every 250 ms).
	Clicks int
	// Helper is the rs-capture binary.
	Helper string
}

// Click is one detected click.
type Click struct {
	Sent  int     `json:"sent"`  // frame the click was played at
	Peak  int     `json:"peak"`  // frames from Sent to the largest sample
	Onset int     `json:"onset"` // frames from Sent to the first sample ≥ 10 % of the peak
	Value float64 `json:"value"` // the peak sample
}

// Reported is what the device said about itself, in frames.
type Reported struct {
	Buffer             int `json:"buffer"`
	InputLatency       int `json:"input_latency"`
	InputSafetyOffset  int `json:"input_safety_offset"`
	OutputLatency      int `json:"output_latency"`
	OutputSafetyOffset int `json:"output_safety_offset"`
}

// Predicted is CoreAudio's round trip for a same-IOProc loopback.
func (r Reported) Predicted() int {
	return 2*r.Buffer + r.InputLatency + r.InputSafetyOffset + r.OutputLatency + r.OutputSafetyOffset
}

// Result is a finished measurement.
type Result struct {
	Device   string   `json:"device"`
	Rate     int      `json:"rate"`
	Out      int      `json:"out"`
	In       int      `json:"in"`
	Reported Reported `json:"reported"`
	Clicks   []Click  `json:"clicks"`
	Sent     int      `json:"sent"`
	// RoundTrip is the median onset; Spread is max-min of the onsets.
	RoundTrip int     `json:"round_trip"`
	Spread    int     `json:"spread"`
	Predicted int     `json:"predicted"`
	GainDB    float64 `json:"gain_db"`
	Inverted  bool    `json:"inverted"`
	// AlsoOn lists other inputs (1-based) where the clicks showed up.
	AlsoOn []int `json:"also_on,omitempty"`
}

// Difference is the measured round trip minus CoreAudio's prediction.
func (r *Result) Difference() int { return r.RoundTrip - r.Predicted }

// Matches reports whether the device's own figures can be trusted for
// this clock lock (within 2 frames).
func (r *Result) Matches() bool {
	d := r.Difference()
	return len(r.Clicks) > 0 && d >= -2 && d <= 2
}

const (
	leadSeconds  = 1.5  // the Symphony is silent for about 1 s after a start
	clickSpacing = 0.25 // seconds
	tailSeconds  = 0.5
	maxDelay     = 0.2 // seconds; a longer round trip is not found
)

type device struct {
	Name    string `json:"name"`
	UID     string `json:"uid"`
	Inputs  int    `json:"channels"`
	Outputs int    `json:"outputs"`
	Rate    int    `json:"rate"`
}

// Measure plays the clicks and analyses what came back.
func Measure(ctx context.Context, cfg Config) (*Result, error) {
	if cfg.LevelDB == 0 {
		cfg.LevelDB = -12
	}
	if cfg.LevelDB > -6 {
		return nil, errors.New("latency: click level above -6 dBFS refused")
	}
	if cfg.Clicks <= 0 {
		cfg.Clicks = 8
	}
	if cfg.Helper == "" {
		cfg.Helper = "rs-capture"
	}
	dev, err := findDevice(cfg.Helper, cfg.Device)
	if err != nil {
		return nil, err
	}
	if cfg.Out < 1 || cfg.Out > dev.Outputs {
		return nil, fmt.Errorf("latency: %s has outputs 1-%d, not %d", dev.Name, dev.Outputs, cfg.Out)
	}
	if cfg.In < 1 || cfg.In > dev.Inputs {
		return nil, fmt.Errorf("latency: %s has inputs 1-%d, not %d", dev.Name, dev.Inputs, cfg.In)
	}
	rate := dev.Rate
	sent, play := clickTrack(rate, dev.Outputs, cfg.Out-1, cfg.Clicks, math.Pow(10, cfg.LevelDB/20))
	tmp, err := os.MkdirTemp("", "rs-latency")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	pf := filepath.Join(tmp, "clicks.f32")
	if err := os.WriteFile(pf, play, 0o644); err != nil {
		return nil, err
	}
	dur := time.Duration(float64(len(play)/4/dev.Outputs)/float64(rate)*float64(time.Second)) + 300*time.Millisecond

	var hdr header
	var pcm []byte
	for attempt := 0; ; attempt++ {
		hdr, pcm, err = runHelper(ctx, cfg, pf, dur)
		if err == nil || !errors.Is(err, errDeviceFault) || attempt == 2 {
			break
		}
		// The device restarted under us (seen at about one start in four
		// on the Symphony); try again.
	}
	if err != nil {
		return nil, err
	}
	if hdr.Channels != dev.Inputs || hdr.Rate != rate {
		return nil, fmt.Errorf("latency: device changed to %d Hz/%d ch while measuring", hdr.Rate, hdr.Channels)
	}
	samples := make([]float32, len(pcm)/4)
	for i := range samples {
		samples[i] = math.Float32frombits(binary.LittleEndian.Uint32(pcm[4*i:]))
	}
	res := Analyze(samples, hdr.Channels, cfg.In-1, sent, rate, math.Pow(10, cfg.LevelDB/20))
	res.Device, res.Rate, res.Out, res.In = hdr.Name, rate, cfg.Out, cfg.In
	res.Reported = hdr.Reported
	res.Predicted = hdr.Reported.Predicted()
	if len(res.Clicks) == 0 {
		return res, fmt.Errorf("latency: no clicks came back on input %d; check the patch from output %d", cfg.In, cfg.Out)
	}
	return res, nil
}

// clickTrack returns the click positions and an interleaved float32
// file with single-sample clicks on output channel out only.
func clickTrack(rate, channels, out, clicks int, level float64) ([]int, []byte) {
	lead := int(leadSeconds * float64(rate))
	step := int(clickSpacing * float64(rate))
	frames := lead + clicks*step + int(tailSeconds*float64(rate))
	b := make([]byte, frames*channels*4)
	sent := make([]int, clicks)
	bits := math.Float32bits(float32(level))
	for i := range sent {
		sent[i] = lead + i*step
		binary.LittleEndian.PutUint32(b[(sent[i]*channels+out)*4:], bits)
	}
	return sent, b
}

// Analyze finds each sent click on input in (0-based) of an interleaved
// capture. Frame 0 of the capture and of the click track are the same
// IO cycle, so the offsets are the round trip.
func Analyze(x []float32, channels, in int, sent []int, rate int, level float64) *Result {
	frames := len(x) / channels
	at := func(f, c int) float64 { return float64(x[f*channels+c]) }
	window := int(maxDelay * float64(rate))

	// Noise floor of each input, from the lead-in after the device has
	// settled and before the first click can arrive.
	noise := make([]float64, channels)
	from, to := int(0.9*float64(rate)), sent[0]
	for c := range noise {
		for f := from; f < to && f < frames; f++ {
			noise[c] = math.Max(noise[c], math.Abs(at(f, c)))
		}
	}
	threshold := func(c int) float64 { return math.Max(4*noise[c], 0.002) }

	res := &Result{Sent: len(sent)}
	var onsets []int
	var gains []float64
	negative := 0
	for _, s := range sent {
		if s+window > frames {
			break
		}
		peak, pv := 0, 0.0
		for i := 0; i < window; i++ {
			if v := at(s+i, in); math.Abs(v) > math.Abs(pv) {
				peak, pv = i, v
			}
		}
		if math.Abs(pv) < threshold(in) {
			continue
		}
		onset := peak
		for i := 0; i <= peak; i++ {
			if math.Abs(at(s+i, in)) >= 0.1*math.Abs(pv) {
				onset = i
				break
			}
		}
		res.Clicks = append(res.Clicks, Click{Sent: s, Peak: peak, Onset: onset, Value: pv})
		onsets = append(onsets, onset)
		gains = append(gains, math.Abs(pv)/level)
		if pv < 0 {
			negative++
		}
	}
	if len(onsets) == 0 {
		return res
	}
	sort.Ints(onsets)
	sort.Float64s(gains)
	res.RoundTrip = onsets[len(onsets)/2]
	res.Spread = onsets[len(onsets)-1] - onsets[0]
	res.GainDB = 20 * math.Log10(gains[len(gains)/2])
	res.Inverted = negative*2 > len(onsets)

	for c := 0; c < channels; c++ {
		if c == in {
			continue
		}
		hit := 0
		for _, k := range res.Clicks {
			p := k.Sent + k.Peak
			for f := max(p-64, 0); f < min(p+64, frames); f++ {
				if math.Abs(at(f, c)) >= threshold(c) {
					hit++
					break
				}
			}
		}
		if hit*2 > len(res.Clicks) {
			res.AlsoOn = append(res.AlsoOn, c+1)
		}
	}
	return res
}

type header struct {
	Rate     int    `json:"rate"`
	Channels int    `json:"channels"`
	Name     string `json:"name"`
	Reported
}

var errDeviceFault = errors.New("the device stopped or restarted")

// runHelper runs rs-capture for dur (playing play if set) and returns
// its header and everything it captured.
func runHelper(ctx context.Context, cfg Config, play string, dur time.Duration) (header, []byte, error) {
	var hdr header
	args := []string{"--device", cfg.Device}
	if cfg.Buffer > 0 {
		args = append(args, "--buffer", strconv.Itoa(cfg.Buffer))
	}
	if play != "" {
		args = append(args, "--play", play)
	}
	cmd := exec.CommandContext(ctx, cfg.Helper, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	se, err := cmd.StderrPipe()
	if err != nil {
		return hdr, nil, err
	}
	if err := cmd.Start(); err != nil {
		return hdr, nil, err
	}
	br := bufio.NewReader(se)
	line, _ := br.ReadBytes('\n')
	rest := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(br); rest <- b }()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	var werr error
	select {
	case werr = <-exited: // ended early
	case <-time.After(dur):
		cmd.Process.Signal(syscall.SIGINT)
		werr = <-exited
	case <-ctx.Done():
		cmd.Process.Kill()
		<-exited
		return hdr, nil, ctx.Err()
	}
	diag := bytes.TrimSpace(<-rest)
	if err := json.Unmarshal(line, &hdr); err != nil || hdr.Rate <= 0 {
		return hdr, nil, fmt.Errorf("rs-capture: %s %s", bytes.TrimSpace(line), diag)
	}
	var ee *exec.ExitError
	if errors.As(werr, &ee) && ee.ExitCode() == 3 || bytes.Contains(diag, []byte("clock jumped")) {
		return hdr, nil, fmt.Errorf("%w: %s", errDeviceFault, diag)
	}
	if werr != nil {
		return hdr, nil, fmt.Errorf("rs-capture: %v: %s", werr, diag)
	}
	return hdr, out.Bytes(), nil
}

func findDevice(helper, want string) (device, error) {
	out, err := exec.Command(helper, "--list").Output()
	if err != nil {
		return device{}, fmt.Errorf("listing devices: %w", err)
	}
	for _, l := range bytes.Split(out, []byte("\n")) {
		var d device
		if json.Unmarshal(l, &d) == nil && (d.Name == want || d.UID == want) {
			if d.Outputs == 0 {
				return d, fmt.Errorf("latency: %s has no outputs", d.Name)
			}
			return d, nil
		}
	}
	return device{}, fmt.Errorf("latency: no device named %q (see rsession devices)", want)
}
