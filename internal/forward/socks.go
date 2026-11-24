package forward

// A SOCKS5 rule: the node listens for SOCKS5 clients, and each connection goes, through
// the rule's peer, to the destination its client asks for.
//
// The node speaks the part of SOCKS5 (RFC 1928) that such a rule needs: the method "no
// authentication required" and the command CONNECT. A destination that the client
// gives as a domain name is looked up by the peer, like the host name of a fixed rule's
// destination (resolve.go). At the peer every destination is held against the allow
// list the peer keeps for this node, as for any other rule.
//
// Whatever a client sends before its request is complete is untrusted and bounded: in
// size by the protocol's own one-byte lengths, in time by socksOpeningTime, and in
// number by maxSocksOpening clients that are in that state at once for one rule.

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/Claw-King/tcpraid/internal/control"
	"github.com/Claw-King/tcpraid/internal/node"
)

const (
	// maxSocksOpening is how many clients of one SOCKS5 rule may be between connecting
	// and the end of their request at the same time. A further one is closed at once.
	maxSocksOpening = 64
)

// socksOpeningTime is how long a client has from connecting to the end of its request.
const socksOpeningTime = 10 * time.Second

// SOCKS5, as RFC 1928 numbers it.
const (
	socksVersion = 5

	socksNoAuth       = 0x00 // method: no authentication required
	socksNoAcceptable = 0xFF // method: none of the client's methods is acceptable

	socksConnect = 1 // command CONNECT

	socksIPv4   = 1 // address types
	socksDomain = 3
	socksIPv6   = 4

	// reply codes
	socksSucceeded       = 0
	socksGeneralFailure  = 1
	socksNotAllowed      = 2 // connection not allowed by ruleset
	socksHostUnreachable = 4
	socksCmdUnsupported  = 7
	socksAddrUnsupported = 8
)

// SocksOpenWarning is what a node says about a SOCKS5 rule that listens on an address
// other hosts can reach.
func SocksOpenWarning(addr, peer string) string {
	return fmt.Sprintf("%s is not a loopback address, and a SOCKS5 rule asks no client who it is. Whoever can connect to that port reaches, through peer %q, every destination that the peer allows this node.", addr, peer)
}

// listenSocks opens the listener of a SOCKS5 rule.
func (rt *Runtime) listenSocks(r node.Rule) (*runningRule, error) {
	ln, err := net.Listen("tcp", r.Socks)
	if err != nil {
		return nil, fmt.Errorf("listening on %s failed: %w", r.Socks, err)
	}
	opening := new(atomic.Int32)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				time.Sleep(50 * time.Millisecond) // out of file descriptors, most likely: it passes
				continue
			}
			if opening.Add(1) > maxSocksOpening {
				opening.Add(-1)
				c.Close()
				continue
			}
			go rt.serveSocks(r, c, opening)
		}
	}()
	return &runningRule{rule: r, lns: []net.Listener{ln}}, nil
}

// socksReply writes a reply with the code rep. The bound address is not known to this
// node, so the reply carries an IPv4 address and a port of zeros.
func socksReply(c net.Conn, rep byte) error {
	c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	defer c.SetWriteDeadline(time.Time{})
	_, err := c.Write([]byte{socksVersion, rep, 0, socksIPv4, 0, 0, 0, 0, 0, 0})
	return err
}

// socksRefuse answers a client with the code rep and ends the connection: the reply,
// the end of this side, and then, for a moment, what the client still sends is read
// and dropped, because a close with unread data cuts off, on some systems, the reply
// that is on its way.
func socksRefuse(c net.Conn, rep byte) {
	defer c.Close()
	if socksReply(c, rep) != nil {
		return
	}
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
	c.SetReadDeadline(time.Now().Add(time.Second))
	io.CopyN(io.Discard, c, 4096)
}

// socksRequest reads the opening of a SOCKS5 client from c and answers the choice of
// method. It returns the destination the client asks for: host is an IP address in
// text, or a domain name that passed node.CheckHostName. rep is the reply code for a
// request that is refused (then err says why), and 0xFF where nothing is to be replied
// because what came is no SOCKS5 or the connection failed.
func socksRequest(c net.Conn) (host string, port int, rep byte, err error) {
	const silent = 0xFF
	var b [262]byte // the longest request: 4 + 1 + 255 + 2
	if _, err = io.ReadFull(c, b[:2]); err != nil {
		return "", 0, silent, err
	}
	if b[0] != socksVersion {
		return "", 0, silent, fmt.Errorf("version %d, not SOCKS5", b[0])
	}
	n := int(b[1]) // the number of methods, 0 to 255: they fit
	if _, err = io.ReadFull(c, b[:n]); err != nil {
		return "", 0, silent, err
	}
	method := byte(socksNoAcceptable)
	for _, m := range b[:n] {
		if m == socksNoAuth {
			method = socksNoAuth
		}
	}
	if _, err = c.Write([]byte{socksVersion, method}); err != nil {
		return "", 0, silent, err
	}
	if method != socksNoAuth {
		return "", 0, silent, errors.New("the client offers no way in without authentication")
	}
	if _, err = io.ReadFull(c, b[:4]); err != nil {
		return "", 0, silent, err
	}
	if b[0] != socksVersion {
		return "", 0, silent, fmt.Errorf("version %d in the request, not SOCKS5", b[0])
	}
	cmd, atyp := b[1], b[3]
	switch atyp {
	case socksIPv4:
		if _, err = io.ReadFull(c, b[:4]); err != nil {
			return "", 0, silent, err
		}
		host = net.IP(b[:4]).String()
	case socksIPv6:
		if _, err = io.ReadFull(c, b[:16]); err != nil {
			return "", 0, silent, err
		}
		host = net.IP(b[:16]).String()
	case socksDomain:
		if _, err = io.ReadFull(c, b[:1]); err != nil {
			return "", 0, silent, err
		}
		l := int(b[0])
		if _, err = io.ReadFull(c, b[:l]); err != nil {
			return "", 0, silent, err
		}
		host = string(b[:l])
	default:
		// how long the rest of the request is cannot be known
		return "", 0, socksAddrUnsupported, fmt.Errorf("address type %d", atyp)
	}
	if _, err = io.ReadFull(c, b[:2]); err != nil {
		return "", 0, silent, err
	}
	port = int(b[0])<<8 | int(b[1])
	switch {
	case cmd != socksConnect:
		return "", 0, socksCmdUnsupported, fmt.Errorf("command %d, and only CONNECT is carried", cmd)
	case port == 0:
		return "", 0, socksGeneralFailure, errors.New("port 0")
	case atyp == socksDomain && net.ParseIP(host) == nil:
		// only a name that can be a host name is passed on to the peer
		if err := node.CheckHostName(host); err != nil {
			return "", 0, socksGeneralFailure, fmt.Errorf("a name that cannot be looked up (%d bytes): %w", len(host), err)
		}
	}
	return host, port, 0, nil
}

// serveSocks takes one client of a SOCKS5 rule: it reads the request, carries the
// connection to the destination the request names, and answers the client. It returns
// when the session has ended.
func (rt *Runtime) serveSocks(r node.Rule, c net.Conn, opening *atomic.Int32) {
	opens := rt.socksOpening
	if opens <= 0 {
		opens = socksOpeningTime
	}
	c.SetDeadline(time.Now().Add(opens))
	host, port, rep, err := socksRequest(c)
	opening.Add(-1)
	c.SetDeadline(time.Time{})
	if err != nil {
		if rep != 0xFF {
			socksRefuse(c, rep)
		} else {
			c.Close()
		}
		return
	}
	oc := rt.newOpenConn(c, c, func() error { return socksReply(c, socksSucceeded) })
	began, _, code := rt.carryEntry(r, oc, host, port)
	if began {
		return
	}
	// The stream never began: the client is told why, as far as the peer said, and
	// the connection is closed the ordinary way, so that the reply arrives.
	rep = socksGeneralFailure
	switch code {
	case control.CodeAllow:
		rep = socksNotAllowed
	case control.CodeDial, control.CodeLookup:
		rep = socksHostUnreachable
	}
	socksRefuse(c, rep)
}
