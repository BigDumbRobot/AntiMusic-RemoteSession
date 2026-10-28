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
	"strings"
)

// Crockford base32: no I, L, O or U, so codes survive being read aloud.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const (
	idLen     = 4
	secretLen = 8
)

// Code is a pairing code.
type Code struct {
	ID     string // rendezvous ID, visible to the relay
	Secret string // never sent anywhere
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

// String formats the code as XXXX-XXXX-XXXX.
func (c Code) String() string {
	return c.ID + "-" + c.Secret[:4] + "-" + c.Secret[4:]
}

// ParseCode accepts a code typed loosely: any case, with or without
// dashes or spaces, and O/I/L for 0/1/1.
func ParseCode(s string) (Code, error) {
	var out []byte
	for _, r := range strings.ToUpper(s) {
		switch {
		case r == '-' || r == ' ':
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
	if len(out) != idLen+secretLen {
		return Code{}, errors.New("pairing code: should be 12 characters, like 7KQM-4MDX-R2PA")
	}
	return Code{ID: string(out[:idLen]), Secret: string(out[idLen:])}, nil
}
