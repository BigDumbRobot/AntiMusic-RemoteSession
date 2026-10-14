package timeline

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"sort"
	"strconv"
	"strings"
)

// CaptureLogFormat is the -stats_enc_pre_fmt the recorder passes to FFmpeg:
// samples sent to the encoder so far, samples in this frame, the input
// frame's PTS with its time base, and the encoder-side PTS with its time
// base. The input PTS carries the device's own capture timestamps (and so
// reveals dropouts); the encoder PTS is made contiguous by FFmpeg and is
// only used when the input has none (AV_NOPTS_VALUE, e.g. raw PCM).
const CaptureLogFormat = "{sn} {samp} {ptsi} {tbi} {pts} {tb}"

const noPTS = math.MinInt64

// CaptureBlock is one block of captured audio as reported by FFmpeg's
// per-frame encoder statistics (see CaptureLogFormat).
type CaptureBlock struct {
	Frame      int64 // master frame of the block's first sample
	Samples    int64 // frames in the block
	HostMicros int64 // capture host time (mach_absolute_time) in µs
}

// CaptureLog is the capture clock of a master: which host time each
// master frame was captured at.
type CaptureLog struct {
	SampleRate int
	Blocks     []CaptureBlock
}

// Discontinuity is a place where host time advanced by more than the
// audio written, i.e. the device or driver dropped input.
type Discontinuity struct {
	Frame         int64
	MissingMicros int64
}

// ReadCaptureLog parses an FFmpeg stats file written with
// CaptureLogFormat. With -copyts, avfoundation input PTS are host times
// (mach_absolute_time) in a 1/1000000 time base; other inputs use their
// own time base, so every PTS is converted to microseconds.
func ReadCaptureLog(path string, sampleRate int) (*CaptureLog, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseCaptureLog(f, sampleRate)
}

func ParseCaptureLog(r io.Reader, sampleRate int) (*CaptureLog, error) {
	cl := &CaptureLog{SampleRate: sampleRate}
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		fs := strings.Fields(sc.Text())
		if len(fs) == 0 {
			continue
		}
		var v [4]int64 // sn, samp, ptsi, pts
		ok := len(fs) >= 6
		for i, fi := range []int{0, 1, 2, 4} {
			if !ok {
				break
			}
			n, err := strconv.ParseInt(fs[fi], 10, 64)
			if err != nil {
				ok = false
				break
			}
			v[i] = n
		}
		var pts, num, den int64
		if ok {
			tb := fs[3]
			pts = v[2]
			if pts == noPTS {
				pts, tb = v[3], fs[5]
			}
			num, den, ok = parseRational(tb)
			ok = ok && pts != noPTS
		}
		if !ok {
			// A process killed mid-write can leave a torn final line;
			// anything else is corruption.
			if !sc.Scan() {
				break
			}
			return nil, fmt.Errorf("capture log line %d: %q", line, fs)
		}
		us := new(big.Int).Mul(big.NewInt(pts), big.NewInt(1_000_000*num))
		us.Quo(us, big.NewInt(den))
		cl.Blocks = append(cl.Blocks, CaptureBlock{Frame: v[0], Samples: v[1], HostMicros: us.Int64()})
	}
	return cl, sc.Err()
}

func parseRational(s string) (num, den int64, ok bool) {
	a, b, found := strings.Cut(s, "/")
	if !found {
		return 0, 0, false
	}
	n, err1 := strconv.ParseInt(a, 10, 64)
	d, err2 := strconv.ParseInt(b, 10, 64)
	if err1 != nil || err2 != nil || d <= 0 {
		return 0, 0, false
	}
	return n, d, true
}

// Frames returns the number of frames the log accounts for.
func (cl *CaptureLog) Frames() int64 {
	if len(cl.Blocks) == 0 {
		return 0
	}
	b := cl.Blocks[len(cl.Blocks)-1]
	return b.Frame + b.Samples
}

// Discontinuities reports gaps where consecutive blocks' host times differ
// from their sample count by more than toleranceMicros.
func (cl *CaptureLog) Discontinuities(toleranceMicros int64) []Discontinuity {
	var out []Discontinuity
	for i := 1; i < len(cl.Blocks); i++ {
		a, b := cl.Blocks[i-1], cl.Blocks[i]
		expect := a.Samples * 1_000_000 / int64(cl.SampleRate)
		if d := b.HostMicros - a.HostMicros - expect; d > toleranceMicros || d < -toleranceMicros {
			out = append(out, Discontinuity{Frame: b.Frame, MissingMicros: d})
		}
	}
	return out
}

// FrameAt returns the master frame captured at host time hostMicros,
// interpolating within the containing block and extrapolating at the
// rate's nominal speed outside the log.
func (cl *CaptureLog) FrameAt(hostMicros int64) (int64, error) {
	if len(cl.Blocks) == 0 {
		return 0, fmt.Errorf("capture log is empty")
	}
	i := sort.Search(len(cl.Blocks), func(i int) bool { return cl.Blocks[i].HostMicros > hostMicros })
	if i > 0 {
		i--
	}
	b := cl.Blocks[i]
	d := hostMicros - b.HostMicros
	// Round to the nearest frame.
	off := (d*int64(cl.SampleRate) + 500_000) / 1_000_000
	if d < 0 {
		off = (d*int64(cl.SampleRate) - 500_000) / 1_000_000
	}
	return b.Frame + off, nil
}
