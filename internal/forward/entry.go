package forward

// Entries that speak a protocol of their own before the stream begins: a SOCKS5 rule
// (socks.go) and the connection a program on this host opens through the API (Connect).
//
// Such an entry has to answer its client: the stream begins, or there is none and why.
// The transport tells this end when the peer has taken a session, and tells it later
// if the peer could not connect the session to its destination. It has no word for
// "the destination is connected". So the answer "the stream begins" is given as soon
// as one of two things has happened: data has arrived from the destination, or the
// peer has taken the session and has reported no failure for settle. A failure that
// the peer reports within that time is answered with its reason. One that it reports
// later reaches the client as a connection that is reset after it began.

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/Claw-King/tcpraid/internal/control"
	"github.com/Claw-King/tcpraid/internal/node"
)

var (
	// entrySettle is how long after the peer has taken a session an entry waits for the
	// peer to report that it could not connect it, before it tells its client that the
	// stream begins. A refusal by the peer's allow list and a destination that refuses
	// are reported right after the session is taken; a destination that does not answer
	// is reported only after the peer's own timeout, which no client is made to wait for.
	entrySettle = 150 * time.Millisecond
	// entryReasonWait is how long an entry waits, after a session ended because the
	// peer could not connect it, for the peer to say why, before it answers its client
	// without that.
	entryReasonWait = 2 * time.Second
)

// openConn is the connection of an entry's client as the transport gets it: the
// application connection of a session. Until the stream begins it holds back what
// would tell the client otherwise: nothing is written, and a close does not close.
type openConn struct {
	net.Conn           // the client's connection: TCP, or TLS over TCP
	rd       io.Reader // what the client sends is read from here
	begin    func() error
	settle   time.Duration

	mu     sync.Mutex
	state  int  // opening, begun or over
	linger *int // the linger asked for while opening, set when the stream begins
	// reset: the linger asked for last makes a close a reset. A TLS connection is
	// then closed underneath, without the alert that says "this is the end": the
	// client must not read a stream that was cut off as one that ended.
	reset bool
	timer *time.Timer
}

const (
	entryOpening = iota // the client has no answer yet
	entryBegun          // the client was told that the stream begins
	entryOver           // closed before the stream began: the entry answers and closes
)

// newOpenConn wraps c. rd is where the rest of what the client sends is read (c itself,
// or a buffer in front of it). begin writes to c the answer that the stream begins.
func (rt *Runtime) newOpenConn(c net.Conn, rd io.Reader, begin func() error) *openConn {
	settle := rt.EntrySettle
	if settle <= 0 {
		settle = entrySettle
	}
	return &openConn{Conn: c, rd: rd, begin: begin, settle: settle}
}

// Read reads what the client sends. An end that is read after the answer to the client
// could not be written is no end of a stream. A reset is reported to one call only: when
// the write of the answer was that call, the read that follows sees the end of the data
// where the connection was in fact reset, and the stream would be passed on as complete.
// open holds the lock while it writes the answer, so a read that returned for that reason
// waits here until open has recorded that the write failed.
func (oc *openConn) Read(p []byte) (int, error) {
	n, err := oc.rd.Read(p)
	if err == io.EOF {
		oc.mu.Lock()
		over := oc.state == entryOver
		oc.mu.Unlock()
		if over {
			return n, io.ErrUnexpectedEOF
		}
	}
	return n, err
}

// Write passes on data that arrived from the destination: the stream has begun.
func (oc *openConn) Write(p []byte) (int, error) {
	if err := oc.open(); err != nil {
		return 0, err
	}
	return oc.Conn.Write(p)
}

// open tells the client that the stream begins, once.
func (oc *openConn) open() error {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	switch oc.state {
	case entryBegun:
		return nil
	case entryOver:
		return net.ErrClosed
	}
	if oc.timer != nil {
		oc.timer.Stop()
	}
	if err := oc.begin(); err != nil {
		oc.state = entryOver
		oc.Conn.SetReadDeadline(time.Now())
		return err
	}
	oc.state = entryBegun
	if oc.linger != nil {
		oc.setLinger(*oc.linger)
	}
	return nil
}

// SessionUp is called by the transport when the peer has taken the session.
func (oc *openConn) SessionUp() {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	if oc.state == entryOpening && oc.timer == nil {
		oc.timer = time.AfterFunc(oc.settle, func() { oc.open() })
	}
}

// Close closes the connection once the stream has begun. Before that it only ends the
// reading: the entry still has to tell the client why there is no stream (refused).
func (oc *openConn) Close() error {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	switch oc.state {
	case entryBegun:
		if t, ok := oc.Conn.(interface{ NetConn() net.Conn }); ok && oc.reset {
			return t.NetConn().Close()
		}
		return oc.Conn.Close()
	case entryOpening:
		oc.state = entryOver
		if oc.timer != nil {
			oc.timer.Stop()
		}
		oc.Conn.SetReadDeadline(time.Now())
	}
	return nil
}

// refused reports whether the connection was closed before the stream began. The
// entry then answers the client on the connection itself and closes it.
func (oc *openConn) refused() bool {
	oc.Close() // no stream begins after this
	oc.mu.Lock()
	defer oc.mu.Unlock()
	return oc.state == entryOver
}

// SetLinger is passed on once the stream has begun: until then a close of the
// connection, also the one the system does when the process dies, is an ordinary one,
// which a client that waits for its answer cannot take for the end of a stream.
func (oc *openConn) SetLinger(sec int) error {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	oc.reset = sec == 0
	if oc.state == entryBegun {
		return oc.setLinger(sec)
	}
	oc.linger = &sec
	return nil
}

// setLinger sets the linger of the TCP connection underneath (oc.mu held).
func (oc *openConn) setLinger(sec int) error {
	c := oc.Conn
	if t, ok := c.(interface{ NetConn() net.Conn }); ok {
		c = t.NetConn() // TLS: the connection it runs over
	}
	if tc, ok := c.(*net.TCPConn); ok {
		return tc.SetLinger(sec)
	}
	return nil
}

// CloseWrite ends the stream toward the client. A destination that closes without
// having sent anything was connected all the same, so the stream begins first.
func (oc *openConn) CloseWrite() error {
	if err := oc.open(); err != nil {
		return err
	}
	if cw, ok := oc.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// SetReadDeadline leaves a connection alone whose reading Close has ended.
func (oc *openConn) SetReadDeadline(t time.Time) error {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	if oc.state == entryOver {
		return nil
	}
	return oc.Conn.SetReadDeadline(t)
}

// carryEntry carries the client connection oc of an entry to host:port at the rule's
// peer and returns when the session has ended, or without one. host is an IP address
// or a host name, which the peer looks up. When the stream never began it returns why,
// with the peer's code for it (control.CodeAllow, control.CodeDial, control.CodeLookup;
// empty for every other reason), and began is false: oc is then still open for the
// entry's answer.
func (rt *Runtime) carryEntry(r node.Rule, oc *openConn, host string, port int) (began bool, why, code string) {
	a := rt.begin(r.Name)
	ip := net.ParseIP(host)
	if ip == nil {
		var err error
		if ip, err = rt.resolveAt(r.Peer, host, port); err != nil {
			a.fail(err.Error())
			var le *lookupError
			if errors.As(err, &le) {
				code = le.code
			}
			oc.Close()
			return false, err.Error(), code
		}
	} else if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	rt.carryAttempt(a, r, oc, ip, port)
	if !oc.refused() {
		return true, "", ""
	}
	why, code = a.reason(entryReasonWait)
	if why == "" {
		why = fmt.Sprintf("the connection to %s through peer %q ended before it began", net.JoinHostPort(host, fmt.Sprint(port)), r.Peer)
	}
	return false, why, code
}

// ConnectRefused is the error of Connect when no stream began: the reason, and whether
// the peer's allow list was it.
type ConnectRefused struct {
	Reason string
	Allow  bool
}

func (e *ConnectRefused) Error() string { return e.Reason }

// Connect carries c, a connection that a program on this host opened through the API,
// to host:port at the peer called name, as one session, and returns when the session
// has ended. host is an IP address or a host name, which the peer looks up; the peer's
// allow list holds for it as for every destination. rd is where the rest of what the
// program sends is read, and begin tells the program that the stream begins. The
// session takes the transport settings of the peer and the node, and the node that
// opens its sub-connections is chosen as for a rule without a dial.
//
// A nil error means that the stream began and c is closed. A *ConnectRefused means
// that it never began: c is still open, for the caller to say why and to close.
func (rt *Runtime) Connect(name, host string, port int, c net.Conn, rd io.Reader, begin func() error) error {
	if _, ok := rt.Node.Peer(name); !ok {
		return &ConnectRefused{Reason: fmt.Sprintf("peer %q is not paired with this node", name)}
	}
	oc := rt.newOpenConn(c, rd, begin)
	began, why, code := rt.carryEntry(node.Rule{Peer: name, Enabled: true}, oc, host, port)
	if began {
		return nil
	}
	return &ConnectRefused{Reason: why, Allow: code == control.CodeAllow}
}
