package deliver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"antimusic.dev/remotesession/internal/pcm"
	"antimusic.dev/remotesession/internal/timeline"
)

// Plugin is one slot of a post-processing chain: an AU or VST3 given by
// file path or AU identifier ("AudioUnit:Effects/aufx,lmtr,appl"), with
// optional saved state.
type Plugin struct {
	ID    string `json:"id"`
	State string `json:"state,omitempty"`
}

// ChainReport is what rs-render reports for one render.
type ChainReport struct {
	Latency int `json:"latency"`
	Plugins []struct {
		Name    string `json:"name"`
		Format  string `json:"format"`
		Latency int    `json:"latency"`
	} `json:"plugins"`
	Frames int64 `json:"frames"`
}

// renderChain renders master frames [seg.MasterStart, +seg.Frames) through
// the plugin chain into a BWF at path. FFmpeg decodes and encodes; the
// data flows FFmpeg -> rs-render -> FFmpeg through OS pipes.
func renderChain(ctx context.Context, opts Options, master string, seg timeline.Segment, channels, rate int,
	tail int64, encodeArgs []string, path string) (*ChainReport, error) {
	decode := exec.CommandContext(ctx, opts.FFmpeg, "-hide_banner", "-nostdin", "-loglevel", "error",
		"-i", master, "-af", fmt.Sprintf("atrim=start_sample=%d:end_sample=%d", seg.MasterStart, seg.MasterStart+seg.Frames),
		"-f", "f32le", "-c:a", "pcm_f32le", "pipe:1")
	args := []string{"--rate", strconv.Itoa(rate), "--channels", strconv.Itoa(channels),
		"--timeline", strconv.FormatInt(seg.TimelineStart, 10), "--tail", strconv.FormatInt(tail, 10)}
	for _, p := range opts.Chain {
		args = append(args, "--plugin", p.ID)
		if p.State != "" {
			args = append(args, "--state", p.State)
		}
	}
	render := exec.CommandContext(ctx, opts.Renderer, args...)
	encode := exec.CommandContext(ctx, opts.FFmpeg, append([]string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-f", "f32le", "-ar", strconv.Itoa(rate), "-ac", strconv.Itoa(channels), "-i", "pipe:0"},
		append(encodeArgs, path)...)...)

	r1, w1, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	r2, w2, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	var decErr, renErr, encErr bytes.Buffer
	decode.Stdout, decode.Stderr = w1, &decErr
	render.Stdin, render.Stdout, render.Stderr = r1, w2, &renErr
	encode.Stdin, encode.Stderr = r2, &encErr
	for _, c := range []*exec.Cmd{decode, render, encode} {
		if err := c.Start(); err != nil {
			return nil, err
		}
	}
	// Each process holds its own copies of the pipe ends.
	for _, f := range []*os.File{r1, w1, r2, w2} {
		f.Close()
	}
	e1, e2, e3 := decode.Wait(), render.Wait(), encode.Wait()
	switch {
	case e1 != nil:
		return nil, fmt.Errorf("decoding: %v: %s", e1, decErr.Bytes())
	case e2 != nil:
		return nil, fmt.Errorf("plugin chain: %v: %s", e2, renErr.Bytes())
	case e3 != nil:
		return nil, fmt.Errorf("encoding: %v: %s", e3, encErr.Bytes())
	}
	lines := strings.Split(strings.TrimSpace(renErr.String()), "\n")
	var rep ChainReport
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &rep); err != nil {
		return nil, fmt.Errorf("plugin chain report: %v: %s", err, renErr.Bytes())
	}
	if rep.Frames != seg.Frames+tail {
		return nil, fmt.Errorf("plugin chain returned %d frames, want %d", rep.Frames, seg.Frames+tail)
	}
	return &rep, nil
}

// Alignment is the result of comparing a processed print with its dry
// source.
type Alignment struct {
	// LagFrames is where the processed signal best matches the dry one;
	// 0 means the chain's latency was compensated exactly.
	LagFrames int64 `json:"lag_frames"`
	// Correlation is the normalized correlation at that lag. Below
	// MinCorrelation the processing changed the signal too much (e.g. a
	// fully wet effect) for alignment to be checked this way.
	Correlation float64 `json:"correlation"`
	Checked     bool    `json:"checked"`
}

// MinCorrelation is the least normalized correlation at which a lag is
// trusted.
const MinCorrelation = 0.5

// CheckAlignment finds the lag between the dry master segment and the
// processed file within ±maxLag frames, using the loudest window of the
// dry signal (channel-summed).
func CheckAlignment(masterPath, processedPath string, seg timeline.Segment, maxLag int) (Alignment, error) {
	window := 12000
	if room := int(seg.Frames) - 2*maxLag; room < window {
		window = room
	}
	if window < 2048 {
		return Alignment{}, nil // too short to judge
	}
	dry, err := pcm.OpenCAF(masterPath)
	if err != nil {
		return Alignment{}, err
	}
	defer dry.Close()
	wet, err := pcm.OpenWAV(processedPath)
	if err != nil {
		return Alignment{}, err
	}
	defer wet.Close()
	ch := dry.Channels
	mono := func(f *pcm.File, start int64, n int) ([]float64, error) {
		buf := make([]float64, n*ch)
		if err := f.ReadFrames(start, n, buf); err != nil {
			return nil, err
		}
		out := make([]float64, n)
		for i := range out {
			for c := 0; c < ch; c++ {
				out[i] += buf[i*ch+c]
			}
		}
		return out, nil
	}
	d, err := mono(dry, seg.MasterStart, int(seg.Frames))
	if err != nil {
		return Alignment{}, err
	}
	// Loudest window, away from the edges so every lag stays in range.
	best, bestE := maxLag, -1.0
	for s := maxLag; s+window+maxLag <= len(d); s += window / 2 {
		e := 0.0
		for _, v := range d[s : s+window] {
			e += v * v
		}
		if e > bestE {
			best, bestE = s, e
		}
	}
	if bestE <= 0 {
		return Alignment{}, nil // silence
	}
	w, err := mono(wet, int64(best-maxLag), window+2*maxLag)
	if err != nil {
		return Alignment{}, err
	}
	x := d[best : best+window]
	var a Alignment
	a.Checked = true
	bestC := 0.0
	for lag := -maxLag; lag <= maxLag; lag++ {
		y := w[maxLag+lag : maxLag+lag+window]
		var xy, yy float64
		for i := range x {
			xy += x[i] * y[i]
			yy += y[i] * y[i]
		}
		if yy == 0 {
			continue
		}
		c := xy / math.Sqrt(bestE*yy)
		if math.Abs(c) > math.Abs(bestC) {
			bestC, a.LagFrames = c, int64(lag)
		}
	}
	a.Correlation = bestC
	if math.Abs(bestC) < MinCorrelation {
		a.Checked = false
	}
	return a, nil
}
