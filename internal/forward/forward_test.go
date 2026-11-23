package forward

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/link"
	"github.com/Claw-King/tcpraid/internal/node"
)

// testNode is a whole node on loopback: state, listeners for peers, engine and rules.
type testNode struct {
	n     *node.Node
	svc   *link.Service
	rt    *Runtime
	e     *engine.Engine
	addr  string // TLS listener
	plain string // plain listener
	logs  *logBuffer
	stop  func() // stops the node before the test ends; may be called more than once
	dir   string // its data directory
	// crash ends the node the way a kill of its process would, as far as a test can:
	// its listeners and control connections close, it dials nothing any more, and it
	// tells nobody; its sessions are not shut down. The connections it accepted stay
	// open (the other node's dropAccepted closes those between the two).
	crash func()

	amu      sync.Mutex
	accepted []net.Conn // every connection its listeners for peers accepted
}

// trackListener notes the connections a node accepts from its peers.
type trackListener struct {
	net.Listener
	tn *testNode
}

func (l trackListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.tn.amu.Lock()
		l.tn.accepted = append(l.tn.accepted, c)
		l.tn.amu.Unlock()
	}
	return c, err
}

// dropAccepted closes every connection the node accepted from its peers so far.
func (tn *testNode) dropAccepted() {
	tn.amu.Lock()
	defer tn.amu.Unlock()
	for _, c := range tn.accepted {
		c.Close()
	}
	tn.accepted = nil
}

type logBuffer struct {
	mu    sync.Mutex
	lines []string
}

func (l *logBuffer) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

var bg = context.Background()

func startNode(t *testing.T, name string, configure ...func(*Runtime)) *testNode {
	t.Helper()
	return startNodeIn(t, filepath.Join(t.TempDir(), "data"), name, configure...)
}

// startNodeIn is startNode with the data directory given: a second call with the
// directory of a node that has ended starts that node again, as a new process would.
func startNodeIn(t *testing.T, dir, name string, configure ...func(*Runtime)) *testNode {
	t.Helper()
	n, err := node.Open(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	logs := &logBuffer{}
	tn := &testNode{n: n, logs: logs, e: engine.New(engine.DefaultConfig()), dir: dir}
	d := &link.Dialer{Identity: n.Identity}
	tn.rt = &Runtime{Node: n, Engine: tn.e, Dialer: d, Version: "test-" + name, Logf: logs.logf}
	tn.svc = &link.Service{Node: n, Dialer: d, Logf: logs.logf, PeerRemoved: tn.rt.PeerRemoved}
	srv := &link.Server{Identity: n.Identity, PeerByFingerprint: n.PeerByFingerprint, HandlePair: tn.svc.HandlePair, Logf: logs.logf}
	acc := &link.Acceptor{PeerByFingerprint: n.PeerByFingerprint, HandleSub: tn.rt.HandleSub, HandleReverse: tn.rt.HandleReverse,
		HandleControl: tn.rt.HandleControl, LogRefused: srv.LogRefused}
	srv.HandlePeer, srv.HandlePlain = acc.HandlePeer, acc.HandlePlain
	ctx, cancel := context.WithCancel(bg)
	var wg sync.WaitGroup
	listen := func(serve func(context.Context, net.Listener) error) string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := serve(ctx, trackListener{ln, tn}); err != nil {
				t.Errorf("listener of %s: %v", name, err)
			}
		}()
		return ln.Addr().String()
	}
	tn.addr = listen(srv.ServeTLS)
	tn.plain = listen(srv.ServePlain)
	tn.svc.Advertise = []string{tn.addr}
	for _, f := range configure {
		f(tn.rt)
	}
	tn.rt.Start(ctx)
	var once, closed sync.Once
	end := func() { cancel(); wg.Wait(); closed.Do(func() { n.Close() }) }
	tn.crash = end
	tn.stop = func() { once.Do(func() { end(); tn.e.Shutdown() }) }
	t.Cleanup(tn.stop)
	return tn
}

// The node looks for peers without a control connection every two seconds; the tests
// do not wait that long.
func init() { superviseInterval = 50 * time.Millisecond }

// pairNodes pairs joiner with maker: joiner gets a path to maker, in the tls tier with
// subs sub-connections; maker gets none to joiner.
func pairNodes(t *testing.T, maker, joiner *testNode, subs int) {
	t.Helper()
	text, _, err := maker.svc.CreatePairingCode(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	p, err := joiner.svc.Join(bg, text, "")
	if err != nil {
		t.Fatal(err)
	}
	path := p.Paths[0]
	path.Subs = subs
	if _, err := joiner.svc.SetPeerPaths(p.Name, []node.Path{path}); err != nil {
		t.Fatal(err)
	}
}

// freePorts returns the first of n consecutive TCP ports on loopback that are free now.
func freePorts(t *testing.T, n int) int {
	t.Helper()
	for try := 0; try < 50; try++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		first := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		ok := first+n-1 <= 65535
		var held []net.Listener
		for i := 0; ok && i < n; i++ {
			l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(first+i))
			if err != nil {
				ok = false
				break
			}
			held = append(held, l)
		}
		for _, l := range held {
			l.Close()
		}
		if ok {
			return first
		}
	}
	t.Fatalf("no %d consecutive free ports", n)
	return 0
}

// arrival is what a sink received on one connection.
type arrival struct {
	port int // the sink's port the connection came to
	n    int64
	sum  [sha256.Size]byte
	err  error
}

// sinks listens on n consecutive loopback ports from first and reports every connection
// when it ends.
func sinks(t *testing.T, first, n int) chan arrival {
	t.Helper()
	got := make(chan arrival, 64)
	for i := 0; i < n; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(first+i))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					h := sha256.New()
					k, err := io.Copy(h, c)
					c.Close()
					a := arrival{port: first + i, n: k, err: err}
					copy(a.sum[:], h.Sum(nil))
					got <- a
				}()
			}
		}()
	}
	return got
}

// send writes size random bytes to addr, closes its side and returns their hash and the
// error the connection ended with on this side (nil: the far end closed cleanly).
func send(t *testing.T, addr string, size int) ([sha256.Size]byte, error) {
	t.Helper()
	data := make([]byte, size)
	rand.Read(data)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		// a listener that resets at once can make even the connect fail
		return sha256.Sum256(data), err
	}
	defer c.Close()
	if _, err := c.Write(data); err != nil {
		return sha256.Sum256(data), err
	}
	c.(*net.TCPConn).CloseWrite()
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, err = io.Copy(io.Discard, c)
	return sha256.Sum256(data), err
}

func wait(t *testing.T, got chan arrival) arrival {
	t.Helper()
	select {
	case a := <-got:
		return a
	case <-time.After(30 * time.Second):
		t.Fatal("nothing arrived at the destination")
		return arrival{}
	}
}

func loop(port int) string { return "127.0.0.1:" + strconv.Itoa(port) }

func statusOf(t *testing.T, tn *testNode, name string) RuleStatus {
	t.Helper()
	for _, st := range tn.rt.Rules() {
		if st.Rule.Name == name {
			return st
		}
	}
	t.Fatalf("no rule %q", name)
	return RuleStatus{}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("it did not happen: %s", what)
		}
	}
}

// A rule carries a connection to the peer's side, in the tls tier and in the off tier,
// and the bytes arrive unchanged.
func TestRuleCarriesData(t *testing.T) {
	a, b := startNode(t, "alpha"), startNode(t, "beta")
	pairNodes(t, a, b, 4)
	dst := freePorts(t, 1)
	got := sinks(t, dst, 1)
	in := freePorts(t, 1)
	if err := b.rt.AddRule(node.Rule{Name: "r", Listen: loop(in), Destination: loop(dst), Peer: "alpha", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, b, "r"); !st.Listening || st.Error != "" {
		t.Fatalf("status after adding: %+v", st)
	}
	for _, tier := range []string{node.SecurityTLS, node.SecurityOff} {
		if tier == node.SecurityOff {
			p, _ := b.n.Peer("alpha")
			path := p.Paths[0]
			path.Addr, path.Security = a.plain, node.SecurityOff
			if _, err := b.svc.SetPeerPaths("alpha", []node.Path{path}); err != nil {
				t.Fatal(err)
			}
		}
		sum, err := send(t, loop(in), 6<<20)
		if err != nil {
			t.Fatalf("%s: the sending side ended with %v", tier, err)
		}
		if arr := wait(t, got); arr.n != 6<<20 || arr.sum != sum || arr.err != nil {
			t.Fatalf("%s: %d bytes arrived (%v), or they differ from what was sent", tier, arr.n, arr.err)
		}
	}
}

// A session shows up with its rule and peer while it runs, on both nodes.
func TestSessionsCarryRuleAndPeer(t *testing.T) {
	a, b := startNode(t, "alpha"), startNode(t, "beta")
	pairNodes(t, a, b, 2)
	dst := freePorts(t, 1)
	got := sinks(t, dst, 1)
	in := freePorts(t, 1)
	if err := b.rt.AddRule(node.Rule{Name: "r", Listen: loop(in), Destination: loop(dst), Peer: "alpha", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", loop(in))
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("hello"))
	eventually(t, "the session is listed on both nodes", func() bool {
		return len(a.e.SessionList()) == 1 && len(b.e.SessionList()) == 1
	})
	sb, sa := b.e.SessionList()[0], a.e.SessionList()[0]
	if sb.Rule != "r" || sb.Peer != "alpha" || !sb.Client || sb.Destination != loop(dst) {
		t.Fatalf("on the carrying node: %+v", sb)
	}
	if sa.Rule != "" || sa.Peer != "beta" || sa.Client {
		t.Fatalf("on the other node: %+v", sa)
	}
	if st := statusOf(t, b, "r"); st.Sessions != 1 {
		t.Fatalf("the rule reports %d sessions", st.Sessions)
	}
	c.(*net.TCPConn).CloseWrite()
	if arr := wait(t, got); arr.n != 5 {
		t.Fatalf("%d bytes arrived", arr.n)
	}
	c.Close()
	eventually(t, "the session ends", func() bool { return statusOf(t, b, "r").Sessions == 0 })
}

// A rule over a range of ports: each port goes to the port of the destination range
// that corresponds to it.
func TestPortRangeRule(t *testing.T) {
	a, b := startNode(t, "alpha"), startNode(t, "beta")
	pairNodes(t, a, b, 2)
	dst := freePorts(t, 3)
	got := sinks(t, dst, 3)
	in := freePorts(t, 3)
	rule := node.Rule{Name: "range", Listen: fmt.Sprintf("127.0.0.1:%d-%d", in, in+2), Destination: fmt.Sprintf("127.0.0.1:%d-%d", dst, dst+2), Peer: "alpha", Enabled: true}
	if err := b.rt.AddRule(rule); err != nil {
		t.Fatal(err)
	}
	for i := 2; i >= 0; i-- {
		sum, err := send(t, loop(in+i), 100000+i)
		if err != nil {
			t.Fatal(err)
		}
		arr := wait(t, got)
		if arr.port != dst+i || arr.n != int64(100000+i) || arr.sum != sum {
			t.Fatalf("port %d of the range arrived at port %d (want %d), %d bytes", in+i, arr.port, dst+i, arr.n)
		}
	}
}

// Rules change while a transfer runs: adding and removing another rule does not touch
// it, and neither does removing its own rule -- the session runs to its end; only new
// connections are refused.
func TestRulesChangeWhileCarrying(t *testing.T) {
	a, b := startNode(t, "alpha"), startNode(t, "beta")
	pairNodes(t, a, b, 3)
	dst := freePorts(t, 1)
	got := sinks(t, dst, 1)
	in := freePorts(t, 1)
	if err := b.rt.AddRule(node.Rule{Name: "one", Listen: loop(in), Destination: loop(dst), Peer: "alpha", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	b.rt.mu.Lock()
	before := b.rt.running["one"].lns[0]
	b.rt.mu.Unlock()

	c, err := net.Dial("tcp", loop(in))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	h := sha256.New()
	piece := make([]byte, 1<<20)
	write := func(n int) {
		for i := 0; i < n; i++ {
			rand.Read(piece)
			h.Write(piece)
			if _, err := c.Write(piece); err != nil {
				t.Fatalf("the transfer broke: %v", err)
			}
		}
	}
	write(3)
	// another rule comes and goes
	in2 := freePorts(t, 1)
	two := node.Rule{Name: "two", Listen: loop(in2), Destination: loop(dst), Peer: "alpha", Enabled: true}
	if err := b.rt.AddRule(two); err != nil {
		t.Fatal(err)
	}
	if !statusOf(t, b, "two").Listening {
		t.Fatal("the second rule does not listen")
	}
	write(3)
	two.Enabled = false
	if err := b.rt.ReplaceRule("two", two); err != nil {
		t.Fatal(err)
	}
	if statusOf(t, b, "two").Listening {
		t.Fatal("a disabled rule still listens")
	}
	if _, err := net.DialTimeout("tcp", loop(in2), time.Second); err == nil {
		t.Fatal("a disabled rule still takes connections")
	}
	if err := b.rt.RemoveRule("two"); err != nil {
		t.Fatal(err)
	}
	b.rt.mu.Lock()
	same := b.rt.running["one"] != nil && b.rt.running["one"].lns[0] == before
	b.rt.mu.Unlock()
	if !same {
		t.Fatal("the listener of a rule that did not change was replaced")
	}
	write(3)
	// its own rule goes: the session stays
	if err := b.rt.RemoveRule("one"); err != nil {
		t.Fatal(err)
	}
	if _, err := net.DialTimeout("tcp", loop(in), time.Second); err == nil {
		t.Fatal("a removed rule still takes connections")
	}
	write(3)
	c.(*net.TCPConn).CloseWrite()
	arr := wait(t, got)
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	if arr.n != 12<<20 || arr.sum != sum {
		t.Fatalf("%d bytes arrived, or they differ from what was sent", arr.n)
	}
}

// A rule to a peer that this node has no path to, and that is not connected to this
// node either, resets its connections and says why.
func TestRuleWithoutPath(t *testing.T) {
	a, b := startNode(t, "alpha"), startNode(t, "beta")
	pairNodes(t, a, b, 2) // alpha made the code: it has no path to beta
	eventually(t, "beta connects to alpha", func() bool { return a.rt.PeerStatus("beta").Online })
	b.stop()
	eventually(t, "alpha sees beta go", func() bool { return !a.rt.PeerStatus("beta").Online })
	in := freePorts(t, 1)
	if err := a.rt.AddRule(node.Rule{Name: "back", Listen: loop(in), Destination: "127.0.0.1:9", Peer: "beta", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, a, "back"); !st.Listening || st.Error != "" {
		t.Fatalf("before any connection: %+v", st)
	}
	if _, err := send(t, loop(in), 1000); err == nil {
		t.Fatal("the connection ended cleanly, want a reset")
	}
	eventually(t, "the rule's status names the reason", func() bool {
		e := statusOf(t, a, "back").Error
		return strings.Contains(e, "no path to peer") && strings.Contains(e, "not connected to this node")
	})
	if len(a.e.SessionList()) != 0 {
		t.Fatal("a session was created")
	}
	// rules that cannot be: unknown peer, taken port
	if err := a.rt.AddRule(node.Rule{Name: "x", Listen: loop(freePorts(t, 1)), Destination: "127.0.0.1:9", Peer: "nobody", Enabled: true}); err == nil {
		t.Fatal("a rule to an unknown peer was stored")
	}
	if err := a.rt.AddRule(node.Rule{Name: "y", Listen: loop(in), Destination: "127.0.0.1:9", Peer: "beta", Enabled: true}); err == nil {
		t.Fatal("a rule on a port another rule has was stored")
	}
}

// A rule whose port something else holds reports that and leaves the other rules alone;
// it starts when the port is free and the rules are applied again.
func TestRuleListenFails(t *testing.T) {
	a, b := startNode(t, "alpha"), startNode(t, "beta")
	pairNodes(t, a, b, 2)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.rt.AddRule(node.Rule{Name: "busy", Listen: busy.Addr().String(), Destination: "127.0.0.1:9", Peer: "alpha", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	in := freePorts(t, 1)
	if err := b.rt.AddRule(node.Rule{Name: "fine", Listen: loop(in), Destination: "127.0.0.1:9", Peer: "alpha", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, b, "busy"); st.Listening || !strings.Contains(st.Error, "listening on") {
		t.Fatalf("the rule on a taken port: %+v", st)
	}
	if st := statusOf(t, b, "fine"); !st.Listening || st.Error != "" {
		t.Fatalf("the other rule: %+v", st)
	}
	busy.Close()
	b.rt.Apply()
	if st := statusOf(t, b, "busy"); !st.Listening || st.Error != "" {
		t.Fatalf("after the port became free: %+v", st)
	}
}

// Ending a pairing resets the sessions with that peer, on the node that ends it,
// whichever end of them it is; a sub-connection that arrives afterwards finds nothing.
func TestPeerRemovedResetsSessions(t *testing.T) {
	for _, who := range []string{"the carrying node", "the other node"} {
		t.Run(who, func(t *testing.T) {
			a, b := startNode(t, "alpha"), startNode(t, "beta")
			pairNodes(t, a, b, 2)
			dst := freePorts(t, 1)
			got := sinks(t, dst, 1)
			in := freePorts(t, 1)
			if err := b.rt.AddRule(node.Rule{Name: "r", Listen: loop(in), Destination: loop(dst), Peer: "alpha", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			c, err := net.Dial("tcp", loop(in))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.Write([]byte("some bytes"))
			eventually(t, "the session is up on both nodes", func() bool {
				return len(a.e.SessionList()) == 1 && len(b.e.SessionList()) == 1
			})
			if who == "the carrying node" {
				err = b.svc.RemovePeer("alpha")
			} else {
				err = a.svc.RemovePeer("beta")
			}
			if err != nil {
				t.Fatal(err)
			}
			// the application's connection is reset, not closed cleanly
			c.SetReadDeadline(time.Now().Add(20 * time.Second))
			if _, err := c.Read(make([]byte, 1)); err == nil || err == io.EOF {
				t.Fatalf("the application connection ended with %v, want a reset", err)
			}
			eventually(t, "no session is left on either node", func() bool {
				return len(a.e.SessionList()) == 0 && len(b.e.SessionList()) == 0
			})
			// the destination saw a cut-off stream, not a complete one
			if arr := wait(t, got); arr.err == nil {
				t.Fatalf("the destination's connection ended cleanly after %d bytes", arr.n)
			}
			if who != "the carrying node" {
				return
			}
			// new connections of the rule get nowhere
			if _, err := send(t, loop(in), 10); err == nil {
				t.Fatal("after the pairing ended, a connection of the rule ended cleanly")
			}
			if st := statusOf(t, b, "r"); !strings.Contains(st.Error, "not paired") {
				t.Fatalf("the rule's status after the pairing ended: %+v", st)
			}
			select {
			case arr := <-got:
				t.Fatalf("after the pairing ended, %d bytes arrived at the destination", arr.n)
			case <-time.After(300 * time.Millisecond):
			}
		})
	}
}

// The allow list of a peer bounds where its sessions may go.
func TestPeerAllowList(t *testing.T) {
	a, b := startNode(t, "alpha"), startNode(t, "beta")
	pairNodes(t, a, b, 2)
	dst := freePorts(t, 2)
	got := sinks(t, dst, 2)
	in := freePorts(t, 2)
	rule := node.Rule{Name: "r", Listen: fmt.Sprintf("127.0.0.1:%d-%d", in, in+1), Destination: fmt.Sprintf("127.0.0.1:%d-%d", dst, dst+1), Peer: "alpha", Enabled: true}
	if err := b.rt.AddRule(rule); err != nil {
		t.Fatal(err)
	}
	allow := fmt.Sprintf("127.0.0.1/32:%d", dst)
	if _, err := a.svc.SetPeerOptions("beta", &allow, nil); err != nil {
		t.Fatal(err)
	}
	sum, err := send(t, loop(in), 5000)
	if err != nil {
		t.Fatal(err)
	}
	if arr := wait(t, got); arr.port != dst || arr.sum != sum {
		t.Fatalf("the allowed destination: %+v", arr)
	}
	if _, err := send(t, loop(in+1), 5000); err == nil {
		t.Fatal("a connection to a destination outside the allow list ended cleanly")
	}
	select {
	case arr := <-got:
		t.Fatalf("%d bytes arrived at a destination outside the allow list", arr.n)
	case <-time.After(300 * time.Millisecond):
	}
}

// A node opens the sub-connections to a peer a few at a time: no more than MaxHandshakes
// are in their handshake at once.
func TestHandshakesAreStaggered(t *testing.T) {
	a, b := startNode(t, "alpha"), startNode(t, "beta")
	pairNodes(t, a, b, 2)
	// a listener that accepts and never answers: every handshake to it hangs
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	var open, most atomic.Int64
	go func() {
		for {
			c, err := silent.Accept()
			if err != nil {
				return
			}
			n := open.Add(1)
			for {
				m := most.Load()
				if n <= m || most.CompareAndSwap(m, n) {
					break
				}
			}
			go func() {
				io.Copy(io.Discard, c)
				open.Add(-1)
				c.Close()
			}()
		}
	}()
	p, _ := b.n.Peer("alpha")
	one, two := p.Paths[0], p.Paths[0]
	one.Addr, one.Subs = silent.Addr().String(), node.MaxSubs
	two.Name, two.Addr, two.Subs = "second", silent.Addr().String(), node.MaxSubs
	if _, err := b.svc.SetPeerPaths("alpha", []node.Path{one, two}); err != nil {
		t.Fatal(err)
	}
	in := freePorts(t, 1)
	if err := b.rt.AddRule(node.Rule{Name: "r", Listen: loop(in), Destination: "127.0.0.1:9", Peer: "alpha", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // two sessions: the bound is per peer, not per session
		c, err := net.Dial("tcp", loop(in))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
	}
	eventually(t, "the handshakes reach the bound", func() bool { return open.Load() == MaxHandshakes })
	time.Sleep(500 * time.Millisecond)
	if m := most.Load(); m != MaxHandshakes {
		t.Fatalf("%d connections were in their handshake at once, want at most %d", m, MaxHandshakes)
	}
	// ending the pairing gives the waiting dials up at once
	if err := b.svc.RemovePeer("alpha"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the hanging handshakes are given up", func() bool { return open.Load() == 0 })
}

func TestMatchTransparent(t *testing.T) {
	rules := []node.Rule{
		{Name: "fixed", Listen: ":1", Destination: "192.0.2.1:1", Peer: "p", Enabled: true},
		{Name: "off", Match: "198.51.100.0/24", Peer: "p0", Enabled: false},
		{Name: "ssh", Match: "198.51.100.0/24:22", Peer: "p1", Enabled: true},
		{Name: "net", Match: "198.51.100.0/24, [2001:db8::/32]:873", Peer: "p2", Enabled: true},
		{Name: "all", Match: "0.0.0.0/0", Peer: "p3", Enabled: true},
	}
	for _, c := range []struct {
		ip   string
		port int
		want string
	}{
		{"198.51.100.7", 22, "ssh"},  // the first that matches, not the widest
		{"198.51.100.7", 873, "net"}, // the disabled one before it does not count
		{"2001:db8::5", 873, "net"},
		{"2001:db8::5", 22, ""},
		{"203.0.113.9", 443, "all"},
	} {
		r, ok := MatchTransparent(rules, net.ParseIP(c.ip), c.port)
		if (c.want == "") == ok || r.Name != c.want {
			t.Errorf("%s:%d matched %q (%v), want %q", c.ip, c.port, r.Name, ok, c.want)
		}
	}
}
