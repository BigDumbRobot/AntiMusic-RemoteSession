package transport

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"

	"antimusic.dev/remotesession/internal/ptsl"
	"antimusic.dev/remotesession/internal/take"
)

// fakePT serves PTSL's single RPC with scripted transport state.
type fakePT struct {
	mu       sync.Mutex
	state    string
	inSample int64
	preRoll  bool
	failing  bool
}

func (f *fakePT) handle(_ any, stream grpc.ServerStream) error {
	var req []byte
	if err := stream.RecvMsg(&req); err != nil {
		return err
	}
	var cmd ptsl.CommandID
	// Request.header (field 1) -> command (field 2).
	b := req
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		b = b[n:]
		if num == 1 && typ == protowire.BytesType {
			h, m := protowire.ConsumeBytes(b)
			for len(h) > 0 {
				hn, ht, k := protowire.ConsumeTag(h)
				h = h[k:]
				if hn == 2 && ht == protowire.VarintType {
					v, k := protowire.ConsumeVarint(h)
					cmd = ptsl.CommandID(v)
					h = h[k:]
					continue
				}
				h = h[protowire.ConsumeFieldValue(hn, ht, h):]
			}
			b = b[m:]
			continue
		}
		b = b[protowire.ConsumeFieldValue(num, typ, b):]
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	status, body := uint64(3), ""
	switch {
	case f.failing:
		status, body = 4, `{"errors":[{"command_error_type":"CEType_PT_UnknownError"}]}`
	case cmd == ptsl.CmdGetTransportState:
		body = fmt.Sprintf(`{"current_setting":%q}`, f.state)
	case cmd == ptsl.CmdGetTimelineSelection:
		body = fmt.Sprintf(`{"in_time":"%d","out_time":"%d","pre_roll_start_time":"%d","pre_roll_enabled":%v}`,
			f.inSample, f.inSample, f.inSample-48000, f.preRoll)
	}
	var h []byte
	h = protowire.AppendTag(h, 2, protowire.VarintType)
	h = protowire.AppendVarint(h, uint64(cmd))
	h = protowire.AppendTag(h, 3, protowire.VarintType)
	h = protowire.AppendVarint(h, status)
	var resp []byte
	resp = protowire.AppendTag(resp, 1, protowire.BytesType)
	resp = protowire.AppendBytes(resp, h)
	field := protowire.Number(2)
	if status != 3 {
		field = 3
	}
	resp = protowire.AppendTag(resp, field, protowire.BytesType)
	resp = protowire.AppendString(resp, body)
	return stream.SendMsg(&resp)
}

func (f *fakePT) set(fn func(*fakePT)) {
	f.mu.Lock()
	fn(f)
	f.mu.Unlock()
}

type passthrough struct{}

func (passthrough) Marshal(v any) ([]byte, error) { return *(v.(*[]byte)), nil }
func (passthrough) Unmarshal(data []byte, v any) error {
	*(v.(*[]byte)) = append([]byte(nil), data...)
	return nil
}
func (passthrough) Name() string { return "proto" }

func TestPTSLWatcher(t *testing.T) {
	pt := &fakePT{state: "TS_TransportStopped"}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.ForceServerCodec(passthrough{}), grpc.UnknownServiceHandler(pt.handle))
	go srv.Serve(lis)
	defer srv.Stop()

	c, err := ptsl.Dial(lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	dir := t.TempDir()
	log, err := take.OpenEventLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var errs int
	w := &PTSLWatcher{Client: c, Log: log, Interval: 5 * time.Millisecond, SampleRate: 48000,
		SessionStart: 172_800_000, OnError: func(error) { errs++ }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	step := func(fn func(*fakePT)) { pt.set(fn); time.Sleep(60 * time.Millisecond) }
	time.Sleep(60 * time.Millisecond)
	step(func(p *fakePT) { p.state, p.inSample = "TS_TransportPlaying", 96000 })
	step(func(p *fakePT) { p.state = "TState_TransportStopped" })
	step(func(p *fakePT) { p.failing = true }) // PTSL outage
	step(func(p *fakePT) {
		p.failing = false
		p.state, p.inSample, p.preRoll = "TS_TransportRecording", 480000, true
	})
	cancel()
	<-done
	log.Close()

	evs, err := take.ReadEvents(dir)
	if err != nil {
		t.Fatal(err)
	}
	type ev struct {
		rolling bool
		tl      int64
	}
	var got []ev
	for _, e := range evs {
		got = append(got, ev{e.Rolling, e.TimelineSample})
		if e.HostMicros == 0 || e.UncertaintyFrames <= 0 || e.Source != "ptsl" {
			t.Errorf("event lacks timing or uncertainty: %+v", e)
		}
	}
	// Initial stop, play at session +96000, stop, (outage), stop again
	// re-established after the outage, record from pre-roll start.
	want := []ev{{false, 0}, {true, 172_896_000}, {false, 0}, {true, 172_800_000 + 480000 - 48000}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	if errs == 0 {
		t.Error("outage not reported")
	}
}
