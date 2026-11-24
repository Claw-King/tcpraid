package forward

import (
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/control"
	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/node"
)

// gate passes connections on to a target, like a forwarding host, until it is shut:
// then the connections it carries are cut and new ones are closed at once, as on a
// path that is down. It can be opened again.
type gate struct {
	addr string

	mu    sync.Mutex
	shut  bool
	conns map[net.Conn]bool
}

func startGate(t *testing.T, target string) *gate {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	g := &gate{addr: ln.Addr().String(), conns: map[net.Conn]bool{}}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				if !g.add(c) {
					return
				}
				defer g.remove(c)
				up, err := net.Dial("tcp", target)
				if err != nil || !g.add(up) {
					return
				}
				defer g.remove(up)
				defer up.Close()
				go func() { io.Copy(c, up); c.Close() }()
				io.Copy(up, c)
			}()
		}
	}()
	return g
}

func (g *gate) add(c net.Conn) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.shut {
		c.Close()
		return false
	}
	g.conns[c] = true
	return true
}

func (g *gate) remove(c net.Conn) {
	g.mu.Lock()
	delete(g.conns, c)
	g.mu.Unlock()
}

// set shuts the gate or opens it.
func (g *gate) set(shut bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.shut = shut
	if shut {
		for c := range g.conns {
			c.Close()
		}
	}
}

// eachWayGated is eachWay with alpha's path to beta through a gate, which it returns.
func eachWayGated(t *testing.T, configure ...func(*Runtime)) (a, b *testNode, g *gate) {
	t.Helper()
	a, b = startNode(t, "alpha", configure...), startNode(t, "beta", configure...)
	pairNodes(t, a, b, 2) // beta gets a path to alpha
	g = startGate(t, b.addr)
	up := node.Path{Name: "default", Type: "tcp", Addr: g.addr, Security: node.SecurityTLS, Roles: []string{node.RoleData}, Subs: 2}
	if _, err := a.svc.SetPeerPaths("beta", []node.Path{up}); err != nil {
		t.Fatal(err)
	}
	helloed(t, a, b)
	eventually(t, "each node has opened a control connection to the other", func() bool {
		own, theirs := dialedControl(a, "beta")
		return own == 1 && theirs == 1
	})
	return a, b, g
}

// session returns the one session the node holds, once it has as many live
// sub-connections as want, of which it opened dialed itself.
func session(t *testing.T, tn *testNode, want, dialed int) engine.SessionInfo {
	t.Helper()
	var si engine.SessionInfo
	for deadline := time.Now().Add(40 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		list := tn.e.SessionList()
		if len(list) == 1 && list[0].Up && list[0].Subs == want && list[0].Dialed == dialed {
			return list[0]
		}
		if len(list) == 1 {
			si = list[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s has no session with %d sub-connections of which it opened %d: it has %d sessions, the last seen with %d and %d\n%s",
				tn.n.Name(), want, dialed, len(list), si.Subs, si.Dialed, tn.logs)
		}
	}
}

// Two nodes with a path each to the other, and a rule for every value of dial: the
// sub-connections of its session are opened by the node the rule says, and the bytes
// arrive unchanged.
func TestRuleSaysWhoDials(t *testing.T) {
	a, b, _ := eachWayGated(t)
	dst, got := sink(t)
	for _, c := range []struct {
		dial                 string
		subs, own, wantAsked int
	}{
		{"", 2, 2, 0},
		{node.DialSelf, 2, 2, 0},
		{node.DialPeer, 2, 0, 1},
		{node.DialBoth, 4, 2, 1},
	} {
		t.Run("dial="+c.dial, func(t *testing.T) {
			in := freePorts(t, 1)
			if err := a.rt.AddRule(node.Rule{Name: "r", Listen: loop(in), Destination: loop(dst), Peer: "beta", Enabled: true, Dial: c.dial}); err != nil {
				t.Fatal(err)
			}
			defer a.rt.RemoveRule("r")
			h := hold(t, loop(in), 1<<20)
			session(t, a, c.subs, c.own)
			session(t, b, c.subs, c.subs-c.own)
			if n := asked(a); n != c.wantAsked {
				t.Errorf("alpha asked its peer for the connections of %d sessions, want %d", n, c.wantAsked)
			}
			if st := statusOf(t, a, "r"); st.PeerDials {
				t.Error("the rule says that the peer dials because a path is down")
			}
			if err := h.finish(); err != nil {
				t.Fatalf("the sending side ended with %v\nalpha:\n%s\nbeta:\n%s", err, a.logs, b.logs)
			}
			if arr := wait(t, got); arr.n != 1<<20 || arr.sum != h.hash || arr.err != nil {
				t.Fatalf("%d bytes arrived (%v), or they differ from what was sent", arr.n, arr.err)
			}
			eventually(t, "the session has ended on both nodes", func() bool { return len(a.e.SessionList())+len(b.e.SessionList()) == 0 })
		})
	}
}

// A session that both nodes opened sub-connections of goes on over those of one node
// while the path of the other is down, and gets the other node's back when the path is.
func TestBothGoesOnOverOneNodesConnections(t *testing.T) {
	a, b, g := eachWayGated(t)
	dst, got := sink(t)
	in := freePorts(t, 1)
	if err := a.rt.AddRule(node.Rule{Name: "r", Listen: loop(in), Destination: loop(dst), Peer: "beta", Enabled: true, Dial: node.DialBoth}); err != nil {
		t.Fatal(err)
	}
	// alpha's path goes down under a session, which ends over beta's connections
	h := hold(t, loop(in), 1<<20)
	session(t, a, 4, 2)
	g.set(true)
	session(t, a, 2, 0)
	if err := h.finish(); err != nil {
		t.Fatalf("the session whose own path went down: %v\nalpha:\n%s\nbeta:\n%s", err, a.logs, b.logs)
	}
	if arr := wait(t, got); arr.n != 1<<20 || arr.sum != h.hash || arr.err != nil {
		t.Fatalf("%d bytes arrived (%v), or they differ from what was sent", arr.n, arr.err)
	}
	eventually(t, "the session has ended on both nodes", func() bool { return len(a.e.SessionList())+len(b.e.SessionList()) == 0 })

	// a session that begins while alpha's path is down has beta's connections only,
	// and alpha's too once the path is back
	h = hold(t, loop(in), 1<<20)
	session(t, a, 2, 0)
	g.set(false)
	session(t, a, 4, 2)
	session(t, b, 4, 2)
	if err := h.finish(); err != nil {
		t.Fatalf("the session that began while the path was down: %v\nalpha:\n%s\nbeta:\n%s", err, a.logs, b.logs)
	}
	if arr := wait(t, got); arr.n != 1<<20 || arr.sum != h.hash || arr.err != nil {
		t.Fatalf("%d bytes arrived (%v), or they differ from what was sent", arr.n, arr.err)
	}
}

// A session that both nodes dial for, begun while the PEER cannot open its share: this
// node's connections carry it, the peer is asked again, and its connections join when
// its path is back.
func TestBothAsksThePeerAgain(t *testing.T) {
	short := func(rt *Runtime) { rt.times.reask = 300 * time.Millisecond }
	// here beta's path to alpha is the one behind the gate
	b, a, g := eachWayGated(t, short) // b is "alpha" of eachWayGated: the node whose path is gated
	dst, got := sink(t)
	in := freePorts(t, 1)
	if err := a.rt.AddRule(node.Rule{Name: "r", Listen: loop(in), Destination: loop(dst), Peer: b.n.Name(), Enabled: true, Dial: node.DialBoth}); err != nil {
		t.Fatal(err)
	}
	g.set(true)
	h := hold(t, loop(in), 1<<20)
	session(t, a, 2, 2)
	eventually(t, "the log says that the peer could not open its share", func() bool {
		return strings.Contains(a.logs.String(), "could not open its share of the connections of a session")
	})
	if st := statusOf(t, a, "r"); st.Error != "" {
		t.Errorf("the rule reports a failure although the session runs: %q", st.Error)
	}
	g.set(false)
	session(t, a, 4, 2)
	if n := strings.Count(a.logs.String(), "could not open its share of the connections of a session"); n != 1 {
		t.Errorf("that the peer could not open its share is logged %d times, want once", n)
	}
	if err := h.finish(); err != nil {
		t.Fatalf("the session: %v\n%s", err, a.logs)
	}
	if arr := wait(t, got); arr.n != 1<<20 || arr.sum != h.hash || arr.err != nil {
		t.Fatalf("%d bytes arrived (%v), or they differ from what was sent", arr.n, arr.err)
	}
}

// What a rule with dial needs and does not have is said in the rule's status, and the
// connection is reset: nothing is done another way without saying so.
func TestDialNeedsWhatItNames(t *testing.T) {
	dst, _ := sink(t)
	for name, c := range map[string]struct {
		dial      string
		rule      string // the node the rule is on: "holder" has the only path, "bare" has none
		configure func(*Runtime)
		want      string
	}{
		"self without a path":       {node.DialSelf, "bare", nil, `the rule lets only this node open the connections (dial self), and this node has no path to peer "alpha"`},
		"both without an own path":  {node.DialBoth, "bare", nil, `which needs a path on each node, and this node has no path to peer "alpha"`},
		"both without a peers path": {node.DialBoth, "holder", nil, `which needs a path on each node, and peer "beta" answered: the node that was asked has no path to the node that asked`},
		"peer without a peers path": {node.DialPeer, "holder", nil, `peer "beta" could not open the connections: the node that was asked has no path to the node that asked`},
		"both with an older peer": {node.DialBoth, "holder", func(rt *Runtime) { rt.noDialBoth = rt.Node.Name() == "beta" },
			`peer "beta" does not open connections of a session that the other node opens connections of too (it runs version "test-beta"). Both nodes need tcpraid 0.6.0 or later.`},
	} {
		t.Run(name, func(t *testing.T) {
			var cfg []func(*Runtime)
			if c.configure != nil {
				cfg = append(cfg, c.configure)
			}
			holder, bare := startNode(t, "alpha", cfg...), startNode(t, "beta", cfg...)
			pairNodes(t, bare, holder, 2) // alpha holds the path, beta has none
			helloed(t, holder, bare)
			on, peer := holder, "beta"
			if c.rule == "bare" {
				on, peer = bare, "alpha"
			}
			in := freePorts(t, 1)
			if err := on.rt.AddRule(node.Rule{Name: "r", Listen: loop(in), Destination: loop(dst), Peer: peer, Enabled: true, Dial: c.dial}); err != nil {
				t.Fatal(err)
			}
			if _, err := send(t, loop(in), 1000); err == nil {
				t.Error("the connection ended cleanly")
			}
			eventually(t, "the rule says what is missing", func() bool { return strings.Contains(statusOf(t, on, "r").Error, c.want) })
			if t.Failed() {
				t.Logf("the rule says: %q\n%s", statusOf(t, on, "r").Error, on.logs)
			}
			eventually(t, "no session is left", func() bool { return len(holder.e.SessionList())+len(bare.e.SessionList()) == 0 })
		})
	}
}

// With dial both, the names of a rule are looked for on each node: each takes the
// paths it has by those names. With dial peer they are the peer's, and are not held
// against this node's paths when the rule is stored.
func TestBothTakesTheNamesEachNodeHas(t *testing.T) {
	a, b, _ := eachWayGated(t)
	// beta's path to alpha is called "default" too; give alpha's another name
	p, _ := a.n.Peer("beta")
	mine := p.Paths[0]
	mine.Name = "mine"
	if _, err := a.svc.SetPeerPaths("beta", []node.Path{mine}); err != nil {
		t.Fatal(err)
	}
	helloed(t, a, b)
	dst, got := sink(t)
	in := freePorts(t, 2)
	if err := a.rt.AddRule(node.Rule{Name: "one", Listen: loop(in), Destination: loop(dst), Peer: "beta", Enabled: true,
		Dial: node.DialBoth, Paths: []string{"mine", "default"}, Subs: 1}); err != nil {
		t.Fatal(err)
	}
	h := hold(t, loop(in), 1<<20)
	session(t, a, 2, 1) // one sub-connection on "mine", one that beta opened on its "default"
	if err := h.finish(); err != nil {
		t.Fatal(err)
	}
	if arr := wait(t, got); arr.n != 1<<20 || arr.sum != h.hash || arr.err != nil {
		t.Fatalf("%d bytes arrived (%v), or they differ from what was sent", arr.n, arr.err)
	}
	eventually(t, "the session has ended", func() bool { return len(a.e.SessionList())+len(b.e.SessionList()) == 0 })

	// a name that only the peer has: fine with dial peer, refused where this node dials
	if err := a.rt.AddRule(node.Rule{Name: "theirs", Listen: loop(in + 1), Destination: loop(dst), Peer: "beta", Enabled: true,
		Dial: node.DialPeer, Paths: []string{"default"}}); err != nil {
		t.Fatalf("a rule with dial peer that names a path of the peer: %v", err)
	}
	h = hold(t, loop(in+1), 1<<20)
	session(t, a, 2, 0)
	if err := h.finish(); err != nil {
		t.Fatal(err)
	}
	wait(t, got)
	err := a.rt.AddRule(node.Rule{Name: "own", Listen: ":0", Destination: loop(dst), Peer: "beta", Enabled: true, Paths: []string{"default"}})
	if err == nil || !strings.Contains(err.Error(), `this node has no path of that name to peer "beta"`) {
		t.Errorf("a rule that this node dials for and that names a path it does not have: %v", err)
	}
	// none of the names on this node: the connection of a rule with dial both fails
	if err := a.rt.AddRule(node.Rule{Name: "none", Listen: loop(freePorts(t, 1)), Destination: loop(dst), Peer: "beta", Enabled: true,
		Dial: node.DialBoth, Paths: []string{"default"}}); err != nil {
		t.Fatal(err)
	}
	st := statusOf(t, a, "none")
	if _, err := send(t, st.Rule.Listen, 100); err == nil {
		t.Error("the connection of a rule none of whose paths this node has ended cleanly")
	}
	eventually(t, "the rule says that this node has none of the paths", func() bool {
		return strings.Contains(statusOf(t, a, "none").Error, "this node has none of the paths it names")
	})
}

// A node that is asked to open sub-connections of a session the asking node dials for
// too numbers its paths apart from the asking node's, and says for good when it has no
// path at all.
func TestOpenThatSaysBoth(t *testing.T) {
	a, b := startNode(t, "alpha"), startNode(t, "beta")
	pairNodes(t, a, b, 2) // beta holds the path
	helloed(t, a, b)
	ps, err := b.rt.peerFor("alpha")
	if err != nil {
		t.Fatal(err)
	}
	for base, want := range map[uint8]uint8{0: 0, control.BothPathBase: control.BothPathBase} {
		if gs := b.rt.reverseGroups(ps, ps.peer, 1, base); len(gs) != 1 || gs[0].Path != want {
			t.Errorf("groups with base %d: %+v", base, gs)
		}
	}
	// a node without a path to the node that asks answers with the code
	bare := joinBare(t, a, "bare")
	cc, _ := bare.control(t)
	expect(t, cc, control.TypeHello)
	if err := cc.Send(control.Message{Type: control.TypeOpen, Request: 9, Session: "00", Both: true}); err != nil {
		t.Fatal(err)
	}
	if m := expect(t, cc, control.TypeOpenFail); m.Code != control.CodeNoPath || m.Request != 9 {
		t.Errorf("the answer of a node without a path: %+v", m)
	}
}
