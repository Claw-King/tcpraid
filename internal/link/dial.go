package link

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/node"
)

// Dialer opens connections to peers.
type Dialer struct {
	Identity *node.Identity
	// RootCAs are the authorities for the owncert tier with verify ca; nil means the
	// system's.
	RootCAs *x509.CertPool
}

// Dial connects to peer over path in the path's security tier. The connection it
// returns is authenticated as far as the tier goes: in the TLS tiers this end has
// checked the other end's certificate; whether the other end accepted this end's shows
// at the first read. Nothing has been sent on it: the opening comes next (Open). Dial
// gives up when ctx is done.
func (d *Dialer) Dial(ctx context.Context, peer node.Peer, path node.Path) (net.Conn, error) {
	if err := path.Check(); err != nil {
		return nil, err
	}
	var cfg *tls.Config
	switch path.Security {
	case node.SecurityOff:
	case node.SecurityTLS:
		cfg = d.pinned(ALPN, peer.Fingerprint, "the node at this address is not the peer: its key has another fingerprint")
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
			cfg = d.pinned(ALPNOwn, path.Pin, "the certificate at this address is not the pinned one: its key has another fingerprint")
		}
	}
	proxy := ""
	if path.Type == "socks5" {
		proxy = path.Proxy
	}
	conn, err := engine.DialContext(ctx, path.Addr, proxy)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return conn, nil
	}
	tc := tls.Client(conn, cfg)
	conn.SetDeadline(time.Now().Add(HandshakeTimeout))
	if err := tc.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, closedInHandshake(err)
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
	conn, err := d.Dial(ctx, peer, path)
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
		return fmt.Errorf("the other node does not accept this node: it is not paired with it, or no longer (%w)", err)
	}
	return err
}

// closedInHandshake adds, to the error of a handshake that the other end cut off, what
// that may mean: a listener closes connections that have not finished their handshake
// when it has too many of them.
func closedInHandshake(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) {
		return fmt.Errorf("%w (the other node closed the connection during the handshake; it may be busy: try again)", err)
	}
	return err
}
