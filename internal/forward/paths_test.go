package forward

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/control"
	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/node"
)

// refusing returns a loopback address that refuses connections: a port that was just
// closed. Should another listener take it meanwhile, a dial of a path still fails
// there, because that listener is not the peer.
func refusing(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// sink listens on a loopback port that the system picks and reports every connection
// when it ends, like sinks.
func sink(t *testing.T) (port int, got chan arrival) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	port = ln.Addr().(*net.TCPAddr).Port
	got = make(chan arrival, 64)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				h := sha256.New()
				k, err := copyAll(h, c)
				c.Close()
				a := arrival{port: port, n: k, err: err}
				copy(a.sum[:], h.Sum(nil))
				got <- a
			}()
		}
	}()
	return port, got
}

// held is an application connection to a rule that has sent its first part and stays
// open until finish sends the rest.
type held struct {
	t    *testing.T
	c    net.Conn
	hash [sha256.Size]byte
	rest []byte
}

// hold connects to addr, sends the first half of size random bytes and keeps the
// connection open.
func hold(t *testing.T, addr string, size int) *held {
	t.Helper()
	data := make([]byte, size)
	rand.Read(data)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := c.Write(data[:size/2]); err != nil {
		t.Fatal(err)
	}
	return &held{t: t, c: c, hash: sha256.Sum256(data), rest: data[size/2:]}
}

// finish sends the rest, closes this side and returns the error the connection ended
// with (nil: the far end closed cleanly).
func (h *held) finish() error {
	h.t.Helper()
	if _, err := h.c.Write(h.rest); err != nil {
		return err
	}
	h.c.(*net.TCPConn).CloseWrite()
	h.c.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, err := copyAll(discard{}, h.c)
	return err
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// copyAll is io.Copy without its import clash in this file's small helpers.
func copyAll(dst interface{ Write([]byte) (int, error) }, src net.Conn) (int64, error) {
	buf := make([]byte, 64<<10)
	var n int64
	for {
		k, err := src.Read(buf)
		if k > 0 {
			dst.Write(buf[:k])
			n += int64(k)
		}
		if err != nil {
			if errors.Is(err, net.ErrClosed) || err.Error() == "EOF" {
				return n, nil
			}
			return n, err
		}
	}
}

// subsOf returns the live sub-connections of the one session the node holds, once it
// has one that the other end has answered and that has as many as want.
func subsOf(t *testing.T, tn *testNode, want int) int {
	t.Helper()
	n := 0
	eventually(t, fmt.Sprintf("a session with %d sub-connections", want), func() bool {
		list := tn.e.SessionList()
		if len(list) != 1 || !list[0].Up {
			return false
		}
		n = list[0].Subs
		return n == want
	})
	return n
}

// narrow keeps the data groups of the paths a rule names, with the number it names, and
// every control and repair group.
func TestNarrow(t *testing.T) {
	groups := []engine.DialGroup{
		{Path: 0, Name: "main", Want: 8},
		{Path: 0, Name: "main", Ctrl: true, Want: 1},
		{Path: 1, Name: "side", Want: 4},
		{Path: 2, Name: "fix", Repair: true, Want: 3},
	}
	show := func(gs []engine.DialGroup) string {
		var parts []string
		for _, g := range gs {
			role := "data"
			if g.Ctrl {
				role = "control"
			} else if g.Repair {
				role = "repair"
			}
			parts = append(parts, fmt.Sprintf("%s/%s x%d", g.Name, role, g.Want))
		}
		return strings.Join(parts, ", ")
	}
	for _, c := range []struct {
		names []string
		subs  int
		want  string
	}{
		{nil, 0, "main/data x8, main/control x1, side/data x4, fix/repair x3"},
		{[]string{"side"}, 0, "main/control x1, side/data x4, fix/repair x3"},
		{[]string{"main"}, 3, "main/data x3, main/control x1, fix/repair x3"},
		{nil, 6, "main/data x6, main/control x1, side/data x4, fix/repair x3"},
		{[]string{"side", "main"}, 1, "main/data x1, main/control x1, side/data x1, fix/repair x3"},
		{nil, 64, "main/data x8, main/control x1, side/data x4, fix/repair x3"},
	} {
		got, missing := narrow(groups, c.names, c.subs)
		if show(got) != c.want || missing != "" {
			t.Errorf("narrow(%v, %d) = %s (missing %q), want %s", c.names, c.subs, show(got), missing, c.want)
		}
	}
	// a name that is no path, and one that is a path without data
	for _, names := range [][]string{{"nope"}, {"main", "nope"}, {"fix"}} {
		if got, missing := narrow(groups, names, 0); got != nil || missing != names[len(names)-1] {
			t.Errorf("narrow(%v) = %s, missing %q", names, show(got), missing)
		}
	}
	// nothing that carries data at all
	if got, missing := narrow(groups[3:], nil, 0); got != nil || missing != "" {
		t.Errorf("narrow of repair groups alone = %s, missing %q", show(got), missing)
	}
	if len(groups) != 4 || groups[0].Want != 8 {
		t.Error("narrow changed the groups it was given")
	}
}

// counting listens on a loopback port, closes every connection that arrives at once and
// counts them: an address that is no node, and that shows whether it was dialed.
func counting(t *testing.T) (addr string, count func() int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var n atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			c.Close()
		}
	}()
	return ln.Addr().String(), n.Load
}

// twoPaths gives holder two data paths to its peer other: "good", which works, and
// "bad", whose address is no node; dialed says how often "bad" was dialed.
func twoPaths(t *testing.T, holder, other *testNode, subs int) (dialed func() int64) {
	t.Helper()
	p, _ := holder.n.Peer(other.n.Name())
	good := p.Paths[0]
	good.Name, good.Subs = "good", subs
	bad := good
	bad.Name = "bad"
	bad.Addr, dialed = counting(t)
	if _, err := holder.svc.SetPeerPaths(other.n.Name(), []node.Path{good, bad}); err != nil {
		t.Fatal(err)
	}
	return dialed
}

// A rule that names a path and a number of sub-connections uses that path alone, with
// that many: on the node that holds the paths, and on the node whose peer holds them.
func TestRuleKeepsToThePathsItNames(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%v", reverse), func(t *testing.T) {
			a, b := startNode(t, "alpha"), startNode(t, "beta")
			pairNodes(t, a, b, 4) // beta holds the path
			badDialed := twoPaths(t, b, a, 4)
			entry, exit := b, a
			if reverse {
				entry, exit = a, b
			}
			helloed(t, a, b)
			dst, got := sink(t)
			in := freePorts(t, 1)
			rule := node.Rule{Name: "r", Listen: loop(in), Destination: loop(dst), Peer: exit.n.Name(), Enabled: true, Paths: []string{"good"}, Subs: 2}
			if err := entry.rt.AddRule(rule); err != nil {
				t.Fatal(err)
			}
			for round := 1; round <= 2; round++ {
				h := hold(t, loop(in), 2<<20)
				// two data sub-connections, and no control or repair path here
				subsOf(t, entry, 2)
				subsOf(t, exit, 2)
				if err := h.finish(); err != nil {
					t.Fatalf("round %d: the sending side ended with %v\nentry:\n%s\nexit:\n%s", round, err, entry.logs, exit.logs)
				}
				if arr := wait(t, got); arr.n != 2<<20 || arr.sum != h.hash || arr.err != nil {
					t.Fatalf("round %d: %d bytes arrived (%v), or they differ from what was sent", round, arr.n, arr.err)
				}
			}
			// the other path was never dialed
			if n := badDialed(); n != 0 {
				t.Errorf("the path the rule does not name was dialed %d times", n)
			}
			if st := statusOf(t, entry, "r"); st.Error != "" || !st.Listening {
				t.Errorf("the rule after two connections: %+v", st)
			}
			// the same rule without a choice uses the other path too, which fails to dial
			rule.Paths, rule.Subs = nil, 0
			if err := entry.rt.ReplaceRule("r", rule); err != nil {
				t.Fatal(err)
			}
			sum, err := send(t, loop(in), 1<<20)
			if err != nil {
				t.Fatalf("the rule without a choice: %v", err)
			}
			if arr := wait(t, got); arr.n != 1<<20 || arr.sum != sum {
				t.Fatalf("the rule without a choice: %d bytes arrived", arr.n)
			}
			eventually(t, "the other path is dialed when the rule names none", func() bool { return badDialed() > 0 })
		})
	}
}

// A name that is no path of the peer: refused when the rule is stored, where this node
// holds the paths and can tell; said by the peer when a connection comes, where the
// peer holds them.
func TestRuleNamesAPathThatIsNotThere(t *testing.T) {
	a, b := startNode(t, "alpha"), startNode(t, "beta")
	pairNodes(t, a, b, 2) // beta holds the path, called default
	helloed(t, a, b)
	dst, _ := sink(t)
	in := freePorts(t, 2)

	// on beta: known at once, and nothing is stored
	bad := node.Rule{Name: "r", Listen: loop(in), Destination: loop(dst), Peer: "alpha", Enabled: true, Paths: []string{"default", "nope"}}
	err := b.rt.AddRule(bad)
	if !errors.Is(err, node.ErrInvalid) || !strings.Contains(err.Error(), `the rule names the path "nope", and this node has no path of that name to peer "alpha" that carries data`) {
		t.Fatalf("a rule that names a path this node does not have: %v", err)
	}
	if len(b.n.Rules()) != 0 {
		t.Fatal("the rule was stored")
	}
	ok := bad
	ok.Paths = []string{"default"}
	if err := b.rt.AddRule(ok); err != nil {
		t.Fatal(err)
	}
	if err := b.rt.ReplaceRule("r", bad); !errors.Is(err, node.ErrInvalid) {
		t.Fatalf("replacing a rule by one that names a path this node does not have: %v", err)
	}
	// a path that is there and carries no data is no path for a rule
	p, _ := b.n.Peer("alpha")
	side := p.Paths[0]
	side.Name, side.Roles = "side", []string{node.RoleControl}
	if _, err := b.svc.SetPeerPaths("alpha", []node.Path{p.Paths[0], side}); err != nil {
		t.Fatal(err)
	}
	onlyControl := ok
	onlyControl.Paths = []string{"side"}
	if err := b.rt.ReplaceRule("r", onlyControl); !errors.Is(err, node.ErrInvalid) {
		t.Fatalf("a rule that names a path without the data role: %v", err)
	}

	// on alpha, whose peer holds the paths: stored, and the peer says it for a connection
	far := node.Rule{Name: "far", Listen: loop(in + 1), Destination: loop(dst), Peer: "beta", Enabled: true, Paths: []string{"nope"}}
	if err := a.rt.AddRule(far); err != nil {
		t.Fatalf("a rule whose peer holds the paths: %v", err)
	}
	// Beta's paths were changed above, so beta opens its control connection afresh, and
	// for a moment alpha has none to ask beta over: a connection that comes in that
	// moment fails for that reason. Connections are tried until one gets as far as the
	// peer's answer.
	want := `peer "beta" could not open the connections: it has no path called "nope" that carries data`
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the rule says %q", statusOf(t, a, "far").Error)
		}
	})
	eventually(t, "the rule says which path the peer does not have", func() bool {
		if _, err := send(t, loop(in+1), 1000); err == nil {
			t.Fatal("a connection over a path the peer does not have ended cleanly")
		}
		for until := time.Now().Add(time.Second); time.Now().Before(until); time.Sleep(10 * time.Millisecond) {
			if statusOf(t, a, "far").Error == want {
				return true
			}
		}
		return false
	})
}

// A peer of a version that does not keep to a rule's paths: the connection is reset
// and the rule says what the peer lacks. Nothing goes over every path instead.
func TestRulePathsNeedAPeerThatKnowsThem(t *testing.T) {
	a := startNode(t, "alpha")
	b := startNode(t, "beta", func(rt *Runtime) { rt.noRulePaths = true })
	pairNodes(t, a, b, 2) // beta holds the path and is the older one
	helloed(t, a, b)
	dst, got := sink(t)
	in := freePorts(t, 1)
	rule := node.Rule{Name: "r", Listen: loop(in), Destination: loop(dst), Peer: "beta", Enabled: true, Subs: 1}
	if err := a.rt.AddRule(rule); err != nil {
		t.Fatal(err)
	}
	if _, err := send(t, loop(in), 1000); err == nil {
		t.Fatal("the connection ended cleanly")
	}
	want := `peer "beta" does not keep a rule to the paths it names (it runs version "test-beta"). Both nodes need tcpraid 0.5.0 or later. Until then, take the paths and the number of sub-connections off the rule.`
	eventually(t, "the rule says what the peer lacks", func() bool { return statusOf(t, a, "r").Error == want })
	select {
	case arr := <-got:
		t.Fatalf("%d bytes arrived at the destination", arr.n)
	case <-time.After(300 * time.Millisecond):
	}
	if n := len(a.e.SessionList()) + len(b.e.SessionList()); n != 0 {
		t.Errorf("%d sessions after a connection that was not carried", n)
	}
	// without a choice the same rule works with that peer, as before
	rule.Subs = 0
	if err := a.rt.ReplaceRule("r", rule); err != nil {
		t.Fatal(err)
	}
	sum, err := send(t, loop(in), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if arr := wait(t, got); arr.n != 1<<20 || arr.sum != sum {
		t.Fatalf("%d bytes arrived, or they differ", arr.n)
	}
}

// What a peer asks for in an open is checked like anything a peer sends.
func TestOpenWithPathsIsChecked(t *testing.T) {
	a := startNode(t, "alpha")
	b := joinBare(t, a, "bare")
	// alpha gets a path to the bare peer, so that it would dial if asked properly
	if _, err := a.svc.SetPeerPaths("bare", []node.Path{{Name: "main", Type: "tcp", Addr: refusing(t), Security: node.SecurityTLS, Roles: []string{node.RoleData}, Subs: 2}}); err != nil {
		t.Fatal(err)
	}
	cc, _ := b.control(t)
	expect(t, cc, control.TypeHello)
	many := make([]string, node.MaxPaths+1)
	for i := range many {
		many[i] = fmt.Sprintf("p%d", i)
	}
	for name, c := range map[string]struct {
		m    control.Message
		want string
	}{
		"too many paths":      {control.Message{RulePaths: many}, "the request names more paths or sub-connections than a peer has"},
		"too many subs":       {control.Message{RuleSubs: node.MaxSubs + 1}, "the request names more paths or sub-connections than a peer has"},
		"a negative number":   {control.Message{RuleSubs: -1}, "the request names more paths or sub-connections than a peer has"},
		"a path it has not":   {control.Message{RulePaths: []string{"nope"}}, `it has no path called "nope" that carries data`},
		"a name that is long": {control.Message{RulePaths: []string{strings.Repeat("x", 500)}}, `it has no path called "` + strings.Repeat("x", node.MaxPathNameLen) + `" that carries data`},
		"a name with escapes": {control.Message{RulePaths: []string{"a\x1b[31mb"}}, `it has no path called "a?[31mb" that carries data`},
	} {
		c.m.Type, c.m.Request, c.m.Session = control.TypeOpen, 7, strings.Repeat("ab", 16)
		if err := cc.Send(c.m); err != nil {
			t.Fatal(err)
		}
		if fail := expect(t, cc, control.TypeOpenFail); fail.Request != 7 || fail.Reason != c.want {
			t.Errorf("%s: the answer is %q, want %q", name, fail.Reason, c.want)
		}
	}
}

// dialedControl counts the control connections tn has with peer, by who opened them.
func dialedControl(tn *testNode, peer string) (own, theirs int) {
	tn.rt.mu.Lock()
	defer tn.rt.mu.Unlock()
	if pc := tn.rt.ctl[peer]; pc != nil {
		for _, cc := range pc.conns {
			if cc.dialed {
				own++
			} else {
				theirs++
			}
		}
	}
	return own, theirs
}

// asked reports how many sessions tn carries whose sub-connections it asked a peer to
// open.
func asked(tn *testNode) int {
	tn.rt.mu.Lock()
	defer tn.rt.mu.Unlock()
	return len(tn.rt.reverse)
}

// eachWay starts two nodes that each hold a path to the other, both with a control
// connection of their own to the other, and returns them with alpha's path to beta.
func eachWay(t *testing.T) (a, b *testNode, up node.Path) {
	t.Helper()
	a, b = startNode(t, "alpha"), startNode(t, "beta")
	pairNodes(t, a, b, 2) // beta gets a path to alpha
	up = node.Path{Name: "default", Type: "tcp", Addr: b.addr, Security: node.SecurityTLS, Roles: []string{node.RoleData}, Subs: 2}
	if _, err := a.svc.SetPeerPaths("beta", []node.Path{up}); err != nil {
		t.Fatal(err)
	}
	helloed(t, a, b)
	eventually(t, "each node has opened a control connection to the other", func() bool {
		own, theirs := dialedControl(a, "beta")
		return own == 1 && theirs == 1
	})
	return a, b, up
}

// takeDown makes alpha's path to beta fail: its address refuses from now on, and the
// control connection alpha had opened over it is closed. It returns when alpha's next
// dial of a control connection has failed.
func takeDown(t *testing.T, a *testNode, up node.Path) {
	t.Helper()
	down := up
	down.Addr = refusing(t)
	if _, err := a.svc.SetPeerPaths("beta", []node.Path{down}); err != nil {
		t.Fatal(err)
	}
	a.rt.mu.Lock()
	for _, cc := range a.rt.ctl["beta"].conns {
		if cc.dialed {
			cc.Close()
		}
	}
	a.rt.mu.Unlock()
	eventually(t, "alpha's dial of a control connection has failed", func() bool {
		a.rt.mu.Lock()
		defer a.rt.mu.Unlock()
		return a.rt.pathDownLocked("beta")
	})
}

// A rule that names paths means the paths of the node it is on. It stays with them when
// they are down and does not ask the peer.
func TestRuleWithPathsDoesNotFallBack(t *testing.T) {
	a, _, up := eachWay(t)
	dst, _ := sink(t)
	in := freePorts(t, 1)
	if err := a.rt.AddRule(node.Rule{Name: "own", Listen: loop(in), Destination: loop(dst), Peer: "beta", Enabled: true, Paths: []string{"default"}}); err != nil {
		t.Fatal(err)
	}
	takeDown(t, a, up)
	if statusOf(t, a, "own").PeerDials {
		t.Error("a rule that names paths says that the peer dials")
	}
	c := hold(t, loop(in), 1000)
	defer c.c.Close()
	eventually(t, "the rule dials alpha's own path, which refuses", func() bool {
		return strings.Contains(statusOf(t, a, "own").Error, "cannot be opened")
	})
	if n := asked(a); n != 0 {
		t.Error("a rule that names paths asked the peer to open its connections")
	}
}

// Two nodes that each hold a path to the other. While alpha's own path is down and
// beta's is up, new connections of alpha's rule are opened by beta; a session that runs
// is left alone; when the path is back, alpha opens them itself again.
func TestPeerDialsWhileOwnPathIsDown(t *testing.T) {
	a, b, up := eachWay(t)
	dst, got := sink(t)
	in := freePorts(t, 1)
	if err := a.rt.AddRule(node.Rule{Name: "r", Listen: loop(in), Destination: loop(dst), Peer: "beta", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	carry := func(what string, wantAsked int) {
		t.Helper()
		h := hold(t, loop(in), 1<<20)
		subsOf(t, b, 2)
		if n := asked(a); n != wantAsked {
			t.Fatalf("%s: alpha asked its peer to open the connections of %d sessions, want %d\nalpha:\n%s", what, n, wantAsked, a.logs)
		}
		if err := h.finish(); err != nil {
			t.Fatalf("%s: the sending side ended with %v\nalpha:\n%s\nbeta:\n%s", what, err, a.logs, b.logs)
		}
		if arr := wait(t, got); arr.n != 1<<20 || arr.sum != h.hash || arr.err != nil {
			t.Fatalf("%s: %d bytes arrived (%v), or they differ from what was sent", what, arr.n, arr.err)
		}
		eventually(t, "the session has ended on both nodes", func() bool { return len(a.e.SessionList())+len(b.e.SessionList()) == 0 })
	}
	carry("both paths up", 0)
	if st := statusOf(t, a, "r"); st.PeerDials {
		t.Fatal("the rule says that the peer dials while this node's path is up")
	}

	// a session that began over alpha's own path and is still running
	running := hold(t, loop(in), 1<<20)
	subsOf(t, b, 2)

	// alpha's path goes down: its address refuses, and the control connection it had
	// opened is gone
	takeDown(t, a, up)
	eventually(t, "alpha takes its own path for down", func() bool { return statusOf(t, a, "r").PeerDials })
	if own, theirs := dialedControl(a, "beta"); own != 0 || theirs != 1 {
		t.Fatalf("control connections of alpha with beta: %d of its own, %d of beta's", own, theirs)
	}

	// the session that was running goes on, over the connections it has
	if err := running.finish(); err != nil {
		t.Fatalf("the session that was running when the path went down: %v", err)
	}
	if arr := wait(t, got); arr.n != 1<<20 || arr.sum != running.hash || arr.err != nil {
		t.Fatalf("the session that was running: %d bytes arrived (%v), or they differ", arr.n, arr.err)
	}
	eventually(t, "the session has ended on both nodes", func() bool { return len(a.e.SessionList())+len(b.e.SessionList()) == 0 })

	carry("alpha's path down", 1)
	carry("alpha's path down, again", 1)
	if !strings.Contains(a.logs.String(), `peer "beta": this node cannot open connections to it at the moment, and it is connected to this node, so it is asked to open the connections of rules`) {
		t.Errorf("alpha's log does not say that the peer dials now:\n%s", a.logs)
	}
	if n := strings.Count(a.logs.String(), "so it is asked to open the connections of rules"); n != 1 {
		t.Errorf("the change is logged %d times", n)
	}
	// the path comes back
	if _, err := a.svc.SetPeerPaths("beta", []node.Path{up}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(40 * time.Second); statusOf(t, a, "r").PeerDials; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("alpha does not take its path for up again\nalpha:\n%s", a.logs)
		}
	}
	carry("alpha's path up again", 0)
	if !strings.Contains(a.logs.String(), `peer "beta": this node opens the connections of rules itself again`) {
		t.Errorf("alpha's log does not say that it dials again:\n%s", a.logs)
	}
}

// With one node that holds a path and one that does not, nothing changes: the node
// without a path is no fallback for itself, and the node with the path never asks.
func TestNoFallbackWithoutAPathEachWay(t *testing.T) {
	a, b := startNode(t, "alpha"), startNode(t, "beta")
	pairNodes(t, a, b, 2) // only beta holds a path
	helloed(t, a, b)
	dst, _ := sink(t)
	in := freePorts(t, 1)
	if err := b.rt.AddRule(node.Rule{Name: "r", Listen: loop(in), Destination: loop(dst), Peer: "alpha", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// beta's path goes down; alpha has no path to beta and opens no control connection
	p, _ := b.n.Peer("alpha")
	down := p.Paths[0]
	down.Addr = refusing(t)
	if _, err := b.svc.SetPeerPaths("alpha", []node.Path{down}); err != nil {
		t.Fatal(err)
	}
	a.dropAccepted()
	eventually(t, "beta has no control connection left", func() bool {
		own, theirs := dialedControl(b, "alpha")
		return own+theirs == 0
	})
	eventually(t, "beta's dial of a control connection has failed", func() bool {
		b.rt.mu.Lock()
		defer b.rt.mu.Unlock()
		return b.rt.ctl["alpha"] != nil && b.rt.ctl["alpha"].dialFailed
	})
	if statusOf(t, b, "r").PeerDials {
		t.Error("the rule says that the peer dials, and the peer has no way to")
	}
	c := hold(t, loop(in), 1000)
	defer c.c.Close()
	eventually(t, "beta dials its own path, which refuses", func() bool {
		return strings.Contains(statusOf(t, b, "r").Error, "cannot be opened")
	})
	if n := asked(b); n != 0 {
		t.Error("beta asked a peer without a path to open the connections")
	}
}
