// Package ptsl is a minimal client for the Pro Tools Scripting Library
// (PTSL) gRPC service.
//
// PTSL exposes one RPC, ptsl.PTSL/SendGrpcRequest, whose envelope carries a
// command id and a JSON body. Only the envelope is protobuf, so it is
// encoded here directly (field numbers taken from the PTSL.proto
// descriptor shipped in Pro Tools 2026.4's libPTSL.dylib; see
// ptsl_wire_test.go, which checks the encoding against that descriptor).
package ptsl

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protowire"
)

// DefaultAddress is where Pro Tools listens for PTSL clients.
const DefaultAddress = "localhost:31416"

const method = "/ptsl.PTSL/SendGrpcRequest"

// CommandID values from ptsl.CommandId.
type CommandID int32

const (
	CmdImport                 CommandID = 2
	CmdHostReadyCheck         CommandID = 13
	CmdGetSessionSampleRate   CommandID = 35
	CmdGetSessionName         CommandID = 42
	CmdGetSessionPath         CommandID = 43
	CmdGetSessionStartTime    CommandID = 44
	CmdGetPTSLVersion         CommandID = 55
	CmdGetTransportState      CommandID = 59
	CmdRegisterConnection     CommandID = 70
	CmdGetTimelineSelection   CommandID = 82
	CmdGetSessionTimeCodeRate CommandID = 38
	CmdGetSessionSystemDelay  CommandID = 110
)

// TaskStatus values from ptsl.TaskStatus.
const (
	statusCompleted = 3
	statusFailed    = 4
)

// Version identifies the PTSL protocol version sent in request headers.
type Version struct{ Major, Minor, Revision int32 }

// Client talks to one Pro Tools instance.
type Client struct {
	conn    *grpc.ClientConn
	mu      sync.Mutex
	session string
	version Version
}

// Dial connects to PTSL at addr. It does not contact Pro Tools until the
// first call.
func Dial(addr string) (*Client, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn}, nil
}

func (c *Client) Close() error { return c.conn.Close() }

// Error is a PTSL command failure reported by Pro Tools.
type Error struct {
	Command CommandID
	Status  int32
	Body    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("ptsl: command %d failed (status %d): %s", e.Command, e.Status, e.Body)
}

// Register performs RegisterConnection and remembers the session id that
// later requests must carry. It first asks for the PTSL version so request
// headers match what this Pro Tools build expects.
func (c *Client) Register(ctx context.Context, company, app string) error {
	var v struct {
		Version         int32 `json:"version"`
		VersionMinor    int32 `json:"version_minor"`
		VersionRevision int32 `json:"version_revision"`
	}
	if err := c.Call(ctx, CmdGetPTSLVersion, nil, &v); err != nil {
		return err
	}
	c.mu.Lock()
	c.version = Version{v.Version, v.VersionMinor, v.VersionRevision}
	c.mu.Unlock()
	var r struct {
		SessionID string `json:"session_id"`
	}
	err := c.Call(ctx, CmdRegisterConnection, map[string]string{
		"company_name": company, "application_name": app,
	}, &r)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.session = r.SessionID
	c.mu.Unlock()
	return nil
}

// Call sends one command. body is marshalled to JSON (nil sends no body);
// out, if non-nil, receives the decoded JSON response body.
func (c *Client) Call(ctx context.Context, cmd CommandID, body any, out any) error {
	var bodyJSON string
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyJSON = string(b)
	}
	c.mu.Lock()
	req := EncodeRequest(cmd, c.session, c.version, bodyJSON)
	c.mu.Unlock()
	var resp []byte
	if err := c.conn.Invoke(ctx, method, &req, &resp, grpc.ForceCodec(rawCodec{})); err != nil {
		return err
	}
	r, err := DecodeResponse(resp)
	if err != nil {
		return err
	}
	if r.Status != statusCompleted {
		msg := r.ErrorJSON
		if msg == "" {
			msg = r.BodyJSON
		}
		return &Error{Command: cmd, Status: r.Status, Body: msg}
	}
	if out != nil && strings.TrimSpace(r.BodyJSON) != "" {
		if err := json.Unmarshal([]byte(r.BodyJSON), out); err != nil {
			return fmt.Errorf("ptsl: decoding response to command %d: %w", cmd, err)
		}
	}
	return nil
}

// EncodeRequest builds a serialized ptsl.Request.
func EncodeRequest(cmd CommandID, session string, v Version, bodyJSON string) []byte {
	var h []byte
	h = protowire.AppendTag(h, 2, protowire.VarintType)
	h = protowire.AppendVarint(h, uint64(cmd))
	if v.Major != 0 {
		h = protowire.AppendTag(h, 3, protowire.VarintType)
		h = protowire.AppendVarint(h, uint64(v.Major))
	}
	if session != "" {
		h = protowire.AppendTag(h, 4, protowire.BytesType)
		h = protowire.AppendString(h, session)
	}
	if v.Minor != 0 {
		h = protowire.AppendTag(h, 5, protowire.VarintType)
		h = protowire.AppendVarint(h, uint64(v.Minor))
	}
	if v.Revision != 0 {
		h = protowire.AppendTag(h, 6, protowire.VarintType)
		h = protowire.AppendVarint(h, uint64(v.Revision))
	}
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendBytes(b, h)
	if bodyJSON != "" {
		b = protowire.AppendTag(b, 2, protowire.BytesType)
		b = protowire.AppendString(b, bodyJSON)
	}
	return b
}

// Response is a decoded ptsl.Response.
type Response struct {
	Command   CommandID
	Status    int32
	BodyJSON  string
	ErrorJSON string
}

// DecodeResponse parses a serialized ptsl.Response.
func DecodeResponse(b []byte) (Response, error) {
	var r Response
	err := walk(b, func(num protowire.Number, typ protowire.Type, v []byte, n uint64) error {
		switch {
		case num == 1 && typ == protowire.BytesType:
			return walk(v, func(num protowire.Number, typ protowire.Type, _ []byte, n uint64) error {
				switch {
				case num == 2 && typ == protowire.VarintType:
					r.Command = CommandID(n)
				case num == 3 && typ == protowire.VarintType:
					r.Status = int32(n)
				}
				return nil
			})
		case num == 2 && typ == protowire.BytesType:
			r.BodyJSON = string(v)
		case num == 3 && typ == protowire.BytesType:
			r.ErrorJSON = string(v)
		}
		return nil
	})
	return r, err
}

func walk(b []byte, fn func(protowire.Number, protowire.Type, []byte, uint64) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		var v []byte
		var x uint64
		switch typ {
		case protowire.VarintType:
			x, n = protowire.ConsumeVarint(b)
		case protowire.BytesType:
			v, n = protowire.ConsumeBytes(b)
		default:
			n = protowire.ConsumeFieldValue(num, typ, b)
		}
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		if err := fn(num, typ, v, x); err != nil {
			return err
		}
	}
	return nil
}

// rawCodec passes pre-encoded protobuf bytes through gRPC unchanged.
type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error) { return *(v.(*[]byte)), nil }
func (rawCodec) Unmarshal(data []byte, v any) error {
	*(v.(*[]byte)) = append([]byte(nil), data...)
	return nil
}
func (rawCodec) Name() string { return "proto" }
