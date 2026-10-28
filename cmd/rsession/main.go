// Command rsession records lossless masters and delivers them as
// timeline-placed Broadcast Wave files.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"antimusic.dev/remotesession/internal/deliver"
	"antimusic.dev/remotesession/internal/link"
	"antimusic.dev/remotesession/internal/pair"
	"antimusic.dev/remotesession/internal/ptsl"
	"antimusic.dev/remotesession/internal/recorder"
	"antimusic.dev/remotesession/internal/session"
	"antimusic.dev/remotesession/internal/take"
	"antimusic.dev/remotesession/internal/timeline"
	"antimusic.dev/remotesession/internal/transport"
	"antimusic.dev/remotesession/internal/xfer"
)

const usage = `usage: rsession <command> [flags]

commands:
  devices        list CoreAudio input devices
  record         record a lossless master, journaling DAW transport
  deliver        render placed BWF deliverables from a take and verify them
  links          list RemoteSession Link plugin instances (-watch <track> for live diagnostics)
  ptsl-status    show Pro Tools session and transport via PTSL
  ptsl-import    import a take's deliverables into Pro Tools at their positions
  send           copy a take to a receiver (resumes; record -send does this live)
  receive        host sessions (engineer): show the code, take stems/takes, stream the monitor mix, send prints
  join           join a studio's session (client): chat, listen, send stems, receive prints
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
	case "send":
		err = sendCmd(args)
	case "receive":
		err = receiveCmd(args)
	case "join":
		err = joinCmd(args)
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
	code := fs.String("code", "", "also copy the take live to the studio with this pairing code")
	relay := fs.String("relay", "", "optional relay host[:port], only for relay codes (default $RS_RELAY)")
	fs.Parse(args)
	if (*device == "") == (*linkSel == "") || *dir == "" {
		return errors.New("record: -dir and exactly one of -device or -link are required")
	}
	if _, err := os.Stat(*dir); err == nil {
		return fmt.Errorf("record: %s already exists; pick a new take directory", *dir)
	}
	if *code == "" {
		return recordTake(*dir, *device, *linkSel, *latency, *codec, *usePTSL, *addr, *poll, *rate, *start, *fps)
	}

	s, resume, err := newSender(*dir, *code, *relay)
	if err != nil {
		return err
	}
	// The upload runs beside the recording and never blocks it. It
	// starts once the recorder has written the take's manifest.
	sctx, scancel := context.WithCancel(context.Background())
	defer scancel()
	var res *xfer.Result
	var serr error
	sendDone := make(chan struct{})
	go func() {
		defer close(sendDone)
		for {
			if _, err := os.Stat(filepath.Join(*dir, take.ManifestFile)); err == nil {
				break
			}
			select {
			case <-sctx.Done():
				serr = sctx.Err()
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
		res, serr = s.Run(sctx)
	}()
	rerr := recordTake(*dir, *device, *linkSel, *latency, *codec, *usePTSL, *addr, *poll, *rate, *start, *fps)
	if _, err := os.Stat(filepath.Join(*dir, take.ManifestFile)); err != nil {
		scancel() // the recording never started
	} else {
		logStderr("send: finishing the copy (Ctrl-C to stop; resume later with: %s)", resume)
	}
	ictx, istop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	select {
	case <-sendDone:
	case <-ictx.Done():
		scancel()
		<-sendDone
	}
	istop()
	if err := reportSend(res, serr); err != nil {
		if rerr == nil {
			rerr = fmt.Errorf("take recorded locally in %s, but sending stopped: %w", *dir, err)
		} else {
			logStderr("send: %v", err)
		}
	}
	return rerr
}

func recordTake(dir, device, linkSel string, latency int64, codec string, usePTSL bool, addr string,
	poll time.Duration, rate int, start, fps string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var sess *take.Session
	var client *ptsl.Client
	if usePTSL {
		c, s, err := connectPTSL(ctx, addr)
		if err != nil {
			return err
		}
		defer c.Close()
		client = c
		sess = &take.Session{Source: "ptsl", Name: s.Name, SampleRate: s.SampleRate,
			StartTimecode: s.StartTimecode, TimecodeRate: s.TimecodeRate, StartSample: s.StartSample}
	} else if rate > 0 {
		fr, ok := frameRates[fps]
		if !ok {
			return fmt.Errorf("record: unknown -fps %q", fps)
		}
		n, err := timeline.TimecodeToSamples(start, fr, rate)
		if err != nil {
			return err
		}
		sess = &take.Session{Source: "manual", SampleRate: rate, StartTimecode: start, StartSample: n}
	}

	if linkSel != "" {
		return recordLink(ctx, linkSel, dir, codec, sess)
	}
	r, err := recorder.Start(recorder.Config{
		Dir: dir, Device: device,
		MasterCodec: codec, LatencyFrames: latency, Session: sess,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "recording %s -> %s (Ctrl-C to stop)\n", device, filepath.Join(dir, take.MasterFile))

	if client != nil {
		w := &transport.PTSLWatcher{
			Client: client, Log: r.Events, Interval: poll, SampleRate: sess.SampleRate, SessionStart: sess.StartSample,
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
	m, _ := take.ReadManifest(dir)
	if m != nil {
		fmt.Fprintf(os.Stderr, "take %s: state %s, %d Hz, %d ch\n", dir, m.State, m.SampleRate, m.Channels)
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
		if strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "Unavailable") {
			return nil, ptsl.SessionInfo{}, errors.New("Pro Tools isn't open, so the take was saved but not imported")
		}
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
	dest := fs.String("dest", "", `folder to copy audio into (default: the session's "Audio Files", asked from Pro Tools)`)
	fs.Parse(args)
	if *dir == "" {
		return errors.New("ptsl-import: -dir is required")
	}
	lines, err := importTake(context.Background(), *dir, *addr, *dest)
	for _, l := range lines {
		fmt.Println(l)
	}
	return err
}

// importTake imports a delivered take into the open Pro Tools session.
// Clips are placed by their absolute timeline position against this
// session's start, so the recording side's session may start elsewhere.
func importTake(ctx context.Context, dir, addr, dest string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(dir, take.DeliverDir, deliver.ReportFile))
	if err != nil {
		return nil, err
	}
	var rep deliver.Report
	if err := json.Unmarshal(b, &rep); err != nil {
		return nil, err
	}
	c, s, err := connectPTSL(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if rep.Session != nil && rep.Session.SampleRate != s.SampleRate {
		return nil, fmt.Errorf("take is %d Hz but the open session is %d Hz", rep.Session.SampleRate, s.SampleRate)
	}
	audioDir := dest
	if audioDir == "" {
		var sessPath struct {
			SessionPath struct {
				Path string `json:"path"`
			} `json:"session_path"`
		}
		// Pro Tools Developer 25.6 fails this call with an internal
		// assertion; -dest avoids it.
		if err := c.Call(ctx, ptsl.CmdGetSessionPath, nil, &sessPath); err == nil && sessPath.SessionPath.Path != "" {
			audioDir = filepath.Join(filepath.Dir(sessPath.SessionPath.Path), "Audio Files")
		} else {
			// Some Pro Tools builds fail this call; keep the audio beside the take.
			abs, aerr := filepath.Abs(filepath.Join(dir, "imported"))
			if aerr != nil {
				return nil, aerr
			}
			audioDir = abs
			if err := os.MkdirAll(audioDir, 0o755); err != nil {
				return nil, err
			}
		}
	}
	var lines []string
	for _, d := range rep.Deliverables {
		abs, err := filepath.Abs(filepath.Join(dir, d.File))
		if err != nil {
			return lines, err
		}
		// Takes land at the same distance from the session start as they
		// were recorded, whatever DAW or start time the sender used. (The
		// BWF timestamp keeps the sender's absolute position.)
		pos := d.SessionOffset
		if pos < 0 {
			return lines, fmt.Errorf("%s starts before the session start", d.File)
		}
		if err := c.ImportSpotted(ctx, []string{abs}, audioDir, pos); err != nil {
			return lines, fmt.Errorf("importing %s: %w", d.File, err)
		}
		lines = append(lines, fmt.Sprintf("imported %s at session +%d samples (timeline %d)", d.File, pos, d.TimeReference))
	}
	return lines, nil
}

// defaultRelay is the relay built into this binary
// (make RELAY=host:port sets it via -ldflags).
var defaultRelay = ""

// relayAddr picks the optional relay: flag, then $RS_RELAY, then the
// built-in one. "" means none.
func relayAddr(flagVal string) (string, error) {
	r := flagVal
	if r == "" {
		r = os.Getenv("RS_RELAY")
	}
	if r == "" {
		r = defaultRelay
	}
	if r == "" {
		return "", nil // no relay: direct, peer-to-peer
	}
	if _, _, err := net.SplitHostPort(r); err != nil {
		r = net.JoinHostPort(r, strconv.Itoa(pair.DefaultRelayPort))
	}
	return r, nil
}

// newSender builds a sender for the studio's pairing code. It also returns
// the command that resumes the upload.
func newSender(dir, code, relay string) (*xfer.Sender, string, error) {
	if code == "" {
		return nil, "", errors.New("give the studio's pairing code with -code")
	}
	c, err := pair.ParseCode(code)
	if err != nil {
		return nil, "", err
	}
	r, err := relayAddr(relay)
	if err != nil {
		return nil, "", err
	}
	label := "studio " + c.ID
	if c.IsDirect() {
		label = "studio at " + c.Addr.String()
	}
	s := &xfer.Sender{Dir: dir, Addr: label, Logf: logStderr,
		Dial: func(ctx context.Context) (net.Conn, error) { return pair.Connect(ctx, r, c) }}
	return s, fmt.Sprintf("rsession send -dir %s -code %s", dir, c), nil
}

// pairingCodeFile holds this studio's code, so it stays the same.
func pairingCodeFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Application Support", "AntiMusic", "RemoteSession", "pairing-code")
}

func studioCode(explicit string, fresh bool) (pair.Code, error) {
	if explicit != "" {
		return pair.ParseCode(explicit)
	}
	path := pairingCodeFile()
	if !fresh {
		if b, err := os.ReadFile(path); err == nil {
			return pair.ParseCode(string(b))
		}
	}
	c := pair.NewCode()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return c, err
	}
	return c, os.WriteFile(path, []byte(c.String()+"\n"), 0o600)
}

func logStderr(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, a...))
}

func sendCmd(args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	dir := fs.String("dir", "", "take directory")
	code := fs.String("code", "", "the studio's pairing code")
	relay := fs.String("relay", "", "optional relay host[:port], only for relay codes (default $RS_RELAY)")
	fs.Parse(args)
	if *dir == "" {
		return errors.New("send: -dir is required")
	}
	s, _, err := newSender(*dir, *code, *relay)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return reportSend(s.Run(ctx))
}

func reportSend(res *xfer.Result, err error) error {
	if errors.Is(err, context.Canceled) {
		return errors.New("sending stopped before the studio had the whole take; run the same send command again to finish")
	}
	if err != nil {
		return err
	}
	logStderr("send: receiver has a verified copy in %q", res.Dir)
	if res.Summary != "" {
		fmt.Fprintln(os.Stderr, res.Summary)
	}
	if res.Error != "" {
		return fmt.Errorf("receiver could not finish processing the take: %s", res.Error)
	}
	return nil
}

func receiveCmd(args []string) error {
	fs := flag.NewFlagSet("receive", flag.ExitOnError)
	port := fs.Int("port", xfer.DefaultPort, "TCP port to accept musicians on (also requested on the router)")
	public := fs.String("public", "", "public ip[:port] when you forwarded the port on the router yourself (skips automatic setup)")
	relay := fs.String("relay", "", "optional relay host[:port] instead of a direct connection (default $RS_RELAY)")
	codeFlag := fs.String("code", "", "use this pairing code's secret instead of the studio's saved one")
	newCode := fs.Bool("new-code", false, "replace the studio's saved secret (old codes stop working)")
	dir := fs.String("dir", "", "folder to receive takes into")
	doDeliver := fs.Bool("deliver", false, "render placed BWF files when a take arrives")
	doImport := fs.Bool("import", false, "also import them into the open Pro Tools session (implies -deliver)")
	addr := fs.String("ptsl-addr", ptsl.DefaultAddress, "PTSL address (with -import)")
	dest := fs.String("dest", "", `with -import: folder to copy audio into (default: the session's "Audio Files")`)
	events := fs.Bool("events", false, "status as JSON lines on stdout, commands as JSON lines on stdin (for the app)")
	listenDev := fs.String("listen-device", "", `monitor mix source clients can listen to (e.g. "Pro Tools Audio Bridge 2-B")`)
	listenCh := fs.String("listen-ch", "1,2", "device channels for left,right of the listen stream")
	name := fs.String("name", "", "studio name shown to clients")
	printsDir := fs.String("prints-dir", "", "where outgoing prints are bundled (default: beside -dir)")
	fs.Parse(args)
	if *dir == "" {
		return errors.New("receive: -dir is required")
	}
	var emu sync.Mutex
	enc := json.NewEncoder(os.Stdout)
	emit := func(v map[string]any) {
		if *events {
			emu.Lock()
			enc.Encode(v)
			emu.Unlock()
		}
	}
	logf := func(f string, a ...any) {
		logStderr("receive: "+f, a...)
		emit(map[string]any{"type": "log", "text": fmt.Sprintf(f, a...)})
	}
	rv := &xfer.Receiver{Root: *dir, Logf: func(f string, a ...any) { logStderr(f, a...) }}
	rv.OnStart = func(d string) {
		emit(map[string]any{"type": "take", "take": filepath.Base(d), "state": "receiving", "dir": d})
	}
	process := func(ctx context.Context, dir string) (string, error) { return "", nil }
	if *doDeliver || *doImport {
		process = func(ctx context.Context, dir string) (string, error) {
			if m, err := take.ReadManifest(dir); err == nil && m.Kind != "" { // stems: already finished files
				if !*doImport {
					return "", nil
				}
				imp, err := importTake(ctx, dir, *addr, *dest)
				return strings.Join(imp, "\n"), err
			}
			rep, err := deliver.Render(ctx, dir, deliver.Options{Codec: "pcm_s24le", Renderer: siblingTool("rs-render")})
			if err != nil {
				return "", fmt.Errorf("deliver: %w", err)
			}
			var lines []string
			for _, w := range rep.Warnings {
				lines = append(lines, "warning: "+w)
			}
			for _, d := range rep.Deliverables {
				lines = append(lines, fmt.Sprintf("delivered %s at timeline %d (%d frames)", d.File, d.TimeReference, d.Segment.Frames))
			}
			if *doImport {
				imp, err := importTake(ctx, dir, *addr, *dest)
				lines = append(lines, imp...)
				if err != nil {
					return strings.Join(lines, "\n"), err
				}
			}
			s := strings.Join(lines, "\n")
			logStderr("receive: %s\n%s", filepath.Base(dir), s)
			return s, nil
		}
	}
	rv.OnComplete = func(ctx context.Context, d string) (string, error) {
		sum, err := process(ctx, d)
		ev := map[string]any{"type": "take", "take": filepath.Base(d), "state": "done", "dir": d, "summary": sum, "imported": *doImport && err == nil}
		if err != nil {
			ev["state"], ev["error"] = "saved", err.Error() // the copy is complete; only processing failed
		}
		emit(ev)
		return sum, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code, err := studioCode(*codeFlag, *newCode)
	if err != nil {
		return err
	}
	r, err := relayAddr(*relay)
	if err != nil {
		return err
	}
	var ln net.Listener
	if r != "" {
		l, err := pair.Listen(ctx, r, code, logf)
		if err != nil {
			return err
		}
		defer l.Close()
		ln = l
		fmt.Fprintf(os.Stderr, "\n    Pairing code:  %s   (through relay %s)\n\n    Give this to the musician. Takes go to %s\n\n", code, r, *dir)
		emit(map[string]any{"type": "code", "code": code.String(), "reach": "relay", "detail": "through relay " + r})
		return serveHost(ctx, ln, rv, emit, *events, *listenDev, *listenCh, *name, *printsDir, *dir, *addr)
	}

	// Direct: the musician connects straight to this machine.
	tl, err := net.Listen("tcp4", fmt.Sprintf(":%d", *port))
	if err != nil {
		return fmt.Errorf("cannot listen on port %d (another receive running?): %w", *port, err)
	}
	var pub netip.AddrPort
	lanOnly := false
	mapped := make(chan struct{})
	close(mapped)
	if *public != "" {
		if pub, err = netip.ParseAddrPort(*public); err != nil {
			a, perr := netip.ParseAddr(*public)
			if perr != nil || !a.Is4() {
				return fmt.Errorf("-public: want an IPv4 address like 70.24.77.77 or 70.24.77.77:%d", *port)
			}
			pub = netip.AddrPortFrom(a, uint16(*port))
		}
	} else {
		mctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		m, err := pair.MapPort(mctx, *port, *port)
		cancel()
		if err == nil {
			pub = m.External
			logf("opened port %d on the router (%s), reachable at %s", m.External.Port(), m.Method, m.External)
			mapped = make(chan struct{})
			go func() { defer close(mapped); m.Keep(ctx, logf) }() // removed again on exit
		} else {
			local, lerr := pair.LocalIPv4()
			if lerr != nil {
				return fmt.Errorf("%v; and no local network address: %v", err, lerr)
			}
			pub, lanOnly = netip.AddrPortFrom(local, uint16(*port)), true
			logf("%v", err)
		}
	}
	defer func() { stop(); <-mapped }()
	l := pair.ListenDirect(ctx, tl, code, logf)
	defer l.Close()
	ln = l
	fmt.Fprintf(os.Stderr, "\n    Pairing code:  %s\n\n    Give this to the musician. Takes go to %s\n", code.Direct(pub), *dir)
	if lanOnly {
		fmt.Fprintf(os.Stderr, "\n    This code only works on this studio's own network: the router would not open a port.\n"+
			"    To work from anywhere, turn on UPnP in the router, or forward TCP port %d to %s\n"+
			"    and run again with -public <your public IP>.\n", *port, pub.Addr())
	}
	fmt.Fprintln(os.Stderr)
	ev := map[string]any{"type": "code", "code": code.Direct(pub).String(), "reach": "internet", "detail": "reachable from anywhere at " + pub.String()}
	if lanOnly {
		ev["reach"] = "lan"
		ev["detail"] = fmt.Sprintf("Only works on this studio's network: the router would not open a port. Turn on UPnP in the router, or forward TCP port %d to %s.", *port, pub.Addr())
	} else if *public != "" {
		ev["detail"] = "using your port forward at " + pub.String()
	}
	emit(ev)
	return serveHost(ctx, ln, rv, emit, *events, *listenDev, *listenCh, *name, *printsDir, *dir, *addr)
}

// serveHost runs the engineer's side of sessions on ln. In events mode it
// also reads commands from stdin: {"cmd":"chat","text":..} and
// {"cmd":"send_prints","files":[..]}.
func serveHost(ctx context.Context, ln net.Listener, rv *xfer.Receiver, emit func(map[string]any), events bool,
	listenDev, listenCh, name, printsDir, dir, ptslAddr string) error {
	host := &session.Host{Takes: rv, Name: name, Events: func(e session.Event) { emit(map[string]any(e)) }}
	if !events {
		host.Events = func(e session.Event) {
			switch e["type"] {
			case "chat":
				fmt.Fprintf(os.Stderr, "[%s] %s\n", e["from"], e["text"])
			case "client":
				logStderr("receive: client %v connected: %v", e["name"], e["connected"])
			}
		}
	}
	if listenDev != "" {
		var l, r int
		if _, err := fmt.Sscanf(listenCh, "%d,%d", &l, &r); err != nil {
			return fmt.Errorf("-listen-ch: want two channels like 1,2")
		}
		host.Monitor = &session.Monitor{Device: listenDev, Channels: [2]int{l, r}, Capture: recorder.FindCaptureHelper()}
	}
	if printsDir == "" {
		printsDir = filepath.Join(filepath.Dir(filepath.Clean(dir)), "Prints Sent")
	}
	go readCommands(ctx, func(c command) {
		switch c.Cmd {
		case "chat":
			host.Chat(c.Text)
		case "send_prints":
			go func() {
				start := int64(-1) // place prints by their Pro Tools timestamps
				if pc, s, err := connectPTSL(ctx, ptslAddr); err == nil {
					start = s.StartSample
					pc.Close()
				}
				d := filepath.Join(printsDir, "prints_"+time.Now().Format("2006-01-02_15-04-05"))
				emit(map[string]any{"type": "prints", "state": "sending", "dir": d})
				if _, err := session.Bundle(d, take.KindPrints, c.Files, start); err != nil {
					emit(map[string]any{"type": "prints", "state": "failed", "error": err.Error()})
					return
				}
				res, err := host.SendPrints(ctx, d)
				if err != nil {
					emit(map[string]any{"type": "prints", "state": "failed", "error": err.Error()})
					return
				}
				emit(map[string]any{"type": "prints", "state": "delivered", "summary": res.Summary, "error": res.Error})
			}()
		}
	}, !events, func(line string) { host.Chat(line) })
	return host.Serve(ctx, ln)
}

type command struct {
	Cmd   string   `json:"cmd"`
	Text  string   `json:"text,omitempty"`
	Files []string `json:"files,omitempty"`
	On    bool     `json:"on,omitempty"`
}

// readCommands reads JSON commands from stdin, or plain chat lines when
// plain is set (typing in a terminal).
func readCommands(ctx context.Context, do func(command), plain bool, chat func(string)) {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() && ctx.Err() == nil {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var c command
		if json.Unmarshal([]byte(line), &c) == nil && c.Cmd != "" {
			do(c)
		} else if plain {
			chat(line)
		}
	}
}

// joinCmd is the client's side of a session.
func joinCmd(args []string) error {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	code := fs.String("code", "", "the studio's pairing code")
	relay := fs.String("relay", "", "optional relay host[:port], only for relay codes (default $RS_RELAY)")
	dir := fs.String("dir", "", "folder for prints from the engineer")
	name := fs.String("name", "", "your name, shown to the engineer")
	doImport := fs.Bool("import", false, "import prints into the open Pro Tools session")
	addr := fs.String("ptsl-addr", ptsl.DefaultAddress, "PTSL address (with -import)")
	listen := fs.Bool("listen", false, "start listening to the engineer's monitor mix right away")
	events := fs.Bool("events", false, "status as JSON lines on stdout, commands as JSON lines on stdin (for the app)")
	fs.Parse(args)
	if *code == "" || *dir == "" {
		return errors.New("join: -code and -dir are required")
	}
	c, err := pair.ParseCode(*code)
	if err != nil {
		return err
	}
	r, err := relayAddr(*relay)
	if err != nil {
		return err
	}
	var emu sync.Mutex
	enc := json.NewEncoder(os.Stdout)
	emit := func(e session.Event) {
		if *events {
			emu.Lock()
			enc.Encode(e)
			emu.Unlock()
			return
		}
		switch e["type"] {
		case "chat":
			fmt.Fprintf(os.Stderr, "[%s] %s\n", e["from"], e["text"])
		default:
			b, _ := json.Marshal(e)
			logStderr("join: %s", b)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dial := func(ctx context.Context) (net.Conn, error) { return pair.Connect(ctx, r, c) }
	prints := &xfer.Receiver{Root: *dir, Logf: func(f string, a ...any) { logStderr(f, a...) }}
	prints.OnStart = func(d string) { emit(session.Event{"type": "prints", "state": "receiving", "take": filepath.Base(d)}) }
	prints.OnComplete = func(ctx context.Context, d string) (string, error) {
		ev := session.Event{"type": "prints", "state": "received", "take": filepath.Base(d), "dir": d}
		var err error
		var lines []string
		if *doImport {
			lines, err = importTake(ctx, d, *addr, "")
			ev["imported"] = err == nil
			if err != nil {
				ev["error"] = err.Error()
			}
		}
		emit(ev)
		return strings.Join(lines, "\n"), err
	}
	g := &session.Guest{Dial: dial, Name: *name, Prints: prints, Events: emit}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}

	var lmu sync.Mutex
	var lcancel context.CancelFunc
	setListen := func(on bool) {
		lmu.Lock()
		defer lmu.Unlock()
		if lcancel != nil {
			lcancel()
			lcancel = nil
		}
		if on {
			var lctx context.Context
			lctx, lcancel = context.WithCancel(ctx)
			go func() {
				err := g.Listen(lctx, playOut)
				if err != nil && lctx.Err() == nil {
					emit(session.Event{"type": "listen", "state": "stopped", "error": err.Error()})
				}
			}()
		} else {
			emit(session.Event{"type": "listen", "state": "stopped"})
		}
	}
	if *listen {
		setListen(true)
	}
	go readCommands(ctx, func(cm command) {
		switch cm.Cmd {
		case "chat":
			if err := g.Chat(cm.Text); err != nil {
				emit(session.Event{"type": "error", "text": err.Error()})
			}
		case "listen":
			setListen(cm.On)
		case "send_stems":
			go func() {
				start := int64(-1)
				if pc, s, err := connectPTSL(ctx, *addr); err == nil {
					start = s.StartSample
					pc.Close()
				}
				d := filepath.Join(filepath.Dir(filepath.Clean(*dir)), "Stems Sent", "stems_"+time.Now().Format("2006-01-02_15-04-05"))
				emit(session.Event{"type": "stems", "state": "sending", "dir": d})
				if _, err := session.Bundle(d, take.KindStems, cm.Files, start); err != nil {
					emit(session.Event{"type": "stems", "state": "failed", "error": err.Error()})
					return
				}
				res, err := (&xfer.Sender{Dir: d, Addr: "studio", Dial: dial, Logf: func(f string, a ...any) { logStderr(f, a...) }}).Run(ctx)
				if err != nil {
					emit(session.Event{"type": "stems", "state": "failed", "error": err.Error()})
					return
				}
				emit(session.Event{"type": "stems", "state": "delivered", "summary": res.Summary, "error": res.Error})
			}()
		}
	}, !*events, func(line string) { g.Chat(line) })
	return g.Run(ctx)
}

// playOut plays a 24-bit stream on the default output device.
func playOut(rate, channels int) (io.WriteCloser, error) {
	cmd := exec.Command("ffmpeg", "-hide_banner", "-nostdin", "-loglevel", "error",
		"-f", "s24le", "-ar", strconv.Itoa(rate), "-ac", strconv.Itoa(channels), "-i", "pipe:0",
		"-c:a", "pcm_f32le", "-f", "audiotoolbox", "-")
	w, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting playback (is FFmpeg installed?): %w", err)
	}
	return &player{w: w, cmd: cmd}, nil
}

type player struct {
	w   io.WriteCloser
	cmd *exec.Cmd
}

func (p *player) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *player) Close() error {
	p.w.Close()
	done := make(chan struct{})
	go func() { p.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		p.cmd.Process.Kill()
		<-done
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
		// Any DAW: the plugin reports positions from the session start,
		// so no DAW connection is needed.
		sess = &take.Session{Source: "link", Name: ring.HostName, SampleRate: int(ring.SampleRate)}
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
	pump := &link.Pump{Reader: ring.NewReader(true), PCM: pw, Log: clog, Events: r.Events, SessionStart: sess.StartSample}
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
