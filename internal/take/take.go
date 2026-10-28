// Package take defines the on-disk layout of a recorded take.
//
// A take directory holds everything needed to place the master later,
// with no dependency on the process, DAW or network that produced it:
//
//	take.json        manifest (rewritten atomically)
//	master.caf       lossless master written by FFmpeg
//	capture.log      FFmpeg per-block capture clock ({sn} {samp} {ptsi})
//	transport.jsonl  transport events, appended and fsynced one per line
//	deliver/         placed deliverables and delivery.json
package take

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	ManifestFile  = "take.json"
	MasterFile    = "master.caf"
	CaptureLog    = "capture.log"
	TransportFile = "transport.jsonl"
	DeliverDir    = "deliver"
)

// Kinds of take directory.
const (
	KindStems  = "stems"  // client to engineer
	KindPrints = "prints" // engineer back to client
)

// State of a take's recording.
const (
	StateRecording = "recording" // still running, or the recorder died
	StateComplete  = "complete"  // FFmpeg finalized the master
	StateFailed    = "failed"    // FFmpeg exited with an error
)

// Session is the DAW session the take was recorded against.
type Session struct {
	Source        string `json:"source"` // "ptsl", "manual", "simulated"
	Name          string `json:"name,omitempty"`
	SampleRate    int    `json:"sample_rate"`
	StartTimecode string `json:"start_timecode,omitempty"`
	TimecodeRate  string `json:"timecode_rate,omitempty"`
	StartSample   int64  `json:"start_sample"`
}

// Manifest describes a take.
type Manifest struct {
	Version int       `json:"version"`
	Created time.Time `json:"created"`
	// Kind is "" for a recorded take, or KindStems / KindPrints for a
	// bundle of finished files to place directly.
	Kind        string   `json:"kind,omitempty"`
	Input       []string `json:"input"` // FFmpeg input arguments
	SampleRate  int      `json:"sample_rate,omitempty"`
	Channels    int      `json:"channels,omitempty"`
	MasterCodec string   `json:"master_codec"`
	State       string   `json:"state"`
	Error       string   `json:"error,omitempty"`
	// LatencyFrames is the capture-path latency used when placing the
	// master (see timeline.Segments).
	LatencyFrames int64    `json:"latency_frames"`
	Session       *Session `json:"session,omitempty"`
}

// Event is one transport observation. Either Frame (exact, from a source
// that knows the capture frame) or HostMicros (capture-clock time, to be
// resolved through capture.log) must be set.
type Event struct {
	Frame          *int64 `json:"frame,omitempty"`
	HostMicros     int64  `json:"host_us,omitempty"`
	Rolling        bool   `json:"rolling"`
	TimelineSample int64  `json:"timeline_sample"`
	Source         string `json:"source"`
	// UncertaintyFrames bounds how far the true transport change may be
	// from the recorded time (0 = exact).
	UncertaintyFrames int64  `json:"uncertainty_frames,omitempty"`
	Note              string `json:"note,omitempty"`
}

// WriteManifest atomically replaces dir/take.json.
func WriteManifest(dir string, m *Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(filepath.Join(dir, ManifestFile), append(b, '\n'))
}

// ReadManifest loads dir/take.json.
func ReadManifest(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", ManifestFile, err)
	}
	return &m, nil
}

// WriteFileAtomic writes via a synced temporary file and rename, so a
// crash leaves either the old or the new contents.
func WriteFileAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// EventLog appends transport events durably.
type EventLog struct {
	mu sync.Mutex
	f  *os.File
}

// OpenEventLog opens dir/transport.jsonl for appending.
func OpenEventLog(dir string) (*EventLog, error) {
	f, err := os.OpenFile(filepath.Join(dir, TransportFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &EventLog{f: f}, nil
}

// Append writes one event and syncs it to disk.
func (l *EventLog) Append(e Event) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.f.Write(append(b, '\n')); err != nil {
		return err
	}
	return l.f.Sync()
}

func (l *EventLog) Close() error { return l.f.Close() }

// ReadEvents loads dir/transport.jsonl. A partial final line (a crash
// mid-append) is ignored; any other malformed line is an error.
func ReadEvents(dir string) ([]Event, error) {
	f, err := os.Open(filepath.Join(dir, TransportFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Event
	var pending error
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		if pending != nil {
			return nil, pending
		}
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			pending = fmt.Errorf("%s line %d: %w", TransportFile, n, err)
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}
