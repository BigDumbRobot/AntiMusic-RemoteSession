// Package pair connects two RemoteSession machines through a relay using
// a pairing code, with no VPN, port forwarding or accounts.
//
// A code like 7KQM-4MDX-R2PA has two parts. The first 4 characters are the
// rendezvous ID the relay uses to match the two sides. The last 8 are a
// secret that never leaves the two machines: it authenticates them to each
// other and keys the encryption, so the relay only ever passes ciphertext.
//
// Both sides connect out to the relay, so this works behind home routers
// and studio firewalls.
package pair

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"unicode"
)

// Crockford base32: no I, L, O or U, so codes survive being read aloud.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const (
	idLen     = 4
	secretLen = 8
)

// Code is a pairing code. A relay code (XXXX-XXXX-XXXX) holds a rendezvous
// ID and the secret. A direct code (XXXXXX-XXXXXX-XXXXXX) holds the
// studio's public address and the secret: no relay or server is involved.
type Code struct {
	ID     string         // rendezvous ID, visible to the relay ("" for direct codes)
	Addr   netip.AddrPort // studio's public IPv4 address (direct codes only)
	Secret string         // never sent anywhere
}

const directLen = 18 // 32-bit IP + 16-bit port + 40-bit secret, in 5-bit characters

// Direct returns the direct code for reaching the studio at addr.
func (c Code) Direct(addr netip.AddrPort) Code {
	return Code{Addr: addr, Secret: c.Secret}
}

// IsDirect reports whether the code carries the studio's address.
func (c Code) IsDirect() bool { return c.Addr.IsValid() }

// salt separates relay and direct keys for the same secret.
func (c Code) salt() string {
	if c.IsDirect() {
		return "direct"
	}
	return c.ID
}

// NewCode returns a random code.
func NewCode() Code {
	b := make([]byte, idLen+secretLen)
	rand.Read(b)
	s := make([]byte, len(b))
	for i, v := range b {
		s[i] = alphabet[int(v)%len(alphabet)] // 256 is a multiple of 32: unbiased
	}
	return Code{ID: string(s[:idLen]), Secret: string(s[idLen:])}
}

// String formats the code as XXXX-XXXX-XXXX, or XXXXXX-XXXXXX-XXXXXX for a
// direct code.
func (c Code) String() string {
	if c.IsDirect() {
		a := c.Addr.Addr().As4()
		bits := uint64(a[0])<<40 | uint64(a[1])<<32 | uint64(a[2])<<24 | uint64(a[3])<<16 | uint64(c.Addr.Port())
		var head [10]byte // 48 bits + 2 zero bits = 10 characters
		v := bits << 2
		for i := 9; i >= 0; i-- {
			head[i] = alphabet[v&31]
			v >>= 5
		}
		s := string(head[:]) + c.Secret
		return s[:6] + "-" + s[6:12] + "-" + s[12:]
	}
	return c.ID + "-" + c.Secret[:4] + "-" + c.Secret[4:]
}

// ParseCode accepts a code typed loosely: any case, with or without
// dashes or spaces, and O/I/L for 0/1/1.
func ParseCode(s string) (Code, error) {
	var out []byte
	for _, r := range strings.ToUpper(s) {
		switch {
		case r == '-' || unicode.IsSpace(r):
			continue
		case r == 'O':
			r = '0'
		case r == 'I' || r == 'L':
			r = '1'
		}
		if r > 127 || !strings.ContainsRune(alphabet, r) {
			return Code{}, fmt.Errorf("pairing code: %q is not a valid character", r)
		}
		out = append(out, byte(r))
	}
	switch len(out) {
	case idLen + secretLen:
		return Code{ID: string(out[:idLen]), Secret: string(out[idLen:])}, nil
	case directLen:
		var v uint64
		for _, ch := range out[:10] {
			v = v<<5 | uint64(strings.IndexByte(alphabet, ch))
		}
		if v&3 != 0 {
			return Code{}, errors.New("pairing code: not a valid code (check for typos)")
		}
		v >>= 2
		ip := netip.AddrFrom4([4]byte{byte(v >> 40), byte(v >> 32), byte(v >> 24), byte(v >> 16)})
		port := uint16(v)
		if port == 0 || !ip.IsGlobalUnicast() {
			return Code{}, errors.New("pairing code: not a valid code (check for typos)")
		}
		return Code{Addr: netip.AddrPortFrom(ip, port), Secret: string(out[10:])}, nil
	}
	return Code{}, errors.New("pairing code: should look like 7KQM4M-DXR2PA-9HTZ3C")
}
