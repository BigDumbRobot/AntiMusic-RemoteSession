// Command rsession records lossless masters and delivers them as
// timeline-placed Broadcast Wave files.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"antimusic.dev/remotesession/internal/deliver"
	"antimusic.dev/remotesession/internal/link"
	"antimusic.dev/remotesession/internal/ptsl"
	"antimusic.dev/remotesession/internal/recorder"
	"antimusic.dev/remotesession/internal/take"
	"antimusic.dev/remotesession/internal/timeline"
	"antimusic.dev/remotesession/internal/transport"
)

const usage = `usage: rsession <command> [flags]

commands:
  devices        list CoreAudio input devices
  record         record a lossless master, journaling DAW transport
  deliver        render placed BWF deliverables from a take and verify them
  links          list RemoteSession Link plugin instances (-watch <track> for live diagnostics)
  ptsl-status    show Pro Tools session and transport via PTSL
  ptsl-import    import a take's deliverables into Pro Tools at their positions
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "devices":
		err = devices()
	case "record":
		err = record(args)
	case "deliver":
		err = deliverCmd(args)
	case "links":
		err = links(args)
	case "ptsl-status":
		err = ptslStatus(args)
	case "ptsl-import":
		err = ptslImport(args)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "rsession:", err)
		os.Exit(1)
	}
}

func devices() error {
	cmd := exec.Command(recorder.FindCaptureHelper(), "--list")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func record(args []string) error {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	device := fs.String("device", "", "CoreAudio input device name or UID (see rsession devices)")
	linkSel := fs.String("link", "", "record a RemoteSession Link plugin instance (track name or .rslink path) with exact transport stamps")
	dir := fs.String("dir", "", "take directory to create")
	latency := fs.Int64("latency", 0, "capture-path latency in frames (measured)")
	codec := fs.String("codec", "pcm_f32le", "master codec: pcm_f32le (bit-exact device float) or pcm_s24le")
	usePTSL := fs.Bool("ptsl", false, "read session and journal transport from Pro Tools via PTSL")
	addr := fs.String("ptsl-addr", ptsl.DefaultAddress, "PTSL address")
	poll := fs.Duration("poll", 10*time.Millisecond, "PTSL transport poll interval")
	rate := fs.Int("session-rate", 0, "session sample rate (without -ptsl)")
	start := fs.String("session-start", "00:00:00:00", "session start timecode (without -ptsl)")
	fps := fs.String("fps", "25", "timecode rate for -session-start: 23.976 24 25 29.97 29.97df 30 50 59.94 59.94df 60")
	fs.Parse(args)
	if (*device == "") == (*linkSel == "") || *dir == "" {
		return errors.New("record: -dir and exactly one of -device or -link are required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var sess *take.Session
	var client *ptsl.Client
	if *usePTSL {
		c, s, err := connectPTSL(ctx, *addr)
		if err != nil {
			return err
		}
		defer c.Close()
		client = c
		sess = &take.Session{Source: "ptsl", Name: s.Name, SampleRate: s.SampleRate,
			StartTimecode: s.StartTimecode, TimecodeRate: s.TimecodeRate, StartSample: s.StartSample}
	} else if *rate > 0 {
		fr, ok := frameRates[*fps]
		if !ok {
			return fmt.Errorf("record: unknown -fps %q", *fps)
		}
		n, err := timeline.TimecodeToSamples(*start, fr, *rate)
		if err != nil {
			return err
		}
		sess = &take.Session{Source: "manual", SampleRate: *rate, StartTimecode: *start, StartSample: n}
	}

	if *linkSel != "" {
		return recordLink(ctx, *linkSel, *dir, *codec, sess)
	}
	r, err := recorder.Start(recorder.Config{
		Dir: *dir, Device: *device,
		MasterCodec: *codec, LatencyFrames: *latency, Session: sess,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "recording %s -> %s (Ctrl-C to stop)\n", *device, filepath.Join(*dir, take.MasterFile))

	if client != nil {
		w := &transport.PTSLWatcher{
			Client: client, Log: r.Events, Interval: *poll, SampleRate: sess.SampleRate, SessionStart: sess.StartSample,
			OnError: func(err error) { fmt.Fprintln(os.Stderr, "ptsl (master unaffected):", err) },
			OnEvent: func(e take.Event) {
				fmt.Fprintf(os.Stderr, "transport %s at timeline %d (±%d frames)\n", e.Note, e.TimelineSample, e.UncertaintyFrames)
			},
		}
		go w.Run(ctx)
	}
	select {
	case <-ctx.Done():
		err = r.Stop(10 * time.Second)
	case <-r.Done():
		err = r.Wait()
	}
	m, _ := take.ReadManifest(*dir)
	if m != nil {
		fmt.Fprintf(os.Stderr, "take %s: state %s, %d Hz, %d ch\n", *dir, m.State, m.SampleRate, m.Channels)
	}
	return err
}

var frameRates = map[string]timeline.FrameRate{
	"23.976": timeline.FPS23976, "24": timeline.FPS24, "25": timeline.FPS25,
	"29.97": timeline.FPS2997, "29.97df": timeline.FPS2997DF, "30": timeline.FPS30,
	"50": timeline.FPS50, "59.94": timeline.FPS5994, "59.94df": timeline.FPS5994DF, "60": timeline.FPS60,
}

func deliverCmd(args []string) error {
	fs := flag.NewFlagSet("deliver", flag.ExitOnError)
	dir := fs.String("dir", "", "take directory")
	codec := fs.String("codec", "pcm_s24le", "pcm_s24le, pcm_s16le or pcm_f32le")
	minFrames := fs.Int64("min-frames", 0, "drop segments shorter than this")
	chain := fs.String("chain", "", `post-processing chain JSON: [{"id":"<AU id or plugin path>","state":"<optional state file>"}, ...]`)
	label := fs.String("label", "processed", "output folder name under deliver/ for a processed print")
	tail := fs.Int64("tail", 0, "frames of plugin decay to keep after each segment")
	fs.Parse(args)
	var plugins []deliver.Plugin
	if *chain != "" {
		b, err := os.ReadFile(*chain)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &plugins); err != nil {
			return fmt.Errorf("%s: %w", *chain, err)
		}
	}
	if *dir == "" {
		return errors.New("deliver: -dir is required")
	}
	rep, err := deliver.Render(context.Background(), *dir, deliver.Options{Codec: *codec, MinFrames: *minFrames,
		Chain: plugins, Label: *label, TailFrames: *tail, Renderer: siblingTool("rs-render")})
	if err != nil {
		return err
	}
	for _, w := range rep.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	if rep.UncertaintyFrames > 0 {
		fmt.Fprintf(os.Stderr, "warning: placement is only known to ±%d frames (transport source is not sample-accurate)\n", rep.UncertaintyFrames)
	}
	for _, d := range rep.Deliverables {
		check := fmt.Sprintf("verified (max error %.2f LSB)", d.MaxErrorLSB)
		if d.Chain != nil {
			check = fmt.Sprintf("chain latency %d compensated", d.Chain.Latency)
			if d.Alignment != nil && d.Alignment.Checked {
				check += fmt.Sprintf(", aligned with dry (correlation %.2f)", d.Alignment.Correlation)
			}
		}
		fmt.Printf("%s\ttimeline %d\tsession +%d\t%d frames\t%s\n",
			d.File, d.TimeReference, d.SessionOffset, d.Segment.Frames, check)
	}
	return nil
}

func connectPTSL(ctx context.Context, addr string) (*ptsl.Client, ptsl.SessionInfo, error) {
	c, err := ptsl.Dial(addr)
	if err != nil {
		return nil, ptsl.SessionInfo{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.Register(cctx, "AntiMusic", "RemoteSession"); err != nil {
		c.Close()
		return nil, ptsl.SessionInfo{}, fmt.Errorf("connecting to Pro Tools at %s: %w", addr, err)
	}
	s, err := c.Session(cctx)
	if err != nil {
		c.Close()
		return nil, s, fmt.Errorf("reading session: %w", err)
	}
	return c, s, nil
}

func ptslStatus(args []string) error {
	fs := flag.NewFlagSet("ptsl-status", flag.ExitOnError)
	addr := fs.String("ptsl-addr", ptsl.DefaultAddress, "PTSL address")
	fs.Parse(args)
	ctx := context.Background()
	c, s, err := connectPTSL(ctx, *addr)
	if err != nil {
		return err
	}
	defer c.Close()
	st, err := c.TransportState(ctx)
	if err != nil {
		return err
	}
	sel, err := c.TimelineSelection(ctx)
	if err != nil {
		return err
	}
	delay, dcEnabled, err := c.SystemDelay(ctx)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"session": s, "transport": st, "selection": sel,
		"system_delay_samples": delay, "delay_compensation": dcEnabled,
	})
}

func ptslImport(args []string) error {
	fs := flag.NewFlagSet("ptsl-import", flag.ExitOnError)
	dir := fs.String("dir", "", "take directory (after deliver)")
	addr := fs.String("ptsl-addr", ptsl.DefaultAddress, "PTSL address")
	fs.Parse(args)
	if *dir == "" {
		return errors.New("ptsl-import: -dir is required")
	}
	b, err := os.ReadFile(filepath.Join(*dir, take.DeliverDir, deliver.ReportFile))
	if err != nil {
		return err
	}
	var rep deliver.Report
	if err := json.Unmarshal(b, &rep); err != nil {
		return err
	}
	ctx := context.Background()
	c, _, err := connectPTSL(ctx, *addr)
	if err != nil {
		return err
	}
	defer c.Close()
	var sessPath struct {
		SessionPath struct {
			Path string `json:"path"`
		} `json:"session_path"`
	}
	if err := c.Call(ctx, ptsl.CmdGetSessionPath, nil, &sessPath); err != nil {
		return err
	}
	audioDir := filepath.Join(filepath.Dir(sessPath.SessionPath.Path), "Audio Files")
	for _, d := range rep.Deliverables {
		abs, err := filepath.Abs(filepath.Join(*dir, d.File))
		if err != nil {
			return err
		}
		pos := d.SessionOffset // PTSL positions are relative to the session start
		if err := c.ImportSpotted(ctx, []string{abs}, audioDir, pos); err != nil {
			return fmt.Errorf("importing %s: %w", d.File, err)
		}
		fmt.Printf("imported %s at session +%d samples (timeline %d)\n", d.File, pos, d.TimeReference)
	}
	return nil
}

// siblingTool finds a helper binary next to this executable, falling back
// to $PATH.
func siblingTool(name string) string {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return name
}

func links(args []string) error {
	fs := flag.NewFlagSet("links", flag.ExitOnError)
	watch := fs.String("watch", "", "print live transport changes and onsets for a Link instance (track name or .rslink path)")
	onset := fs.Float64("onset", 0.5, "with -watch: peak level (0-1) that counts as an onset after silence")
	fs.Parse(args)
	if *watch != "" {
		return watchLink(*watch, *onset)
	}
	infos, err := link.List(link.Dir())
	if err != nil {
		return err
	}
	for _, i := range infos {
		state := map[uint32]string{link.StateActive: "active", link.StateClosed: "closed"}[i.State]
		if !i.Alive {
			state = "stale"
		}
		fmt.Printf("%s\t%q\t%s\t%.0f Hz\t%d ch\t%s\n", state, i.TrackName, i.HostName, i.SampleRate, i.Channels, i.Path)
	}
	return nil
}

// openLink opens a Link instance by .rslink path, or by track name (the
// newest active instance on that track wins).
func openLink(sel string) (*link.Ring, error) {
	path := sel
	if !strings.HasSuffix(sel, ".rslink") {
		infos, err := link.List(link.Dir())
		if err != nil {
			return nil, err
		}
		path = ""
		for _, i := range infos {
			if i.Alive && i.State == link.StateActive && i.TrackName == sel {
				path = i.Path
			}
		}
		if path == "" {
			return nil, fmt.Errorf("no active Link instance on a track named %q (see rsession links)", sel)
		}
	}
	return link.Open(path)
}

// watchLink prints what the host tells a Link instance: transport state
// changes, how the timeline position moves, and the exact timeline sample
// of each onset (a peak after silence). It is a diagnostic for checking
// host behaviour, e.g. delay compensation and the stopped state.
func watchLink(sel string, onset float64) error {
	ring, err := openLink(sel)
	if err != nil {
		return err
	}
	defer ring.Close()
	rate := ring.SampleRate
	fmt.Printf("watching Link %q (%s), %.0f Hz, %d ch. Ctrl-C to stop.\n", ring.TrackName, ring.HostName, rate, ring.Channels)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rd := ring.NewReader(true)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()

	var (
		prev     link.Block
		have     bool
		motion   string
		quiet    int64 // consecutive frames below onset/10
		armAfter = int64(rate / 10)
		blocks   int
		last     = time.Now()
	)
	pos := func(b link.Block, off int64) string {
		if b.Flags&link.HasTimeline == 0 {
			return fmt.Sprintf("stream frame %d (no timeline)", int64(b.Frame)+off)
		}
		s := b.TimelineSample + off
		return fmt.Sprintf("timeline %d (%s)", s, clock(s, rate))
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		bs, lost := rd.Read()
		if lost > 0 {
			fmt.Printf("! %d blocks lost (reader fell behind)\n", lost)
		}
		for _, b := range bs {
			blocks++
			if !have || b.Flags != prev.Flags {
				fmt.Printf("state %-20s at %s, block %d frames\n", flagNames(b.Flags), pos(b, 0), b.Frames)
				motion = ""
			}
			if have && b.Flags&prev.Flags&link.HasTimeline != 0 {
				d := b.TimelineSample - prev.TimelineSample
				m := "jump"
				switch d {
				case int64(prev.Frames):
					m = "advancing"
				case 0:
					m = "static"
				}
				if m == "jump" {
					fmt.Printf("jump  %+d samples: %d -> %s\n", d-int64(prev.Frames), prev.TimelineSample+int64(prev.Frames), pos(b, 0))
				} else if m != motion {
					fmt.Printf("position %s at %s\n", m, pos(b, 0))
				}
				motion = m
			}
			ch := ring.Channels
			for f := 0; f < int(b.Frames); f++ {
				peak := 0.0
				for c := 0; c < ch; c++ {
					if v := math.Abs(float64(b.Audio[f*ch+c])); v > peak {
						peak = v
					}
				}
				switch {
				case peak >= onset && quiet >= armAfter:
					fmt.Printf("onset peak %.3f at %s\n", peak, pos(b, int64(f)))
					quiet = 0
				case peak < onset/10:
					quiet++
				default:
					quiet = 0
				}
			}
			prev, have = b, true
		}
		if time.Since(last) >= 2*time.Second {
			if have {
				fmt.Printf("…     %d blocks in %.0fs, now %s, %s\n", blocks, time.Since(last).Seconds(), flagNames(prev.Flags), pos(prev, 0))
			} else {
				fmt.Printf("…     no blocks in %.0fs (host is not calling the plugin)\n", time.Since(last).Seconds())
			}
			blocks, last = 0, time.Now()
		}
	}
}

func flagNames(f uint32) string {
	var parts []string
	if f&link.Playing != 0 {
		parts = append(parts, "playing")
	} else {
		parts = append(parts, "stopped")
	}
	if f&link.Recording != 0 {
		parts = append(parts, "recording")
	}
	if f&link.HasTimeline == 0 {
		parts = append(parts, "no-timeline")
	}
	return strings.Join(parts, "+")
}

// clock formats a sample position as h:mm:ss.mmm from sample 0.
func clock(s int64, rate float64) string {
	sign := ""
	if s < 0 {
		sign, s = "-", -s
	}
	ms := int64(float64(s) * 1000 / rate)
	return fmt.Sprintf("%s%d:%02d:%02d.%03d", sign, ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// recordLink records a Link plugin instance. Transport events come from
// the plugin's per-block host positions, so placement is exact.
func recordLink(ctx context.Context, sel, dir, codec string, sess *take.Session) error {
	ring, err := openLink(sel)
	if err != nil {
		return err
	}
	defer ring.Close()
	if sess == nil {
		return errors.New("record -link needs session details (-ptsl, or -session-rate/-session-start)")
	}
	if float64(sess.SampleRate) != ring.SampleRate {
		return fmt.Errorf("Link runs at %.0f Hz but the session is %d Hz", ring.SampleRate, sess.SampleRate)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	r, err := recorder.Start(recorder.Config{Dir: dir, Stream: pr, StreamRate: int(ring.SampleRate),
		StreamChannels: ring.Channels, StreamName: ring.TrackName, MasterCodec: codec, Session: sess})
	pr.Close()
	if err != nil {
		return err
	}
	clog, err := os.Create(filepath.Join(dir, take.CaptureLog))
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "recording Link %q (%s) -> %s (Ctrl-C to stop)\n", ring.TrackName, ring.HostName, dir)
	pump := &link.Pump{Reader: ring.NewReader(true), PCM: pw, Log: clog, Events: r.Events}
	perr := pump.Run(ctx)
	clog.Close()
	pw.Close()
	werr := r.Wait()
	fmt.Fprintf(os.Stderr, "take %s: %d frames, %d transport events, %d frames lost and filled\n",
		dir, pump.Stats.Frames, pump.Stats.Events, pump.Stats.FilledFrames)
	if perr != nil {
		return perr
	}
	return werr
}
