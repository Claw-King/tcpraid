package forward

// A rule whose destination is a host name.
//
// The destination of a rule is in the peer's network, so the peer is the one to say
// which address a name stands for there. For every connection of such a rule this node
// asks the peer over a control connection (resolve) and carries the connection to the
// address the peer answers, exactly as it carries one to a destination that was given
// as an address. The session itself knows nothing of the name.
//
// The peer answers with the first address of the name that its allow list for this
// node lets through, with the port of the rule. A name does not get a peer past the
// allow list: the session to the address is checked against it again, like any other.

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"time"

	"github.com/Claw-King/tcpraid/internal/control"
	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/node"
)

const (
	// ResolveWait is how long a connection waits for the peer to answer which address a
	// name stands for; then it is reset.
	ResolveWait = 10 * time.Second
	// lookupTimeout bounds one lookup this node does for a peer. It is shorter than
	// ResolveWait, so that the peer hears why and does not just stop waiting.
	lookupTimeout = 5 * time.Second
	// maxResolving is how many names a node looks up for its peers at the same time; a
	// further request is answered at once with control.CodeBusy.
	maxResolving = 32
)

// resolved is the answer to a resolve: an address, or why there is none.
type resolved struct {
	ip           net.IP
	code, reason string
}

// resolveAt asks the peer called name which address host stands for in its network,
// for a connection that goes to port there. The error says, for a rule's status, why
// there is no address.
func (rt *Runtime) resolveAt(name, host string, port int) (net.IP, error) {
	rt.mu.Lock()
	pc := rt.ctl[name]
	var cc *ctlConn
	if pc != nil && len(pc.conns) > 0 {
		cc = pc.conns[0]
	}
	switch {
	case cc == nil:
		rt.mu.Unlock()
		return nil, fmt.Errorf("peer %q is not connected to this node, so it cannot be asked which address %s stands for. The rule's destination is a host name, which the peer looks up.", name, host)
	case !pc.greeted:
		rt.mu.Unlock()
		return nil, fmt.Errorf("the connection with peer %q has only just come up, and it could not be asked yet which address %s stands for. Try again in a moment.", name, host)
	case !slices.Contains(pc.features, control.FeatureResolve):
		version := pc.version
		rt.mu.Unlock()
		return nil, fmt.Errorf("peer %q does not look up host names for this node (it runs version %q). Both nodes need tcpraid 0.4.0 or later. Until then, give the rule the IP address of %s.", name, version, host)
	}
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		rt.mu.Unlock()
		return nil, err
	}
	key := reverseKey{name, binary.BigEndian.Uint64(b[:])}
	answer := make(chan resolved, 1)
	rt.resolves[key] = answer
	rt.resolveSent++
	rt.mu.Unlock()
	defer func() {
		rt.mu.Lock()
		delete(rt.resolves, key)
		rt.mu.Unlock()
	}()
	if err := cc.Send(control.Message{Type: control.TypeResolve, Request: key.request, Host: host, Port: port}); err != nil {
		return nil, fmt.Errorf("peer %q could not be asked which address %s stands for: %v", name, host, err)
	}
	wait := time.NewTimer(ResolveWait)
	defer wait.Stop()
	select {
	case r := <-answer:
		switch {
		case r.ip != nil:
			return r.ip, nil
		case r.code == control.CodeAllow:
			return nil, fmt.Errorf("peer %q refused the destination %s, because the name stands for no address in the allow list it keeps for this node. Change the list on that node with \"tcpraid peers set\".", name, net.JoinHostPort(host, strconv.Itoa(port)))
		case r.code == control.CodeBusy:
			return nil, fmt.Errorf("peer %q looks up as many host names as it does at a time, and did not look up %s. Try again in a moment.", name, host)
		case r.reason != "":
			return nil, fmt.Errorf("peer %q could not find the address of %s: %s", name, host, r.reason)
		}
		return nil, fmt.Errorf("peer %q could not find the address of %s", name, host)
	case <-wait.C:
		return nil, fmt.Errorf("peer %q did not say within %v which address %s stands for", name, ResolveWait, host)
	case <-rt.ctx.Done():
		return nil, errors.New("the node is stopping")
	}
}

// resolveAnswered handles a peer's answer to a resolve. What the peer names as the
// address is used only if it is an IP address.
func (rt *Runtime) resolveAnswered(peer string, m control.Message) {
	rt.mu.Lock()
	answer := rt.resolves[reverseKey{peer, m.Request}]
	rt.mu.Unlock()
	if answer == nil {
		return
	}
	r := resolved{code: printable(m.Code, 16), reason: printable(m.Reason, maxReason)}
	if m.Addr != "" {
		// An answer that names something else than an address counts as no answer.
		// IPv4 in the short form, for the session header and the allow list.
		if r.ip = net.ParseIP(m.Addr); r.ip == nil {
			r.code, r.reason = control.CodeLookup, "the peer answered with something that is not an IP address"
		} else if v4 := r.ip.To4(); v4 != nil {
			r.ip = v4
		}
	}
	select {
	case answer <- r:
	default: // answered already
	}
}

// serveResolve answers a peer's question which address a host name stands for here: the
// first one that this node's allow list for the peer lets through, with the port the
// peer names. The lookup runs beside the control connection and is bounded in time and
// in number.
func (rt *Runtime) serveResolve(cc *ctlConn, m control.Message) {
	reply := func(r control.Message) {
		r.Type, r.Request = control.TypeResolveReply, m.Request
		cc.Send(r)
	}
	if err := node.CheckHostName(m.Host); err != nil || m.Port < 1 || m.Port > 65535 {
		reply(control.Message{Code: control.CodeLookup, Reason: "the request names no host name and port that can be looked up"})
		return
	}
	peer, ok := rt.Node.Peer(cc.peer)
	if !ok {
		// the pairing has just ended
		reply(control.Message{Code: control.CodeLookup, Reason: "the node that was asked is no longer paired with this one"})
		return
	}
	// a stored allow list has passed this parse
	allow, err := engine.ParseAllow(peer.Allow)
	if err != nil {
		reply(control.Message{Code: control.CodeLookup, Reason: "the node that was asked cannot read its allow list"})
		return
	}
	rt.mu.Lock()
	if rt.ctx == nil || rt.ctx.Err() != nil {
		rt.mu.Unlock()
		return
	}
	if rt.resolving >= maxResolving {
		rt.mu.Unlock()
		reply(control.Message{Code: control.CodeBusy})
		return
	}
	rt.resolving++
	ctx, cancel := context.WithTimeout(rt.ctx, lookupTimeout)
	rt.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			rt.mu.Lock()
			rt.resolving--
			rt.mu.Unlock()
		}()
		ips, err := rt.lookup(ctx, m.Host)
		if err != nil || len(ips) == 0 {
			// the peer is told in a few words; the resolver's own stay in this log
			rt.logf("peer %q asked for the address of %s, and the lookup failed: %v", cc.peer, m.Host, err)
			reply(control.Message{Code: control.CodeLookup, Reason: lookupFailed(err)})
			return
		}
		for _, ip := range ips {
			if allow.Allows(ip, m.Port) {
				reply(control.Message{Addr: ip.String()})
				return
			}
		}
		// the addresses are not named: the allow list says what the peer may learn of
		// this network
		reply(control.Message{Code: control.CodeAllow})
	}()
}

// lookupFailed says, for the peer that asked, why a name has no address. The resolver's
// own error is not passed on: it names the servers that were asked, which are this
// network's business.
func lookupFailed(err error) string {
	var de *net.DNSError
	switch {
	case err == nil:
		return "the name has no address"
	case errors.As(err, &de) && de.IsNotFound:
		return "no such host"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &de) && de.IsTimeout:
		return "the lookup took longer than " + lookupTimeout.String()
	}
	return "the lookup failed. The log of that node says why."
}

// lookup returns the addresses of host: from Lookup if it is set, else from the
// system's resolver.
func (rt *Runtime) lookup(ctx context.Context, host string) ([]net.IP, error) {
	if rt.Lookup != nil {
		return rt.Lookup(ctx, host)
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	return ips, nil
}
