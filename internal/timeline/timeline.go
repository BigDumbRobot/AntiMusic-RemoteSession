// Package timeline maps frames of a local master recording onto the DAW
// session timeline.
//
// Three positions are kept distinct:
//   - master frame: index of a sample frame in the local master file;
//   - host time: the capture clock (mach_absolute_time) at which a frame
//     was captured, used only to translate host-timed events into frames;
//   - timeline sample: absolute session position in samples since
//     00:00:00:00, the unit of a BWF TimeReference.
//
// The DAW transport, not wall-clock time, decides which timeline sample a
// master frame belongs to.
package timeline

import (
	"errors"
	"sort"
)

// Event records a transport state that begins at a given master frame.
type Event struct {
	Frame   int64 `json:"frame"`
	Rolling bool  `json:"rolling"`
	// TimelineSample is the session position of Frame. Only meaningful
	// while Rolling.
	TimelineSample int64 `json:"timeline_sample"`
}

// Segment is a contiguous run of master frames with a single, linear
// placement on the timeline.
type Segment struct {
	MasterStart   int64 `json:"master_start"`
	Frames        int64 `json:"frames"`
	TimelineStart int64 `json:"timeline_start"`
}

// TimelineEnd returns the first timeline sample after the segment.
func (s Segment) TimelineEnd() int64 { return s.TimelineStart + s.Frames }

// Segments derives the placeable segments of a master of masterFrames
// frames from a transport event list.
//
// latency is the number of frames by which captured audio lags the
// transport position reported for the same frame (input/converter/
// monitoring path latency). Audio for timeline position T while the
// transport was at frame F lands at master frame F+latency, so master
// ranges are shifted by +latency and clipped to the master.
//
// A rolling event that continues the current run linearly does not split
// it; any jump in position (locate, loop, punch) starts a new segment.
// Segments shorter than minFrames are dropped.
func Segments(events []Event, masterFrames, latency, minFrames int64) ([]Segment, error) {
	if masterFrames < 0 {
		return nil, errors.New("timeline: negative master length")
	}
	evs := normalize(events)
	var out []Segment
	for i := 0; i < len(evs); i++ {
		e := evs[i]
		if !e.Rolling {
			continue
		}
		// Extend the run through events that continue it linearly.
		j := i + 1
		for j < len(evs) && evs[j].Rolling &&
			evs[j].TimelineSample-e.TimelineSample == evs[j].Frame-e.Frame {
			j++
		}
		end := masterFrames - latency // run lasts to the end of the take
		if j < len(evs) {
			end = evs[j].Frame
		}
		seg := Segment{
			MasterStart:   e.Frame + latency,
			Frames:        end - e.Frame,
			TimelineStart: e.TimelineSample,
		}
		if seg.MasterStart < 0 {
			cut := -seg.MasterStart
			seg.MasterStart = 0
			seg.Frames -= cut
			seg.TimelineStart += cut
		}
		if over := seg.MasterStart + seg.Frames - masterFrames; over > 0 {
			seg.Frames -= over
		}
		if seg.Frames > 0 && seg.Frames >= minFrames {
			out = append(out, seg)
		}
		i = j - 1
	}
	return out, nil
}

// TrimBefore cuts the part of each segment that lies before timeline sample
// start, dropping segments left shorter than minFrames (or empty). Hosts
// pre-roll by the delay-compensation amount, so a run started at the
// session start begins slightly before it; Pro Tools cannot place audio
// there. It returns the trimmed segments and the number of frames cut.
func TrimBefore(segs []Segment, start, minFrames int64) ([]Segment, int64) {
	var out []Segment
	var cut int64
	for _, s := range segs {
		if d := start - s.TimelineStart; d > 0 {
			d = min(d, s.Frames)
			s.MasterStart += d
			s.Frames -= d
			s.TimelineStart += d
			cut += d
		}
		if s.Frames > 0 && s.Frames >= minFrames {
			out = append(out, s)
		}
	}
	return out, cut
}

// normalize sorts events by frame and keeps only the last event recorded
// for any given frame.
func normalize(events []Event) []Event {
	evs := append([]Event(nil), events...)
	sort.SliceStable(evs, func(a, b int) bool { return evs[a].Frame < evs[b].Frame })
	out := evs[:0]
	for _, e := range evs {
		if n := len(out); n > 0 && out[n-1].Frame == e.Frame {
			out[n-1] = e
			continue
		}
		out = append(out, e)
	}
	return out
}
