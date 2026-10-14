// Package wav reads RIFF/RF64 WAVE headers, including the EBU Tech 3285
// Broadcast Wave "bext" chunk. It exists so deliverables can be verified by
// code that is independent of the FFmpeg muxer that wrote them.
package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	FormatPCM        = 0x0001
	FormatFloat      = 0x0003
	FormatExtensible = 0xFFFE
)

// Info describes a WAVE file's audio layout and BWF metadata.
type Info struct {
	RF64          bool
	FormatTag     uint16 // as stored; see SubFormat for extensible files
	SubFormat     uint16 // effective format (PCM or float)
	Channels      int
	SampleRate    int
	BitsPerSample int
	BlockAlign    int
	DataOffset    int64 // byte offset of the first sample
	DataBytes     int64
	HasBext       bool
	Bext          Bext
}

// Frames returns the number of sample frames in the data chunk.
func (i Info) Frames() int64 {
	if i.BlockAlign == 0 {
		return 0
	}
	return i.DataBytes / int64(i.BlockAlign)
}

// Bext holds the fields of the Broadcast Audio Extension chunk that matter
// for placement and provenance.
type Bext struct {
	Description         string
	Originator          string
	OriginatorReference string
	OriginationDate     string
	OriginationTime     string
	TimeReference       uint64 // samples since midnight
	Version             uint16
	CodingHistory       string
}

// ReadFile parses the header chunks of a WAVE file.
func ReadFile(path string) (Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	return Read(f)
}

// Read parses the header chunks of a WAVE stream.
func Read(r io.ReadSeeker) (Info, error) {
	var info Info
	var hdr [12]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return info, err
	}
	switch string(hdr[0:4]) {
	case "RIFF":
	case "RF64":
		info.RF64 = true
	default:
		return info, errors.New("wav: not a RIFF/RF64 file")
	}
	if string(hdr[8:12]) != "WAVE" {
		return info, errors.New("wav: not a WAVE file")
	}
	var ds64Data int64 = -1
	pos := int64(12)
	haveFmt, haveData := false, false
	for !haveData {
		var ch [8]byte
		if _, err := io.ReadFull(r, ch[:]); err != nil {
			return info, fmt.Errorf("wav: reading chunk header at %d: %w", pos, err)
		}
		id := string(ch[0:4])
		size := int64(binary.LittleEndian.Uint32(ch[4:8]))
		body := pos + 8
		switch id {
		case "ds64":
			b := make([]byte, size)
			if _, err := io.ReadFull(r, b); err != nil {
				return info, err
			}
			if len(b) >= 16 {
				ds64Data = int64(binary.LittleEndian.Uint64(b[8:16]))
			}
		case "fmt ":
			b := make([]byte, size)
			if _, err := io.ReadFull(r, b); err != nil {
				return info, err
			}
			if err := parseFmt(b, &info); err != nil {
				return info, err
			}
			haveFmt = true
		case "bext":
			b := make([]byte, size)
			if _, err := io.ReadFull(r, b); err != nil {
				return info, err
			}
			if err := parseBext(b, &info.Bext); err != nil {
				return info, err
			}
			info.HasBext = true
		case "data":
			if !haveFmt {
				return info, errors.New("wav: data chunk before fmt chunk")
			}
			info.DataOffset = body
			info.DataBytes = size
			if info.RF64 && size == 0xFFFFFFFF {
				if ds64Data < 0 {
					return info, errors.New("wav: RF64 without ds64 data size")
				}
				info.DataBytes = ds64Data
			}
			haveData = true
			continue
		}
		pos = body + size + size&1
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			return info, err
		}
	}
	return info, nil
}

func parseFmt(b []byte, info *Info) error {
	if len(b) < 16 {
		return errors.New("wav: short fmt chunk")
	}
	info.FormatTag = binary.LittleEndian.Uint16(b[0:2])
	info.Channels = int(binary.LittleEndian.Uint16(b[2:4]))
	info.SampleRate = int(binary.LittleEndian.Uint32(b[4:8]))
	info.BlockAlign = int(binary.LittleEndian.Uint16(b[12:14]))
	info.BitsPerSample = int(binary.LittleEndian.Uint16(b[14:16]))
	info.SubFormat = info.FormatTag
	if info.FormatTag == FormatExtensible {
		if len(b) < 40 {
			return errors.New("wav: short extensible fmt chunk")
		}
		// The first two bytes of the SubFormat GUID carry the format code.
		info.SubFormat = binary.LittleEndian.Uint16(b[24:26])
	}
	return nil
}

func parseBext(b []byte, x *Bext) error {
	// EBU Tech 3285 v2 layout: fixed part is 602 bytes.
	if len(b) < 602 {
		return fmt.Errorf("wav: short bext chunk (%d bytes)", len(b))
	}
	str := func(s []byte) string { return string(bytes.TrimRight(s, "\x00")) }
	x.Description = str(b[0:256])
	x.Originator = str(b[256:288])
	x.OriginatorReference = str(b[288:320])
	x.OriginationDate = str(b[320:330])
	x.OriginationTime = str(b[330:338])
	x.TimeReference = uint64(binary.LittleEndian.Uint32(b[338:342])) |
		uint64(binary.LittleEndian.Uint32(b[342:346]))<<32
	x.Version = binary.LittleEndian.Uint16(b[346:348])
	x.CodingHistory = str(b[602:])
	return nil
}
