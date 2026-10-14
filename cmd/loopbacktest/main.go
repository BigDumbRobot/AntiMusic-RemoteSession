// Command loopbacktest checks that a real capture device yields a
// bit-exact master. It plays the deterministic test signal into a
// loopback device with FFmpeg's AudioToolbox output while the recorder
// captures the same device, then checks every captured block against the
// signal: each must be an exact copy of the signal, and between blocks the
// signal must advance exactly as far as the master did (or the gap must be
// one the capture helper marked as filled).
package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"antimusic.dev/remotesession/internal/pcm"
	"antimusic.dev/remotesession/internal/recorder"
	"antimusic.dev/remotesession/internal/take"
	"antimusic.dev/remotesession/internal/testsignal"
	"antimusic.dev/remotesession/internal/timeline"
)

func main() {
	device := flag.String("device", "VB-Cable", "loopback device name (input and output)")
	outIndex := flag.Int("out-index", -1, "play with FFmpeg AudioToolbox to this output index (a second device client); -1 plays from the capture IOProc itself")
	rate := flag.Int("rate", 44100, "device sample rate")
	channels := flag.Int("channels", 8, "device channel count")
	seconds := flag.Float64("seconds", 10, "signal length")
	lead := flag.Duration("lead", time.Second, "recording time before playback starts")
	dir := flag.String("dir", "", "take directory (default: temp)")
	flag.Parse()
	if err := run(*device, *outIndex, *rate, *channels, *seconds, *lead, *dir); err != nil {
		fmt.Fprintln(os.Stderr, "loopbacktest:", err)
		os.Exit(1)
	}
}

func run(device string, outIndex, rate, channels int, seconds float64, lead time.Duration, dir string) error {
	if dir == "" {
		d, err := os.MkdirTemp("", "loopback")
		if err != nil {
			return err
		}
		dir = filepath.Join(d, "take")
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	frames := int64(seconds * float64(rate))
	sig := filepath.Join(filepath.Dir(dir), "signal.f32")
	b := make([]byte, 0, frames*int64(channels)*4)
	for t := int64(0); t < frames; t++ {
		for c := 0; c < channels; c++ {
			b = binary.LittleEndian.AppendUint32(b, math.Float32bits(testsignal.Value(t, c)))
		}
	}
	// FFmpeg's AudioToolbox output drops its last buffers on close, so
	// trailing silence keeps the whole signal audible.
	b = append(b, make([]byte, 2*rate*channels*4)...)
	if err := os.WriteFile(sig, b, 0o644); err != nil {
		return err
	}

	cfg := recorder.Config{
		Dir: dir, Device: device,
		Session: &take.Session{Source: "loopbacktest", SampleRate: rate},
	}
	if outIndex < 0 {
		// Lead-in silence, then the signal, played by the capture IOProc.
		pad := make([]byte, int(lead.Seconds()*float64(rate))*channels*4)
		if err := os.WriteFile(sig, append(pad, b...), 0o644); err != nil {
			return err
		}
		cfg.CaptureArgs = []string{"--play", sig}
	}
	r, err := recorder.Start(cfg)
	if err != nil {
		return err
	}
	var out []byte
	var perr error
	if outIndex >= 0 {
		time.Sleep(lead)
		play := exec.Command("ffmpeg", "-hide_banner", "-nostdin", "-loglevel", "error",
			"-f", "f32le", "-ar", fmt.Sprint(rate), "-ac", fmt.Sprint(channels), "-i", sig,
			"-c:a", "pcm_f32le", "-f", "audiotoolbox", "-audio_device_index", fmt.Sprint(outIndex), "-")
		out, perr = play.CombinedOutput()
	} else {
		time.Sleep(lead + time.Duration(seconds*float64(time.Second)))
	}
	time.Sleep(time.Second)
	if err := r.Stop(10 * time.Second); err != nil {
		return err
	}
	if perr != nil {
		return fmt.Errorf("playback: %v: %s", perr, out)
	}
	for _, l := range strings.Split(r.Stderr(), "\n") {
		if strings.Contains(l, "stopped after") || strings.Contains(l, "fault") {
			fmt.Println(l)
		}
	}

	m, err := pcm.OpenCAF(filepath.Join(dir, take.MasterFile))
	if err != nil {
		return err
	}
	defer m.Close()
	if m.SampleRate != rate || m.Channels != channels {
		return fmt.Errorf("master is %d Hz/%d ch, expected %d/%d", m.SampleRate, m.Channels, rate, channels)
	}
	cl, err := timeline.ReadCaptureLog(filepath.Join(dir, take.CaptureLog), rate)
	if err != nil {
		return err
	}
	filled, err := filledBlocks(filepath.Join(dir, take.CaptureLog))
	if err != nil {
		return err
	}

	// Index the signal by the values of its first two channels (single
	// 24-bit values collide across a long signal; pairs practically never).
	key := func(get func(c int) float32) [2]float32 {
		k := [2]float32{get(0)}
		if channels > 1 {
			k[1] = get(1)
		}
		return k
	}
	at := make(map[[2]float32]int64, frames)
	for t := int64(0); t < frames; t++ {
		at[key(func(c int) float32 { return testsignal.Value(t, c) })] = t
	}
	var exact, foreign, outside, fills, breaks int64
	prevMaster, prevSig := int64(-1), int64(-1)
	var buf []float64
	for _, blk := range cl.Blocks {
		if filled[blk.Frame] {
			fills++
			continue
		}
		n := int(blk.Samples)
		if blk.Frame+blk.Samples > m.Frames {
			break // torn tail
		}
		buf = append(buf[:0], make([]float64, n*channels)...)
		if err := m.ReadFrames(blk.Frame, n, buf); err != nil {
			return err
		}
		start := int64(-1) // signal frame at the block's first frame
		matches := func(st int64) bool {
			for j := 0; j < n; j++ {
				for c := 0; c < channels; c++ {
					want := float32(0)
					if t := st + int64(j); t >= 0 && t < frames {
						want = testsignal.Value(t, c)
					}
					if buf[j*channels+c] != float64(want) {
						return false
					}
				}
			}
			return true
		}
		predicted := prevSig + blk.Frame - prevMaster
		if prevSig >= 0 && predicted < frames && matches(predicted) {
			start = predicted
		}
		for i := 0; i < n && start < 0; i++ {
			if buf[i*channels] == 0 {
				continue // silence; 0.0 also occurs in the signal, never anchor on it
			}
			if t, ok := at[key(func(c int) float32 { return float32(buf[i*channels+c]) })]; ok {
				start = t - int64(i)
			}
		}
		if start < 0 && prevSig >= 0 && predicted < frames {
			start = predicted // report the difference against the expected position
		}
		if start < 0 {
			outside++ // silence before/after the signal
			continue
		}
		ok := true
		for j := 0; j < n && ok; j++ {
			for c := 0; c < channels && ok; c++ {
				want := float32(0)
				if st := start + int64(j); st >= 0 && st < frames {
					want = testsignal.Value(st, c)
				}
				ok = buf[j*channels+c] == float64(want)
			}
		}
		if !ok {
			foreign++
			if foreign <= 4 {
				describe(blk.Frame, start, n, channels, frames, buf)
			}
			continue
		}
		exact++
		if prevSig >= 0 && start-prevSig != blk.Frame-prevMaster {
			breaks++
			if breaks <= 5 {
				fmt.Printf("  continuity: at master frame %d the signal advanced %d frames, the master %d\n",
					blk.Frame, start-prevSig, blk.Frame-prevMaster)
			}
		}
		prevMaster, prevSig = blk.Frame, start
	}
	fmt.Printf("take: %s\nmaster: %d frames (%.2f s)\n", dir, m.Frames, float64(m.Frames)/float64(rate))
	fmt.Printf("capture blocks: %d bit-exact signal, %d corrupted, %d silence, %d filled gaps\n", exact, foreign, outside, fills)
	fmt.Printf("continuity breaks between exact blocks: %d\n", breaks)
	if foreign > 0 {
		return fmt.Errorf("captured audio differs from the signal")
	}
	if breaks > fills {
		return fmt.Errorf("audio was lost without being detected")
	}
	return nil
}

// filledBlocks returns the master frames of capture-log lines that
// rs-capture marked as filled (last field 1).
func filledBlocks(path string) (map[int64]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[int64]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) == 7 && fs[6] == "1" {
			var fr int64
			fmt.Sscan(fs[0], &fr)
			out[fr] = true
		}
	}
	return out, sc.Err()
}

// describe prints how a non-matching block differs from the signal at the
// expected position.
func describe(master, start int64, n, channels int, frames int64, buf []float64) {
	var same, zero int
	var maxDiff, ratio float64
	for j := 0; j < n; j++ {
		for c := 0; c < channels; c++ {
			want := float64(0)
			if st := start + int64(j); st >= 0 && st < frames {
				want = float64(testsignal.Value(st, c))
			}
			got := buf[j*channels+c]
			switch {
			case got == want:
				same++
			case got == 0:
				zero++
			}
			if d := math.Abs(got - want); d > maxDiff {
				maxDiff = d
				if want != 0 {
					ratio = got / want
				}
			}
		}
	}
	fmt.Printf("  differs at master %d (signal %d): %d/%d samples equal, %d zero, max |diff| %.3g (got/want %.4f)\n",
		master, start, same, n*channels, zero, maxDiff, ratio)
}
