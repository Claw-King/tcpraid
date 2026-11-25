// Package client lets a Go program work with a tcpraid node that runs on the same host,
// through the node's local API: read its status, its peers, its rules and its sessions,
// change rules, paths and settings, run a path test, and open a connection to a
// destination at a peer (Dial).
//
// A Client is made from the data directory of the node. The node keeps there the
// address of its API, the access token and, when the API is served over TLS, the
// certificate it presents. The Client reads them for every request, so it goes on
// working when the node is started again. Over TLS it talks only to an API that holds
// the key of the recorded certificate, and sends nothing, the token least of all, to
// any other. A program that uses this package needs the same access to the data
// directory as the tcpraid commands: whoever can read the token can do everything the
// API offers.
//
// The package may still change before version 1.0 of tcpraid; a change is named in the
// release notes of the version that brings it.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/Claw-King/tcpraid/internal/node"
)

// ErrNoNode is matched by the error of a request that could not be put to the node: no
// node runs on the data directory, the directory is not a node's, or what answers at
// the recorded address is not the node.
var ErrNoNode = errors.New("the node is not running or -data is not its data directory")

// ErrTimeout is matched by the error of a request that the node did not answer in time.
// The node is there and busy, which is not the same as no node.
var ErrTimeout = errors.New("timed out")

// Error is what the node answered to a request it did not carry out: the HTTP status
// and the reason the API gave.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

// Client reaches the API of the node that runs on one data directory.
type Client struct {
	// Timeout bounds one request of Call and of the methods built on it, from sending
	// it to the end of the answer. Zero: no bound but the context's. It does not
	// bound a connection of Dial.
	Timeout time.Duration

	dir string
}

// New returns a Client for the node whose data directory is dataDir. Nothing is read
// or sent until the first request.
func New(dataDir string) *Client { return &Client{dir: dataDir} }

// DataDir returns the data directory the Client was made for.
func (c *Client) DataDir() string { return c.dir }

// access is what one request needs: where the API is, the token, and how a connection
// is checked when the API is served over TLS.
type access struct {
	base  string // "http://HOST:PORT" or "https://HOST:PORT"
	addr  string
	token string
	tls   *tls.Config // nil: the API is served without TLS
}

// open reads, from the files the node keeps in its data directory, where the API is,
// the token, and whether the API is served over TLS. With TLS one certificate only is
// accepted: the one the node recorded in the data directory, by its key. Nothing is
// sent, the token least of all, to an API that presents another.
func (c *Client) open() (*access, error) {
	token, err := node.ReadToken(c.dir)
	if err != nil {
		return nil, err
	}
	addr, err := node.ReadAPIAddr(c.dir)
	if err != nil {
		return nil, err
	}
	a := &access{base: "http://" + addr, addr: addr, token: token}
	switch cert, err := node.ReadAPITLS(c.dir); {
	case err == nil:
		want := node.FingerprintBytes(cert)
		a.base = "https://" + addr
		a.tls = &tls.Config{
			MinVersion: tls.VersionTLS12,
			// The certificate is not checked against the authorities of the system or
			// against a name: it has to be the one in the data directory, which is
			// checked here, before the handshake ends and anything is sent.
			InsecureSkipVerify: true,
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return errors.New("the API presented no certificate")
				}
				if got := node.FingerprintBytes(cs.PeerCertificates[0]); got != want {
					return fmt.Errorf("the API at %s presents another certificate than %s holds. Nothing was sent to it.", addr, node.APITLSFile)
				}
				return nil
			},
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	return a, nil
}

// Raw sends one request to the node and returns the HTTP status and the body of the
// answer as received, at most 4 MB of it. body, if not nil, is sent as JSON. The error
// is the one of reading the node's files or of the exchange, as it is; Call says what
// it means.
func (c *Client) Raw(ctx context.Context, method, path string, body []byte) (status int, answer []byte, err error) {
	a, err := c.open()
	if err != nil {
		return 0, nil, err
	}
	return c.raw(ctx, a, method, path, body)
}

func (c *Client) raw(ctx context.Context, a *access, method, path string, body []byte) (int, []byte, error) {
	// no proxy from the environment: the token goes to the node and nowhere else
	tr := &http.Transport{Proxy: nil, TLSClientConfig: a.tls}
	defer tr.CloseIdleConnections()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Transport: tr, Timeout: c.Timeout}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, answer, nil
}

// Call sends one request to the node and decodes the answer into out (nil: the answer
// has no body that matters). in, if not nil, is sent as the JSON body. An error that
// matches ErrNoNode means the node could not be asked, one that matches ErrTimeout that
// it did not answer in time; an *Error is what the node answered.
func (c *Client) Call(ctx context.Context, method, path string, in, out any) error {
	a, err := c.open()
	if err != nil {
		return fmt.Errorf("%w (%v)", ErrNoNode, err)
	}
	var body []byte
	if in != nil {
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	status, answer, err := c.raw(ctx, a, method, path, body)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			if c.Timeout > 0 {
				return fmt.Errorf("%w: the node did not answer within %v", ErrTimeout, c.Timeout)
			}
			return fmt.Errorf("%w: the node did not answer in time", ErrTimeout)
		}
		return fmt.Errorf("%w (%v)", ErrNoNode, err)
	}
	if status < 200 || status > 299 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(answer, &e) != nil || e.Error == "" {
			// not this API: something else listens at the recorded address
			return fmt.Errorf("%w (%s answered %d %s)", ErrNoNode, a.addr, status, http.StatusText(status))
		}
		return &Error{Status: status, Message: e.Error}
	}
	if out == nil || len(bytes.TrimSpace(answer)) == 0 {
		// nothing to read, or a node that answers this request without a body: out
		// stays as it was
		return nil
	}
	if err := json.Unmarshal(answer, out); err != nil {
		return fmt.Errorf("%w (%s did not answer as the node does: %v)", ErrNoNode, a.addr, err)
	}
	return nil
}

func peerPath(name string) string { return PathPeers + "/" + url.PathEscape(name) }
func rulePath(name string) string { return PathRules + "/" + url.PathEscape(name) }

// Status returns the status of the node.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var st Status
	err := c.Call(ctx, http.MethodGet, PathStatus, nil, &st)
	return st, err
}

// Sessions returns the live sessions of the node.
func (c *Client) Sessions(ctx context.Context) ([]Session, error) {
	var list []Session
	err := c.Call(ctx, http.MethodGet, PathSessions, nil, &list)
	return list, err
}

// Peers returns the peers of the node.
func (c *Client) Peers(ctx context.Context) ([]Peer, error) {
	var list []Peer
	err := c.Call(ctx, http.MethodGet, PathPeers, nil, &list)
	return list, err
}

// Peer returns the peer called name.
func (c *Client) Peer(ctx context.Context, name string) (Peer, error) {
	var p Peer
	err := c.Call(ctx, http.MethodGet, peerPath(name), nil, &p)
	return p, err
}

// UpdatePeer changes the paths, the allow list, the UDP address or the transport
// settings of the peer called name, whichever of them u names, and returns the peer as
// stored.
func (c *Client) UpdatePeer(ctx context.Context, name string, u PeerUpdate) (Peer, error) {
	var p Peer
	err := c.Call(ctx, http.MethodPut, peerPath(name), u, &p)
	return p, err
}

// SetPath changes one path of a peer, or adds it when the peer has none of that name.
func (c *Client) SetPath(ctx context.Context, peer, path string, u PathUpdate) (PathSet, error) {
	var set PathSet
	err := c.Call(ctx, http.MethodPut, peerPath(peer)+"/paths/"+url.PathEscape(path), u, &set)
	return set, err
}

// RemovePath removes one path of a peer and returns the peer as stored.
func (c *Client) RemovePath(ctx context.Context, peer, path string) (Peer, error) {
	var p Peer
	err := c.Call(ctx, http.MethodDelete, peerPath(peer)+"/paths/"+url.PathEscape(path), nil, &p)
	return p, err
}

// CheckPeer tries every path to the peer called name. It can take as long as the
// connections take to fail.
func (c *Client) CheckPeer(ctx context.Context, name string) (PeerCheck, error) {
	var chk PeerCheck
	err := c.Call(ctx, http.MethodPost, peerPath(name)+"/check", nil, &chk)
	return chk, err
}

// TestPeer sends test data to the peer called name and measures the paths. It lasts
// two runs of req.Seconds for each path, so give the Client a Timeout that leaves room
// for it, or none.
func (c *Client) TestPeer(ctx context.Context, name string, req PeerTestRequest) (PeerTest, error) {
	var res PeerTest
	err := c.Call(ctx, http.MethodPost, peerPath(name)+"/test", req, &res)
	return res, err
}

// Rules returns the rules of the node with their status.
func (c *Client) Rules(ctx context.Context) ([]Rule, error) {
	var list []Rule
	err := c.Call(ctx, http.MethodGet, PathRules, nil, &list)
	return list, err
}

// AddRule makes a rule.
func (c *Client) AddRule(ctx context.Context, spec RuleSpec) (Rule, error) {
	var r Rule
	err := c.Call(ctx, http.MethodPost, PathRules, spec, &r)
	return r, err
}

// ReplaceRule replaces the rule called name by spec. What spec leaves out of the
// fields that a RuleSpec says are kept stays as the stored rule has it.
func (c *Client) ReplaceRule(ctx context.Context, name string, spec RuleSpec) (Rule, error) {
	var r Rule
	err := c.Call(ctx, http.MethodPut, rulePath(name), spec, &r)
	return r, err
}

// RemoveRule removes the rule called name. Its sessions run to their end.
func (c *Client) RemoveRule(ctx context.Context, name string) error {
	return c.Call(ctx, http.MethodDelete, rulePath(name), nil, nil)
}

// Transport returns the transport settings of the node.
func (c *Client) Transport(ctx context.Context) (Transport, error) {
	var t Transport
	err := c.Call(ctx, http.MethodGet, PathTransport, nil, &t)
	return t, err
}

// SetTransport sets transport settings of the node while it runs: a name with a value
// is set to it, a name with nil goes back to its default. The change acts on sessions
// that are created afterwards.
func (c *Client) SetTransport(ctx context.Context, set map[string]*string) (Transport, error) {
	var t Transport
	err := c.Call(ctx, http.MethodPut, PathTransport, TransportUpdate{Set: set}, &t)
	return t, err
}
