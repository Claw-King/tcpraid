package forward

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/control"
	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/node"
)

// names is a stand-in for a node's resolver: what it knows, and what it was asked.
type names struct {
	mu    sync.Mutex
	known map[string][]net.IP
	asked []string
	hold  chan struct{} // if not nil, a lookup waits until it is closed or its time is up
}

func (n *names) lookup(ctx context.Context, host string) ([]net.IP, error) {
	n.mu.Lock()
	n.asked = append(n.asked, host)
	ips, ok := n.known[host]
	hold := n.hold
	n.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, Server: "192.0.2.53:53", IsNotFound: true}
	}
	return ips, nil
}

func (n *names) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.asked)
}

// helloed waits until each of the two nodes has the other's hello: what a peer can do
// is known from it.
func helloed(t *testing.T, a, b *testNode) {
	t.Helper()
	eventually(t, "the two nodes have each other's hello", func() bool {
		sa, sb := a.rt.PeerStatus(b.n.Name()), b.rt.PeerStatus(a.n.Name())
		return sa.Online && sb.Online && sa.Version != "" && sb.Version != ""
	})
}

// namedRule makes two paired nodes and a rule whose destination is the host name "nas"
// at the exit node, which knows the name by its own resolver. With reverse, the node
// with the rule has no path and its peer dials.
func namedRule(t *testing.T, reverse bool, known map[string][]net.IP, configure ...func(*Runtime)) (entry, exit *testNode, res *names, in, dst int) {
	t.Helper()
	res = &names{known: known}
	withNames := func(rt *Runtime) { rt.Lookup = res.lookup }
	a, b := startNode(t, "alpha", append([]func(*Runtime){withNames}, configure...)...), startNode(t, "beta", append([]func(*Runtime){withNames}, configure...)...)
	pairNodes(t, a, b, 2) // beta has the path
	entry, exit = b, a
	if reverse {
		entry, exit = a, b
	}
	in, dst = freePorts(t, 1), freePorts(t, 1)
	rule := node.Rule{Name: "r", Listen: loop(in), Destination: fmt.Sprintf("nas:%d", dst), Peer: exit.n.Name(), Enabled: true}
	if err := entry.rt.AddRule(rule); err != nil {
		t.Fatal(err)
	}
	helloed(t, a, b)
	return entry, exit, res, in, dst
}

// A rule whose destination is a host name: the peer looks the name up, and the
// connection arrives at the address it stands for there. With a path of the node's own
// and with a peer that dials.
func TestRuleToAHostName(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%v", reverse), func(t *testing.T) {
			entry, exit, res, in, dst := namedRule(t, reverse, map[string][]net.IP{"nas": {net.ParseIP("127.0.0.1")}})
			got := sinks(t, dst, 1)
			for round := 1; round <= 2; round++ {
				sum, err := send(t, loop(in), 3<<20)
				if err != nil {
					t.Fatalf("round %d: the sending side ended with %v\nentry:\n%s\nexit:\n%s", round, err, entry.logs, exit.logs)
				}
				if arr := wait(t, got); arr.n != 3<<20 || arr.sum != sum || arr.err != nil {
					t.Fatalf("round %d: %d bytes arrived (%v), or they differ from what was sent", round, arr.n, arr.err)
				}
				// every connection asks again: nothing is kept from the one before
				entry.rt.mu.Lock()
				sent := entry.rt.resolveSent
				entry.rt.mu.Unlock()
				if n := res.count(); n != round || sent != round {
					t.Fatalf("after %d connections: %d requests sent, %d lookups at the exit node", round, sent, n)
				}
			}
			if st := statusOf(t, entry, "r"); !st.Listening || st.Error != "" {
				t.Errorf("the rule after two connections: %+v", st)
			}
			// the status and the listing show the destination as it was given
			if st := statusOf(t, entry, "r"); st.Rule.Destination != fmt.Sprintf("nas:%d", dst) {
				t.Errorf("the rule's destination reads %q", st.Rule.Destination)
			}
		})
	}
}

// A rule whose destination is an address asks nobody: no lookup at the peer, and it
// works with a peer that cannot look up at all.
func TestRuleToAnAddressAsksNobody(t *testing.T) {
	res := &names{known: map[string][]net.IP{}}
	a := startNode(t, "alpha", func(rt *Runtime) { rt.Lookup, rt.noResolve = res.lookup, true })
	b := startNode(t, "beta", func(rt *Runtime) { rt.Lookup = res.lookup })
	pairNodes(t, a, b, 2)
	dst, in := freePorts(t, 1), freePorts(t, 1)
	got := sinks(t, dst, 1)
	if err := b.rt.AddRule(node.Rule{Name: "r", Listen: loop(in), Destination: loop(dst), Peer: "alpha", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	helloed(t, a, b)
	sum, err := send(t, loop(in), 1<<20)
	if err != nil {
		t.Fatalf("the sending side ended with %v", err)
	}
	if arr := wait(t, got); arr.n != 1<<20 || arr.sum != sum {
		t.Fatalf("%d bytes arrived, or they differ from what was sent", arr.n)
	}
	b.rt.mu.Lock()
	sent := b.rt.resolveSent
	b.rt.mu.Unlock()
	if sent != 0 || res.count() != 0 {
		t.Errorf("a rule to an address: %d requests sent, %d lookups at the peer, want none", sent, res.count())
	}
	if strings.Contains(a.logs.String()+b.logs.String(), "stands for") {
		t.Errorf("the logs speak of a name:\n%s\n%s", a.logs, b.logs)
	}
}

// What can go wrong with a name is said in the rule's status, and the connection is
// reset: a name the peer does not know, a peer that is too old to be asked, a peer that
// is not connected, a name whose addresses the peer's allow list does not hold.
func TestRuleToAHostNameSaysWhy(t *testing.T) {
	failed := func(t *testing.T, entry *testNode, in int, wants ...string) {
		t.Helper()
		before := entry.e.Counters()
		defer func() {
			t.Helper()
			if sessionsTotal(entry.e.Counters()) != sessionsTotal(before) {
				t.Error("a session was made for a connection whose name gave no address")
			}
		}()
		if _, err := send(t, loop(in), 1000); err == nil {
			t.Fatal("a connection that could not be carried ended cleanly")
		}
		eventually(t, "the rule says why: "+strings.Join(wants, " ... "), func() bool {
			st := statusOf(t, entry, "r")
			for _, want := range wants {
				if !strings.Contains(st.Error, want) {
					return false
				}
			}
			return !st.ErrorAt.IsZero()
		})
		// no session was made for it (one of a connection before may take a moment to go)
		eventually(t, "no session is left", func() bool { return len(entry.e.SessionList()) == 0 })
	}
	t.Run("unknown name", func(t *testing.T) {
		for _, reverse := range []bool{false, true} {
			entry, exit, _, in, _ := namedRule(t, reverse, map[string][]net.IP{})
			failed(t, entry, in, fmt.Sprintf("peer %q could not find the address of nas: no such host", exit.n.Name()))
			// the resolver's own words, with the server it asked, stay at the peer, in its log
			if st := statusOf(t, entry, "r"); strings.Contains(st.Error, "192.0.2.53") {
				t.Errorf("the status names the peer's resolver: %s", st.Error)
			}
			if strings.Contains(entry.logs.String(), "192.0.2.53") {
				t.Errorf("the log of the node that asked names the peer's resolver:\n%s", entry.logs)
			}
			if want := fmt.Sprintf("peer %q asked for the address of nas, and the lookup failed", entry.n.Name()); !strings.Contains(exit.logs.String(), want) || !strings.Contains(exit.logs.String(), "192.0.2.53") {
				t.Errorf("the log of the node that looked up does not say why:\n%s", exit.logs)
			}
		}
	})
	t.Run("the peer is too old", func(t *testing.T) {
		entry, exit, res, in, _ := namedRule(t, false, map[string][]net.IP{"nas": {net.ParseIP("127.0.0.1")}}, func(rt *Runtime) {
			if rt.Node.Name() == "alpha" {
				rt.noResolve = true
			}
		})
		failed(t, entry, in, fmt.Sprintf("peer %q does not look up host names for this node (it runs version \"test-alpha\")", exit.n.Name()), "0.4.0 or later", "IP address of nas")
		if res.count() != 0 {
			t.Error("a peer that does not have the feature was asked all the same")
		}
	})
	t.Run("the peer is not connected", func(t *testing.T) {
		entry, exit, _, in, _ := namedRule(t, true, map[string][]net.IP{"nas": {net.ParseIP("127.0.0.1")}})
		exit.stop()
		eventually(t, "the control connection is gone", func() bool { return !entry.rt.PeerStatus(exit.n.Name()).Online })
		failed(t, entry, in, fmt.Sprintf("peer %q is not connected to this node", exit.n.Name()), "which address nas stands for")
	})
	t.Run("the allow list", func(t *testing.T) {
		for _, reverse := range []bool{false, true} {
			entry, exit, _, in, dst := namedRule(t, reverse, map[string][]net.IP{"nas": {net.ParseIP("192.0.2.77"), net.ParseIP("127.0.0.1")}})
			got := sinks(t, dst, 1)
			// the list holds the second address only: that is the one the connection goes to
			allow := fmt.Sprintf("127.0.0.1/32:%d", dst)
			if _, err := exit.svc.UpdatePeer(entry.n.Name(), nil, &allow, nil, nil); err != nil {
				t.Fatal(err)
			}
			sum, err := send(t, loop(in), 1<<20)
			if err != nil {
				t.Fatalf("reverse=%v: the sending side ended with %v\nentry:\n%s\nexit:\n%s", reverse, err, entry.logs, exit.logs)
			}
			if arr := wait(t, got); arr.n != 1<<20 || arr.sum != sum {
				t.Fatalf("reverse=%v: %d bytes arrived, or they differ from what was sent", reverse, arr.n)
			}
			// the list holds none of the name's addresses: refused, and the addresses are not told
			allow = "198.51.100.0/24"
			if _, err := exit.svc.UpdatePeer(entry.n.Name(), nil, &allow, nil, nil); err != nil {
				t.Fatal(err)
			}
			failed(t, entry, in, fmt.Sprintf("peer %q refused the destination nas:%d", exit.n.Name(), dst), "allow list")
			if st := statusOf(t, entry, "r"); strings.Contains(st.Error, "192.0.2.77") || strings.Contains(st.Error, "127.0.0.1") {
				t.Errorf("the status names an address the allow list does not hold: %s", st.Error)
			}
			// the same port on the right address, another port in the list: the port counts too
			allow = "127.0.0.1/32:1"
			if _, err := exit.svc.UpdatePeer(entry.n.Name(), nil, &allow, nil, nil); err != nil {
				t.Fatal(err)
			}
			failed(t, entry, in, "refused the destination")
		}
	})
}

// sessionsTotal is the number of sessions an engine has made so far.
func sessionsTotal(cs []engine.Counter) int64 {
	for _, c := range cs {
		if c.Name == "sessions_total" {
			return c.Value
		}
	}
	return -1
}

// What a peer answers is not trusted: an answer that names no IP address is no answer,
// and a peer that does not answer is not waited for longer than the time a lookup may
// take.
func TestResolveAnswerIsChecked(t *testing.T) {
	entry, exit, _, in, dst := namedRule(t, false, map[string][]net.IP{"nas": {net.ParseIP("127.0.0.1")}})
	// answers as a peer could send them, fed to the node that asked
	for addr, want := range map[string]string{
		"nas.example.net":    "something that is not an IP address",
		"127.0.0.1:22":       "something that is not an IP address",
		"127.0.0.1\x1b[31m":  "something that is not an IP address",
		"":                   "could not find the address of nas",
		"192.0.2.999":        "something that is not an IP address",
		"127.0.0.1/8":        "something that is not an IP address",
		"0x7f.0.0.1":         "something that is not an IP address",
		"127.0.0.1 ; reboot": "something that is not an IP address",
	} {
		entry.rt.mu.Lock()
		key := reverseKey{exit.n.Name(), 4242}
		answer := make(chan resolved, 1)
		entry.rt.resolves[key] = answer
		entry.rt.mu.Unlock()
		entry.rt.resolveAnswered(exit.n.Name(), control.Message{Type: control.TypeResolveReply, Request: 4242, Addr: addr})
		r := <-answer
		entry.rt.mu.Lock()
		delete(entry.rt.resolves, key)
		entry.rt.mu.Unlock()
		if r.ip != nil {
			t.Errorf("the answer %q was taken for the address %v", addr, r.ip)
		}
		if addr != "" && !strings.Contains(r.reason, want) {
			t.Errorf("the answer %q: reason %q", addr, r.reason)
		}
	}
	// an answer for a request this node did not make, or from another peer, is dropped
	entry.rt.resolveAnswered("nobody", control.Message{Type: control.TypeResolveReply, Request: 1, Addr: "127.0.0.1"})
	entry.rt.resolveAnswered(exit.n.Name(), control.Message{Type: control.TypeResolveReply, Request: 1, Addr: "127.0.0.1"})

	// a reason the peer wrote is made printable and cut
	entry.rt.mu.Lock()
	key := reverseKey{exit.n.Name(), 4243}
	answer := make(chan resolved, 1)
	entry.rt.resolves[key] = answer
	entry.rt.mu.Unlock()
	entry.rt.resolveAnswered(exit.n.Name(), control.Message{Type: control.TypeResolveReply, Request: 4243, Code: control.CodeLookup, Reason: "bad\x1b[31m\n" + strings.Repeat("x", 500)})
	if r := <-answer; strings.ContainsAny(r.reason, "\x1b\n") || len(r.reason) > maxReason {
		t.Errorf("a reason of the peer was kept as it came: %q", r.reason)
	}
	entry.rt.mu.Lock()
	delete(entry.rt.resolves, key)
	entry.rt.mu.Unlock()

	// the rule still works after all that
	got := sinks(t, dst, 1)
	sum, err := send(t, loop(in), 1<<20)
	if err != nil {
		t.Fatalf("the sending side ended with %v", err)
	}
	if arr := wait(t, got); arr.n != 1<<20 || arr.sum != sum {
		t.Fatalf("%d bytes arrived, or they differ from what was sent", arr.n)
	}
}

// The node that is asked refuses what cannot be a host name, bounds how many names it
// looks up at a time, and gives a lookup up after its time.
func TestResolveIsBoundedAtTheNodeThatIsAsked(t *testing.T) {
	a, b := startNode(t, "alpha"), startNode(t, "beta")
	hold := make(chan struct{})
	res := &names{known: map[string][]net.IP{"nas": {net.ParseIP("127.0.0.1")}}, hold: hold}
	a.rt.Lookup = res.lookup
	pairNodes(t, a, b, 2)
	helloed(t, a, b)
	ask := func(host string, port int) (net.IP, error) { return b.rt.resolveAt("alpha", host, port) }

	// held lookups fill the places; the next request is told at once
	var wg sync.WaitGroup
	results := make(chan error, maxResolving)
	for i := 0; i < maxResolving; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ask("nas", 22)
			results <- err
		}()
	}
	eventually(t, "the node looks up as many names as it takes", func() bool {
		a.rt.mu.Lock()
		defer a.rt.mu.Unlock()
		return a.rt.resolving == maxResolving
	})
	began := time.Now()
	if _, err := ask("nas", 22); err == nil || !strings.Contains(err.Error(), "as many host names as it does at a time") {
		t.Errorf("a request beyond the bound: %v", err)
	}
	if took := time.Since(began); took > 3*time.Second {
		t.Errorf("the refusal took %v", took)
	}
	close(hold)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Errorf("a lookup that was held and then let go: %v", err)
		}
	}
	eventually(t, "the places are free again", func() bool {
		a.rt.mu.Lock()
		defer a.rt.mu.Unlock()
		return a.rt.resolving == 0
	})

	// what is no host name is not looked up
	before := res.count()
	a.rt.mu.Lock()
	cc := a.rt.ctl["beta"].conns[0]
	a.rt.mu.Unlock()
	for _, m := range []control.Message{
		{Host: "", Port: 22}, {Host: "nas", Port: 0}, {Host: "nas", Port: 70000}, {Host: "nas", Port: -1},
		{Host: "127.0.0.1", Port: 22}, {Host: "nas/../etc", Port: 22}, {Host: strings.Repeat("a", 300), Port: 22}, {Host: "nas\x00", Port: 22},
	} {
		m.Type, m.Request = control.TypeResolve, 7
		a.rt.serveResolve(cc, m)
	}
	time.Sleep(100 * time.Millisecond)
	if res.count() != before {
		t.Errorf("%d lookups for requests that name no host name and port", res.count()-before)
	}
}

// A lookup that takes too long is given up by the node that does it, which says so.
func TestResolveLookupTimesOut(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the lookup time")
	}
	a, b := startNode(t, "alpha"), startNode(t, "beta")
	res := &names{known: map[string][]net.IP{"nas": {net.ParseIP("127.0.0.1")}}, hold: make(chan struct{})}
	a.rt.Lookup = res.lookup
	pairNodes(t, a, b, 2)
	helloed(t, a, b)
	began := time.Now()
	_, err := b.rt.resolveAt("alpha", "nas", 22)
	if err == nil || !strings.Contains(err.Error(), "took longer than") {
		t.Errorf("a lookup that never returns: %v", err)
	}
	if took := time.Since(began); took < lookupTimeout-time.Second || took > ResolveWait+5*time.Second {
		t.Errorf("it was given up after %v, want about %v", took, lookupTimeout)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Error("the error of the node that asked is the context's")
	}
}
