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
//	hello         both ways, once, first       name, version, protocols, instance, features
//	ping          both ways                    seq
//	pong          the answer to a ping         seq
//	open          to the end with the path     request, session, test_path, test_subs
//	open_fail     the answer when it fails     request, reason
//	session_fail  to the end that carries      session, code, reason
//	paths         to the end with the paths    request
//	paths_reply   the answer                   request, paths
//	test_end      to the end with the sink     session
//	resolve       to the end that will connect  request, host, port
//	resolve_reply the answer                   request, addr, or code and reason
//
// session_fail tells the end that carries a session why the other end could not connect
// it to its destination. paths asks an end for the names and sizes of the paths it holds
// to the asking end, for a path test that the asking end runs (FeaturePathTest); an open
// with test_path asks for the sub-connections of such a test, and test_end says that
// the asking end has ended one of the test's sessions.
//
// resolve asks the other end which address a host name stands for in its network, for
// a session that the asking end is about to carry to that name (FeatureResolve). The
// answer names one IP address, or says why there is none.
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
	// since 0.3.0
	TypeSessionFail = "session_fail"
	TypePaths       = "paths"
	TypePathsReply  = "paths_reply"
	TypeTestEnd     = "test_end"
	// since 0.4.0
	TypeResolve      = "resolve"
	TypeResolveReply = "resolve_reply"
)

// FeaturePathTest in a hello says that the sending node takes the sessions of a path
// test and answers what such a test asks over the control connection (paths, and open
// with test_path).
const FeaturePathTest = "path-test"

// FeatureResolve in a hello says that the sending node answers resolve: it looks up a
// host name for a peer that carries a connection to it.
const FeatureResolve = "resolve"

// The codes of a session_fail.
const (
	CodeAllow = "allow" // the destination is not in the allow list kept for the other node
	CodeDial  = "dial"  // connecting to the destination failed; the reason is the system's error
)

// The codes of a resolve_reply that names no address. CodeAllow is one of them too:
// the name stands for no address that the allow list kept for the asking node holds.
const (
	CodeLookup = "lookup" // the name could not be looked up; the reason is the resolver's error
	CodeBusy   = "busy"   // the node looks up as many names as it does at a time
)

// PathInfo is one path in a paths_reply: what the holder of the path calls it, how many
// data sub-connections it keeps on it, and whether it carries data at all. It names no
// address.
type PathInfo struct {
	Name string `json:"name"`
	Subs int    `json:"subs"`
	Data bool   `json:"data"`
}

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
	// hello: what the sending node can do beyond the first version of this format
	// (FeaturePathTest); absent from versions before 0.3.0
	Features []string `json:"features,omitempty"`
	// ping, pong
	Seq uint64 `json:"seq,omitempty"`
	// open, open_fail: the number the asking end gave its request
	Request uint64 `json:"request,omitempty"`
	// open: the id of the session, in hex (for the logs of the two ends to match).
	// session_fail, test_end: the session the message is about
	Session string `json:"session,omitempty"`
	// open_fail, session_fail: why, for a person to read
	Reason string `json:"reason,omitempty"`
	// open, for a path test: the path to dial, counted from 1 in the order of the
	// paths_reply (0: not a test, every path), and how many data sub-connections to
	// keep on it (0: as many as the path has)
	TestPath int `json:"test_path,omitempty"`
	TestSubs int `json:"test_subs,omitempty"`
	// session_fail: CodeAllow or CodeDial. resolve_reply without an address: CodeLookup,
	// CodeAllow or CodeBusy
	Code string `json:"code,omitempty"`
	// resolve: the host name to look up, and the port the session will go to (the
	// allow list of the answering node may depend on it)
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
	// resolve_reply: the IP address the name stands for at the answering node, as text;
	// empty when there is none (Code and Reason say why)
	Addr string `json:"addr,omitempty"`
	// paths_reply: the paths the sending node holds to the node that asked
	Paths []PathInfo `json:"paths,omitempty"`
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
