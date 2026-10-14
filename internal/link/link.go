// Package link reads the timeline-stamped audio that the RemoteSession
// Link plugin publishes through a memory-mapped ring
// (native/link/LinkShared.h; offsets here must match it).
package link

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"unsafe"
)

const (
	headerBytes = 4096
	metaBytes   = 64

	offWriteFrames = 64
	offWriteBlocks = 72
	offState       = 44
	offTrackName   = 128
	offHostName    = 384
)

// Block flags.
const (
	Playing     = 1 << 0
	Recording   = 1 << 1
	HasTimeline = 1 << 2
)

// Link states.
const (
	StateActive = 1
	StateClosed = 2
)

// Dir returns the directory link files are published in (RS_LINK_DIR
// overrides it, as in the plugin).
func Dir() string {
	if d := os.Getenv("RS_LINK_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Application Support", "AntiMusic", "RemoteSession", "links")
}

// Info describes one plugin instance's ring.
type Info struct {
	Path           string
	PID            int
	Channels       int
	SampleRate     float64
	CapacityFrames uint64
	MetaSlots      uint64
	State          uint32
	TrackName      string
	HostName       string
	Alive          bool // owning process still running
}

// Ring is an open, read-only mapping of a link file.
type Ring struct {
	Info
	mem []byte
}

// Open maps a link file.
func Open(path string) (*Ring, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() < headerBytes {
		return nil, errors.New("link: file too small")
	}
	mem, err := syscall.Mmap(int(f.Fd()), 0, int(st.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	r := &Ring{mem: mem}
	if string(mem[0:8]) != "RSLINK01" || binary.LittleEndian.Uint32(mem[8:]) != 1 {
		r.Close()
		return nil, fmt.Errorf("link: %s is not a version 1 link file", path)
	}
	le := binary.LittleEndian
	r.Path = path
	r.Channels = int(le.Uint32(mem[12:]))
	r.SampleRate = math.Float64frombits(le.Uint64(mem[16:]))
	r.CapacityFrames = le.Uint64(mem[24:])
	r.MetaSlots = le.Uint64(mem[32:])
	r.PID = int(le.Uint32(mem[40:]))
	want := headerBytes + r.MetaSlots*metaBytes + r.CapacityFrames*uint64(r.Channels)*4
	if r.Channels <= 0 || uint64(len(mem)) < want {
		r.Close()
		return nil, fmt.Errorf("link: %s is truncated", path)
	}
	r.refresh()
	return r, nil
}

func (r *Ring) refresh() {
	r.State = atomic.LoadUint32((*uint32)(unsafe.Pointer(&r.mem[offState])))
	r.TrackName = cstr(r.mem[offTrackName : offTrackName+256])
	r.HostName = cstr(r.mem[offHostName : offHostName+64])
	r.Alive = syscall.Kill(r.PID, 0) == nil
}

func cstr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

func (r *Ring) Close() error { return syscall.Munmap(r.mem) }

func (r *Ring) writeFrames() uint64 {
	return atomic.LoadUint64((*uint64)(unsafe.Pointer(&r.mem[offWriteFrames])))
}

func (r *Ring) writeBlocks() uint64 {
	return atomic.LoadUint64((*uint64)(unsafe.Pointer(&r.mem[offWriteBlocks])))
}

// Block is one processBlock call of the plugin.
type Block struct {
	Frame          uint64 // plugin stream frame of the first sample
	TimelineSample int64
	HostMicros     uint64
	Frames         uint32
	Flags          uint32
	SampleRate     float64
	Audio          []float32 // interleaved
}

func (b Block) Rolling() bool { return b.Flags&Playing != 0 && b.Flags&HasTimeline != 0 }

// Reader consumes blocks in order.
type Reader struct {
	r    *Ring
	next uint64
}

// NewReader starts reading at the oldest block still held in the ring, or
// at the newest if fromNow is set.
func (r *Ring) NewReader(fromNow bool) *Reader {
	wb := r.writeBlocks()
	next := uint64(0)
	if fromNow {
		next = wb
	} else if wb > r.MetaSlots {
		next = wb - r.MetaSlots
	}
	return &Reader{r: r, next: next}
}

// Read returns the blocks published since the last call. lost counts
// blocks that were overwritten before they could be read.
func (rd *Reader) Read() (blocks []Block, lost uint64) {
	r := rd.r
	wb := r.writeBlocks()
	if wb-rd.next > r.MetaSlots {
		lost += wb - r.MetaSlots - rd.next
		rd.next = wb - r.MetaSlots
	}
	le := binary.LittleEndian
	ch := uint64(r.Channels)
	audioBase := headerBytes + r.MetaSlots*metaBytes
	for ; rd.next < wb; rd.next++ {
		m := r.mem[headerBytes+(rd.next%r.MetaSlots)*metaBytes:]
		b := Block{
			Frame:          le.Uint64(m[0:]),
			TimelineSample: int64(le.Uint64(m[8:])),
			HostMicros:     le.Uint64(m[16:]),
			Frames:         le.Uint32(m[24:]),
			Flags:          le.Uint32(m[28:]),
			SampleRate:     math.Float64frombits(le.Uint64(m[32:])),
		}
		if r.writeFrames()-b.Frame > r.CapacityFrames {
			lost++
			continue
		}
		b.Audio = make([]float32, uint64(b.Frames)*ch)
		for f := uint64(0); f < uint64(b.Frames); f++ {
			src := audioBase + ((b.Frame+f)%r.CapacityFrames)*ch*4
			for c := uint64(0); c < ch; c++ {
				b.Audio[f*ch+c] = math.Float32frombits(le.Uint32(r.mem[src+c*4:]))
			}
		}
		// The writer may have lapped this block while it was copied.
		if r.writeFrames()-b.Frame > r.CapacityFrames || r.writeBlocks()-rd.next > r.MetaSlots {
			lost++
			continue
		}
		blocks = append(blocks, b)
	}
	return blocks, lost
}

// List returns the link files in dir, newest instance last.
func List(dir string) ([]Info, error) {
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".rslink") {
			continue
		}
		r, err := Open(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, r.Info)
		r.Close()
	}
	sort.Slice(out, func(i, j int) bool { return instanceKey(out[i].Path) < instanceKey(out[j].Path) })
	return out, nil
}

func instanceKey(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".rslink")
	pid, inst, _ := strings.Cut(base, "-")
	p, _ := strconv.Atoi(pid)
	i, _ := strconv.Atoi(inst)
	return fmt.Sprintf("%010d-%010d", p, i)
}
