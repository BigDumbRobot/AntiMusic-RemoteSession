package ptsl

import (
	"bytes"
	"os"
	"strconv"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

const libPTSL = "/Applications/Pro Tools.app/Contents/Frameworks/libPTSL.dylib"

// installedSchema loads the PTSL.proto descriptor embedded in the
// installed Pro Tools, so the envelope encoding is checked against Avid's
// schema rather than against this package's own assumptions.
func installedSchema(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()
	lib, err := os.ReadFile(libPTSL)
	if err != nil {
		t.Skipf("Pro Tools not installed: %v", err)
	}
	start := bytes.Index(lib, []byte("\x0a\x0aPTSL.proto\x12\x04ptsl"))
	if start < 0 {
		t.Fatal("PTSL.proto descriptor not found in libPTSL.dylib")
	}
	// The serialized FileDescriptorProto is not length-prefixed; consume
	// length-delimited top-level fields until the bytes stop parsing as one.
	b, end := lib[start:], 0
	for {
		num, typ, n := protowire.ConsumeTag(b[end:])
		if n < 0 || typ != protowire.BytesType || num < 1 || num > 14 {
			break
		}
		_, m := protowire.ConsumeBytes(b[end+n:])
		if m < 0 {
			break
		}
		end += n + m
	}
	var fdp descriptorpb.FileDescriptorProto
	if err := proto.Unmarshal(b[:end], &fdp); err != nil {
		t.Fatal(err)
	}
	fd, err := protodesc.NewFile(&fdp, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fd
}

func TestEnvelopeMatchesInstalledSchema(t *testing.T) {
	fd := installedSchema(t)
	reqDesc := fd.Messages().ByName("Request")
	req := dynamicpb.NewMessage(reqDesc)
	wire := EncodeRequest(CmdGetTimelineSelection, "sess-1", Version{2026, 4, 1}, `{"time_scale":"Samples"}`)
	if err := proto.Unmarshal(wire, req); err != nil {
		t.Fatal(err)
	}
	if len(req.GetUnknown()) != 0 {
		t.Fatalf("unknown fields in request: %x", req.GetUnknown())
	}
	hdr := req.Get(reqDesc.Fields().ByName("header")).Message()
	hd := hdr.Descriptor().Fields()
	cmd := hd.ByName("command")
	if got := hdr.Get(cmd).Enum(); got != cmd.Enum().Values().ByName("CId_GetTimelineSelection").Number() {
		t.Errorf("command = %d", got)
	}
	if got := hdr.Get(hd.ByName("session_id")).String(); got != "sess-1" {
		t.Errorf("session_id = %q", got)
	}
	if hdr.Get(hd.ByName("version")).Int() != 2026 || hdr.Get(hd.ByName("version_minor")).Int() != 4 ||
		hdr.Get(hd.ByName("version_revision")).Int() != 1 {
		t.Errorf("version fields wrong: %v", hdr)
	}
	if got := req.Get(reqDesc.Fields().ByName("request_body_json")).String(); got != `{"time_scale":"Samples"}` {
		t.Errorf("body = %q", got)
	}

	// Every command id used by this package must name the intended command.
	want := map[CommandID]string{
		CmdImport: "CId_Import", CmdCreateSession: "CId_CreateSession", CmdGetTrackList: "CId_GetTrackList", CmdGetClipList: "CId_GetClipList",
		CmdSelectAllClipsOnTrack: "CId_SelectAllClipsOnTrack", CmdCloseSession: "CId_CloseSession",
		CmdSetSessionStartTime: "CId_SetSessionStartTime", CmdSetTimelineSelection: "CId_SetTimelineSelection", CmdHostReadyCheck: "CId_HostReadyCheck",
		CmdGetSessionSampleRate: "CId_GetSessionSampleRate", CmdGetSessionName: "CId_GetSessionName",
		CmdGetSessionPath: "CId_GetSessionPath", CmdGetSessionStartTime: "CId_GetSessionStartTime",
		CmdGetPTSLVersion: "CId_GetPTSLVersion", CmdGetTransportState: "CId_GetTransportState",
		CmdRegisterConnection: "CId_RegisterConnection", CmdGetTimelineSelection: "CId_GetTimelineSelection",
		CmdGetSessionTimeCodeRate: "CId_GetSessionTimeCodeRate", CmdGetSessionSystemDelay: "CId_GetSessionSystemDelayInfo",
	}
	ids := cmd.Enum().Values()
	for id, name := range want {
		v := ids.ByName(protoreflect.Name(name))
		if v == nil || v.Number() != protoreflect.EnumNumber(id) {
			t.Errorf("command %s: want number %d in schema", name, id)
		}
	}
}

func TestDecodeResponseFromInstalledSchema(t *testing.T) {
	fd := installedSchema(t)
	respDesc := fd.Messages().ByName("Response")
	resp := dynamicpb.NewMessage(respDesc)
	hdrDesc := respDesc.Fields().ByName("header").Message()
	hdr := dynamicpb.NewMessage(hdrDesc)
	hdr.Set(hdrDesc.Fields().ByName("command"), protoreflect.ValueOfEnum(protoreflect.EnumNumber(CmdGetTransportState)))
	hdr.Set(hdrDesc.Fields().ByName("status"), protoreflect.ValueOfEnum(statusCompleted))
	hdr.Set(hdrDesc.Fields().ByName("task_id"), protoreflect.ValueOfString("t1"))
	resp.Set(respDesc.Fields().ByName("header"), protoreflect.ValueOfMessage(hdr))
	resp.Set(respDesc.Fields().ByName("response_body_json"), protoreflect.ValueOfString(`{"current_setting":"TState_TransportPlaying"}`))
	wire, err := proto.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	r, err := DecodeResponse(wire)
	if err != nil {
		t.Fatal(err)
	}
	if r.Command != CmdGetTransportState || r.Status != statusCompleted || r.BodyJSON != `{"current_setting":"TState_TransportPlaying"}` {
		t.Fatalf("decoded %+v", r)
	}
}

// The enum value names this package sends or matches must exist.
func TestEnumNamesExistInSchema(t *testing.T) {
	fd := installedSchema(t)
	check := func(enum string, names ...string) {
		e := fd.Enums().ByName(protoreflect.Name(enum))
		if e == nil {
			t.Fatalf("enum %s missing", enum)
		}
		for _, n := range names {
			if e.Values().ByName(protoreflect.Name(n)) == nil {
				t.Errorf("%s.%s missing", enum, n)
			}
		}
	}
	// Every alias of the values matched on must normalize the same way.
	ts := fd.Enums().ByName("TransportState").Values()
	for i := 0; i < ts.Len(); i++ {
		v := ts.Get(i)
		want := v.Number() == 1 || v.Number() == 3 // Playing, Recording
		if Rolling(string(v.Name())) != want {
			t.Errorf("Rolling(%s) = %v", v.Name(), !want)
		}
	}
	tc := fd.Enums().ByName("SessionTimeCodeRate").Values()
	for suffix := range timecodeRates {
		found := false
		for i := 0; i < tc.Len(); i++ {
			found = found || enumSuffix(string(tc.Get(i).Name())) == suffix
		}
		if !found {
			t.Errorf("no SessionTimeCodeRate value with suffix %s", suffix)
		}
	}
	sr := fd.Enums().ByName("SampleRate").Values()
	for i := 0; i < sr.Len(); i++ {
		v := sr.Get(i)
		if n, err := strconv.Atoi(enumSuffix(string(v.Name()))); v.Number() > 1 && (err != nil || n != int(v.Number())) {
			t.Errorf("SampleRate %s does not parse to %d", v.Name(), v.Number())
		}
	}
	check("TrackOffsetOptions", "Samples")
	check("ImportType", "Audio")
	check("AudioOperations", "CopyAudio")
	check("MediaDestination", "MD_NewTrack")
	check("MediaLocation", "ML_Spot")
	check("SpotLocationType", "Start")
}
