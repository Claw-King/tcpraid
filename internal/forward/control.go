package forward

// The control connections of a node, and what they are for in this version: carrying a
// connection to a peer that this node cannot dial.
//
// A node keeps one control connection to every peer it holds a path to, and takes the
// ones its peers open. A peer is "online" while at least one is up, whoever opened it.
//
// A rule whose peer has no path cannot open sub-connections. Its node creates the
// session anyway and asks the peer, over a control connection, to open them ("open",
// with a request number). The peer dials back over its own paths with openings of the
// reverse type that carry the request number; this node attaches each connection to the
// session the request stands for. From then on the session runs like any other; the
// peer, which dials, keeps the number of sub-connections up.
//
// The control connections also tell a node that a peer's process was replaced by a new
// one (the node was restarted, or killed and started again): every hello carries a
// number the process picked at its start. A new process knows nothing of the sessions
// of the old one, so this node resets the sessions it still holds with the old one
// instead of keeping them until the grace time is over.

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Claw-King/tcpraid/internal/control"
	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/link"
	"github.com/Claw-King/tcpraid/internal/node"
)

// maxControlConns is how many control connections with one peer a node keeps at the
// same time; two is the most that two well-behaved nodes have (one opened by each).
const maxControlConns = 4

// MaxOpenRequests is how many sessions a node serves at the same time because one peer
// asked it to open their sub-connections; a further request is answered with a failure.
const MaxOpenRequests = 64

// ReverseWait is how long a session waits for the first sub-connection that a peer was
// asked to open; then its application connection is reset.
const ReverseWait = 15 * time.Second

var (
	// how long to wait before dialing a control connection again, after one, two, ...
	// attempts in a row that gave no lasting connection
	controlBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	// how often the node looks for peers that have a path and no control connection
	// (a variable so that tests need not wait for it; set before any node starts)
	superviseInterval = 2 * time.Second
)

// times are the times of the control connections and of a request of one Runtime: the
// constants, unless a test set others before Start.
type times struct {
	ping    time.Duration // control.PingInterval
	timeout time.Duration // control.Timeout
	wait    time.Duration // ReverseWait
}

func (t *times) fill() {
	if t.ping == 0 {
		t.ping = control.PingInterval
	}
	if t.timeout == 0 {
		t.timeout = control.Timeout
	}
	if t.wait == 0 {
		t.wait = ReverseWait
	}
}

// PeerStatus is what the control connections say about a peer.
type PeerStatus struct {
	// Online: a control connection with the peer is up, in either direction.
	Online bool
	// Version is the program version the peer reported; empty before it has.
	Version string
	// LastMessage is when the last message from the peer arrived; zero if none has.
	LastMessage time.Time
}

// peerControl is what the runtime keeps about the control connections with one peer.
type peerControl struct {
	ctx     context.Context // ends when the pairing ends or the runtime stops
	cancel  context.CancelFunc
	conns   []*ctlConn // the control connections that are up
	dialing bool       // dialControl runs
	greeted bool       // a hello of the peer has arrived
	// dialFailed: the last attempt of this node to open a control connection to the
	// peer over its own path failed. fellBack: connections of rules are, for that
	// reason, being opened by the peer (see pathDownLocked); kept to log the change once.
	dialFailed, fellBack bool
	version              string
	last                 time.Time
	serving              int // sessions this node serves because the peer asked (see MaxOpenRequests)
	// instance is the number the peer's process said in the hello that counted last;
	// empty before one did. previous is the one before it: a hello with it is a late
	// one of the process that is gone. lastOf is when the last message arrived on a
	// connection whose hello said instance: the process was alive then.
	instance, previous string
	lastOf             time.Time
	// features is what the peer's last hello said it can do (control.FeaturePathTest).
	features []string
}

type ctlConn struct {
	*control.Conn
	peer     string // name of the peer at the other end
	dialed   bool   // this node opened it, over a path of its own
	instance string // what the hello on this connection said (rt.mu)
}

// hello notes the instance number of a hello that arrived on cc (rt.mu held). When it
// shows that the peer runs as another process than before, hello returns the time up to
// which the old process is known to have lived: the sessions with the peer that began
// before it were the old one's. Otherwise it returns the zero time: the first number
// from the peer, the same number again, no number (a version that has none), or the
// number of the process before this one.
func (pc *peerControl) hello(cc *ctlConn, instance string) (oldUntil time.Time) {
	cc.instance = instance
	switch {
	case instance == "" || instance == pc.instance || instance == pc.previous:
		return time.Time{}
	case pc.instance == "":
		pc.instance = instance
		return time.Time{}
	}
	oldUntil = pc.lastOf
	pc.previous, pc.instance = pc.instance, instance
	return oldUntil
}

// reverseKey names a request this node made: the peer it asked and the number it gave.
type reverseKey struct {
	peer    string
	request uint64
}

// reverseEntry is a session this node carries whose sub-connections a peer opens. fail
// is told why, when the peer answers that it cannot open them.
type reverseEntry struct {
	rv   *engine.Reverse
	fail func(text string)
}

// features are what this node says it can do in every hello.
var features = []string{control.FeaturePathTest, control.FeatureResolve, control.FeatureRulePaths}

// maxFeatures bounds what is kept of the features a peer names.
const maxFeatures = 16

// controlLocked returns the control state of the peer called name, made if need be
// (rt.mu held).
func (rt *Runtime) controlLocked(name string) *peerControl {
	pc := rt.ctl[name]
	if pc == nil {
		pc = &peerControl{}
		pc.ctx, pc.cancel = context.WithCancel(rt.ctx)
		rt.ctl[name] = pc
	}
	return pc
}

// superviseControl starts, now and whenever one is missing, the dialing of a control
// connection to every peer this node has a path to, until ctx is done.
func (rt *Runtime) superviseControl(ctx context.Context) {
	tick := time.NewTicker(superviseInterval)
	defer tick.Stop()
	for {
		for _, p := range rt.Node.Peers() {
			if len(p.Paths) == 0 {
				continue
			}
			rt.mu.Lock()
			if ctx.Err() == nil {
				if pc := rt.controlLocked(p.Name); !pc.dialing {
					pc.dialing = true
					go rt.dialControl(p.Name, pc)
				}
			}
			rt.mu.Unlock()
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return
		}
	}
}

// controlPath returns the path a control connection to the peer takes: the first with
// the control role, or else the first.
func controlPath(peer node.Peer) node.Path {
	for _, p := range peer.Paths {
		for _, role := range p.Roles {
			if role == node.RoleControl {
				return p
			}
		}
	}
	return peer.Paths[0]
}

// dialControl keeps a control connection to the peer called name: it dials, serves the
// connection until it is gone, and dials again after a pause that grows while no
// attempt gives a lasting connection. It ends when the pairing ends, the runtime stops,
// or the peer has no path any more.
func (rt *Runtime) dialControl(name string, pc *peerControl) {
	defer func() {
		rt.mu.Lock()
		pc.dialing = false
		rt.mu.Unlock()
	}()
	fails := 0
	for pc.ctx.Err() == nil {
		ps, err := rt.peerFor(name)
		if err != nil {
			// no path any more, or the pairing has ended: nothing is known about a path
			rt.mu.Lock()
			pc.dialFailed = false
			rt.mu.Unlock()
			return
		}
		peer := ps.peer
		// the handshake takes one of the peer's MaxHandshakes places, like any other
		conn, err := ps.dial(func(ctx context.Context) (net.Conn, error) {
			return rt.Dialer.OpenControl(ctx, peer, controlPath(peer))
		})
		// whether this node's own path works, as far as this connection shows it
		rt.mu.Lock()
		pc.dialFailed = err != nil && pc.ctx.Err() == nil
		rt.mu.Unlock()
		if err == nil {
			began := time.Now()
			rt.runControl(peer, conn, true)
			if time.Since(began) > rt.times.timeout {
				fails = 0 // it was a connection that worked
			}
		}
		wait := controlBackoff[min(fails, len(controlBackoff)-1)]
		fails++
		select {
		case <-time.After(wait):
		case <-pc.ctx.Done():
			return
		}
	}
}

// HandleControl takes a control connection that a peer opened (link.Acceptor.HandleControl).
// It returns at once.
func (rt *Runtime) HandleControl(peer node.Peer, conn net.Conn) {
	go rt.runControl(peer, conn, false)
}

// runControl serves one control connection with the peer of the record, whichever end
// opened it (dialed: this one), and returns when it is gone; it closes conn.
func (rt *Runtime) runControl(peer node.Peer, conn net.Conn, dialed bool) {
	key, err := control.Key(peer.PairSecret)
	if err != nil {
		conn.Close()
		return
	}
	cc := &ctlConn{Conn: control.NewConn(conn, key), peer: peer.Name, dialed: dialed}
	rt.mu.Lock()
	// the pairing that the record stands for may have ended since the opening was made
	// or checked: the same test as for a new session, under the lock PeerRemoved takes
	if rt.ctx == nil || rt.ctx.Err() != nil || !rt.validFor(peer)() {
		rt.mu.Unlock()
		conn.Close()
		return
	}
	pc := rt.controlLocked(peer.Name)
	if len(pc.conns) >= maxControlConns {
		rt.mu.Unlock()
		conn.Close()
		return
	}
	pc.conns = append(pc.conns, cc)
	rt.mu.Unlock()
	rt.logf("peer %q: control connection with %s is up", peer.Name, conn.RemoteAddr())
	stop := context.AfterFunc(pc.ctx, func() { cc.Close() })
	done := make(chan struct{})
	why := rt.serveControl(pc, cc, done)
	close(done)
	stop()
	cc.Close()
	rt.mu.Lock()
	for i, x := range pc.conns {
		if x == cc {
			pc.conns = append(pc.conns[:i], pc.conns[i+1:]...)
			break
		}
	}
	rt.mu.Unlock()
	rt.logf("peer %q: control connection with %s is down: %v", peer.Name, conn.RemoteAddr(), why)
}

// serveControl says hello, pings, and handles the messages of cc until it fails; it
// returns why. done is closed by the caller when it has returned.
func (rt *Runtime) serveControl(pc *peerControl, cc *ctlConn, done chan struct{}) error {
	hello := control.Message{Type: control.TypeHello, Name: rt.Node.Name(), Version: rt.Version, Protocols: []int{control.Protocol}, Instance: rt.instance}
	if !rt.noFeatures {
		hello.Features = features
		if rt.noResolve {
			hello.Features = []string{control.FeaturePathTest}
		} else if rt.noRulePaths {
			hello.Features = []string{control.FeaturePathTest, control.FeatureResolve}
		}
	}
	if err := cc.Send(hello); err != nil {
		return err
	}
	go func() {
		tick := time.NewTicker(rt.times.ping)
		defer tick.Stop()
		for seq := uint64(1); ; seq++ {
			select {
			case <-tick.C:
				if cc.Send(control.Message{Type: control.TypePing, Seq: seq}) != nil {
					cc.Close() // the reading side sees it and ends the connection
					return
				}
			case <-done:
				return
			}
		}
	}()
	for {
		m, err := cc.Receive(rt.times.timeout)
		if err != nil {
			return err
		}
		rt.mu.Lock()
		now := time.Now()
		pc.last = now
		var oldUntil time.Time
		if m.Type == control.TypeHello {
			pc.greeted = true
			pc.version = printable(m.Version, 64)
			pc.features = nil
			for _, f := range m.Features[:min(len(m.Features), maxFeatures)] {
				pc.features = append(pc.features, printable(f, 32))
			}
			oldUntil = pc.hello(cc, printable(m.Instance, 64))
		}
		if cc.instance != "" && cc.instance == pc.instance {
			pc.lastOf = now
		}
		rt.mu.Unlock()
		if !oldUntil.IsZero() {
			rt.peerRestarted(cc.peer, oldUntil)
		}
		switch m.Type {
		case control.TypePing:
			if err := cc.Send(control.Message{Type: control.TypePong, Seq: m.Seq}); err != nil {
				return err
			}
		case control.TypeOpen:
			rt.serveOpen(pc, cc, m)
		case control.TypeOpenFail:
			rt.openFailed(cc.peer, m)
		case control.TypeSessionFail:
			rt.sessionFailed(cc.peer, m)
		case control.TypePaths:
			rt.servePaths(cc, m)
		case control.TypePathsReply:
			rt.pathsAnswered(cc.peer, m)
		case control.TypeResolve:
			rt.serveResolve(cc, m)
		case control.TypeResolveReply:
			rt.resolveAnswered(cc.peer, m)
		case control.TypeTestEnd:
			if id, err := hex.DecodeString(m.Session); err == nil && len(id) == 16 {
				rt.Engine.EndTest(cc.peer, [16]byte(id))
			}
		}
		// hello and pong need nothing more; a type this version does not know is ignored
	}
}

// peerRestarted resets the sessions that this node holds with the process the peer
// called name ran as before: those that began before oldUntil. The new process does not
// know them; left alone, each would wait out the grace time with its application
// connection open.
func (rt *Runtime) peerRestarted(name string, oldUntil time.Time) {
	owners := rt.Engine.AbortPeerBefore(name, oldUntil, "the peer's node was restarted")
	if len(owners) == 0 {
		return
	}
	rt.logf("peer %q runs as a new process: %d sessions that its old process held were reset", name, len(owners))
	told := map[string]bool{}
	for _, o := range owners {
		if o.Rule != "" && !told[o.Rule] {
			told[o.Rule] = true
			rt.note(o.Rule, fmt.Sprintf("peer %q was restarted while it carried a connection of this rule, and the connection was reset", name))
		}
	}
}

// printable returns s cut to max bytes, with everything that is not printable ASCII
// replaced: the other node chose the text, and it is shown to people.
func printable(s string, max int) string {
	if len(s) > max {
		s = s[:max]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return '?'
		}
		return r
	}, s)
}

// peerCan reports whether the peer called name said in its hello that it has the
// feature. The error, for a rule's status, says what the peer does not do (lacks), from
// which version on a node does it (since), and what to do until then (meanwhile).
func (rt *Runtime) peerCan(name, feature, lacks, since, meanwhile string) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	pc := rt.ctl[name]
	switch {
	case pc == nil || len(pc.conns) == 0:
		return fmt.Errorf("peer %q is not connected to this node", name)
	case !pc.greeted:
		return fmt.Errorf("the connection with peer %q has only just come up, and what it can do is not known yet. Try again in a moment.", name)
	case !slices.Contains(pc.features, feature):
		return fmt.Errorf("peer %q %s (it runs version %q). Both nodes need tcpraid %s or later. %s", name, lacks, pc.version, since, meanwhile)
	}
	return nil
}

// pathDownLocked reports whether this node's own way to the peer called name is down
// while the peer's way to this node is up (rt.mu held): the last control connection
// this node tried to open over its path failed, none that it opened is up, and one that
// the peer opened is. Connections of rules are then opened by the peer, as for a peer
// this node has no path to.
//
// The signal is the control connection, which this node keeps dialing anyway. A path
// that stops carrying traffic without refusing connections shows only when the control
// connection on it times out and the next dial fails.
func (rt *Runtime) pathDownLocked(name string) bool {
	pc := rt.ctl[name]
	if pc == nil || !pc.dialFailed {
		return false
	}
	theirs := false
	for _, cc := range pc.conns {
		if cc.dialed {
			return false
		}
		theirs = true
	}
	return theirs
}

// peerDials reports whether a new connection of a rule to peer, which this node has
// paths to, is to be opened by the peer for now (pathDownLocked), and logs when that
// changes.
func (rt *Runtime) peerDials(peer string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	down := rt.pathDownLocked(peer)
	if pc := rt.ctl[peer]; pc != nil && pc.fellBack != down {
		pc.fellBack = down
		if down {
			rt.logf("peer %q: this node cannot open connections to it at the moment, and it is connected to this node, so it is asked to open the connections of rules", peer)
		} else {
			rt.logf("peer %q: this node opens the connections of rules itself again", peer)
		}
	}
	return down
}

// liveControl returns a control connection with the peer called name that is up, or nil.
func (rt *Runtime) liveControl(name string) *ctlConn {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if pc := rt.ctl[name]; pc != nil && len(pc.conns) > 0 {
		return pc.conns[0]
	}
	return nil
}

// PeerStatus returns what the control connections say about the peer called name.
func (rt *Runtime) PeerStatus(name string) PeerStatus {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	pc := rt.ctl[name]
	if pc == nil {
		return PeerStatus{}
	}
	return PeerStatus{Online: len(pc.conns) > 0, Version: pc.version, LastMessage: pc.last}
}

// dropControlLocked ends everything about the control connections with the peer called name
// (rt.mu held): its connections close, its dialing stops.
func (rt *Runtime) dropControlLocked(name string) {
	if pc := rt.ctl[name]; pc != nil {
		pc.cancel()
		delete(rt.ctl, name)
	}
}

// carryReverse takes one application connection of a rule to ip:port at a peer that
// this node has no path to, or whose path is down for the moment (pathDownLocked): it
// asks the peer to open the sub-connections. It returns when the session has ended.
func (rt *Runtime) carryReverse(a *attempt, r node.Rule, peer node.Peer, c net.Conn, ip net.IP, port int) {
	cc := rt.liveControl(peer.Name)
	if cc == nil && len(peer.Paths) > 0 {
		// the peer went away between the choice of this way and now
		a.fail(fmt.Sprintf("this node cannot open connections to peer %q at the moment, and the peer, which would open them, is no longer connected to this node.", peer.Name))
		reset(c)
		return
	}
	if cc == nil {
		a.fail(fmt.Sprintf("this node has no path to peer %q, and the peer, which would open the connections, is not connected to this node. Start the peer, or add a path with \"tcpraid peers path set\".", peer.Name))
		reset(c)
		return
	}
	var ask control.Message
	if r.Narrowed() {
		// the paths are the peer's: it is the one to keep to the rule's choice
		if err := rt.peerCan(peer.Name, control.FeatureRulePaths, "does not keep a rule to the paths it names", "0.5.0",
			"Until then, take the paths and the number of sub-connections off the rule."); err != nil {
			a.fail(err.Error())
			reset(c)
			return
		}
		ask.RulePaths, ask.RuleSubs = r.Paths, r.Subs
	}
	keys, err := pairKeys(peer)
	if err != nil {
		a.fail(err.Error())
		reset(c)
		return
	}
	end, ok := rt.runReverse(cc, peer, keys, c, ip, port, engine.Owner{Peer: peer.Name, Rule: r.Name}, ask, a.fail)
	if ok {
		rt.ended(a, peer.Name, net.JoinHostPort(ip.String(), strconv.Itoa(port)), end)
	}
}

// runReverse carries c to ip:port at a peer that this node cannot dial, as one session
// whose sub-connections the peer is asked to open over cc, and returns when the session
// has ended. ask holds what the request says beyond its number and the session. fail is
// told why the peer did not open them, if it did not. ok is false when no session was
// created (c is reset then).
func (rt *Runtime) runReverse(cc *ctlConn, peer node.Peer, keys engine.Keys, c net.Conn, ip net.IP, port int, owner engine.Owner,
	ask control.Message, fail func(text string)) (end engine.End, ok bool) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		reset(c)
		return end, false
	}
	key := reverseKey{peer.Name, binary.BigEndian.Uint64(b[:])}
	rv := rt.Engine.NewReverse(c, ip, port, keys, peer.UDPDial, owner, rt.validFor(peer))
	if rv == nil {
		return end, false // refused and reset: the session limit, or the node is stopping
	}
	rt.mu.Lock()
	rt.reverse[key] = &reverseEntry{rv: rv, fail: fail}
	rt.mu.Unlock()
	defer func() {
		rt.mu.Lock()
		delete(rt.reverse, key)
		rt.mu.Unlock()
	}()
	id := rv.ID()
	ask.Type, ask.Request, ask.Session = control.TypeOpen, key.request, hex.EncodeToString(id[:])
	if err := cc.Send(ask); err != nil {
		fail(fmt.Sprintf("peer %q could not be asked to open the connections: %v", peer.Name, err))
		rv.Abort("the request for sub-connections could not be sent")
		return rv.Run(), true
	}
	late := time.AfterFunc(rt.times.wait, func() {
		if !rv.Up() {
			fail(fmt.Sprintf("peer %q was asked to open the connections and none arrived within %v", peer.Name, rt.times.wait))
			rv.Abort("no sub-connection arrived")
		}
	})
	defer late.Stop()
	return rv.Run(), true
}

// HandleReverse takes a connection that a peer opened because this node asked it to
// (link.Acceptor.HandleReverse): it becomes a sub-connection of the session that the
// request stands for. A request this node did not make to that peer, or whose session
// has ended, closes the connection.
func (rt *Runtime) HandleReverse(peer node.Peer, request uint64, path, role byte, conn net.Conn) {
	rt.mu.Lock()
	ent := rt.reverse[reverseKey{peer.Name, request}]
	rt.mu.Unlock()
	if ent == nil {
		conn.Close()
		return
	}
	ent.rv.Attach(conn, path, role == link.RoleControl, role == link.RoleRepair)
}

// openFailed handles the answer of a peer that could not open the sub-connections this
// node asked for.
func (rt *Runtime) openFailed(peerName string, m control.Message) {
	rt.mu.Lock()
	ent := rt.reverse[reverseKey{peerName, m.Request}]
	rt.mu.Unlock()
	if ent == nil {
		return
	}
	ent.fail(fmt.Sprintf("peer %q could not open the connections: %s", peerName, printable(m.Reason, maxReason)))
	go ent.rv.Abort("the other end could not open sub-connections")
}

// serveOpen handles a peer's request to open the sub-connections of a session it
// carries: this node dials the peer over its own paths, and the transport takes it from
// there. Where it dials is this node's business alone; the request names no address.
func (rt *Runtime) serveOpen(pc *peerControl, cc *ctlConn, m control.Message) {
	fail := func(reason string) {
		cc.Send(control.Message{Type: control.TypeOpenFail, Request: m.Request, Reason: reason})
	}
	ps, err := rt.peerFor(cc.peer)
	if err != nil {
		// no path to the peer, or the pairing has just ended
		fail("the node that was asked has no path to the node that asked")
		return
	}
	rt.mu.Lock()
	if pc.serving >= MaxOpenRequests {
		rt.mu.Unlock()
		fail("the node that was asked serves too many requests already")
		return
	}
	pc.serving++
	rt.mu.Unlock()
	go func() {
		defer func() {
			rt.mu.Lock()
			pc.serving--
			rt.mu.Unlock()
		}()
		peer := ps.peer
		keys, err := pairKeys(peer)
		if err != nil {
			return
		}
		// a stored allow list has passed this parse
		allow, err := engine.ParseAllow(peer.Allow)
		if err != nil {
			return
		}
		groups := rt.reverseGroups(ps, peer, m.Request)
		if m.TestPath != 0 {
			// a path test of the peer: one path, with as many data sub-connections as it asks for
			if groups = testGroups(groups, m.TestPath-1, m.TestSubs); groups == nil {
				fail("it has no such path for data")
				return
			}
		} else if len(m.RulePaths) > 0 || m.RuleSubs != 0 {
			// a rule of the peer that does not use everything; what it names is checked
			// like anything a peer sends
			if len(m.RulePaths) > node.MaxPaths || m.RuleSubs < 0 || m.RuleSubs > node.MaxSubs {
				fail("the request names more paths or sub-connections than a peer has")
				return
			}
			var missing string
			if groups, missing = narrow(groups, m.RulePaths, m.RuleSubs); groups == nil {
				if missing == "" {
					fail("it has no path that carries data")
				} else {
					fail(fmt.Sprintf("it has no path called %q that carries data", printable(missing, node.MaxPathNameLen)))
				}
				return
			}
		}
		first := 0
		for i, g := range groups {
			if !g.Ctrl && !g.Repair {
				first = i
				break
			}
		}
		conn, err := groups[first].Dial()
		if err != nil {
			rt.logf("peer %q asked for sub-connections (session %s) and could not be reached: %v", peer.Name, printable(m.Session, 32), err)
			fail("the node that was asked could not connect to the node that asked")
			return
		}
		rt.Engine.NewRoute(groups, keys, peer.UDPDial).ServeReverse(conn, engine.SubOptions{
			Peer:       peer.Name,
			Allow:      allow,
			AllowName:  "the allow list of the peer",
			Valid:      ps.valid,
			Sink:       rt.sink,
			DialFailed: rt.tellFailure(peer.Name),
		})
	}()
}

// reverseGroups returns the dial groups for a session that the peer carries and asked
// this node, with the request, to open the sub-connections of: like groups, but each
// connection starts with an opening of the reverse type that says the request, the
// index of the path and the role.
func (rt *Runtime) reverseGroups(ps *peerState, peer node.Peer, request uint64) []engine.DialGroup {
	var gs []engine.DialGroup
	for i, path := range peer.Paths {
		for _, role := range path.Roles {
			g := engine.DialGroup{Path: uint8(i), Name: path.Name, Want: path.Subs}
			code := byte(link.RoleData)
			switch role {
			case node.RoleControl:
				g.Ctrl, g.Want, code = true, 1, link.RoleControl
			case node.RoleRepair:
				g.Repair, code = true, link.RoleRepair
			}
			g.Dial = func() (net.Conn, error) {
				return ps.dial(func(ctx context.Context) (net.Conn, error) {
					return rt.Dialer.OpenReverse(ctx, peer, path, request, uint8(i), code)
				})
			}
			gs = append(gs, g)
		}
	}
	return gs
}
