package forward

import (
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/control"
	"github.com/Claw-King/tcpraid/internal/node"
)

// spares sets how many spare connections a node asks for and keeps.
func spares(n int) func(*Runtime) { return func(rt *Runtime) { rt.Spares = n } }

// sparesOf returns the spare connections the node counts with the peer.
func sparesOf(tn *testNode, peer string) int { return tn.rt.PeerStatus(peer).Spares }

func used(tn *testNode) int {
	tn.rt.mu.Lock()
	defer tn.rt.mu.Unlock()
	return tn.rt.sparesUsed
}

// sparePair starts alpha, which has no path, and beta, which holds one to alpha, each
// with its own number of spares, and waits until they have each other's hello.
func sparePair(t *testing.T, alpha, beta int, configure ...func(*Runtime)) (a, b *testNode) {
	t.Helper()
	a = startNode(t, "alpha", append([]func(*Runtime){spares(alpha)}, configure...)...)
	b = startNode(t, "beta", append([]func(*Runtime){spares(beta)}, configure...)...)
	pairNodes(t, a, b, 2)
	helloed(t, a, b)
	return a, b
}

// The node that holds the path keeps the spare connections its peer asks for. A session
// whose sub-connections the peer opens begins on one, the rest of its sub-connections
// come as before, and the spare is replaced.
func TestSessionBeginsOnASpare(t *testing.T) {
	a, b := sparePair(t, 2, 2)
	eventually(t, "beta keeps two spare connections to alpha", func() bool { return sparesOf(a, "beta") == 2 && sparesOf(b, "alpha") == 2 })
	dst, got := sink(t)
	in := freePorts(t, 1)
	if err := a.rt.AddRule(node.Rule{Name: "r", Listen: loop(in), Destination: loop(dst), Peer: "beta", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for round := 1; round <= 3; round++ {
		h := hold(t, loop(in), 1<<20)
		// the spare is one of the two sub-connections of the path, not a third
		session(t, a, 2, 0)
		session(t, b, 2, 2)
		if n := used(a); n != round {
			t.Fatalf("round %d: %d sessions began on a spare", round, n)
		}
		eventually(t, "the spare that was used is replaced", func() bool { return sparesOf(a, "beta") == 2 && sparesOf(b, "alpha") == 2 })
		time.Sleep(300 * time.Millisecond)
		if si := session(t, a, 2, 0); si.Subs != 2 {
			t.Fatalf("round %d: the session has %d sub-connections a moment later", round, si.Subs)
		}
		if err := h.finish(); err != nil {
			t.Fatalf("round %d: the sending side ended with %v\nalpha:\n%s\nbeta:\n%s", round, err, a.logs, b.logs)
		}
		if arr := wait(t, got); arr.n != 1<<20 || arr.sum != h.hash || arr.err != nil {
			t.Fatalf("round %d: %d bytes arrived (%v), or they differ from what was sent", round, arr.n, arr.err)
		}
		eventually(t, "the session has ended on both nodes", func() bool { return len(a.e.SessionList())+len(b.e.SessionList()) == 0 })
	}
	// a rule that names a number of sub-connections does without a spare
	if err := a.rt.ReplaceRule("r", node.Rule{Name: "r", Listen: loop(in), Destination: loop(dst), Peer: "beta", Enabled: true, Subs: 1}); err != nil {
		t.Fatal(err)
	}
	h := hold(t, loop(in), 1000)
	session(t, a, 1, 0)
	if n := used(a); n != 3 {
		t.Errorf("a rule that names a number of sub-connections began on a spare (%d used)", n)
	}
	if err := h.finish(); err != nil {
		t.Fatal(err)
	}
	wait(t, got)
}

// Spare connections are kept only where both nodes want them: the one without a path
// asks, and the one with the path keeps at most what it is set to.
func TestSparesNeedBothNodes(t *testing.T) {
	for name, c := range map[string]struct {
		alpha, beta int
		configure   func(*Runtime)
		want        int
	}{
		"the node without a path wants none":  {0, 2, nil, 0},
		"the node with the path keeps none":   {2, 0, nil, 0},
		"the node with the path keeps fewer":  {4, 1, nil, 1},
		"the node without a path asks fewer":  {1, 4, nil, 1},
		"more than the most there is":         {99, 99, nil, control.MaxSpares},
		"a peer that does not know spares":    {2, 2, func(rt *Runtime) { rt.noSpare = rt.Node.Name() == "beta" }, 0},
		"an older node that is asked by none": {2, 2, func(rt *Runtime) { rt.noSpare = rt.Node.Name() == "alpha" }, 0},
	} {
		t.Run(name, func(t *testing.T) {
			var cfg []func(*Runtime)
			if c.configure != nil {
				cfg = append(cfg, c.configure)
			}
			a, b := sparePair(t, c.alpha, c.beta, cfg...)
			if c.want > 0 {
				eventually(t, "the spares are kept", func() bool { return sparesOf(a, "beta") == c.want && sparesOf(b, "alpha") == c.want })
			}
			// no more come, and none where none are wanted
			time.Sleep(500 * time.Millisecond)
			if x, y := sparesOf(a, "beta"), sparesOf(b, "alpha"); x != c.want || y != c.want {
				t.Errorf("alpha counts %d spare connections and beta %d, want %d\nalpha:\n%s\nbeta:\n%s", x, y, c.want, a.logs, b.logs)
			}
		})
	}
}

// A node takes spare connections only from a peer it asked, and only as many as it
// asked for: any other is closed at once.
func TestSpareThatWasNotAskedForIsClosed(t *testing.T) {
	closed := func(t *testing.T, a, b *testNode, what string) {
		t.Helper()
		peer, _ := b.n.Peer("alpha")
		conn, err := b.rt.Dialer.OpenSpare(bg, peer, peer.Paths[0], 0)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, err := conn.Read(make([]byte, 1)); err != io.EOF {
			t.Errorf("%s: the connection ended with %v, want it closed by alpha", what, err)
		}
	}
	t.Run("none asked for", func(t *testing.T) {
		a, b := sparePair(t, 0, 2)
		closed(t, a, b, "a spare to a node that asked for none")
		if n := sparesOf(a, "beta"); n != 0 {
			t.Errorf("alpha keeps %d spare connections", n)
		}
	})
	t.Run("one more than asked for", func(t *testing.T) {
		a, b := sparePair(t, 1, 4)
		eventually(t, "the one spare is kept", func() bool { return sparesOf(a, "beta") == 1 && sparesOf(b, "alpha") == 1 })
		closed(t, a, b, "a spare beyond the number asked for")
		if n := sparesOf(a, "beta"); n != 1 {
			t.Errorf("alpha keeps %d spare connections, want 1", n)
		}
	})
}

// When the node that asked gets a path of its own, it wants no spares any more; when
// the pairing ends, they go with it.
func TestSparesGoWhenTheyAreNotWanted(t *testing.T) {
	a, b := sparePair(t, 2, 2)
	eventually(t, "the spares are kept", func() bool { return sparesOf(a, "beta") == 2 && sparesOf(b, "alpha") == 2 })
	up := node.Path{Name: "default", Type: "tcp", Addr: b.addr, Security: node.SecurityTLS, Roles: []string{node.RoleData}, Subs: 2}
	if _, err := a.svc.SetPeerPaths("beta", []node.Path{up}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the spares are given up", func() bool { return sparesOf(a, "beta") == 0 && sparesOf(b, "alpha") == 0 })
	if _, err := a.svc.SetPeerPaths("beta", nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the spares are kept again", func() bool { return sparesOf(a, "beta") == 2 && sparesOf(b, "alpha") == 2 })
	if err := b.svc.RemovePeer("alpha"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "beta keeps none to a node it is not paired with", func() bool { return sparesOf(b, "alpha") == 0 })
	eventually(t, "alpha's spares from beta have ended", func() bool { return sparesOf(a, "beta") == 0 })
}

// A spare connection that ends while it waits is replaced, and a session does not
// begin on one that has ended.
func TestSpareThatEndsIsReplaced(t *testing.T) {
	a, b := sparePair(t, 2, 2)
	eventually(t, "the spares are kept", func() bool { return sparesOf(a, "beta") == 2 && sparesOf(b, "alpha") == 2 })
	// alpha loses every connection it accepted from beta: the spares, and the control
	// connection, which beta opens again
	a.dropAccepted()
	eventually(t, "new spares are kept", func() bool {
		return sparesOf(a, "beta") == 2 && sparesOf(b, "alpha") == 2 && a.rt.PeerStatus("beta").Online
	})
	dst, got := sink(t)
	in := freePorts(t, 1)
	if err := a.rt.AddRule(node.Rule{Name: "r", Listen: loop(in), Destination: loop(dst), Peer: "beta", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	h := hold(t, loop(in), 1<<20)
	session(t, a, 2, 0)
	if err := h.finish(); err != nil {
		t.Fatalf("the sending side ended with %v\nalpha:\n%s\nbeta:\n%s", err, a.logs, b.logs)
	}
	if arr := wait(t, got); arr.n != 1<<20 || arr.sum != h.hash || arr.err != nil {
		t.Fatalf("%d bytes arrived (%v), or they differ from what was sent", arr.n, arr.err)
	}
}

// When the last control connection with a peer is gone, the node that kept spare
// connections for it keeps none and opens none until the peer asks again. A peer that
// comes back wanting none, because of another setting or an older version, gets none.
func TestSparesAreNotKeptWithoutAControlConnection(t *testing.T) {
	a, b := sparePair(t, 2, 2)
	eventually(t, "the spares are kept", func() bool { return sparesOf(a, "beta") == 2 && sparesOf(b, "alpha") == 2 })
	a.stop()
	eventually(t, "beta keeps and opens no spares for a peer it has no control connection with", func() bool {
		b.rt.mu.Lock()
		defer b.rt.mu.Unlock()
		pc := b.rt.ctl["alpha"]
		return pc == nil || (len(pc.conns) == 0 && pc.spareWant == 0 && !pc.spareKeeping && len(pc.spareIdle) == 0)
	})
}

// oneSided passes connections on to a target, and can end one of them on the side that
// connected only: its side at the target stays open and silent, as when something on
// the way drops a connection that one end goes on believing in.
type oneSided struct {
	addr string

	mu   sync.Mutex
	in   map[string]net.Conn // the side that connected, by the address its side at the target has there
	all  []net.Conn          // both sides of every connection
	left map[net.Conn]bool   // sides that connected and were ended by cut
}

func startOneSided(t *testing.T, target string) *oneSided {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &oneSided{addr: ln.Addr().String(), in: map[string]net.Conn{}, left: map[net.Conn]bool{}}
	t.Cleanup(func() {
		ln.Close()
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, c := range r.all {
			c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				c.Close()
				continue
			}
			r.mu.Lock()
			r.in[up.LocalAddr().String()] = c
			r.all = append(r.all, c, up)
			r.mu.Unlock()
			go func() { io.Copy(c, up); c.Close() }()
			go func() {
				io.Copy(up, c)
				r.mu.Lock()
				left := r.left[c]
				r.mu.Unlock()
				if !left {
					up.Close()
				}
			}()
		}
	}()
	return r
}

// cut ends, on the side that connected only, the connection that the target sees
// coming from the address from. It returns false if no such connection went through.
func (r *oneSided) cut(from string) bool {
	r.mu.Lock()
	c := r.in[from]
	if c != nil {
		r.left[c] = true
	}
	r.mu.Unlock()
	if c == nil {
		return false
	}
	c.Close()
	return true
}

// controlFrom returns where the node sees its control connection with the peer come
// from, or "" unless it has exactly one.
func controlFrom(tn *testNode, peer string) string {
	tn.rt.mu.Lock()
	defer tn.rt.mu.Unlock()
	pc := tn.rt.ctl[peer]
	if pc == nil || len(pc.conns) != 1 {
		return ""
	}
	return pc.conns[0].RemoteAddr().String()
}

// The node that holds the path can lose its control connection where its peer does not
// see it. It opens another and has dropped the number of spares it was asked for. The
// peer still has the old connection, so for it nothing was lost; it says the number
// again when the hello arrives on the new connection, and the spares come back.
func TestSparesComeBackAfterALossOnlyOneNodeSaw(t *testing.T) {
	a, b := sparePair(t, 2, 2)
	eventually(t, "the spares are kept", func() bool { return sparesOf(a, "beta") == 2 && sparesOf(b, "alpha") == 2 })
	r := startOneSided(t, a.addr)
	p, ok := b.n.Peer("alpha")
	if !ok {
		t.Fatal("beta has no peer alpha")
	}
	path := p.Paths[0]
	path.Addr = r.addr
	if _, err := b.svc.SetPeerPaths("alpha", []node.Path{path}); err != nil {
		t.Fatal(err)
	}
	// alpha ends what beta opened over the old path, and beta opens its control
	// connection and the spares again, through the relay. The relay then ends the
	// control connection on beta's side only
	a.dropAccepted()
	downs := func() int { return strings.Count(b.logs.String(), "is down") }
	var before int
	eventually(t, "the control connection goes through the relay, the spares are kept, and the relay ends the control connection on beta's side", func() bool {
		if sparesOf(a, "beta") != 2 || sparesOf(b, "alpha") != 2 {
			return false
		}
		before = downs()
		return r.cut(controlFrom(a, "beta"))
	})
	eventually(t, "beta sees its control connection end", func() bool { return downs() > before })
	eventually(t, "beta is asked again and keeps the spares", func() bool {
		b.rt.mu.Lock()
		pc := b.rt.ctl["alpha"]
		asked := pc != nil && len(pc.conns) > 0 && pc.spareWant == 2
		b.rt.mu.Unlock()
		return asked && sparesOf(a, "beta") == 2 && sparesOf(b, "alpha") == 2
	})
	a.rt.mu.Lock()
	n := len(a.rt.ctl["beta"].conns)
	a.rt.mu.Unlock()
	if n != 2 {
		t.Errorf("alpha has %d control connections with beta, want 2: the old one, which it has not seen end, and the new one", n)
	}
}

// spareAllow starts a pair that keeps spares, with a rule on alpha to a sink through
// beta. ready waits until both spares are kept, and set changes the allow list that
// beta keeps for alpha.
func spareAllow(t *testing.T) (a *testNode, in, dst int, got chan arrival, ready func(), set func(string)) {
	t.Helper()
	a, b := sparePair(t, 2, 2)
	ready = func() {
		t.Helper()
		eventually(t, "beta keeps two spare connections to alpha", func() bool { return sparesOf(a, "beta") == 2 && sparesOf(b, "alpha") == 2 })
	}
	dst, got = sink(t)
	in = freePorts(t, 1)
	if err := a.rt.AddRule(node.Rule{Name: "r", Listen: loop(in), Destination: loop(dst), Peer: "beta", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	set = func(allow string) {
		t.Helper()
		if _, err := b.svc.SetPeerOptions("alpha", &allow, nil); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("alpha:\n%s\nbeta:\n%s", a.logs, b.logs)
		}
	})
	return a, in, dst, got, ready, set
}

// A session that begins on a spare connection is held to the allow list that the node
// keeps for the peer when the session begins, not to the one of the time the spare was
// opened. A destination that was taken out of the list is refused at once.
func TestSpareRefusesWhatWasTakenOutOfTheAllowList(t *testing.T) {
	a, in, _, _, ready, set := spareAllow(t)
	// A connection does not always begin on a spare: one may just have ended. The
	// refusal is asked of every try, and the test is over with the first try whose
	// connection did begin on a spare that was opened while beta kept no list.
	for try := 1; ; try++ {
		set("")
		ready()
		before := used(a)
		set("192.0.2.0/24:873")
		if _, err := send(t, loop(in), 5000); err == nil {
			t.Fatal("the destination was taken out of the allow list, and a connection that began on a spare opened before that was carried")
		}
		if used(a) == before+1 {
			return
		}
		if try == 5 {
			t.Fatal("none of 5 connections began on a spare: the test did not test what it should")
		}
	}
}

// And a destination that was put into the list is let through at once.
func TestSpareCarriesWhatWasPutIntoTheAllowList(t *testing.T) {
	a, in, dst, got, ready, set := spareAllow(t)
	for try := 1; ; try++ {
		set("192.0.2.0/24:873")
		// two spares are used up, whatever becomes of the connections, so that both
		// spares are of the time of the narrow list
		for start, n := used(a), 0; used(a) < start+2; n++ {
			if n == 8 {
				t.Fatal("8 connections used up fewer than two spares")
			}
			ready()
			send(t, loop(in), 100)
		}
		ready()
		before := used(a)
		set("127.0.0.1/32:" + strconv.Itoa(dst))
		if _, err := send(t, loop(in), 5000); err != nil {
			t.Fatalf("the destination was put into the allow list, and a connection that began on a spare opened before that ended with %v", err)
		}
		for wait(t, got).n != 5000 {
			// what a version that kept the old list carried before
		}
		if used(a) == before+1 {
			return
		}
		if try == 5 {
			t.Fatal("none of 5 connections began on a spare: the test did not test what it should")
		}
	}
}
