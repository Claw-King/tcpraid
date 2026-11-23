// Package control is the format of the control connection between two paired nodes: a
// long-lived connection on which the nodes say that they are there and ask each other
// for things -- in this version, to open the sub-connections of a session.
//
// A control connection starts like every connection between paired nodes (the opening
// of package link, with the control type). After it, each end sends messages:
//
//	length (2, big endian) | JSON | MAC (16)
//
// The length counts the JSON. The MAC is HMAC-SHA256, cut to 16 bytes, over the two
// length bytes and the JSON, under a key derived from the pair secret (Key). The JSON is
// an object with a "type"; a type that the receiving end does not know is to be ignored,
// and so are fields it does not know.
//
//	hello      both ways, once, first    name, version, protocols, instance
//	ping       both ways                 seq
//	pong       the answer to a ping      seq
//	open       to the end with the path  request, session
//	open_fail  the answer when it fails  request, reason
//
// A message longer than MaxMessage, or one whose MAC is wrong, ends the connection.
package control

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/Claw-King/tcpraid/internal/node"
)

const (
	// MaxMessage is the longest JSON a message may hold, in bytes.
	MaxMessage = 16 << 10
	// PingInterval is how often an end sends a ping.
	PingInterval = 10 * time.Second
	// Timeout is how long an end waits for any message before it takes the connection
	// for dead.
	Timeout = 30 * time.Second
	// Protocol is the version of this format.
	Protocol = 1

	macLen       = 16
	writeTimeout = 10 * time.Second
	keyLen       = sha256.Size
)

// The types of message.
const (
	TypeHello    = "hello"
	TypePing     = "ping"
	TypePong     = "pong"
	TypeOpen     = "open"
	TypeOpenFail = "open_fail"
)

// Message is one message. Which fields count depends on Type.
type Message struct {
	Type string `json:"type"`
	// hello
	Name      string `json:"name,omitempty"`
	Version   string `json:"version,omitempty"`
	Protocols []int  `json:"protocols,omitempty"`
	// hello: a number the sending process picked at its start, as text. It differs
	// from one run of a node to the next; absent from versions before 0.1.2
	Instance string `json:"instance,omitempty"`
	// ping, pong
	Seq uint64 `json:"seq,omitempty"`
	// open, open_fail: the number the asking end gave its request
	Request uint64 `json:"request,omitempty"`
	// open: the id of the session, in hex (for the logs of the two ends to match)
	Session string `json:"session,omitempty"`
	// open_fail: why, for a person to read
	Reason string `json:"reason,omitempty"`
}

// ErrTooLong is the error of a message that is longer than MaxMessage.
var ErrTooLong = errors.New("control message too long")

// ErrMAC is the error of a message whose MAC is wrong.
var ErrMAC = errors.New("control message not made with the pair secret of this pairing")

// Key derives the key of the messages' MAC from a pair secret. It is not the key of the
// opening or of the transport.
func Key(pairSecret []byte) ([]byte, error) {
	if len(pairSecret) != node.PairSecretLen {
		return nil, fmt.Errorf("the pair secret is %d bytes, not %d", len(pairSecret), node.PairSecretLen)
	}
	m := hmac.New(sha256.New, pairSecret)
	m.Write([]byte("tcpraid control v1"))
	return m.Sum(nil), nil
}

func sum(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)[:macLen]
}

// Encode returns m as it goes on the connection, with its MAC under key.
func Encode(m Message, key []byte) ([]byte, error) {
	if len(key) != keyLen {
		return nil, errors.New("no key for the control connection")
	}
	body, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if len(body) > MaxMessage {
		return nil, ErrTooLong
	}
	b := make([]byte, 2, 2+len(body)+macLen)
	binary.BigEndian.PutUint16(b, uint16(len(body)))
	b = append(b, body...)
	return append(b, sum(key, b)...), nil
}

// Read reads one message from r and checks its MAC under key.
func Read(r io.Reader, key []byte) (Message, error) {
	var m Message
	if len(key) != keyLen {
		return m, errors.New("no key for the control connection")
	}
	var head [2]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return m, err
	}
	n := int(binary.BigEndian.Uint16(head[:]))
	if n > MaxMessage {
		return m, ErrTooLong
	}
	b := make([]byte, 2+n+macLen)
	copy(b, head[:])
	if _, err := io.ReadFull(r, b[2:]); err != nil {
		return m, err
	}
	if !hmac.Equal(b[2+n:], sum(key, b[:2+n])) {
		return m, ErrMAC
	}
	if err := json.Unmarshal(b[2:2+n], &m); err != nil {
		return m, fmt.Errorf("control message: %w", err)
	}
	return m, nil
}

// Conn is one end of a control connection, after the opening.
type Conn struct {
	c   net.Conn
	key []byte
	wmu sync.Mutex
}

// NewConn returns the end of a control connection on c, with the key of Key.
func NewConn(c net.Conn, key []byte) *Conn {
	return &Conn{c: c, key: key}
}

// Send writes m. It is safe to call from several goroutines.
func (c *Conn) Send(m Message) error {
	b, err := Encode(m, c.key)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.c.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err = c.c.Write(b)
	return err
}

// Receive waits for the next message, for timeout at most (Timeout is the time a node
// uses).
func (c *Conn) Receive(timeout time.Duration) (Message, error) {
	c.c.SetReadDeadline(time.Now().Add(timeout))
	return Read(c.c, c.key)
}

// Close closes the connection.
func (c *Conn) Close() error { return c.c.Close() }

// RemoteAddr returns the address of the other end.
func (c *Conn) RemoteAddr() net.Addr { return c.c.RemoteAddr() }
