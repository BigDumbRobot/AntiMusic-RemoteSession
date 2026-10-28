// Package xfer copies a take directory from the recording machine to a
// receiver over TCP while it is being recorded.
//
// The local take stays the master copy: the network is never in the
// recording path, and a dropped connection resumes where it left off.
// During recording the sender ships what each file has grown by. When the
// take is finished, both sides compare per-block hashes and the sender
// resends whatever differs (FFmpeg rewrites the CAF header on close), until
// the receiver's copy is identical.
//
// One TCP connection per take. Frames are [type u8][length u32 BE][payload].
// Run it over a private network (e.g. Tailscale); the shared token only
// keeps strangers' takes out, it does not encrypt.
package xfer

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"antimusic.dev/remotesession/internal/take"
)

// Version is the protocol version; both sides must match.
const Version = 1

// DefaultPort is the receiver's default TCP port.
const DefaultPort = 7400

const (
	msgHello  byte = 1 // S→R json hello
	msgHave   byte = 2 // R→S json have
	msgData   byte = 3 // S→R binary: name, offset, bytes
	msgPut    byte = 4 // S→R binary: name, whole file (replaces it)
	msgFinish byte = 5 // S→R json finish
	msgNeed   byte = 6 // R→S json need
	msgDone   byte = 7 // R→S json done
	msgError  byte = 8 // R→S json errorMsg

	maxFrame  = 16 << 20
	chunkSize = 1 << 20 // bytes per data frame while recording
	blockSize = 1 << 20 // bytes per verification hash
)

// Files shipped as they grow while a take records. Every other file in
// the directory (prints, delivery reports) is sent by the final
// comparison. The manifest is always replaced whole.
var appendFiles = []string{take.MasterFile, take.CaptureLog, take.TransportFile}

// skipDirs are produced on the receiving side and never sent.
var skipDirs = map[string]bool{"imported": true}

// validName accepts slash-separated relative paths inside the take
// directory: no "..", no absolute paths, no hidden files.
func validName(name string) bool {
	if name == "" || len(name) > 512 || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return false
	}
	if path.Clean(name) != name {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == ".." || strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}

type hello struct {
	Version int    `json:"version"`
	Token   string `json:"token,omitempty"`
	Take    string `json:"take"`    // sender's take directory name
	Created string `json:"created"` // manifest creation time: identifies the take
}

type have struct {
	Dir   string           `json:"dir"`   // receiver's directory name for the take
	Sizes map[string]int64 `json:"sizes"` // bytes the receiver already holds
}

type fileSum struct {
	Size   int64    `json:"size"`
	Blocks []string `json:"blocks"`
}

type finish struct {
	Files map[string]fileSum `json:"files"`
}

type need struct {
	Blocks map[string][]int `json:"blocks"` // empty: identical
}

type done struct {
	Dir     string `json:"dir"`
	Summary string `json:"summary,omitempty"`
	Error   string `json:"error,omitempty"` // post-processing failed; the copy is still complete
}

type errorMsg struct {
	Error string `json:"error"`
	Fatal bool   `json:"fatal"` // retrying will not help
}

func writeFrame(w io.Writer, t byte, p []byte) error {
	var h [5]byte
	h[0] = t
	binary.BigEndian.PutUint32(h[1:], uint32(len(p)))
	if _, err := w.Write(h[:]); err != nil {
		return err
	}
	_, err := w.Write(p)
	return err
}

func writeJSON(w io.Writer, t byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeFrame(w, t, b)
}

func readFrame(r io.Reader) (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("xfer: frame of %d bytes is too large", n)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return 0, nil, err
	}
	return h[0], p, nil
}

// readJSON reads one frame of type want. An error frame from the peer is
// returned as a *PeerError.
func readJSON(r io.Reader, want byte, v any) error {
	t, p, err := readFrame(r)
	if err != nil {
		return err
	}
	if t == msgError {
		var e errorMsg
		if err := json.Unmarshal(p, &e); err != nil {
			return err
		}
		return &PeerError{Msg: e.Error, Fatal: e.Fatal}
	}
	if t != want {
		return fmt.Errorf("xfer: got message %d, want %d", t, want)
	}
	return json.Unmarshal(p, v)
}

// PeerError is an error reported by the other side.
type PeerError struct {
	Msg   string
	Fatal bool
}

func (e *PeerError) Error() string { return "receiver: " + e.Msg }

// IsFatal reports whether err means retrying cannot succeed.
func IsFatal(err error) bool {
	var pe *PeerError
	if errors.As(err, &pe) {
		return pe.Fatal
	}
	var f interface{ Fatal() bool }
	return errors.As(err, &f) && f.Fatal()
}

// data payload: [name len u16][name][offset u64][bytes]
func encodeData(name string, off int64, b []byte) []byte {
	p := make([]byte, 0, 2+len(name)+8+len(b))
	p = binary.BigEndian.AppendUint16(p, uint16(len(name)))
	p = append(p, name...)
	p = binary.BigEndian.AppendUint64(p, uint64(off))
	return append(p, b...)
}

func decodeData(p []byte) (name string, off int64, b []byte, err error) {
	if len(p) < 2 {
		return "", 0, nil, errors.New("xfer: short data frame")
	}
	n := int(binary.BigEndian.Uint16(p))
	if len(p) < 2+n+8 {
		return "", 0, nil, errors.New("xfer: short data frame")
	}
	name = string(p[2 : 2+n])
	off = int64(binary.BigEndian.Uint64(p[2+n:]))
	return name, off, p[2+n+8:], nil
}

// sumFile hashes path in blockSize blocks. A missing file sums as empty.
func sumFile(path string) (fileSum, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return fileSum{}, nil
	}
	if err != nil {
		return fileSum{}, err
	}
	defer f.Close()
	var s fileSum
	buf := make([]byte, blockSize)
	for {
		n, err := io.ReadFull(f, buf)
		if n > 0 {
			h := sha256.Sum256(buf[:n])
			s.Blocks = append(s.Blocks, hex.EncodeToString(h[:16]))
			s.Size += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return s, nil
		}
		if err != nil {
			return s, err
		}
	}
}

// sumDir hashes every regular file under dir, skipping hidden entries and
// receiver-side folders.
func sumDir(dir string) (map[string]fileSum, error) {
	out := map[string]fileSum{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		name := filepath.ToSlash(rel)
		if p == dir {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") || (d.IsDir() && skipDirs[name]) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !validName(name) {
			return nil
		}
		s, err := sumFile(p)
		if err != nil {
			return err
		}
		out[name] = s
		return nil
	})
	return out, err
}
