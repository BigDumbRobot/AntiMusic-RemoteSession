// Package caf reads the header of Apple Core Audio Format files holding
// linear PCM, enough to locate and decode sample frames directly.
package caf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

const (
	flagFloat        = 1 << 0
	flagLittleEndian = 1 << 1
)

// Info describes a CAF file's PCM layout.
type Info struct {
	SampleRate    float64
	Channels      int
	BitsPerSample int
	Float         bool
	LittleEndian  bool
	BytesPerFrame int
	DataOffset    int64 // first sample byte
	DataBytes     int64 // whole frames only
	// SizeUnknown is true when the data chunk size was never finalized
	// (-1), i.e. the writer did not shut down cleanly.
	SizeUnknown bool
	// TrailingBytes counts bytes after the last whole frame (a torn
	// final write).
	TrailingBytes int64
}

// Frames returns the number of whole frames in the file.
func (i Info) Frames() int64 { return i.DataBytes / int64(i.BytesPerFrame) }

// ReadFile parses path's header.
func ReadFile(path string) (Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Info{}, err
	}
	return Read(f, st.Size())
}

// Read parses a CAF header from r, whose total length is size.
func Read(r io.ReadSeeker, size int64) (Info, error) {
	var info Info
	var fh [8]byte
	if _, err := io.ReadFull(r, fh[:]); err != nil {
		return info, err
	}
	if string(fh[0:4]) != "caff" {
		return info, errors.New("caf: not a CAF file")
	}
	pos := int64(8)
	haveDesc := false
	for {
		var ch [12]byte
		if _, err := io.ReadFull(r, ch[:]); err != nil {
			return info, fmt.Errorf("caf: no data chunk: %w", err)
		}
		typ := string(ch[0:4])
		n := int64(binary.BigEndian.Uint64(ch[4:12]))
		body := pos + 12
		switch typ {
		case "desc":
			var d [32]byte
			if _, err := io.ReadFull(r, d[:]); err != nil {
				return info, err
			}
			info.SampleRate = math.Float64frombits(binary.BigEndian.Uint64(d[0:8]))
			if string(d[8:12]) != "lpcm" {
				return info, fmt.Errorf("caf: format %q is not linear PCM", d[8:12])
			}
			flags := binary.BigEndian.Uint32(d[12:16])
			info.BytesPerFrame = int(binary.BigEndian.Uint32(d[16:20]))
			info.Channels = int(binary.BigEndian.Uint32(d[24:28]))
			info.BitsPerSample = int(binary.BigEndian.Uint32(d[28:32]))
			info.Float = flags&flagFloat != 0
			info.LittleEndian = flags&flagLittleEndian != 0
			haveDesc = true
		case "data":
			if !haveDesc || info.BytesPerFrame <= 0 {
				return info, errors.New("caf: data chunk before desc chunk")
			}
			info.DataOffset = body + 4 // skip mEditCount
			avail := size - info.DataOffset
			if n == -1 {
				info.SizeUnknown = true
			} else if n-4 < avail {
				avail = n - 4
			}
			info.TrailingBytes = avail % int64(info.BytesPerFrame)
			info.DataBytes = avail - info.TrailingBytes
			return info, nil
		}
		if n < 0 {
			return info, fmt.Errorf("caf: chunk %q has unknown size", typ)
		}
		pos = body + n
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			return info, err
		}
	}
}
