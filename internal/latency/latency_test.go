package latency

import (
	"encoding/binary"
	"math"
	"math/rand"
	"testing"
)

// fakeLoop builds the capture a loopback would return: each click
// arrives delay frames later on input in, shaped like a converter's
// impulse response (a little pre-ringing, the main peak one frame after
// the onset), with gain g, plus noise on every input.
func fakeLoop(channels, in, delay int, g float64, sent []int, frames int, bleed int) []float32 {
	rng := rand.New(rand.NewSource(1))
	x := make([]float32, frames*channels)
	for i := range x {
		x[i] = float32((rng.Float64() - 0.5) * 2e-4)
	}
	shape := map[int]float64{-5: -0.027, 0: 0.4, 1: 1, 2: 0.35, 3: -0.1}
	for _, s := range sent {
		for k, v := range shape {
			f := s + delay + k
			x[f*channels+in] += float32(g * v)
			if bleed >= 0 {
				x[f*channels+bleed] += float32(0.05 * g * v)
			}
		}
	}
	return x
}

func TestAnalyzeFindsRoundTrip(t *testing.T) {
	const rate, channels, in = 48000, 16, 2
	sent, _ := clickTrack(rate, channels, 2, 8, 0.25)
	frames := sent[len(sent)-1] + rate
	x := fakeLoop(channels, in, 1292, 0.45, sent, frames, 7)

	r := Analyze(x, channels, in, sent, rate, 0.25)
	if len(r.Clicks) != 8 {
		t.Fatalf("found %d clicks, want 8", len(r.Clicks))
	}
	if r.RoundTrip != 1292 || r.Spread != 0 {
		t.Fatalf("round trip %d spread %d, want 1292 and 0", r.RoundTrip, r.Spread)
	}
	if r.Clicks[0].Peak != 1293 {
		t.Errorf("peak at %d, want 1293", r.Clicks[0].Peak)
	}
	if math.Abs(r.GainDB-20*math.Log10(0.45/0.25)) > 0.01 {
		t.Errorf("gain %.2f dB", r.GainDB)
	}
	if r.Inverted {
		t.Error("reported inverted")
	}
	if len(r.AlsoOn) != 1 || r.AlsoOn[0] != 8 {
		t.Errorf("also on %v, want [8]", r.AlsoOn)
	}
	r.Reported = Reported{Buffer: 512, InputLatency: 43, InputSafetyOffset: 108, OutputLatency: 44, OutputSafetyOffset: 73}
	r.Predicted = r.Reported.Predicted()
	if r.Predicted != 1292 || !r.Matches() {
		t.Errorf("predicted %d, matches %v", r.Predicted, r.Matches())
	}
}

func TestAnalyzeInvertedAndMissing(t *testing.T) {
	const rate, channels, in = 48000, 2, 0
	sent, _ := clickTrack(rate, channels, 0, 4, 0.25)
	frames := sent[len(sent)-1] + rate
	x := fakeLoop(channels, in, 300, -0.25, sent, frames, -1)
	r := Analyze(x, channels, in, sent, rate, 0.25)
	if r.RoundTrip != 300 || !r.Inverted || len(r.AlsoOn) != 0 {
		t.Fatalf("round trip %d inverted %v also %v", r.RoundTrip, r.Inverted, r.AlsoOn)
	}

	quiet := fakeLoop(channels, in, 300, 0, sent, frames, -1)
	if r := Analyze(quiet, channels, in, sent, rate, 0.25); len(r.Clicks) != 0 {
		t.Fatalf("found %d clicks in silence", len(r.Clicks))
	}
}

func TestClickTrackOnlyOnOneOutput(t *testing.T) {
	sent, b := clickTrack(48000, 16, 2, 3, 0.25)
	for i := 0; i < len(b)/4; i++ {
		v := math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
		if v == 0 {
			continue
		}
		f, c := i/16, i%16
		if c != 2 || (f != sent[0] && f != sent[1] && f != sent[2]) || v != 0.25 {
			t.Fatalf("unexpected sample %v at frame %d channel %d", v, f, c)
		}
	}
}
