package link

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/node"
	"github.com/Claw-King/tcpraid/internal/pair"
)

// Dialer opens connections to peers.
type Dialer struct {
	Identity *node.Identity
	// RootCAs are the authorities for the owncert tier with verify ca; nil means the
	// system's.
	RootCAs *x509.CertPool

	mu sync.Mutex
	// first is, for a path with several addresses, the address that is tried first: the
	// one that a connection was last made to, or the one that comes after those that
	// were tried in vain. It is kept in memory only.
	first map[string]string
}

// AddrTimeout bounds the attempt at one address of a path that has several: connecting
// and the handshake of the path's tier. A path with one address has the time of the
// caller alone, as before paths had several.
const AddrTimeout = 8 * time.Second

// addrTimeout is AddrTimeout, which a test shortens.
var addrTimeout = AddrTimeout

// addrErrors is the error of a path none of whose addresses gave a connection.
type addrErrors struct {
	addrs []string
	errs  []error // as many as were tried
}

func (e *addrErrors) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "none of the path's %d addresses gave a connection.", len(e.addrs))
	for i, addr := range e.addrs {
		if i < len(e.errs) {
			fmt.Fprintf(&b, " %s: %v.", addr, e.errs[i])
		} else {
			fmt.Fprintf(&b, " %s was not tried, because the time was up.", addr)
		}
	}
	return b.String()
}

func (e *addrErrors) Unwrap() []error { return e.errs }

// pathKey names a path of a peer with the addresses it has, so that what is remembered
// about it goes when its addresses change.
func pathKey(peer node.Peer, path node.Path) string {
	return peer.Fingerprint + "\x00" + path.Name + "\x00" + strings.Join(path.Addresses(), "\x00")
}

// order returns the addresses of path in the order to try them: the remembered one
// first, then the others in the order the path has them.
func (d *Dialer) order(peer node.Peer, path node.Path) []string {
	addrs := path.Addresses()
	d.mu.Lock()
	first := d.first[pathKey(peer, path)]
	d.mu.Unlock()
	if first == "" || first == addrs[0] || !slices.Contains(addrs, first) {
		return addrs
	}
	out := []string{first}
	for _, a := range addrs {
		if a != first {
			out = append(out, a)
		}
	}
	return out
}

// remember notes the address of path to try first next time.
func (d *Dialer) remember(peer node.Peer, path node.Path, addr string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.first == nil {
		d.first = map[string]string{}
	}
	if len(d.first) >= maxRemembered {
		clear(d.first) // paths come and go; what is lost is only an order
	}
	d.first[pathKey(peer, path)] = addr
}

// maxRemembered bounds the paths a Dialer remembers an address of.
const maxRemembered = 1024

// Dial connects to peer over path in the path's security tier. The connection it
// returns is authenticated as far as the tier goes: in the TLS tiers this end has
// checked the other end's certificate; whether the other end accepted this end's shows
// at the first read. Nothing has been sent on it: the opening comes next (Open). Dial
// gives up when ctx is done.
//
// A path with several addresses is tried address by address, each for at most
// AddrTimeout, beginning with the one that worked last. The error then says what
// happened at each.
func (d *Dialer) Dial(ctx context.Context, peer node.Peer, path node.Path) (net.Conn, error) {
	if err := path.Check(); err != nil {
		return nil, err
	}
	if len(path.Addrs) == 0 {
		return d.DialAt(ctx, peer, path, path.Addr)
	}
	addrs := d.order(peer, path)
	failed := &addrErrors{addrs: addrs}
	for i, addr := range addrs {
		actx, cancel := context.WithTimeout(ctx, addrTimeout)
		conn, err := d.DialAt(actx, peer, path, addr)
		cancel()
		if err == nil {
			if i > 0 || addr != path.Addr {
				d.remember(peer, path, addr)
			}
			return conn, nil
		}
		failed.errs = append(failed.errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	// the next dial begins where this one did not get to, or at the top again
	next := addrs[len(failed.errs)%len(addrs)]
	if len(failed.errs) == len(addrs) {
		next = path.Addr
	}
	d.remember(peer, path, next)
	return nil, failed
}

// DialAt is Dial for one address of the path, which need not be one the path has: the
// path says everything else.
func (d *Dialer) DialAt(ctx context.Context, peer node.Peer, path node.Path, addr string) (net.Conn, error) {
	if err := path.Check(); err != nil {
		return nil, err
	}
	path.Addr, path.Addrs = addr, nil
	var cfg *tls.Config
	switch path.Security {
	case node.SecurityOff:
	case node.SecurityTLS:
		cfg = d.pinned(ALPN, peer.Fingerprint, "the node at this address is not the peer. Its key has another fingerprint.")
	case node.SecurityOwnCert:
		if path.Verify == node.VerifyCA {
			host, _, err := net.SplitHostPort(path.Addr)
			if err != nil {
				return nil, fmt.Errorf("address %q: want host:port", path.Addr)
			}
			cfg = &tls.Config{
				MinVersion:   tls.VersionTLS13,
				Certificates: []tls.Certificate{d.Identity.TLSCertificate()},
				NextProtos:   []string{ALPNOwn},
				ServerName:   host,
				RootCAs:      d.RootCAs,
			}
		} else {
			cfg = d.pinned(ALPNOwn, path.Pin, "the certificate at this address is not the pinned one. Its key has another fingerprint.")
		}
	}
	proxy := ""
	if path.Type == "socks5" {
		proxy = path.Proxy
	}
	conn, err := engine.DialContextMark(ctx, path.Addr, proxy, path.Mark)
	if err != nil {
		if path.Mark != 0 && errors.Is(err, os.ErrPermission) {
			return nil, fmt.Errorf("the path's socket mark %d could not be set, because the node lacks the capability CAP_NET_ADMIN. Give the node that capability, or take the mark off the path. (%w)", path.Mark, err)
		}
		return nil, err
	}
	if cfg == nil {
		return conn, nil
	}
	tc := tls.Client(conn, cfg)
	conn.SetDeadline(time.Now().Add(HandshakeTimeout))
	if err := tc.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, handshakeFailed(err, path)
	}
	conn.SetDeadline(time.Time{})
	if got := tc.ConnectionState().NegotiatedProtocol; got != cfg.NextProtos[0] {
		conn.Close()
		return nil, errors.New("the other end is not a tcpraid node listener")
	}
	return tc, nil
}

// pinned returns the configuration of a TLS client that shows the identity certificate
// and accepts exactly the certificate whose key has the fingerprint.
func (d *Dialer) pinned(proto, fingerprint, mismatch string) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{d.Identity.TLSCertificate()},
		NextProtos:   []string{proto},
		// Not checked against authorities or a host name: VerifyConnection compares the
		// key with the fingerprint this node holds.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || node.Fingerprint(cs.PeerCertificates[0]) != fingerprint {
				return errors.New(mismatch)
			}
			return nil
		},
	}
}

// Open connects to peer over path (Dial) and sends the opening of a sub-connection
// (TypeSub): the connection it returns is ready for the transport's header. It gives up
// when ctx is done.
func (d *Dialer) Open(ctx context.Context, peer node.Peer, path node.Path) (net.Conn, error) {
	return d.open(ctx, peer, path, TypeSub, nil)
}

// OpenReverse connects to peer over path and sends the opening of a sub-connection that
// the peer asked for (TypeReverse), with the request it gave and the path and role this
// node gives the sub-connection: the peer sends the transport's header on the
// connection it returns. It gives up when ctx is done.
func (d *Dialer) OpenReverse(ctx context.Context, peer node.Peer, path node.Path, request uint64, pathIndex, role byte) (net.Conn, error) {
	return d.open(ctx, peer, path, TypeReverse, func(o *Opening) { o.Request, o.Path, o.Role = request, pathIndex, role })
}

// OpenControl connects to peer over path and sends the opening of the control
// connection (TypeControl). It gives up when ctx is done.
func (d *Dialer) OpenControl(ctx context.Context, peer node.Peer, path node.Path) (net.Conn, error) {
	return d.open(ctx, peer, path, TypeControl, nil)
}

func (d *Dialer) open(ctx context.Context, peer node.Peer, path node.Path, typ byte, fill func(*Opening)) (net.Conn, error) {
	conn, err := d.Dial(ctx, peer, path)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	_, _, err = sendOpening(conn, typ, d.Identity.FingerprintBytes(), peer.PairSecret, fill)
	if !stop() {
		conn.Close()
		return nil, ctx.Err()
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// Check opens a connection to peer over path, asks the other end to prove that it holds
// the pair secret of this pairing (TypeCheck), and closes the connection. A nil error
// means that the path works, that the node at its end is the peer, and that both nodes
// hold the same pair secret -- in every tier.
func (d *Dialer) Check(ctx context.Context, peer node.Peer, path node.Path) error {
	return d.CheckAt(ctx, peer, path, path.Addr)
}

// CheckAt is Check at one address of the path.
func (d *Dialer) CheckAt(ctx context.Context, peer node.Peer, path node.Path, addr string) error {
	conn, err := d.DialAt(ctx, peer, path, addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	err = checkConn(conn, d.Identity.FingerprintBytes(), peer.PairSecret)
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil && path.Security != node.SecurityOff && strings.Contains(err.Error(), "bad certificate") {
		// what a TLS server answers when it refuses the client's certificate
		return fmt.Errorf("the other node does not accept this node, which is not paired with it or no longer is (%w)", err)
	}
	if errors.Is(err, errNoAnswer) && path.Security == node.SecurityOff {
		// a TLS listener closes a connection that does not begin with a TLS handshake
		return fmt.Errorf("%w On this path, whose security is off, the address may also be the other node's -listen, which takes TLS only, so that the security tiers of the two ends do not match. Give the path the address of the other node's -listen-plain, or set the path's security to tls.", err)
	}
	return err
}

// handshakeFailed says what the error of a TLS handshake on path means, where that can
// be told.
func handshakeFailed(err error, path node.Path) error {
	// the alert of a TLS listener that knows none of the protocol names it was offered
	if strings.Contains(err.Error(), "no application protocol") {
		if path.Security == node.SecurityOwnCert {
			return fmt.Errorf("the security tiers of the two ends do not match. This path is of the owncert tier, and the listener at %s has no certificate of its operator to show. Start the other node with -cert and -key, or set the path's security to tls. (%w)", path.Addr, err)
		}
		return fmt.Errorf("the listener at %s speaks TLS and is not the listener of a tcpraid node (%w)", path.Addr, err)
	}
	return closedInHandshake(err)
}

// closedInHandshake adds, to the error of a handshake that the other end cut off, what
// that may mean: a listener closes connections that have not finished their handshake
// when it has too many of them.
func closedInHandshake(err error) error {
	if errors.Is(err, io.EOF) || pair.IsReset(err) {
		return fmt.Errorf("%w. The other node closed the connection during the handshake and may be busy. Try again. If it happens every time, the address may be the other node's -listen-plain, which takes paths whose security is off, so that the security tiers of the two ends do not match.", err)
	}
	return err
}
