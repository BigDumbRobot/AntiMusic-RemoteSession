package link

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"time"

	"antimusic.dev/remotesession/internal/take"
)

// PumpStats are a Pump's running totals.
type PumpStats struct {
	Frames       int64 // master frames written, including filled gaps
	FilledFrames int64
	LostBlocks   uint64
	Events       int
}

// Pump turns a Link stream into a take. Its state persists across Run
// calls, so a take may be pumped in several stretches.
type Pump struct {
	Reader *Reader
	PCM    io.Writer // the recorder's raw float32 input
	Log    io.Writer // capture.log
	Events *take.EventLog
	Poll   time.Duration
	Stats  PumpStats
	// SessionStart (samples since midnight) is added to plugin positions:
	// hosts report them relative to the session start (verified in Pro
	// Tools), while take events are absolute.
	SessionStart int64

	w            *bufio.Writer
	started      bool
	nextStream   uint64 // expected plugin frame of the next block
	prevRolling  bool
	nextTimeline int64
	buf          []byte
}

// Run pumps until ctx ends or the plugin closes its link: it writes the audio as raw float32
// to pcm (the recorder's FFmpeg input), one capture-log line per block to
// clog (timeline.CaptureLogFormat plus a filled flag), and a transport
// event whenever the plugin's transport starts, stops, or jumps (locate,
// loop). Events carry exact master frames: the plugin stamps each block
// with the host's own timeline position, so no polling uncertainty
// applies.
//
// Blocks lost to a reader that fell behind are filled with silence so
// master frames stay aligned to the plugin's stream, and re-anchored with
// an event. Pump returns when ctx ends or the plugin closes its link.
func (p *Pump) Run(ctx context.Context) error {
	if p.w == nil {
		p.w = bufio.NewWriterSize(p.PCM, 1<<16)
	}
	if p.Poll <= 0 {
		p.Poll = 2 * time.Millisecond
	}
	st := &p.Stats
	rd := p.Reader
	ch := rd.r.Channels
	rate := rd.r.SampleRate
	emit := func(rolling bool, tl int64, note string) error {
		f := st.Frames
		st.Events++
		if rolling {
			tl += p.SessionStart
		}
		return p.Events.Append(take.Event{Frame: &f, Rolling: rolling, TimelineSample: tl, Source: "link", Note: note})
	}
	for {
		blocks, lost := rd.Read()
		st.LostBlocks += lost
		for _, b := range blocks {
			if b.SampleRate != rate {
				return fmt.Errorf("link: sample rate changed from %v to %v", rate, b.SampleRate)
			}
			forceEvent := false
			if p.started && b.Frame != p.nextStream {
				if b.Frame < p.nextStream {
					return fmt.Errorf("link: stream went backwards (%d after %d)", b.Frame, p.nextStream)
				}
				gap := int64(b.Frame - p.nextStream)
				if _, err := p.w.Write(make([]byte, gap*int64(ch)*4)); err != nil {
					return err
				}
				fmt.Fprintf(p.Log, "%d %d %d 1/1000000 %d 1/%d 1\n", st.Frames, gap, b.HostMicros, p.nextStream, int(rate))
				st.Frames += gap
				st.FilledFrames += gap
				forceEvent = true
			}
			rolling := b.Rolling()
			switch {
			case !p.started || rolling != p.prevRolling || forceEvent:
				tl := int64(0)
				if rolling {
					tl = b.TimelineSample
				}
				if err := emit(rolling, tl, noteFor(b)); err != nil {
					return err
				}
			case rolling && b.TimelineSample != p.nextTimeline:
				if err := emit(true, b.TimelineSample, "locate"); err != nil {
					return err
				}
			}
			p.started, p.prevRolling = true, rolling
			p.nextTimeline = b.TimelineSample + int64(b.Frames)
			p.nextStream = b.Frame + uint64(b.Frames)

			if cap(p.buf) < len(b.Audio)*4 {
				p.buf = make([]byte, len(b.Audio)*4)
			}
			p.buf = p.buf[:len(b.Audio)*4]
			for i, v := range b.Audio {
				binary.LittleEndian.PutUint32(p.buf[i*4:], math.Float32bits(v))
			}
			if _, err := p.w.Write(p.buf); err != nil {
				return err
			}
			fmt.Fprintf(p.Log, "%d %d %d 1/1000000 %d 1/%d 0\n", st.Frames, b.Frames, b.HostMicros, b.Frame, int(rate))
			st.Frames += int64(b.Frames)
		}
		if err := p.w.Flush(); err != nil {
			return err
		}
		if len(blocks) == 0 {
			rd.r.refresh()
			if rd.r.State == StateClosed || !rd.r.Alive {
				return nil
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(p.Poll):
			}
		}
	}
}

func noteFor(b Block) string {
	switch {
	case b.Flags&Recording != 0:
		return "recording"
	case b.Flags&Playing != 0:
		return "playing"
	default:
		return "stopped"
	}
}
