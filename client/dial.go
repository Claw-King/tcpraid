package client

// Dial: one connection to a destination at a peer, carried by the node.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// RefusedError is the error of a Dial that the node took and that did not lead to a
// connection: the peer is not connected, the peer's allow list does not hold the
// destination, the destination did not take the connection, the name was not found.
// Message is the reason the node gave.
type RefusedError struct {
	Message string
}

func (e *RefusedError) Error() string { return e.Message }

// dialSetup bounds the time to reach the API and to put the request to it when the
// context of a Dial has no earlier deadline.
const dialSetup = 10 * time.Second

// Dial opens a connection to address ("host:port") as seen from the peer, through the
// node, and returns it. The host is an IP address or a name, which the peer looks up.
// At the peer the destination is held against the allow list the peer keeps for this
// node.
//
// The context bounds the time until the node has answered. When it ends before that,
// Dial returns its error and resets the connection to the node, so that nothing
// reaches the destination as a stream. How long the node waits for the connection to
// begin is the node's to bound. An error that matches
// ErrNoNode means the node could not be asked, ErrOlderNode that it does not carry such
// connections; a *RefusedError means it was asked and no connection began; an *Error
// is another answer of the node, such as a request it does not take.
//
// The node says that the connection began when data arrives from the destination, or
// shortly after the peer took the connection if the peer has reported no failure by
// then. A destination that does not answer at all therefore shows as a connection that
// begins and is then cut off.
//
// On the Conn that Dial returns a stream that was cut off is an error, never the end
// of the data: Read returns io.EOF only when the destination ended its side in order.
// The other direction ends with CloseWrite, which passes the end of what was written
// on to the destination. Close before CloseWrite resets the connection, so that the
// destination does not take what it got for all there was.
func (c *Client) Dial(ctx context.Context, peer, address string) (*Conn, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%q is not host:port: %v", address, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 || host == "" {
		return nil, fmt.Errorf("%q does not name a host and a port from 1 to 65535", address)
	}
	a, err := c.open()
	if err != nil {
		return nil, fmt.Errorf("%w (%v) (-data %s)", ErrNoNode, err, c.dir)
	}
	deadline := time.Now().Add(dialSetup)
	ctxDeadline, bounded := ctx.Deadline()
	if bounded && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	// noNode is the error of a step that failed before the node had the request: the
	// context's own where the context has ended or its time is over, and else that
	// the node could not be asked.
	noNode := func(err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return fmt.Errorf("the dial was given up before the node had the request: %w", cerr)
		}
		if bounded && !time.Now().Before(ctxDeadline) {
			return fmt.Errorf("the dial was given up before the node had the request: %w", context.DeadlineExceeded)
		}
		return fmt.Errorf("%w (%v) (-data %s)", ErrNoNode, err, c.dir)
	}
	raw, err := (&net.Dialer{Deadline: deadline}).DialContext(ctx, "tcp", a.addr)
	if err != nil {
		return nil, noNode(err)
	}
	tcp := raw.(*net.TCPConn)
	// From the first moment a close of this end is a reset, also the one the system
	// does when the process is killed, and the one of a context that ends while the
	// node answers. The node then ends what it began as cut off, and the destination
	// never reads the end of a stream that this end did not end. Only CloseWrite
	// followed by Close closes in order.
	tcp.SetLinger(0)
	conn := net.Conn(tcp)
	fail := func(err error) (*Conn, error) {
		tcp.Close()
		return nil, err
	}
	if a.tls != nil {
		// the same check as every request: the certificate the node recorded, before
		// anything is sent
		tc := tls.Client(tcp, a.tls)
		tc.SetDeadline(deadline)
		if err := tc.HandshakeContext(ctx); err != nil {
			return fail(noNode(err))
		}
		tc.SetDeadline(time.Time{})
		conn = tc
	}
	body, err := json.Marshal(ConnectRequest{Peer: peer, Host: host, Port: port})
	if err != nil {
		return fail(err)
	}
	hr, err := http.NewRequest(http.MethodPost, a.base+PathConnect, bytes.NewReader(body))
	if err != nil {
		return fail(err)
	}
	hr.Header.Set("Authorization", "Bearer "+a.token)
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Connection", "Upgrade")
	hr.Header.Set("Upgrade", ConnectUpgrade)
	conn.SetWriteDeadline(deadline)
	if err := hr.Write(conn); err != nil {
		return fail(noNode(err))
	}
	conn.SetWriteDeadline(time.Time{})
	// The node answers when the connection to the destination begins, or when it is
	// clear that none will: it bounds that time itself. A context that ends before
	// that ends the wait.
	stop := context.AfterFunc(ctx, func() { tcp.Close() })
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, hr)
	if !stop() {
		// the context ended while the node answered, and the connection is closed
		return fail(fmt.Errorf("the wait for the answer of the node was given up: %w", ctx.Err()))
	}
	if err != nil {
		return fail(fmt.Errorf("%w (%s did not answer the request: %v) (-data %s)", ErrNoNode, a.addr, err, c.dir))
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Upgrade") != ConnectUpgrade {
		answer, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		var e struct {
			Error string `json:"error"`
		}
		switch json.Unmarshal(answer, &e); {
		case e.Error == "":
			return fail(fmt.Errorf("%w (%s answered %s) (-data %s)", ErrNoNode, a.addr, resp.Status, c.dir))
		case resp.StatusCode == http.StatusNotFound && e.Error == "no such path":
			return fail(ErrOlderNode)
		case resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusForbidden:
			return fail(&RefusedError{Message: e.Error})
		}
		// the node was asked and said no: its own words, not "the node is not running"
		return fail(&Error{Status: resp.StatusCode, Message: e.Error})
	}
	return &Conn{tcp: tcp, c: conn, br: br, cl: c, session: resp.Header.Get(SessionHeader)}, nil
}

// ErrOlderNode is the error of a Dial at a node that has no route for it.
var ErrOlderNode = errors.New("the node on this host does not carry connections for other programs. It runs a version before 0.9.0. Update the node.")

// Conn is a connection to a destination at a peer, as Dial returns it. It is a
// net.Conn. Its two directions end apart: CloseWrite ends the one to the destination,
// and Read returns io.EOF when the one from the destination has ended in order.
type Conn struct {
	tcp *net.TCPConn  // the connection to the node's API
	c   net.Conn      // tcp itself, or TLS over it
	br  *bufio.Reader // reads of c; it may hold data that came with the node's answer

	cl      *Client // the client that dialed, for Done
	session string  // the id of the session that carries the connection; empty from a node before 0.11.0

	mu      sync.Mutex
	written bool // CloseWrite passed the end on
}

// ErrNoWord is the error of Done on a connection whose node does not say how a session
// ended: it runs a version before 0.11.0.
var ErrNoWord = errors.New("the node does not say how the connection ended")

// CutError is the error of Done for a connection whose stream did not end completely
// in both directions. Reason is the reason the node gave.
type CutError struct {
	Reason string
}

func (e *CutError) Error() string {
	return "the connection did not end complete at the node: " + e.Reason
}

// Done waits until the node has ended the session that carried the connection and
// returns nil when the stream ended completely in both directions: the other node
// wrote everything that was written here to the destination and ended that direction
// in order, and everything the destination sent was handed over. Call it after
// CloseWrite and after Read has returned io.EOF. Before that the session has not
// ended, and Done waits.
//
// It returns a *CutError when the session ended another way, ErrNoWord when the node
// is of a version that does not say, and the context's error when the context ends
// first. A node that no longer knows the session answers with an *Error of status 404:
// it keeps how its last sessions ended for a few minutes only. That is no word that
// the stream ended complete.
func (c *Conn) Done(ctx context.Context) error {
	if c.session == "" {
		return ErrNoWord
	}
	// each request waits at the node for a while, within the time the Client gives a
	// request
	wait := 30
	if t := c.cl.Timeout; t > 0 {
		wait = max(0, min(wait, int((t-2*time.Second)/time.Second)))
	}
	for {
		var end SessionEnd
		err := c.cl.Call(ctx, http.MethodGet, PathSessions+"/"+c.session+"/end?wait="+strconv.Itoa(wait), nil, &end)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			return err
		case !end.Ended:
			// the node waited and the session goes on. A moment between two requests,
			// for a node that answers at once
			select {
			case <-time.After(200 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		case end.Complete:
			return nil
		}
		return &CutError{Reason: end.Reason}
	}
}

// Read reads data from the destination. It returns io.EOF only when the destination
// ended its side and everything it sent was read; a connection that was cut off gives
// another error.
func (c *Conn) Read(p []byte) (int, error) { return c.br.Read(p) }

// Write sends data to the destination. That it returned without an error says that
// the node took the data, which the node may still hold.
func (c *Conn) Write(p []byte) (int, error) { return c.c.Write(p) }

// CloseWrite ends the direction to the destination: the destination reads the end of
// a complete stream once it has read all that was written. Reading goes on.
func (c *Conn) CloseWrite() error {
	cw, ok := c.c.(interface{ CloseWrite() error })
	if !ok {
		return errors.New("the connection cannot be closed in one direction")
	}
	if err := cw.CloseWrite(); err != nil {
		return err
	}
	c.mu.Lock()
	c.written = true
	c.mu.Unlock()
	return nil
}

// Close ends the connection. After CloseWrite it is an ordinary close. Before it, the
// connection is reset: the node ends the session as cut off, and the destination sees
// a reset and not the end of a stream.
func (c *Conn) Close() error {
	c.mu.Lock()
	written := c.written
	c.mu.Unlock()
	if written {
		c.tcp.SetLinger(-1)
	}
	// the TCP connection itself: over TLS the end of what was written has gone out
	// with CloseWrite, and a reset must not be preceded by one
	return c.tcp.Close()
}

// LocalAddr returns the local address of the connection to the node.
func (c *Conn) LocalAddr() net.Addr { return c.c.LocalAddr() }

// RemoteAddr returns the address of the node's API, which carries the connection. The
// destination is the one given to Dial.
func (c *Conn) RemoteAddr() net.Addr { return c.c.RemoteAddr() }

// SetDeadline sets the read and write deadlines.
func (c *Conn) SetDeadline(t time.Time) error { return c.c.SetDeadline(t) }

// SetReadDeadline sets the deadline of Read.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.c.SetReadDeadline(t) }

// SetWriteDeadline sets the deadline of Write.
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.c.SetWriteDeadline(t) }

var _ net.Conn = (*Conn)(nil)
