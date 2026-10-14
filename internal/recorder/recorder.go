// Package recorder captures a lossless master with FFmpeg.
//
// The master path has no network dependency. Device audio comes from
// rs-capture (native/capture), a real-time-safe CoreAudio reader whose
// raw float32 output is piped straight into FFmpeg's stdin; FFmpeg's own
// avfoundation input is not used for devices because it was measured
// dropping whole buffers (docs/ARCHITECTURE.md). Any other FFmpeg input
// (files, test sources) can be recorded directly.
//
// FFmpeg writes master.caf: CAF's data chunk is written with an
// "unknown" (-1) size that is only patched on clean shutdown, so a killed
// recorder still leaves a valid file that reads to EOF. Every packet is
// flushed as it is written (-flush_packets 1).
package recorder

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"antimusic.dev/remotesession/internal/take"
	"antimusic.dev/remotesession/internal/timeline"
)

// Config describes one take. Set exactly one of Device or Input.
type Config struct {
	Dir string
	// Device is a CoreAudio input device name or UID, captured with
	// rs-capture (all input channels).
	Device string
	// CaptureHelper is the rs-capture binary (default: next to this
	// executable, then $PATH).
	CaptureHelper string
	// CaptureArgs are extra rs-capture arguments (test rigs: --play).
	CaptureArgs []string
	// Stream supplies raw interleaved float32 PCM (e.g. from the Link
	// plugin) at StreamRate/StreamChannels. The caller writes capture.log,
	// keeps ownership of Stream, and ends the take by ending the stream
	// (then Wait).
	Stream         io.Reader
	StreamRate     int
	StreamChannels int
	StreamName     string
	// Input holds FFmpeg input options ending with "-i <source>".
	Input []string
	// MasterCodec defaults to pcm_f32le, which stores the device's
	// float32 samples bit for bit.
	MasterCodec   string
	FFmpeg        string
	LatencyFrames int64
	Session       *take.Session
}

// Recorder is a running capture.
type Recorder struct {
	dir     string
	ffmpeg  *exec.Cmd
	capture *exec.Cmd // nil for plain FFmpeg inputs
	stderr  *tailBuffer
	done    chan struct{}
	err     error

	mu       sync.Mutex
	manifest take.Manifest
	stopping bool
	Events   *take.EventLog
}

// Start creates the take directory and starts capturing.
func Start(cfg Config) (*Recorder, error) {
	sources := 0
	for _, set := range []bool{cfg.Device != "", len(cfg.Input) > 0, cfg.Stream != nil} {
		if set {
			sources++
		}
	}
	if sources != 1 {
		return nil, errors.New("recorder: set exactly one of Device, Input or Stream")
	}
	if cfg.MasterCodec == "" {
		cfg.MasterCodec = "pcm_f32le"
	}
	if cfg.FFmpeg == "" {
		cfg.FFmpeg = "ffmpeg"
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(cfg.Dir, take.MasterFile)); err == nil {
		return nil, fmt.Errorf("recorder: %s already holds a master", cfg.Dir)
	}
	r := &Recorder{
		dir:    cfg.Dir,
		stderr: &tailBuffer{max: 64 << 10},
		done:   make(chan struct{}),
		manifest: take.Manifest{
			Version:       1,
			Created:       time.Now().UTC(),
			MasterCodec:   cfg.MasterCodec,
			State:         take.StateRecording,
			LatencyFrames: cfg.LatencyFrames,
			Session:       cfg.Session,
		},
	}
	master := filepath.Join(cfg.Dir, take.MasterFile)
	clog := filepath.Join(cfg.Dir, take.CaptureLog)
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "warning"}
	var stdin io.Reader
	if cfg.Device != "" {
		capture, out, hdr, err := startCapture(cfg, clog, r.stderr)
		if err != nil {
			return nil, err
		}
		r.capture = capture
		stdin = out
		r.manifest.Input = []string{"rs-capture", hdr.Name, hdr.UID}
		r.manifest.SampleRate, r.manifest.Channels = hdr.Rate, hdr.Channels
		args = append(args, "-f", "f32le", "-ar", strconv.Itoa(hdr.Rate), "-ac", strconv.Itoa(hdr.Channels), "-i", "pipe:0")
	} else if cfg.Stream != nil {
		stdin = cfg.Stream
		r.manifest.Input = []string{"stream", cfg.StreamName}
		r.manifest.SampleRate, r.manifest.Channels = cfg.StreamRate, cfg.StreamChannels
		args = append(args, "-f", "f32le", "-ar", strconv.Itoa(cfg.StreamRate), "-ac", strconv.Itoa(cfg.StreamChannels), "-i", "pipe:0")
	} else {
		r.manifest.Input = cfg.Input
		args = append(args, "-copyts")
		args = append(args, cfg.Input...)
		args = append(args, "-stats_enc_pre", clog, "-stats_enc_pre_fmt", timeline.CaptureLogFormat)
	}
	args = append(args, "-map", "0:a:0", "-c:a", cfg.MasterCodec, "-flush_packets", "1", "-f", "caf", master)

	var err error
	if r.Events, err = take.OpenEventLog(cfg.Dir); err != nil {
		r.killCapture()
		return nil, err
	}
	if err := take.WriteManifest(cfg.Dir, &r.manifest); err != nil {
		r.killCapture()
		return nil, err
	}
	r.ffmpeg = exec.Command(cfg.FFmpeg, args...)
	r.ffmpeg.Stdin = stdin
	r.ffmpeg.Stderr = r.stderr
	if err := r.ffmpeg.Start(); err != nil {
		r.killCapture()
		r.Events.Close()
		return nil, err
	}
	if c, ok := stdin.(io.Closer); ok && r.capture != nil {
		c.Close() // FFmpeg holds its own copy of the capture pipe
	}
	go r.wait()
	return r, nil
}

type captureHeader struct {
	Rate     int    `json:"rate"`
	Channels int    `json:"channels"`
	Name     string `json:"name"`
	UID      string `json:"uid"`
}

// startCapture launches rs-capture and reads its JSON header. The
// returned reader is the read end of the PCM pipe, for FFmpeg's stdin.
func startCapture(cfg Config, logPath string, diag io.Writer) (*exec.Cmd, *os.File, captureHeader, error) {
	var hdr captureHeader
	helper := cfg.CaptureHelper
	if helper == "" {
		helper = FindCaptureHelper()
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, nil, hdr, err
	}
	cmd := exec.Command(helper, append([]string{"--device", cfg.Device, "--log", logPath}, cfg.CaptureArgs...)...)
	cmd.Stdout = pw
	se, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, hdr, err
	}
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, nil, hdr, fmt.Errorf("starting %s: %w", helper, err)
	}
	pw.Close()
	br := bufio.NewReader(se)
	line, err := br.ReadBytes('\n')
	if err == nil {
		err = json.Unmarshal(line, &hdr)
	}
	if err != nil || hdr.Rate <= 0 || hdr.Channels <= 0 {
		rest, _ := io.ReadAll(br)
		cmd.Wait()
		pr.Close()
		return nil, nil, hdr, fmt.Errorf("rs-capture: %s%s", bytes.TrimSpace(line), bytes.TrimSpace(rest))
	}
	go io.Copy(diag, br)
	return cmd, pr, hdr, nil
}

// FindCaptureHelper locates rs-capture next to the running executable,
// falling back to $PATH.
func FindCaptureHelper() string {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "rs-capture")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "rs-capture"
}

func (r *Recorder) killCapture() {
	if r.capture != nil {
		r.capture.Process.Kill()
		r.capture.Wait()
	}
}

func (r *Recorder) wait() {
	err := r.ffmpeg.Wait()
	r.mu.Lock()
	stopping := r.stopping
	r.mu.Unlock()
	// FFmpeg exits 255 after a requested SIGINT; that is a clean stop.
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 255 && stopping {
		err = nil
	}
	if r.capture != nil {
		// FFmpeg has exited, so the capture helper's writes now fail and
		// it exits on its own; make sure it does.
		r.capture.Process.Signal(syscall.SIGTERM)
		cerr := r.capture.Wait()
		switch {
		case err != nil || cerr == nil:
		case errors.As(cerr, &ee) && ee.ExitCode() == 3:
			err = fmt.Errorf("capture device fault (master kept up to the fault): %w", cerr)
		case !stopping:
			err = fmt.Errorf("capture: %w", cerr)
		}
	}
	r.mu.Lock()
	r.err = err
	r.mu.Unlock()
	close(r.done)
}

// Done is closed when recording has ended.
func (r *Recorder) Done() <-chan struct{} { return r.done }

// Stop ends the take cleanly and finalizes the manifest. With a capture
// helper, the helper is interrupted and FFmpeg finishes on end of input;
// otherwise FFmpeg is interrupted. Anything that does not exit within
// timeout is killed; the CAF master stays readable either way.
func (r *Recorder) Stop(timeout time.Duration) error {
	r.mu.Lock()
	r.stopping = true
	r.mu.Unlock()
	select {
	case <-r.done:
	default:
		if r.capture != nil {
			_ = r.capture.Process.Signal(syscall.SIGINT)
		} else {
			_ = r.ffmpeg.Process.Signal(syscall.SIGINT)
		}
		select {
		case <-r.done:
		case <-time.After(timeout):
			_ = r.ffmpeg.Process.Kill()
			<-r.done
		}
	}
	return r.finalize()
}

// Wait blocks until recording ends on its own (finite input, device
// fault) and finalizes the manifest.
func (r *Recorder) Wait() error {
	<-r.done
	return r.finalize()
}

// Abort kills everything without finalizing, as a crash or power loss of
// the recording process would. The take is left in the "recording" state.
func (r *Recorder) Abort() {
	_ = r.ffmpeg.Process.Kill()
	if r.capture != nil {
		_ = r.capture.Process.Kill()
	}
	<-r.done
	r.Events.Close()
}

func (r *Recorder) finalize() error {
	r.Events.Close()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		r.manifest.State = take.StateFailed
		r.manifest.Error = fmt.Sprintf("%v: %s", r.err, bytes.TrimSpace(r.stderr.Bytes()))
	} else {
		r.manifest.State = take.StateComplete
	}
	if p, err := Probe(context.Background(), filepath.Join(r.dir, take.MasterFile)); err == nil {
		r.manifest.SampleRate, r.manifest.Channels = p.SampleRate, p.Channels
	}
	if err := take.WriteManifest(r.dir, &r.manifest); err != nil {
		return err
	}
	if r.err != nil {
		return errors.New(r.manifest.Error)
	}
	return nil
}

// Stderr returns the tail of FFmpeg's and the capture helper's
// diagnostic output.
func (r *Recorder) Stderr() string { return r.stderr.String() }

// StreamInfo is the audio layout of a file as reported by ffprobe.
type StreamInfo struct {
	SampleRate int
	Channels   int
	Codec      string
}

// Probe reads the first audio stream's layout with ffprobe.
func Probe(ctx context.Context, path string) (StreamInfo, error) {
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=sample_rate,channels,codec_name", "-of", "json", path).Output()
	if err != nil {
		return StreamInfo{}, fmt.Errorf("ffprobe %s: %w", path, err)
	}
	var v struct {
		Streams []struct {
			SampleRate string `json:"sample_rate"`
			Channels   int    `json:"channels"`
			CodecName  string `json:"codec_name"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &v); err != nil || len(v.Streams) == 0 {
		return StreamInfo{}, fmt.Errorf("ffprobe %s: no audio stream", path)
	}
	sr, _ := strconv.Atoi(v.Streams[0].SampleRate)
	return StreamInfo{SampleRate: sr, Channels: v.Streams[0].Channels, Codec: v.Streams[0].CodecName}, nil
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) Bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]byte(nil), t.buf...)
}

func (t *tailBuffer) String() string { return string(t.Bytes()) }
