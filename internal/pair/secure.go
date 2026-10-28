package pair

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// Handshake (both sides send, then read; the relay cannot complete it):
//
//	→ "RSP1" role(1) X25519 public key(32)
//	K      = Argon2id(secret, "rs-pair-v1|"+ID)       slow: guessing codes is costly
//	PRK    = HKDF-Extract(SHA-256, ECDH shared secret, salt K)
//	keys   = HKDF-Expand(PRK, label || transcript)   one AES-256-GCM key per direction
//	→ HMAC-SHA256(confirm key of my role, transcript)
//
// A peer without the secret cannot produce the confirmation; ephemeral
// keys give forward secrecy. After the handshake every message is an
// AES-GCM frame [len u32][ciphertext] with a per-direction counter nonce.

const (
	magic        = "RSP1"
	roleSender   = 'S'
	roleReceiver = 'R'
	maxPlain     = 32 << 10
)

// ErrBadCode means the other side does not hold the same pairing code.
var ErrBadCode error = badCode{}

type badCode struct{}

func (badCode) Error() string { return "pairing code does not match the studio's code" }

// Fatal tells retry loops that trying again cannot help.
func (badCode) Fatal() bool { return true }

var keyCache sync.Map // Code → []byte

func codeKey(c Code) []byte {
	if k, ok := keyCache.Load(c); ok {
		return k.([]byte)
	}
	k := argon2.IDKey([]byte(c.Secret), []byte("rs-pair-v1|"+c.salt()), 2, 64*1024, 4, 32)
	keyCache.Store(c, k)
	return k
}

// secureConn is a net.Conn that encrypts and authenticates its stream.
type secureConn struct {
	net.Conn
	rmu    sync.Mutex
	rd     cipher.AEAD
	rn     uint64
	rbuf   []byte // decrypted bytes not yet returned
	wmu    sync.Mutex
	wr     cipher.AEAD
	wn     uint64
	header [4]byte
}

func newGCM(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func nonce(n uint64) []byte {
	var b [12]byte
	binary.BigEndian.PutUint64(b[4:], n)
	return b[:]
}

// handshake authenticates c as role with code and returns the encrypted
// connection. It closes nothing on failure.
func handshake(c net.Conn, code Code, role byte) (net.Conn, error) {
	c.SetDeadline(time.Now().Add(30 * time.Second))
	defer c.SetDeadline(time.Time{})
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	mine := append(append([]byte(magic), role), priv.PublicKey().Bytes()...)
	if _, err := c.Write(mine); err != nil {
		return nil, err
	}
	theirs := make([]byte, len(mine))
	if _, err := io.ReadFull(c, theirs); err != nil {
		return nil, fmt.Errorf("pairing handshake: %w", err)
	}
	want := byte(roleReceiver)
	if role == roleReceiver {
		want = roleSender
	}
	if string(theirs[:4]) != magic || theirs[4] != want {
		return nil, errors.New("pairing handshake: the other side is not a matching RemoteSession")
	}
	peer, err := ecdh.X25519().NewPublicKey(theirs[5:])
	if err != nil {
		return nil, err
	}
	shared, err := priv.ECDH(peer)
	if err != nil {
		return nil, err
	}
	pubS, pubR := mine[5:], theirs[5:]
	if role == roleReceiver {
		pubS, pubR = pubR, pubS
	}
	transcript := string(append(append([]byte(magic), pubS...), pubR...))
	prk, err := hkdf.Extract(sha256.New, shared, codeKey(code))
	if err != nil {
		return nil, err
	}
	expand := func(label string) []byte {
		k, err := hkdf.Expand(sha256.New, prk, label+"|"+transcript, 32)
		if err != nil {
			panic(err) // only fails for absurd lengths
		}
		return k
	}
	kS2R, kR2S := expand("s2r"), expand("r2s")
	cS, cR := expand("confirm-s"), expand("confirm-r")
	myConf, theirConf, wKey, rKey := cS, cR, kS2R, kR2S
	if role == roleReceiver {
		myConf, theirConf, wKey, rKey = cR, cS, kR2S, kS2R
	}
	mac := func(k []byte) []byte {
		h := hmac.New(sha256.New, k)
		h.Write([]byte(transcript))
		return h.Sum(nil)
	}
	if _, err := c.Write(mac(myConf)); err != nil {
		return nil, err
	}
	got := make([]byte, sha256.Size)
	if _, err := io.ReadFull(c, got); err != nil {
		return nil, fmt.Errorf("pairing handshake: %w (wrong code?)", err)
	}
	if !hmac.Equal(got, mac(theirConf)) {
		return nil, ErrBadCode
	}
	wr, err := newGCM(wKey)
	if err != nil {
		return nil, err
	}
	rd, err := newGCM(rKey)
	if err != nil {
		return nil, err
	}
	return &secureConn{Conn: c, rd: rd, wr: wr}, nil
}

func (s *secureConn) Write(p []byte) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	n := 0
	for len(p) > 0 {
		chunk := p[:min(len(p), maxPlain)]
		ct := s.wr.Seal(nil, nonce(s.wn), chunk, nil)
		s.wn++
		binary.BigEndian.PutUint32(s.header[:], uint32(len(ct)))
		if _, err := s.Conn.Write(append(s.header[:], ct...)); err != nil {
			return n, err
		}
		n += len(chunk)
		p = p[len(chunk):]
	}
	return n, nil
}

func (s *secureConn) Read(p []byte) (int, error) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if len(s.rbuf) == 0 {
		var h [4]byte
		if _, err := io.ReadFull(s.Conn, h[:]); err != nil {
			return 0, err
		}
		n := binary.BigEndian.Uint32(h[:])
		if n > maxPlain+uint32(s.rd.Overhead()) {
			return 0, errors.New("pair: oversized frame")
		}
		ct := make([]byte, n)
		if _, err := io.ReadFull(s.Conn, ct); err != nil {
			return 0, err
		}
		pt, err := s.rd.Open(ct[:0], nonce(s.rn), ct, nil)
		if err != nil {
			return 0, errors.New("pair: corrupted or tampered data")
		}
		s.rn++
		s.rbuf = pt
	}
	n := copy(p, s.rbuf)
	s.rbuf = s.rbuf[n:]
	return n, nil
}
