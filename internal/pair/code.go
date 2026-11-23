// Package pair is how two nodes get to know each other: the pairing code one node hands
// out, and the exchange in which the other node redeems it.
//
// A pairing code is one line of text that a person carries from the node that made it
// to the node that joins:
//
//	tcpraid-pair-1-<base32>
//
// "1" is the version of the format. The base32 part (RFC 4648 alphabet in lower case, no
// padding) encodes a payload followed by the first 4 bytes of its SHA-256:
//
//	id (4) | one-time secret (16) | fingerprint (32) | expiry (8) | n (1) | n x ( len (1) | address )
//
// The id is public and names the code. The secret proves that the joining node was given
// the code. The fingerprint is that of the identity key of the node that made the code,
// so the joining node knows whom it must reach. The expiry is Unix time in seconds, big
// endian. The addresses are host:port, where the joining node connects.
package pair

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"github.com/Claw-King/tcpraid/internal/node"
)

const (
	prefix  = "tcpraid-pair-"
	version = "1"

	idLen     = 4
	secretLen = 16
	sumLen    = 4

	// MaxAddresses is how many addresses a code can carry.
	MaxAddresses = 8
	// DefaultTTL is how long a code is valid when nothing else is asked for.
	DefaultTTL = time.Hour
	// MinTTL and MaxTTL are the shortest and the longest validity a code can be given.
	MinTTL = time.Second
	MaxTTL = 30 * 24 * time.Hour
)

// Errors of Parse. Callers match them with errors.Is.
var (
	ErrNotACode  = errors.New("this does not start with " + prefix + " and is not a tcpraid pairing code")
	ErrVersion   = errors.New("the pairing code is of a version this program does not know")
	ErrChecksum  = errors.New("the pairing code is damaged. Its checksum does not match. Copy it again, complete and unchanged.")
	ErrMalformed = errors.New("the pairing code is malformed")
	ErrExpired   = errors.New("the pairing code has expired")
)

var encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// Code is a pairing code.
type Code struct {
	ID          [idLen]byte
	Secret      [secretLen]byte
	Fingerprint [sha256.Size]byte
	Expires     time.Time // whole seconds
	Addresses   []string  // host:port, at least one
}

// New makes a pairing code for the node whose identity key has the fingerprint, valid
// for ttl from now, with a fresh random id and secret.
func New(fingerprint [sha256.Size]byte, addresses []string, ttl time.Duration, now time.Time) (*Code, error) {
	if ttl < MinTTL || ttl > MaxTTL || ttl%time.Second != 0 {
		return nil, fmt.Errorf("a pairing code is valid for whole seconds, from %v to %v, not %v", MinTTL, MaxTTL, ttl)
	}
	if err := checkAddresses(addresses); err != nil {
		return nil, err
	}
	c := &Code{Fingerprint: fingerprint, Expires: time.Unix(now.Add(ttl).Unix(), 0), Addresses: append([]string{}, addresses...)}
	if _, err := rand.Read(c.ID[:]); err != nil {
		return nil, err
	}
	if _, err := rand.Read(c.Secret[:]); err != nil {
		return nil, err
	}
	return c, nil
}

// CheckAddress reports whether addr is a host:port that a code can carry and a node can
// dial. It is the rule for the address of a path.
func CheckAddress(addr string) error { return node.CheckAddr(addr) }

func checkAddresses(addresses []string) error {
	if len(addresses) == 0 {
		return errors.New("a pairing code needs at least one address")
	}
	if len(addresses) > MaxAddresses {
		return fmt.Errorf("a pairing code carries at most %d addresses", MaxAddresses)
	}
	for _, a := range addresses {
		if err := CheckAddress(a); err != nil {
			return err
		}
	}
	return nil
}

// IDString returns the id as 8 hex digits, the form in which it is shown and stored.
func (c *Code) IDString() string { return hex.EncodeToString(c.ID[:]) }

// SecretHash returns the SHA-256 of the one-time secret. The node that made the code
// keeps this and not the secret; both nodes use it as the key of the pairing proof.
func (c *Code) SecretHash() [sha256.Size]byte { return sha256.Sum256(c.Secret[:]) }

// String names the code by its id. It does not print the secret, so a Code that ends up
// in a log line or an error gives nothing away, whether it is printed by value or by
// pointer, with %v, %+v or %s; Text returns the code itself.
func (c Code) String() string { return "pairing code " + hex.EncodeToString(c.ID[:]) }

// GoString is String: %#v does not print the fields either.
func (c Code) GoString() string { return c.String() }

// Text returns the code as the text a person copies. It holds the secret.
func (c *Code) Text() string {
	b := make([]byte, 0, 128)
	b = append(b, c.ID[:]...)
	b = append(b, c.Secret[:]...)
	b = append(b, c.Fingerprint[:]...)
	b = binary.BigEndian.AppendUint64(b, uint64(c.Expires.Unix()))
	b = append(b, byte(len(c.Addresses)))
	for _, a := range c.Addresses {
		b = append(b, byte(len(a)))
		b = append(b, a...)
	}
	sum := sha256.Sum256(b)
	b = append(b, sum[:sumLen]...)
	return prefix + version + "-" + strings.ToLower(encoding.EncodeToString(b))
}

// Parse reads a pairing code as a person pasted it: white space anywhere in it and the
// case of its letters do not matter. A code that is not one, is of an unknown version,
// is damaged, is malformed or has expired at the time now gives the matching error.
func Parse(text string, now time.Time) (*Code, error) {
	text = strings.ToLower(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, text))
	rest, ok := strings.CutPrefix(text, prefix)
	if !ok {
		return nil, ErrNotACode
	}
	ver, body, ok := strings.Cut(rest, "-")
	if !ok {
		return nil, fmt.Errorf("%w: nothing follows the version", ErrMalformed)
	}
	if ver != version {
		// what stands in the version's place is not repeated: with a dash missing it is
		// the code's body, secret and all
		return nil, fmt.Errorf("%w: this program reads version %s", ErrVersion, version)
	}
	b, err := encoding.DecodeString(strings.ToUpper(body))
	if err != nil {
		return nil, fmt.Errorf("%w: it holds a character that does not belong there", ErrMalformed)
	}
	if len(b) < sumLen {
		return nil, fmt.Errorf("%w: too short", ErrMalformed)
	}
	payload, sum := b[:len(b)-sumLen], b[len(b)-sumLen:]
	if want := sha256.Sum256(payload); !bytes.Equal(sum, want[:sumLen]) {
		return nil, ErrChecksum
	}
	c := &Code{}
	r := bytes.NewReader(payload)
	var expires uint64
	var n uint8
	for _, field := range []any{c.ID[:], c.Secret[:], c.Fingerprint[:], &expires, &n} {
		if err := binary.Read(r, binary.BigEndian, field); err != nil {
			return nil, fmt.Errorf("%w: too short", ErrMalformed)
		}
	}
	if expires > 1<<62 {
		return nil, fmt.Errorf("%w: the expiry is out of range", ErrMalformed)
	}
	c.Expires = time.Unix(int64(expires), 0)
	for i := 0; i < int(n); i++ {
		l, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("%w: too short", ErrMalformed)
		}
		a := make([]byte, l)
		if _, err := io.ReadFull(r, a); err != nil {
			return nil, fmt.Errorf("%w: too short", ErrMalformed)
		}
		c.Addresses = append(c.Addresses, string(a))
	}
	if r.Len() != 0 {
		return nil, fmt.Errorf("%w: it is longer than its content", ErrMalformed)
	}
	if err := checkAddresses(c.Addresses); err != nil {
		// the address is part of the code and is not repeated
		return nil, fmt.Errorf("%w: it holds no address, too many, or one that is not host:port", ErrMalformed)
	}
	if !c.Expires.After(now) {
		return nil, fmt.Errorf("%w. It expired at %s. Ask for a new one.", ErrExpired, c.Expires.UTC().Format(time.RFC3339))
	}
	return c, nil
}
