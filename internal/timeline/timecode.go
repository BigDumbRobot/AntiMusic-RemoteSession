package timeline

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// FrameRate is a SMPTE timecode rate.
type FrameRate struct {
	Nominal   int   // frames counted per timecode second (24, 25, 30, ...)
	Num, Den  int64 // actual frames per second as a ratio, e.g. 30000/1001
	DropFrame bool
}

var (
	FPS23976  = FrameRate{24, 24000, 1001, false}
	FPS24     = FrameRate{24, 24, 1, false}
	FPS25     = FrameRate{25, 25, 1, false}
	FPS2997   = FrameRate{30, 30000, 1001, false}
	FPS2997DF = FrameRate{30, 30000, 1001, true}
	FPS30     = FrameRate{30, 30, 1, false}
	FPS50     = FrameRate{50, 50, 1, false}
	FPS5994   = FrameRate{60, 60000, 1001, false}
	FPS5994DF = FrameRate{60, 60000, 1001, true}
	FPS60     = FrameRate{60, 60, 1, false}
)

// TimecodeToSamples converts "HH:MM:SS:FF" (";" before FF marks drop
// frame) to samples since 00:00:00:00 at the given sample rate, rounding
// to the nearest sample.
func TimecodeToSamples(tc string, rate FrameRate, sampleRate int) (int64, error) {
	tc = strings.TrimSpace(tc)
	parts := strings.FieldsFunc(tc, func(r rune) bool { return r == ':' || r == ';' || r == '.' })
	if len(parts) != 4 {
		return 0, fmt.Errorf("timecode %q: want HH:MM:SS:FF", tc)
	}
	var v [4]int64
	for i, p := range parts {
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("timecode %q: bad field %q", tc, p)
		}
		v[i] = n
	}
	h, m, s, f := v[0], v[1], v[2], v[3]
	if m > 59 || s > 59 || f >= int64(rate.Nominal) {
		return 0, fmt.Errorf("timecode %q out of range for %d fps", tc, rate.Nominal)
	}
	nom := int64(rate.Nominal)
	frames := ((h*60+m)*60+s)*nom + f
	if rate.DropFrame {
		drop := nom / 15 // 2 frames at 30, 4 at 60
		if m%10 != 0 && s == 0 && f < drop {
			return 0, fmt.Errorf("timecode %q does not exist in drop frame", tc)
		}
		totalMin := h*60 + m
		frames -= drop * (totalMin - totalMin/10)
	}
	// samples = frames * sampleRate * Den / Num, rounded half up.
	x := new(big.Int).Mul(big.NewInt(frames), big.NewInt(int64(sampleRate)*rate.Den))
	q, r := new(big.Int).QuoRem(x, big.NewInt(rate.Num), new(big.Int))
	if 2*r.Int64() >= rate.Num {
		q.Add(q, big.NewInt(1))
	}
	return q.Int64(), nil
}
