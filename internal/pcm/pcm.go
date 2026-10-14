// Package pcm reads interleaved PCM frames from CAF and WAV files without
// FFmpeg, so recordings can be checked by code independent of the tool
// that wrote them.
package pcm

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"

	"antimusic.dev/remotesession/internal/caf"
	"antimusic.dev/remotesession/internal/wav"
)

// File is an open PCM file.
type File struct {
	f             *os.File
	SampleRate    int
	Channels      int
	Bits          int
	Float         bool
	littleEndian  bool
	offset        int64
	bytesPerFrame int
	Frames        int64
}

// OpenCAF opens a linear-PCM CAF file.
func OpenCAF(path string) (*File, error) {
	info, err := caf.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &File{f: f, SampleRate: int(info.SampleRate), Channels: info.Channels, Bits: info.BitsPerSample,
		Float: info.Float, littleEndian: info.LittleEndian, offset: info.DataOffset,
		bytesPerFrame: info.BytesPerFrame, Frames: info.Frames()}, nil
}

// OpenWAV opens a PCM or float WAV/RF64 file.
func OpenWAV(path string) (*File, error) {
	info, err := wav.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if info.SubFormat != wav.FormatPCM && info.SubFormat != wav.FormatFloat {
		return nil, fmt.Errorf("pcm: %s: unsupported WAVE format %#x", path, info.SubFormat)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &File{f: f, SampleRate: info.SampleRate, Channels: info.Channels, Bits: info.BitsPerSample,
		Float: info.SubFormat == wav.FormatFloat, littleEndian: true, offset: info.DataOffset,
		bytesPerFrame: info.BlockAlign, Frames: info.Frames()}, nil
}

func (p *File) Close() error { return p.f.Close() }

// ReadFrames decodes n frames starting at frame start into dst
// (interleaved, len >= n*Channels), scaled to [-1, 1).
func (p *File) ReadFrames(start int64, n int, dst []float64) error {
	if start < 0 || start+int64(n) > p.Frames {
		return fmt.Errorf("pcm: frames [%d,%d) outside file of %d", start, start+int64(n), p.Frames)
	}
	buf := make([]byte, n*p.bytesPerFrame)
	if _, err := p.f.ReadAt(buf, p.offset+start*int64(p.bytesPerFrame)); err != nil && err != io.EOF {
		return err
	}
	var bo binary.ByteOrder = binary.BigEndian
	if p.littleEndian {
		bo = binary.LittleEndian
	}
	bps := p.Bits / 8
	for i := 0; i < n*p.Channels; i++ {
		b := buf[i*bps : (i+1)*bps]
		switch {
		case p.Float && bps == 4:
			dst[i] = float64(math.Float32frombits(bo.Uint32(b)))
		case p.Float && bps == 8:
			dst[i] = math.Float64frombits(bo.Uint64(b))
		case !p.Float && bps == 2:
			dst[i] = float64(int16(bo.Uint16(b))) / (1 << 15)
		case !p.Float && bps == 3:
			var v int32
			if p.littleEndian {
				v = int32(b[0]) | int32(b[1])<<8 | int32(int8(b[2]))<<16
			} else {
				v = int32(b[2]) | int32(b[1])<<8 | int32(int8(b[0]))<<16
			}
			dst[i] = float64(v) / (1 << 23)
		case !p.Float && bps == 4:
			dst[i] = float64(int32(bo.Uint32(b))) / (1 << 31)
		default:
			return fmt.Errorf("pcm: unsupported sample format (%d bits, float=%v)", p.Bits, p.Float)
		}
	}
	return nil
}
