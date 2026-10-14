// Package transport records DAW transport changes into a take's journal.
package transport

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sys/unix"

	"antimusic.dev/remotesession/internal/ptsl"
	"antimusic.dev/remotesession/internal/take"
)

// HostMicros reads the capture clock: mach_absolute_time in µs, the clock
// avfoundation stamps captured audio with (verified against FFmpeg's
// reported start PTS; see docs/ARCHITECTURE.md).
func HostMicros() int64 {
	var ts unix.Timespec
	_ = unix.ClockGettime(unix.CLOCK_UPTIME_RAW, &ts)
	return ts.Sec*1_000_000 + ts.Nsec/1000
}

// PTSLWatcher polls Pro Tools' transport and logs rolling/stopped changes.
//
// PTSL has no playhead query and no transport notifications, so a change
// is only known to have happened between two polls. Each event is
// stamped at the midpoint of that window with half its width as the
// declared uncertainty. This is a coarse placement source; a host plugin
// reporting per-buffer positions is required for sample accuracy.
type PTSLWatcher struct {
	Client     *ptsl.Client
	Log        *take.EventLog
	Interval   time.Duration
	SampleRate int
	// OnError is called for each failed poll; polling continues. The
	// master recording is unaffected by PTSL failures.
	OnError func(error)
	// OnEvent is called after each logged event.
	OnEvent func(take.Event)
}

// Run polls until ctx is cancelled.
func (w *PTSLWatcher) Run(ctx context.Context) {
	if w.Interval <= 0 {
		w.Interval = 10 * time.Millisecond
	}
	tick := time.NewTicker(w.Interval)
	defer tick.Stop()
	rolling, known := false, false
	lastPoll := HostMicros()
	for {
		before := HostMicros()
		state, err := w.Client.TransportState(ctx)
		after := HostMicros()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if w.OnError != nil {
				w.OnError(err)
			}
			known = false // re-establish state after an outage
		} else if r := ptsl.Rolling(state); !known || r != rolling {
			// The change happened after the previous successful poll
			// started and before this poll's reply.
			lo, hi := lastPoll, after
			if !known {
				lo = before
			}
			if lerr := w.logChange(ctx, r, state, lo, hi); lerr != nil {
				if w.OnError != nil {
					w.OnError(lerr)
				}
			} else {
				rolling, known = r, true
			}
		}
		if err == nil {
			lastPoll = before
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (w *PTSLWatcher) logChange(ctx context.Context, rolling bool, state string, lo, hi int64) error {
	e := take.Event{
		HostMicros:        (lo + hi) / 2,
		Rolling:           rolling,
		Source:            "ptsl",
		UncertaintyFrames: (hi - lo) / 2 * int64(w.SampleRate) / 1_000_000,
		Note:              state,
	}
	if rolling {
		sel, err := w.Client.TimelineSelection(ctx)
		if err != nil {
			return fmt.Errorf("reading timeline selection: %w", err)
		}
		// Playback starts at the selection start, or at the pre-roll
		// start when pre-roll is enabled. (Pro Tools semantics; not yet
		// verified against a running session.)
		e.TimelineSample = sel.InSample
		if sel.PreRollEnabled {
			e.TimelineSample = sel.PreRollStartSample
		}
	}
	if err := w.Log.Append(e); err != nil {
		return err
	}
	if w.OnEvent != nil {
		w.OnEvent(e)
	}
	return nil
}
