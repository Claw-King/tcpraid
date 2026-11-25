package link

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/Claw-King/tcpraid/internal/node"
)

// Every connection between paired nodes starts with an opening, sent by the dialing
// end, in every tier:
//
//	"TCPL" | version (1) | type (1) | fingerprint of the dialing node (32) |
//	Unix time (8) | nonce (16) | request (8) | path (1) | role (1) | MAC (16)
//
// The MAC is HMAC-SHA256, cut to 16 bytes, over everything before it, under a key
// derived from the pair secret (OpeningKey). It proves that the dialing end holds the
// pair secret; in the off tier it is the only proof of who dials. Request, path and role
// are set in an opening of TypeReverse. TypeSpare sets the path, and since 0.12.0
// TypeSub may set the request to the number of a set of settings (Dialer.OpenSet). They
// are zero otherwise.
const (
	openingMagic   = "TCPL"
	openingVersion = 1
	// OpeningLen is the length of an opening in bytes.
	OpeningLen = 88
	openingMAC = 16

	// TypeSub opens a sub-connection of a session that the dialing end carries: the
	// transport's header follows.
	TypeSub = 1
	// TypeReverse opens a sub-connection of a session that the listening end carries
	// and asked the dialing end to connect to (the request of the opening says which):
	// the listening end sends the transport's header on it.
	TypeReverse = 2
	// TypeControl opens the control connection of the two nodes (package control).
	TypeControl = 3
	// TypeCheck asks the listening end to prove that it holds the pair secret too. The
	// answer is: status (1, zero) | MAC (16) over a label, a zero byte and the nonce of
	// the opening, under the opening key.
	TypeCheck = 4
	// TypeSpare opens a spare connection (since 0.6.0): one that the listening end may
	// later make a sub-connection of a session it carries, by sending the transport's
	// header on it, as on a connection of TypeReverse. The path of the opening says
	// which path of the dialing end it came over; the role is data. A listening end
	// that did not ask for spare connections closes it.
	TypeSpare = 5

	checkAnswerLen = 1 + openingMAC

	// The roles a sub-connection can have, as the role byte of an opening of
	// TypeReverse says them.
	RoleData    = 0
	RoleRepair  = 1
	RoleControl = 2

	// OpeningSkew is how far the time in an opening may be from the listener's clock.
	OpeningSkew = 10 * time.Minute
	// OpeningTimeout bounds the wait for an opening, and for the answer to a check.
	OpeningTimeout = 10 * time.Second
)

// Opening is the content of an opening.
type Opening struct {
	Type        byte
	Fingerprint [sha256.Size]byte // of the dialing node's identity key
	Time        time.Time
	Nonce       [16]byte
	Request     uint64
	Path, Role  byte
}

// OpeningKey derives the key of the opening's MAC from a pair secret. It is not the key
// of the transport (TransportKey).
func OpeningKey(pairSecret []byte) ([]byte, error) {
	if len(pairSecret) != node.PairSecretLen {
		return nil, fmt.Errorf("the pair secret is %d bytes, not %d", len(pairSecret), node.PairSecretLen)
	}
	m := hmac.New(sha256.New, pairSecret)
	m.Write([]byte("tcpraid link opening v1"))
	return m.Sum(nil), nil
}

func openingSum(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)[:openingMAC]
}

// newOpening returns an opening of the type from the node with the fingerprint, with
// the current time and a fresh nonce.
func newOpening(typ byte, fingerprint [sha256.Size]byte) (*Opening, error) {
	o := &Opening{Type: typ, Fingerprint: fingerprint, Time: time.Now()}
	if _, err := rand.Read(o.Nonce[:]); err != nil {
		return nil, err
	}
	return o, nil
}

// marshal returns the opening as it goes on the connection, with its MAC under key.
func (o *Opening) marshal(key []byte) []byte {
	b := make([]byte, OpeningLen)
	copy(b[0:4], openingMagic)
	b[4] = openingVersion
	b[5] = o.Type
	copy(b[6:38], o.Fingerprint[:])
	binary.BigEndian.PutUint64(b[38:46], uint64(o.Time.Unix()))
	copy(b[46:62], o.Nonce[:])
	binary.BigEndian.PutUint64(b[62:70], o.Request)
	b[70] = o.Path
	b[71] = o.Role
	copy(b[72:], openingSum(key, b[:72]))
	return b
}

// parseOpening reads the fields of an opening. It checks the form only: whoever sent it
// is not known before verify.
func parseOpening(b []byte) (*Opening, error) {
	if len(b) != OpeningLen || string(b[0:4]) != openingMagic {
		return nil, errors.New("not the opening of a tcpraid node")
	}
	if b[4] != openingVersion {
		return nil, fmt.Errorf("opening of version %d, this node speaks %d", b[4], openingVersion)
	}
	o := &Opening{Type: b[5], Request: binary.BigEndian.Uint64(b[62:70]), Path: b[70], Role: b[71]}
	copy(o.Fingerprint[:], b[6:38])
	o.Time = time.Unix(int64(binary.BigEndian.Uint64(b[38:46])), 0)
	copy(o.Nonce[:], b[46:62])
	return o, nil
}

// verifyOpening checks the MAC of the opening b under key, and then that its time is
// within OpeningSkew of now.
func verifyOpening(b []byte, o *Opening, key []byte, now time.Time) error {
	if !hmac.Equal(b[72:], openingSum(key, b[:72])) {
		return errors.New("the opening was not made with the pair secret of this pairing")
	}
	if d := now.Sub(o.Time); d > OpeningSkew || d < -OpeningSkew {
		return errors.New("the clocks of the two nodes are more than ten minutes apart, or the opening is old")
	}
	return nil
}

// checkAnswer is what a listener answers to an opening of TypeCheck.
func checkAnswer(key []byte, nonce [16]byte) []byte {
	msg := append([]byte("tcpraid link check v1\x00"), nonce[:]...)
	return append([]byte{0}, openingSum(key, msg)...)
}

// sendOpening writes an opening of the type on conn, from the node with the fingerprint
// that shares pairSecret with the other end, and returns it. fill, if not nil, sets the
// fields that the type uses before the opening is sent.
func sendOpening(conn net.Conn, typ byte, fingerprint [sha256.Size]byte, pairSecret []byte, fill func(*Opening)) (*Opening, []byte, error) {
	key, err := OpeningKey(pairSecret)
	if err != nil {
		return nil, nil, err
	}
	o, err := newOpening(typ, fingerprint)
	if err != nil {
		return nil, nil, err
	}
	if fill != nil {
		fill(o)
	}
	conn.SetWriteDeadline(time.Now().Add(OpeningTimeout))
	defer conn.SetWriteDeadline(time.Time{})
	if _, err := conn.Write(o.marshal(key)); err != nil {
		return nil, nil, err
	}
	return o, key, nil
}

// errNoAnswer is the error of a check whose opening the other end answered by closing
// the connection.
var errNoAnswer = errors.New("the other end closed the connection without answering. Either the address is not a tcpraid listener, or the other node is not paired with this one, or their pair secrets differ. Pairing again fixes the last two.")

// checkConn sends an opening of TypeCheck on conn and checks the answer: nil means the
// other end holds pairSecret.
func checkConn(conn net.Conn, fingerprint [sha256.Size]byte, pairSecret []byte) error {
	o, key, err := sendOpening(conn, TypeCheck, fingerprint, pairSecret, nil)
	if err != nil {
		return err
	}
	conn.SetReadDeadline(time.Now().Add(OpeningTimeout))
	defer conn.SetReadDeadline(time.Time{})
	got := make([]byte, checkAnswerLen)
	if _, err := io.ReadFull(conn, got); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return errNoAnswer
		}
		return err
	}
	if !hmac.Equal(got, checkAnswer(key, o.Nonce)) {
		return errors.New("the other end did not answer with the pair secret of this pairing")
	}
	return nil
}

// Acceptor takes the openings of the connections that arrive from peers. Its HandlePeer
// and HandlePlain are a Server's handlers of the same names.
type Acceptor struct {
	// PeerByFingerprint finds the peer whose identity key has the fingerprint.
	PeerByFingerprint func(fingerprint string) (node.Peer, bool)
	// HandleSub gets a connection whose opening asked for a sub-connection, after the
	// opening has been verified, with the request of the opening: 0, or the number of
	// a set of settings that the peer named for its rules (since 0.12.0). The
	// transport's header is the next thing on it. It owns the connection and returns
	// without waiting for it to end. nil: such connections are closed.
	HandleSub func(peer node.Peer, request uint64, conn net.Conn)
	// HandleReverse gets a connection whose verified opening is of TypeReverse, with
	// the request, path and role of the opening; this node sends the transport's
	// header on it. It owns the connection and returns without waiting for it to end.
	// nil: such connections are closed.
	HandleReverse func(peer node.Peer, request uint64, path, role byte, conn net.Conn)
	// HandleControl gets a connection whose verified opening is of TypeControl. It
	// owns the connection and returns without waiting for it to end. nil: such
	// connections are closed.
	HandleControl func(peer node.Peer, conn net.Conn)
	// HandleSpare gets a connection whose verified opening is of TypeSpare, with the
	// path of the opening. It owns the connection and returns without waiting for it
	// to end. nil: such connections are closed.
	HandleSpare func(peer node.Peer, path byte, conn net.Conn)
	// LogRefused logs one line about a connection that was refused; it should be a
	// limited logger (Server.LogRefused). nil: nothing is logged.
	LogRefused func(format string, args ...any)
}

// HandlePeer serves a connection from a peer that TLS has authenticated: the opening
// must name that peer and carry the MAC of its pair secret.
func (a *Acceptor) HandlePeer(peer node.Peer, conn net.Conn) {
	b, o, err := readOpening(conn)
	if err == nil && hex.EncodeToString(o.Fingerprint[:]) != peer.Fingerprint {
		err = errors.New("the opening names another node than the one TLS authenticated")
	}
	if err == nil {
		err = a.verify(b, o, peer)
	}
	if err != nil {
		a.refuse(conn, err)
		return
	}
	a.dispatch(peer, o, conn)
}

// HandlePlain serves a connection from the plain listener: the opening says which peer
// dials, and its MAC is the proof. A connection that passes counts as authenticated.
func (a *Acceptor) HandlePlain(conn net.Conn, authenticated func() bool) {
	b, o, err := readOpening(conn)
	var peer node.Peer
	if err == nil {
		var ok bool
		if peer, ok = a.PeerByFingerprint(hex.EncodeToString(o.Fingerprint[:])); !ok {
			err = errors.New("the opening names a node that is not a peer of this node")
		} else {
			err = a.verify(b, o, peer)
		}
	}
	if err != nil {
		a.refuse(conn, err)
		return
	}
	if !authenticated() {
		// closed to make room while its opening was read
		conn.Close()
		return
	}
	a.dispatch(peer, o, conn)
}

func readOpening(conn net.Conn) ([]byte, *Opening, error) {
	b := make([]byte, OpeningLen)
	conn.SetReadDeadline(time.Now().Add(OpeningTimeout))
	if _, err := io.ReadFull(conn, b); err != nil {
		return nil, nil, fmt.Errorf("no opening: %w", err)
	}
	conn.SetReadDeadline(time.Time{})
	o, err := parseOpening(b)
	return b, o, err
}

func (a *Acceptor) verify(b []byte, o *Opening, peer node.Peer) error {
	key, err := OpeningKey(peer.PairSecret)
	if err != nil {
		return err
	}
	return verifyOpening(b, o, key, time.Now())
}

// refuse closes a connection whose opening did not pass. Nothing is answered.
func (a *Acceptor) refuse(conn net.Conn, err error) {
	conn.Close()
	// a connection that this node closed itself (to make room, to stop) has its line
	if a.LogRefused != nil && !errors.Is(err, net.ErrClosed) {
		a.LogRefused("refused a connection from %s: %v", conn.RemoteAddr(), err)
	}
}

func (a *Acceptor) dispatch(peer node.Peer, o *Opening, conn net.Conn) {
	switch o.Type {
	case TypeSub:
		if a.HandleSub == nil {
			conn.Close()
			return
		}
		a.HandleSub(peer, o.Request, conn)
	case TypeCheck:
		// verify derived this key already; a peer record always has a proper secret
		if key, err := OpeningKey(peer.PairSecret); err == nil {
			conn.SetWriteDeadline(time.Now().Add(OpeningTimeout))
			conn.Write(checkAnswer(key, o.Nonce))
		}
		closeAfterAnswer(conn)
	case TypeReverse:
		if a.HandleReverse == nil || o.Role > RoleControl {
			conn.Close()
			return
		}
		a.HandleReverse(peer, o.Request, o.Path, o.Role, conn)
	case TypeControl:
		if a.HandleControl == nil {
			conn.Close()
			return
		}
		a.HandleControl(peer, conn)
	case TypeSpare:
		if a.HandleSpare == nil {
			conn.Close()
			return
		}
		a.HandleSpare(peer, o.Path, conn)
	default:
		conn.Close()
	}
}

// closeAfterAnswer closes a connection whose last bytes were just written, after the
// other end has closed it or a moment has passed: closing at once could cut the answer
// off on the way.
func closeAfterAnswer(conn net.Conn) {
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	io.Copy(io.Discard, conn)
	conn.Close()
}
