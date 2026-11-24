// Package forward runs the forwarding rules of a node: it listens where the rules say,
// carries each connection that arrives to the rule's peer over the transport, and, at
// the other end, takes the sub-connections a peer opens and connects their sessions to
// the destinations.
//
// A fixed rule has listeners of its own, one per port of its range. Transparent rules
// share the node's transparent listener, if it has one: connections that the host
// redirected there are matched against the transparent rules in their order.
//
// Rules can change while the node runs (Apply): only the listeners of the rules that
// changed are closed or opened. A session lives on when its rule is changed, switched
// off or removed; it ends when the pairing with its peer is ended (PeerRemoved).
//
// A connection goes over the paths that this node holds to the rule's peer. Where it
// holds none, it asks the peer, over the control connection of the two nodes, to open
// the sub-connections from its end (control.go). A rule whose peer can be reached
// neither way resets its connections and says why in its status.
//
// A rule's status also says what went wrong with the last connection it could not
// carry, and when (status.go): the reasons this node knows itself, and the one the peer
// reports when it could not connect the session to its destination.
package forward

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/link"
	"github.com/Claw-King/tcpraid/internal/node"
)

const (
	// MaxHandshakes is how many connections to one peer may be in their handshake at
	// the same time; further dials wait. A peer's listener keeps only a bounded number
	// of connections that have not yet proved who they are, shared among everyone who
	// connects to it, so a node does not open all its sub-connections at once.
	MaxHandshakes = 32
	// DialTimeout bounds one dial of a sub-connection: connecting, the handshake of the
	// path's tier and the opening.
	DialTimeout = 30 * time.Second
)

// Runtime runs the rules of one node. Set the exported fields, then call Start.
type Runtime struct {
	Node   *node.Node
	Engine *engine.Engine
	Dialer *link.Dialer
	// Version is the program version this node reports to its peers.
	Version string
	// Logf logs a line about a rule or a peer: started, stopped, failed. nil: nothing
	// is logged.
	Logf func(format string, args ...any)

	mu          sync.Mutex
	ctx         context.Context
	running     map[string]*runningRule        // fixed rules that listen, by name
	problems    map[string]problem             // rule name -> why it does not listen, or what went wrong last
	seq         uint64                         // the number of the last connection or event a problem can be about
	logged      map[string]string              // rule name -> the failure logged last, so that a repeated one is logged once
	dialErr     map[string]dialTrouble         // peer name -> the last dial of a sub-connection that failed
	details     map[failKey]failDetail         // what peers said about sessions that have not ended here yet
	recent      map[failKey]recentEnd          // sessions that ended here before their peer said why
	testing     bool                           // a path test runs
	sinks       int                            // sessions of path tests that end in this node
	pathsAsked  map[reverseKey]chan []pathSpec // requests for a peer's paths that wait for the answer
	peers       map[string]*peerState          // by peer name
	ctl         map[string]*peerControl        // control connections, by peer name
	reverse     map[reverseKey]*reverseEntry   // sessions whose sub-connections a peer was asked to open
	transparent bool                           // ServeTransparent runs
	times       times                          // set before Start, read after
	// instance is the number this process says in every hello: picked at Start, so it
	// is another one after a restart of the node. Empty: none is said (tests set
	// noInstance before Start to be a version that has none).
	instance   string
	noInstance bool
	// noFeatures: the hello names no features (tests set it before Start to be a
	// version before 0.3.0).
	noFeatures bool
}

// runningRule is a fixed rule with its listeners.
type runningRule struct {
	rule node.Rule
	lns  []net.Listener
}

// peerState is what the runtime keeps per peer it carries connections to.
type peerState struct {
	ctx    context.Context // ends when the pairing ends or the runtime stops
	cancel context.CancelFunc
	sem    chan struct{} // MaxHandshakes places
	peer   node.Peer     // the record that route was made from
	route  *engine.Route
	valid  func() bool
}

// RuleStatus is a rule and how it is doing.
type RuleStatus struct {
	Rule node.Rule
	// Listening: connections for the rule are being taken (a fixed rule has its
	// listeners; a transparent rule is enabled and the node has a transparent listener).
	Listening bool
	// Error says why the rule does not listen, or what went wrong with the last
	// connection it could not carry; empty when there is nothing to report. ErrorAt is
	// when that happened; zero where Error is a state and not an event.
	Error   string
	ErrorAt time.Time
	// Sessions is the number of live sessions the rule carries.
	Sessions int
}

func (rt *Runtime) logf(format string, args ...any) {
	if rt.Logf != nil {
		rt.Logf(format, args...)
	}
}

// Start begins to run the enabled rules and returns; the rules stop when ctx is done.
// A rule that cannot listen is reported in its status and in the log, and does not keep
// the others from starting.
func (rt *Runtime) Start(ctx context.Context) {
	rt.times.fill()
	rt.mu.Lock()
	rt.ctx = ctx
	if !rt.noInstance {
		var b [8]byte
		if _, err := rand.Read(b[:]); err == nil {
			rt.instance = hex.EncodeToString(b[:])
		}
	}
	rt.running = map[string]*runningRule{}
	rt.problems = map[string]problem{}
	rt.logged = map[string]string{}
	rt.dialErr = map[string]dialTrouble{}
	rt.details = map[failKey]failDetail{}
	rt.recent = map[failKey]recentEnd{}
	rt.pathsAsked = map[reverseKey]chan []pathSpec{}
	rt.peers = map[string]*peerState{}
	rt.ctl = map[string]*peerControl{}
	rt.reverse = map[reverseKey]*reverseEntry{}
	rt.mu.Unlock()
	go rt.superviseControl(ctx)
	context.AfterFunc(ctx, func() {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		for name, rr := range rt.running {
			rr.close()
			delete(rt.running, name)
		}
	})
	rt.Apply()
}

func (rr *runningRule) close() {
	for _, ln := range rr.lns {
		ln.Close()
	}
}

// Apply brings the listeners in line with the rules the node has now: a fixed rule that
// is gone, switched off or changed loses its listeners, one that is new, switched on or
// changed gets them. Rules that did not change are not touched. A rule that could not
// listen before is tried again.
func (rt *Runtime) Apply() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.ctx == nil || rt.ctx.Err() != nil {
		return
	}
	rules := rt.Node.Rules()
	want := map[string]node.Rule{}
	known := map[string]bool{}
	for _, r := range rules {
		known[r.Name] = true
		if r.Enabled && !r.Transparent() {
			want[r.Name] = r
		}
	}
	for name, rr := range rt.running {
		if r, ok := want[name]; !ok || r != rr.rule {
			rr.close()
			delete(rt.running, name)
			rt.logf("rule %q: stopped listening on %s", name, rr.rule.Listen)
		}
	}
	for name := range rt.problems {
		if _, runs := rt.running[name]; !runs || !known[name] {
			delete(rt.problems, name)
		}
	}
	for name := range rt.logged {
		if !known[name] {
			delete(rt.logged, name)
		}
	}
	for _, r := range rules { // in the order of the list, so that the log reads in order
		if _, ok := want[r.Name]; !ok {
			continue
		}
		if _, runs := rt.running[r.Name]; runs {
			continue
		}
		rr, err := rt.listen(r)
		if err != nil {
			rt.problems[r.Name] = problem{text: err.Error()}
			rt.logf("rule %q does not listen: %v", r.Name, err)
			continue
		}
		rt.running[r.Name] = rr
		rt.logf("rule %q: listening on %s for peer %q, destination %s", r.Name, r.Listen, r.Peer, r.Destination)
	}
}

// listen opens every listener of a fixed rule, or none.
func (rt *Runtime) listen(r node.Rule) (*runningRule, error) {
	from, to, err := r.Ranges()
	if err != nil {
		return nil, err
	}
	dst := net.ParseIP(to.Host)
	rr := &runningRule{rule: r}
	for i := 0; i < from.Ports(); i++ {
		ln, err := net.Listen("tcp", from.Addr(i))
		if err != nil {
			rr.close()
			return nil, fmt.Errorf("listening on %s failed: %w", from.Addr(i), err)
		}
		rr.lns = append(rr.lns, ln)
	}
	for i, ln := range rr.lns {
		go rt.accept(ln, func(c net.Conn) { rt.carry(r, c, dst, to.First+i) })
	}
	return rr, nil
}

// accept hands the connections of a listener to handle until the listener is closed.
func (rt *Runtime) accept(ln net.Listener, handle func(net.Conn)) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// out of file descriptors, most likely: it passes
			time.Sleep(50 * time.Millisecond)
			continue
		}
		// an application connection: whatever ends this process resets it, unless
		// its session has ended cleanly by then
		engine.ArmReset(c)
		go handle(c)
	}
}

// ServeTransparent takes connections from ln, the node's transparent listener, until
// ctx is done, then closes ln and returns nil. Each connection is matched, by the
// address it was going to before the host redirected it, against the enabled
// transparent rules in their order; one that no rule matches is reset.
func (rt *Runtime) ServeTransparent(ctx context.Context, ln net.Listener) error {
	rt.mu.Lock()
	rt.transparent = true
	rt.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	rt.accept(ln, func(c net.Conn) {
		ip, port, err := engine.OriginalDst(c)
		if err != nil {
			reset(c)
			return
		}
		if r, ok := MatchTransparent(rt.Node.Rules(), ip, port); ok {
			rt.carry(r, c, ip, port)
			return
		}
		reset(c)
	})
	if ctx.Err() == nil {
		return errors.New("the transparent listener was closed")
	}
	return nil
}

// MatchTransparent returns the first enabled transparent rule, in the order of rules,
// whose match holds ip:port.
func MatchTransparent(rules []node.Rule, ip net.IP, port int) (node.Rule, bool) {
	for _, r := range rules {
		if !r.Enabled || !r.Transparent() {
			continue
		}
		// a stored rule has passed this parse
		if m, err := engine.ParseAllow(r.Match); err == nil && m.Match(ip, port) {
			return r, true
		}
	}
	return node.Rule{}, false
}

// reset closes an application connection with a reset: a clean close would read as the
// complete end of a stream that was never carried.
func reset(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetLinger(0)
	}
	c.Close()
}

// carry takes one application connection of a rule to ip:port at the rule's peer. It
// returns when the session has ended.
func (rt *Runtime) carry(r node.Rule, c net.Conn, ip net.IP, port int) {
	a := rt.begin(r.Name)
	if peer, ok := rt.Node.Peer(r.Peer); ok && len(peer.Paths) == 0 {
		// no way to dial the peer: it is asked to dial
		rt.carryReverse(a, r, peer, c, ip, port)
		return
	}
	ps, err := rt.peerFor(r.Peer)
	if err != nil {
		a.fail(err.Error())
		reset(c)
		return
	}
	if end, ok := ps.route.Carry(c, ip, port, engine.Owner{Peer: r.Peer, Rule: r.Name}, ps.valid); ok {
		rt.ended(a, r.Peer, net.JoinHostPort(ip.String(), strconv.Itoa(port)), end)
	}
}

// peerFor returns the state for carrying connections to the peer called name, made
// from the peer's record as it is now.
func (rt *Runtime) peerFor(name string) (*peerState, error) {
	peer, ok := rt.Node.Peer(name)
	if !ok {
		return nil, fmt.Errorf("peer %q is not paired with this node", name)
	}
	if len(peer.Paths) == 0 {
		return nil, fmt.Errorf("this node has no path to peer %q", name)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.ctx == nil || rt.ctx.Err() != nil {
		return nil, errors.New("the node is stopping")
	}
	ps := rt.peers[name]
	if ps != nil && reflect.DeepEqual(ps.peer, peer) {
		return ps, nil
	}
	keys, err := pairKeys(peer)
	if err != nil {
		return nil, err
	}
	next := &peerState{peer: peer}
	if ps != nil {
		// the same pairing, with other paths or settings: dials of sessions that run
		// keep their place in the same queue
		next.ctx, next.cancel, next.sem = ps.ctx, ps.cancel, ps.sem
	} else {
		next.ctx, next.cancel = context.WithCancel(rt.ctx)
		next.sem = make(chan struct{}, MaxHandshakes)
	}
	next.valid = rt.validFor(peer)
	next.route = rt.Engine.NewRoute(rt.groups(next, peer), keys, peer.UDPDial)
	rt.peers[name] = next
	return next, nil
}

// pairKeys returns the transport keys of the pairing with peer.
func pairKeys(peer node.Peer) (engine.Keys, error) {
	material, err := link.TransportKey(peer.PairSecret)
	if err != nil {
		return engine.Keys{}, err
	}
	return engine.NewKeys(material), nil
}

// validFor returns the check that a session of the pairing that the record stands for
// may still be created: the peer is still there, with the same pair secret.
func (rt *Runtime) validFor(peer node.Peer) func() bool {
	return func() bool {
		now, ok := rt.Node.PeerByFingerprint(peer.Fingerprint)
		return ok && bytes.Equal(now.PairSecret, peer.PairSecret)
	}
}

// groups returns the dial groups of a peer: for each path, one group per role. A data
// or repair role keeps the path's number of sub-connections; a control role keeps one.
func (rt *Runtime) groups(ps *peerState, peer node.Peer) []engine.DialGroup {
	var gs []engine.DialGroup
	for i, path := range peer.Paths {
		dial := func() (net.Conn, error) {
			conn, err := ps.dial(func(ctx context.Context) (net.Conn, error) { return rt.Dialer.Open(ctx, peer, path) })
			rt.dialed(ps, peer.Name, err)
			return conn, err
		}
		for _, role := range path.Roles {
			g := engine.DialGroup{Path: uint8(i), Name: path.Name, Want: path.Subs, Dial: dial}
			switch role {
			case node.RoleControl:
				g.Ctrl, g.Want = true, 1
			case node.RoleRepair:
				g.Repair = true
			}
			gs = append(gs, g)
		}
	}
	return gs
}

// dial runs open, which dials the peer and sends an opening, in one of the peer's
// MaxHandshakes places and within DialTimeout.
func (ps *peerState) dial(open func(ctx context.Context) (net.Conn, error)) (net.Conn, error) {
	// the place is held from here until the opening is sent
	select {
	case ps.sem <- struct{}{}:
	case <-ps.ctx.Done():
		return nil, ps.ctx.Err()
	}
	defer func() { <-ps.sem }()
	ctx, cancel := context.WithTimeout(ps.ctx, DialTimeout)
	defer cancel()
	return open(ctx)
}

// HandleSub takes a sub-connection that a peer opened (link.Acceptor.HandleSub): the
// transport reads its header and connects the session to its destination, within the
// peer's allow list if it has one. It returns at once.
func (rt *Runtime) HandleSub(peer node.Peer, conn net.Conn) {
	keys, err := pairKeys(peer)
	if err != nil {
		conn.Close()
		return
	}
	// a stored allow list has passed this parse
	allow, err := engine.ParseAllow(peer.Allow)
	if err != nil {
		conn.Close()
		return
	}
	go rt.Engine.ServeSub(conn, engine.SubOptions{
		Keys:       keys,
		Peer:       peer.Name,
		Allow:      allow,
		AllowName:  "the allow list of the peer",
		UDPDial:    peer.UDPDial,
		Valid:      rt.validFor(peer),
		Sink:       rt.sink,
		DialFailed: rt.tellFailure(peer.Name),
	})
}

// PeerRemoved ends everything that runs with the peer called name: dials in progress
// are given up, its control connections are closed and its sessions, in both
// directions, are reset. Call it after the peer
// was removed from the node's state.
func (rt *Runtime) PeerRemoved(name string) {
	rt.mu.Lock()
	if ps := rt.peers[name]; ps != nil {
		ps.cancel()
		delete(rt.peers, name)
	}
	rt.dropControlLocked(name)
	delete(rt.dialErr, name)
	rt.mu.Unlock()
	if n := rt.Engine.AbortPeer(name); n > 0 {
		rt.logf("peer %q removed: %d sessions reset", name, n)
	}
}

// Rules returns the node's rules, in their order, each with its status.
func (rt *Runtime) Rules() []RuleStatus {
	sessions := map[string]int{}
	// rules with a session that the peer has not answered on any sub-connection yet,
	// and how the last sub-connection it did not answer ended, if one has
	waiting := map[string]string{}
	for _, si := range rt.Engine.SessionList() {
		if si.Client && si.Rule != "" {
			sessions[si.Rule]++
			if !si.Up && waiting[si.Rule] == "" {
				waiting[si.Rule] = noReply(si.NoReply)
			}
		}
	}
	rules := rt.Node.Rules()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]RuleStatus, 0, len(rules))
	for _, r := range rules {
		p := rt.problems[r.Name]
		st := RuleStatus{Rule: r, Sessions: sessions[r.Name], Error: p.text, ErrorAt: p.at}
		switch {
		case !r.Enabled:
			st.Error, st.ErrorAt = "", time.Time{}
		case r.Transparent():
			st.Listening = rt.transparent
			if !rt.transparent {
				st.Error, st.ErrorAt = "the node has no transparent listener. Start it with \"tcpraid run -transparent ADDR\".", time.Time{}
			}
		default:
			_, st.Listening = rt.running[r.Name]
		}
		// a connection of the rule waits for its first sub-connection, and this node
		// knows what the attempts run into: said while it lasts
		if why, waits := waiting[r.Name]; waits && r.Enabled && st.Error == "" {
			if d, ok := rt.dialErr[r.Peer]; ok && time.Since(d.at) < dialTroubleAge {
				st.Error = fmt.Sprintf("the connections to peer %q cannot be opened: %s", r.Peer, d.text)
			} else if why != "" {
				st.Error = fmt.Sprintf("peer %q does not answer on the connections this node opens: %s", r.Peer, why)
			}
		}
		out = append(out, st)
	}
	return out
}

// AddRule stores a new rule and starts it if it is enabled.
func (rt *Runtime) AddRule(r node.Rule) error {
	if err := rt.Node.AddRule(r); err != nil {
		return err
	}
	rt.logf("rule %q added", r.Name)
	rt.Apply()
	return nil
}

// ReplaceRule puts r in the place of the rule called name and applies the change.
func (rt *Runtime) ReplaceRule(name string, r node.Rule) error {
	if err := rt.Node.ReplaceRule(name, r); err != nil {
		return err
	}
	rt.logf("rule %q changed", name)
	rt.Apply()
	return nil
}

// RemoveRule forgets the rule called name and closes its listeners. Its sessions run
// to their end.
func (rt *Runtime) RemoveRule(name string) error {
	if err := rt.Node.RemoveRule(name); err != nil {
		return err
	}
	rt.logf("rule %q removed", name)
	rt.Apply()
	return nil
}
