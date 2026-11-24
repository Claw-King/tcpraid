package forward

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/control"
	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/node"
)

// relay passes connections on to a target, like a forwarding host, and holds back what
// goes from the side that connects to the target: each connection to its own rate, or
// all of them together to one rate, as something that puts them into one would.
type relay struct {
	addr   string
	rate   float64 // bytes a second
	shared bool

	mu   sync.Mutex
	next time.Time // shared: when the next bytes may go
}

func startRelay(t *testing.T, target string, rate float64, shared bool) *relay {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	r := &relay{addr: ln.Addr().String(), rate: rate, shared: shared}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				up, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer up.Close()
				go func() { io.Copy(c, up); c.Close() }()
				r.pace(up, c)
			}()
		}
	}()
	return r
}

// pace copies src to dst no faster than the relay's rate.
func (r *relay) pace(dst, src net.Conn) {
	buf := make([]byte, 16<<10)
	own := time.Now()
	for {
		n, err := src.Read(buf)
		if n > 0 {
			cost := time.Duration(float64(n) / r.rate * float64(time.Second))
			var at time.Time
			if r.shared {
				r.mu.Lock()
				r.next = laterOf(r.next, time.Now()).Add(cost)
				at = r.next
				r.mu.Unlock()
			} else {
				own = laterOf(own, time.Now()).Add(cost)
				at = own
			}
			time.Sleep(time.Until(at))
			if _, err := dst.Write(buf[:n]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// testPair makes alpha and beta, where beta has one path to alpha with subs
// sub-connections that goes through a relay of the given kind, and waits until they are
// connected. The nodes have a small window, so a test does not fill 128 MB.
func testPair(t *testing.T, subs int, rate float64, shared bool) (a, b *testNode) {
	t.Helper()
	testWindow = 4 << 20
	defer func() { testWindow = 0 }()
	a, b = startNode(t, "alpha"), startNode(t, "beta")
	pairNodes(t, a, b, subs)
	r := startRelay(t, a.addr, rate, shared)
	p, _ := b.n.Peer("alpha")
	path := p.Paths[0]
	path.Addr = r.addr
	if _, err := b.svc.SetPeerPaths("alpha", []node.Path{path}); err != nil {
		t.Fatal(err)
	}
	// connected, and each has the other's hello: a test asked for before that is refused
	eventually(t, "the two nodes are connected", func() bool {
		sa, sb := a.rt.PeerStatus("beta"), b.rt.PeerStatus("alpha")
		return sa.Online && sb.Online && sa.Version != "" && sb.Version != ""
	})
	return a, b
}

// noTestLeft waits until neither node holds a session any more and the receiving end has
// given back the places of its sinks.
func noTestLeft(t *testing.T, nodes ...*testNode) {
	t.Helper()
	eventually(t, "no session of the test is left", func() bool {
		for _, tn := range nodes {
			tn.rt.mu.Lock()
			sinks := tn.rt.sinks
			tn.rt.mu.Unlock()
			if sinks != 0 || len(tn.e.SessionList()) != 0 {
				return false
			}
		}
		return true
	})
}

// Each connection is held to its own rate, as on a line where a connection is slow and
// several add up: the sub-connections together carry a multiple of what one carries, and
// the test says so. Run from the node that holds the path.
func TestPathTestSeesSubConnectionsAddUp(t *testing.T) {
	a, b := testPair(t, 4, 2<<20, false)
	res, err := b.rt.TestPeer(bg, "alpha", TestOptions{Seconds: 3})
	if err != nil {
		t.Fatalf("%v\n%s\n%s", err, a.logs, b.logs)
	}
	if res.PeerDials || len(res.Paths) != 1 {
		t.Fatalf("result %+v", res)
	}
	p := res.Paths[0]
	t.Logf("one %.2f MB/s, all %.2f MB/s", p.One/(1<<20), p.All/(1<<20))
	if p.Error != "" || p.Name != "default" || p.Subs != 4 {
		t.Fatalf("path %+v", p)
	}
	// 2 MB/s a connection: one carries about 2, four about 8. A slow machine carries
	// less of both; only more than the relay lets through would be wrong
	if p.One <= 0 || p.One > 3<<20 {
		t.Errorf("one sub-connection carried %.2f MB/s through a relay that lets 2 MB/s through each", p.One/(1<<20))
	}
	if p.LittleGain || p.All < 2*p.One {
		t.Errorf("four sub-connections carried %.2f MB/s and one %.2f MB/s: want well over twice as much, and no warning", p.All/(1<<20), p.One/(1<<20))
	}
	noTestLeft(t, a, b)
}

// All connections together are held to one rate, as when something on the path puts
// them into one connection: several carry no more than one, and the test says so.
func TestPathTestSeesSubConnectionsGainLittle(t *testing.T) {
	a, b := testPair(t, 4, 4<<20, true)
	res, err := b.rt.TestPeer(bg, "alpha", TestOptions{Seconds: 3})
	if err != nil {
		t.Fatalf("%v\n%s\n%s", err, a.logs, b.logs)
	}
	p := res.Paths[0]
	t.Logf("one %.2f MB/s, all %.2f MB/s", p.One/(1<<20), p.All/(1<<20))
	if p.Error != "" || !p.LittleGain {
		t.Fatalf("path %+v: want the warning that the sub-connections gain little", p)
	}
	if p.One <= 0 || p.One > 5<<20 || p.All > 6<<20 {
		t.Errorf("through a relay that lets 4 MB/s through in all: one %.2f MB/s, four %.2f MB/s", p.One/(1<<20), p.All/(1<<20))
	}
	noTestLeft(t, a, b)
}

// The node without a path runs the test: it learns the paths from its peer, and the peer
// opens the sub-connections, one on the first run and all on the second.
func TestPathTestWhenThePeerDials(t *testing.T) {
	a, b := testPair(t, 4, 2<<20, false)
	res, err := a.rt.TestPeer(bg, "beta", TestOptions{Seconds: 3})
	if err != nil {
		t.Fatalf("%v\n%s\n%s", err, a.logs, b.logs)
	}
	if !res.PeerDials || len(res.Paths) != 1 {
		t.Fatalf("result %+v", res)
	}
	p := res.Paths[0]
	t.Logf("one %.2f MB/s, all %.2f MB/s", p.One/(1<<20), p.All/(1<<20))
	if p.Error != "" || p.Name != "default" || p.Subs != 4 {
		t.Fatalf("path %+v", p)
	}
	// alpha sends, and what goes from alpha to beta is not held back by the relay:
	// only that data arrived, and that one run used fewer sub-connections, is checked
	if p.One <= 0 || p.All <= 0 {
		t.Errorf("rates %v and %v", p.One, p.All)
	}
	noTestLeft(t, a, b)
	// a path that the peer does not have
	if _, err := a.rt.TestPeer(bg, "beta", TestOptions{Seconds: 1, Path: "nope"}); !errors.Is(err, node.ErrNotFound) {
		t.Errorf("a path the peer does not have: %v", err)
	}
}

// The first run of a test that the peer dials for has one data sub-connection, the
// second has all of them.
func TestPathTestRunsUseTheNumberAskedFor(t *testing.T) {
	for _, peerDials := range []bool{false, true} {
		a, b := testPair(t, 3, 2<<20, false)
		tester, other := b, a
		if peerDials {
			tester, other = a, b
		}
		var mu sync.Mutex
		most := 0
		seen := map[int]bool{} // the numbers of sub-connections the test session was seen with
		stop := make(chan struct{})
		watched := make(chan struct{})
		go func() {
			defer close(watched)
			for {
				select {
				case <-stop:
					return
				case <-time.After(20 * time.Millisecond):
				}
				for _, si := range tester.e.SessionList() {
					if si.Test && si.Client && si.Up {
						mu.Lock()
						seen[si.Subs] = true
						most = max(most, si.Subs)
						mu.Unlock()
					}
				}
			}
		}()
		res, err := tester.rt.TestPeer(bg, other.n.Name(), TestOptions{Seconds: 2})
		close(stop)
		<-watched
		if err != nil || res.Paths[0].Error != "" {
			t.Fatalf("peer dials %v: %v %+v", peerDials, err, res)
		}
		// the path carries data only: a run has its data sub-connections and no other
		mu.Lock()
		if !seen[1] || most != 3 {
			t.Errorf("peer dials %v: the test sessions had %v sub-connections, want 1 on the first run and 3 on the second", peerDials, seen)
		}
		mu.Unlock()
		noTestLeft(t, a, b)
		a.stop()
		b.stop()
	}
}

// A peer that does not name the test in its hello (0.2.0 and before) is not tested, and
// nothing is sent to it. Only one test runs at a time. A peer that is not connected, or
// not there, is said to be.
func TestPathTestRefusals(t *testing.T) {
	testWindow = 4 << 20
	a := startNode(t, "alpha")
	old := startNode(t, "old", func(rt *Runtime) { rt.noFeatures = true })
	b := startNode(t, "beta")
	testWindow = 0
	pairNodes(t, old, a, 2) // alpha holds a path to old
	pairNodes(t, a, b, 2)   // beta holds a path to alpha
	// connected, with the hello of each peer in: what a peer can do is known from it
	eventually(t, "connected", func() bool {
		for _, st := range []PeerStatus{a.rt.PeerStatus("old"), b.rt.PeerStatus("alpha"), old.rt.PeerStatus("alpha"), a.rt.PeerStatus("beta")} {
			if !st.Online || st.Version == "" {
				return false
			}
		}
		return true
	})
	// a connection whose hello has not arrived yet: not an old peer, come back later
	b.rt.mu.Lock()
	b.rt.ctl["alpha"].greeted = false
	b.rt.mu.Unlock()
	if _, err := b.rt.TestPeer(bg, "alpha", TestOptions{Seconds: 1}); !errors.Is(err, ErrTestRefused) || !strings.Contains(err.Error(), "has only just come up") {
		t.Errorf("testing a peer whose hello has not arrived: %v", err)
	}
	b.rt.mu.Lock()
	b.rt.ctl["alpha"].greeted = true
	b.rt.mu.Unlock()
	// either way round: the new node does not test an old one
	_, err := a.rt.TestPeer(bg, "old", TestOptions{Seconds: 1})
	if !errors.Is(err, ErrTestRefused) || !strings.Contains(err.Error(), `peer "old" does not have this test (it runs version "test-old"). Both nodes need tcpraid 0.3.0 or later.`) {
		t.Errorf("testing a peer without the feature: %v", err)
	}
	if n := len(old.e.SessionList()) + len(a.e.SessionList()); n != 0 {
		t.Errorf("%d sessions after a test that was refused", n)
	}
	if _, err := a.rt.TestPeer(bg, "nobody", TestOptions{}); !errors.Is(err, node.ErrNotFound) {
		t.Errorf("an unknown peer: %v", err)
	}
	for _, seconds := range []int{-1, 61} {
		if _, err := a.rt.TestPeer(bg, "beta", TestOptions{Seconds: seconds}); !errors.Is(err, node.ErrInvalid) {
			t.Errorf("%d seconds: %v", seconds, err)
		}
	}
	// one at a time
	first := make(chan error, 1)
	go func() {
		_, err := b.rt.TestPeer(bg, "alpha", TestOptions{Seconds: 2})
		first <- err
	}()
	eventually(t, "the first test runs", func() bool {
		b.rt.mu.Lock()
		defer b.rt.mu.Unlock()
		return b.rt.testing
	})
	if _, err := b.rt.TestPeer(bg, "alpha", TestOptions{Seconds: 1}); !errors.Is(err, ErrTestRefused) || !strings.Contains(err.Error(), "another one is running") {
		t.Errorf("a second test while one runs: %v", err)
	}
	if err := <-first; err != nil {
		t.Errorf("the first test: %v", err)
	}
	// a caller that goes away ends the test
	ctx, cancel := context.WithCancel(bg)
	go func() { time.Sleep(700 * time.Millisecond); cancel() }()
	began := time.Now()
	if _, err := b.rt.TestPeer(ctx, "alpha", TestOptions{Seconds: 30}); !errors.Is(err, context.Canceled) {
		t.Errorf("a test whose caller went away: %v", err)
	}
	if d := time.Since(began); d > 10*time.Second {
		t.Errorf("the test went on for %v after its caller left", d)
	}
	noTestLeft(t, a, b)
	// not connected
	b.stop()
	eventually(t, "beta is gone", func() bool { return !a.rt.PeerStatus("beta").Online })
	if _, err := a.rt.TestPeer(bg, "beta", TestOptions{Seconds: 1}); !errors.Is(err, ErrTestRefused) || !strings.Contains(err.Error(), "is not connected to this node") {
		t.Errorf("a peer that is not connected: %v", err)
	}
}

// The receiving end of a test bounds it without help from the sending end: so many
// sessions at a time, so much data, so long.
func TestSinkBoundsATest(t *testing.T) {
	a := startNode(t, "alpha")
	var held []net.Conn
	for i := 0; i < maxSinks; i++ {
		c, why := a.rt.sink()
		if c == nil {
			t.Fatalf("sink %d refused: %s", i, why)
		}
		held = append(held, c)
	}
	if c, why := a.rt.sink(); c != nil || why == "" {
		t.Fatalf("one sink more than the node takes: %v %q", c, why)
	}
	held[0].Close()
	held[0].Close() // twice gives the place back once
	c, _ := a.rt.sink()
	if c == nil {
		t.Fatal("no sink after one was given back")
	}
	if c2, _ := a.rt.sink(); c2 != nil {
		t.Fatal("closing a sink twice gave two places back")
	}
	// more data than a run may bring ends it
	s := c.(*sinkConn)
	s.mu.Lock()
	s.left = 1000
	s.mu.Unlock()
	if n, err := s.Write(make([]byte, 1000)); n != 1000 || err != nil {
		t.Fatalf("writing what is allowed: %d %v", n, err)
	}
	if _, err := s.Write([]byte{1}); err == nil {
		t.Fatal("a byte more than a run may bring was taken")
	}
	read := make(chan error, 1)
	go func() { _, err := s.Read(make([]byte, 1)); read <- err }()
	select {
	case err := <-read:
		if err == nil {
			t.Fatal("reading from a sink that is over gave data")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a sink that took too much still blocks its reader: the session would not end")
	}
	// its time is up: the reader is released, which ends the session
	s2 := held[1].(*sinkConn)
	s2.timer.Reset(50 * time.Millisecond)
	go func() { _, err := s2.Read(make([]byte, 1)); read <- err }()
	select {
	case <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("a sink past its time still blocks its reader")
	}
	for _, c := range held {
		c.Close()
	}
	a.rt.mu.Lock()
	left := a.rt.sinks
	a.rt.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d sinks counted after all were closed", left)
	}
}

// The data of a test is dense and comes in whole buffers; after the total, and after a
// close, nothing more comes.
func TestGenerator(t *testing.T) {
	g := newGenerator(1000)
	buf := make([]byte, 300)
	for i := 0; i < 3; i++ {
		if n, err := g.Read(buf); n != 300 || err != nil {
			t.Fatalf("read %d: %d %v", i, n, err)
		}
	}
	zeros := 0
	for _, b := range buf {
		if b == 0 {
			zeros++
		}
	}
	if zeros > 30 {
		t.Errorf("%d of 300 bytes are zero", zeros)
	}
	if done, given := g.spent(); done || given != 900 {
		t.Errorf("after 900 of 1000 bytes: done %v, given %d", done, given)
	}
	// 100 bytes are left, less than a buffer: none are given, and the read waits
	got := make(chan error, 1)
	go func() { _, err := g.Read(buf); got <- err }()
	eventually(t, "the generator is spent", func() bool { done, _ := g.spent(); return done })
	select {
	case err := <-got:
		t.Fatalf("a read past the total returned (%v)", err)
	case <-time.After(100 * time.Millisecond):
	}
	g.Close()
	if err := <-got; err == nil {
		t.Fatal("a read from a closed generator gave no error")
	}
	if _, err := g.Read(buf); err == nil {
		t.Fatal("a read after the close gave no error")
	}
}

// A run uses the data group of one path, with the number of sub-connections asked for,
// and the control and repair groups of every path.
func TestTestGroups(t *testing.T) {
	groups := []engine.DialGroup{
		{Path: 0, Name: "a", Want: 8},
		{Path: 0, Name: "a", Want: 1, Ctrl: true},
		{Path: 1, Name: "b", Want: 4, Repair: true},
		{Path: 2, Name: "c", Want: 2},
	}
	got := testGroups(groups, 0, 1)
	if len(got) != 3 || got[0].Want != 1 || got[0].Name != "a" || !got[1].Ctrl || !got[2].Repair || got[2].Want != 4 {
		t.Errorf("one sub-connection on the first path: %+v", got)
	}
	if groups[0].Want != 8 {
		t.Error("the groups that were passed in were changed")
	}
	if got := testGroups(groups, 2, 0); len(got) != 3 || got[2].Name != "c" || got[2].Want != 2 {
		t.Errorf("all sub-connections of the third path: %+v", got)
	}
	if got := testGroups(groups, 0, 99); got[0].Want != 8 {
		t.Errorf("more than the path has: %+v", got)
	}
	for _, index := range []int{1, 3, -1} {
		if got := testGroups(groups, index, 1); got != nil {
			t.Errorf("path %d has no data group, got %+v", index, got)
		}
	}
}

// What a node tells its peer about its paths: names, sizes, whether they carry data --
// and no address.
func TestPathsReplyNamesNoAddress(t *testing.T) {
	a := startNode(t, "alpha", quick)
	b := joinBare(t, a, "bare")
	// alpha gets two paths to the bare peer; nothing listens there
	paths := []node.Path{
		{Name: "main", Type: "tcp", Addr: "192.0.2.77:9851", Security: node.SecurityTLS, Roles: []string{node.RoleData, node.RoleControl}, Subs: 6},
		{Name: "side", Type: "tcp", Addr: "192.0.2.78:9851", Security: node.SecurityTLS, Roles: []string{node.RoleRepair}, Subs: 2},
	}
	if _, err := a.svc.SetPeerPaths("bare", paths); err != nil {
		t.Fatal(err)
	}
	cc, _ := b.control(t)
	hello := expect(t, cc, control.TypeHello)
	if !slices.Equal(hello.Features, []string{control.FeaturePathTest, control.FeatureResolve, control.FeatureRulePaths}) {
		t.Errorf("the hello names the features %v", hello.Features)
	}
	if err := cc.Send(control.Message{Type: control.TypePaths, Request: 7}); err != nil {
		t.Fatal(err)
	}
	reply := expect(t, cc, control.TypePathsReply)
	raw, _ := json.Marshal(reply)
	if reply.Request != 7 || string(raw) != `{"type":"paths_reply","request":7,"paths":[{"name":"main","subs":6,"data":true},{"name":"side","subs":2,"data":false}]}` {
		t.Errorf("the answer: %s", raw)
	}
	if strings.Contains(string(raw), "192.0.2") {
		t.Error("the answer names an address")
	}
	// a request for a path that carries no data, or that is not there, is refused
	for _, n := range []int{2, 9} {
		if err := cc.Send(control.Message{Type: control.TypeOpen, Request: uint64(100 + n), Session: "00", TestPath: n, TestSubs: 1}); err != nil {
			t.Fatal(err)
		}
		if f := expect(t, cc, control.TypeOpenFail); f.Request != uint64(100+n) || !strings.Contains(f.Reason, "no such path") {
			t.Errorf("test of path %d: %+v", n, f)
		}
	}
}

// A peer that takes no more test sessions says so, and the test passes the reason on.
func TestPathTestSaysWhyThePeerDidNotTakeIt(t *testing.T) {
	a, b := testPair(t, 2, 2<<20, false)
	a.rt.mu.Lock()
	a.rt.sinks = maxSinks // as if four tests of other peers were running
	a.rt.mu.Unlock()
	res, err := b.rt.TestPeer(bg, "alpha", TestOptions{Seconds: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := res.Paths[0].Error, `peer "alpha" did not take the test: the node takes as many path tests as it can already`; got != want {
		t.Errorf("the path says %q, want %q\n%s", got, want, b.logs)
	}
	if res.Paths[0].One != 0 || res.Paths[0].All != 0 {
		t.Errorf("rates of a path that was not measured: %+v", res.Paths[0])
	}
	a.rt.mu.Lock()
	a.rt.sinks = 0
	a.rt.mu.Unlock()
	noTestLeft(t, a, b)
}
