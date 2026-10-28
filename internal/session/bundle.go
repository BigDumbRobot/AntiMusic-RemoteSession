// Package session runs an attended remote session between a client and an
// engineer: stems in, a live listen stream of the engineer's monitor mix,
// chat, and prints back, all over pairing-code connections the client
// dials (so it works behind any home router).
package session

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"antimusic.dev/remotesession/internal/deliver"
	"antimusic.dev/remotesession/internal/take"
	"antimusic.dev/remotesession/internal/timeline"
	"antimusic.dev/remotesession/internal/wav"
)

// Bundle makes dir a sendable take of kind (take.KindStems or
// take.KindPrints) holding copies of files, each placed relative to the
// session start.
//
// A file with a BWF timestamp is placed at timestamp - sessionStart when
// sessionStart is known (>= 0), as Pro Tools stamps recorded prints. A file
// without one, or when the start is unknown, is placed at the session
// start: DAWs bounce stems from the start of the song.
func Bundle(dir, kind string, files []string, sessionStart int64) (*deliver.Report, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("no files to send")
	}
	out := filepath.Join(dir, take.DeliverDir)
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, err
	}
	rep := &deliver.Report{Created: time.Now().UTC(), Codec: "as recorded"}
	rate := 0
	used := map[string]bool{}
	for _, f := range files {
		info, err := wav.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w (send WAV/BWF files)", filepath.Base(f), err)
		}
		if rate == 0 {
			rate = info.SampleRate
		} else if info.SampleRate != rate {
			return nil, fmt.Errorf("%s is %d Hz but the others are %d Hz", filepath.Base(f), info.SampleRate, rate)
		}
		name := filepath.Base(f)
		for i := 2; used[strings.ToLower(name)]; i++ {
			name = fmt.Sprintf("%s_%d%s", strings.TrimSuffix(filepath.Base(f), filepath.Ext(f)), i, filepath.Ext(f))
		}
		used[strings.ToLower(name)] = true
		if err := copyFile(f, filepath.Join(out, name)); err != nil {
			return nil, err
		}
		var off int64
		tref := int64(0)
		if info.HasBext {
			tref = int64(info.Bext.TimeReference)
			if sessionStart >= 0 {
				off = max(tref-sessionStart, 0)
			}
		}
		frames := info.DataBytes / int64(info.BlockAlign)
		rep.Deliverables = append(rep.Deliverables, deliver.Deliverable{
			File:          take.DeliverDir + "/" + name,
			Segment:       timeline.Segment{Frames: frames, TimelineStart: tref},
			TimeReference: tref,
			SessionOffset: off,
		})
	}
	rep.Session = &take.Session{Source: kind, SampleRate: rate}
	rep.SampleRate = rate
	if err := writeJSON(filepath.Join(out, deliver.ReportFile), rep); err != nil {
		return nil, err
	}
	m := &take.Manifest{Version: 1, Created: time.Now().UTC(), Kind: kind, State: take.StateComplete,
		SampleRate: rate, Session: rep.Session}
	return rep, take.WriteManifest(dir, m)
}

// copyFile hard-links when it can (same disk, instant) and copies otherwise.
func copyFile(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	o, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(o, in); err != nil {
		o.Close()
		return err
	}
	return o.Close()
}
