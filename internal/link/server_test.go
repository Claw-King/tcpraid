package link

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/node"
	"github.com/Claw-King/tcpraid/internal/pair"
)

// failingListener fails the first n calls of Accept the way a process out of file
// descriptors does, then accepts from the listener it wraps.
type failingListener struct {
	net.Listener
	left  atomic.Int32
	calls atomic.Int32
}

func (l *failingListener) Accept() (net.Conn, error) {
	l.calls.Add(1)
	if l.left.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: syscall.EMFILE}
	}
	return l.Listener.Accept()
}

// An Accept that fails does not end the listener: it is logged, tried again after a
// pause, and connections are taken again once it works.
func TestServeSurvivesAcceptErrors(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := &failingListener{Listener: inner}
	ln.left.Store(4)
	logs := &logBuffer{}
	srv := &Server{Logf: logs.logf, HandlePlain: func(c net.Conn, _ func() bool) { echoByte(c) }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- srv.ServePlain(ctx, ln) }()

	c, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := askByte(c); err != nil {
		t.Fatalf("after four failed accepts the listener does not answer: %v", err)
	}
	// four pauses: 5 + 10 + 20 + 40 ms
	if d := time.Since(start); d < 75*time.Millisecond {
		t.Errorf("the failed accepts were retried without pausing (%v)", d)
	}
	if !logs.has("too many open files") {
		t.Errorf("the failed accepts were not logged:\n%s", logs)
	}
	select {
	case err := <-done:
		t.Fatalf("the listener ended: %v", err)
	default:
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("the listener ended with %v, want nil", err)
	}
}

// A listener that something else closed is gone for good: that is an error.
func TestServeEndsWhenListenerIsClosed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{}
	done := make(chan error, 1)
	go func() { done <- srv.ServePlain(context.Background(), ln) }()
	ln.Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("got %v, want an error matching net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the listener did not end")
	}
}

// closedSoon reports whether the other end closes c within a second.
func closedSoon(c net.Conn) bool {
	c.SetReadDeadline(time.Now().Add(time.Second))
	_, err := c.Read(make([]byte, 1))
	return errors.Is(err, io.EOF) || pair.IsReset(err)
}

// Connections that have not proved who they are are bounded, and a newcomer is never the
// one refused: a waiting connection gives way. Someone who holds connections open
// without finishing the handshake therefore cannot keep a real node out.
func TestWaitingConnectionGivesWayToNewcomer(t *testing.T) {
	a := startNode(t, "alpha", nil, func(s *Server) { s.MaxUnauthenticated = 2; s.FirstByteTimeout = time.Minute })

	idle := func() net.Conn {
		t.Helper()
		c, err := net.Dial("tcp", a.addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	stillOpen := func(c net.Conn) bool {
		c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		_, err := c.Read(make([]byte, 1))
		return errors.Is(err, os.ErrDeadlineExceeded)
	}
	first, second := idle(), idle()
	time.Sleep(100 * time.Millisecond) // both are accepted and wait, silent
	third := idle()
	if !closedSoon(first) {
		t.Fatal("the oldest waiting connection was kept when a third arrived")
	}
	if !stillOpen(second) || !stillOpen(third) {
		t.Fatal("a newer connection was closed instead of the oldest")
	}
	if !a.logs.has("to make room") {
		t.Errorf("the closed connection was not logged:\n%s", a.logs)
	}
	// that one line is all: the handler of the closed connection adds none of its own
	time.Sleep(100 * time.Millisecond)
	if a.logs.has("use of closed network connection") {
		t.Errorf("a connection closed to make room was logged a second time:\n%s", a.logs)
	}
	// with the listener still full of waiting connections, a pairing goes through at once
	b := startNode(t, "beta", nil)
	text, _, err := a.svc.CreatePairingCode(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.svc.Join(bg, text, ""); err != nil {
		t.Fatalf("a pairing was kept out by connections that only wait: %v", err)
	}
}

// The choice of the connection that gives way, on its own.
func TestWaitingVictim(t *testing.T) {
	conn := func(addr string) net.Conn { return addrConn{addr: addr} }
	w := newWaiting(16) // a source's share is 2
	admit := func(addr string) (func() bool, string) {
		leave, ev := w.admit(conn(addr))
		if ev == nil {
			return leave, ""
		}
		return leave, ev.RemoteAddr().String()
	}
	// sixteen sources, one connection each: the set is full, nothing gives way yet
	var leaveFirst func() bool
	for i := 1; i <= 16; i++ {
		leave, ev := admit(fmt.Sprintf("192.0.2.%d:1", i))
		if ev != "" {
			t.Fatalf("evicted %s below the bound", ev)
		}
		if i == 1 {
			leaveFirst = leave
		}
	}
	// the next arrival pushes out the one that has waited longest
	if _, ev := admit("198.51.100.1:1"); ev != "192.0.2.1:1" {
		t.Fatalf("evicted %q, want the oldest connection", ev)
	}
	if leaveFirst() {
		t.Fatal("leave reports an evicted connection as still waiting")
	}
	// a source within its share is treated like any other: still the oldest of all goes
	if _, ev := admit("198.51.100.1:2"); ev != "192.0.2.2:1" {
		t.Fatalf("evicted %q, want the oldest connection", ev)
	}
	// a third connection puts that source over its share: from now on it gives way,
	// its oldest first, although others have waited longer
	if _, ev := admit("198.51.100.1:3"); ev != "192.0.2.3:1" {
		t.Fatalf("evicted %q, want the oldest connection (the source was not over its share before this one)", ev)
	}
	if _, ev := admit("203.0.113.1:1"); ev != "198.51.100.1:1" {
		t.Fatalf("evicted %q, want the oldest of the source over its share", ev)
	}
	// back within its share: the oldest of all again
	if _, ev := admit("203.0.113.2:1"); ev != "192.0.2.4:1" {
		t.Fatalf("evicted %q, want the oldest connection", ev)
	}
	if w.n != 16 {
		t.Fatalf("%d waiting, want the bound of 16", w.n)
	}
	// leaving frees a place, once
	leave, ev := admit("203.0.113.3:1")
	if ev == "" || w.n != 16 {
		t.Fatalf("evicted %q with %d waiting", ev, w.n)
	}
	if !leave() || leave() || w.n != 15 {
		t.Fatalf("leave: want true then false and 15 waiting, have %d waiting", w.n)
	}
}

// What the rule promises a node whose handshake is in progress, whatever the others do
// and send.
func TestWaitingKeepsANodeInItsHandshake(t *testing.T) {
	const limit = 64 // a source's share is 8
	conn := func(addr string) net.Conn { return addrConn{addr: addr} }
	const node = "192.0.2.9:1"
	// admissions until the node's connection is evicted, with others arriving from next()
	untilEvicted := func(t *testing.T, w *waiting, max int, next func(i int) string) int {
		t.Helper()
		for i := 1; i <= max; i++ {
			if _, ev := w.admit(conn(next(i))); ev != nil && ev.RemoteAddr().String() == node {
				return i
			}
		}
		return 0
	}

	t.Run("one source, as fast as it likes", func(t *testing.T) {
		w := newWaiting(limit)
		w.admit(conn(node))
		if at := untilEvicted(t, w, 100*limit, func(i int) string { return fmt.Sprintf("198.51.100.1:%d", 1024+i) }); at != 0 {
			t.Fatalf("the node was evicted by a single source, at admission %d", at)
		}
	})
	t.Run("a few sources", func(t *testing.T) {
		// seven sources can hold 56 of the 64 places within their shares: they never fill
		// the set without one of them going over its share
		w := newWaiting(limit)
		w.admit(conn(node))
		if at := untilEvicted(t, w, 100*limit, func(i int) string { return fmt.Sprintf("198.51.100.%d:%d", 1+i%7, 1024+i) }); at != 0 {
			t.Fatalf("the node was evicted by seven sources, at admission %d", at)
		}
	})
	t.Run("as many sources as places", func(t *testing.T) {
		w := newWaiting(limit)
		w.admit(conn(node))
		at := untilEvicted(t, w, 100*limit, func(i int) string { return fmt.Sprintf("[2001:db8:%x::1]:1024", i) })
		if at != limit {
			t.Fatalf("the node was evicted at admission %d, want exactly %d: a whole set of newer connections", at, limit)
		}
	})
	t.Run("many sources, and some connections from the node's own address", func(t *testing.T) {
		// the set is full of single connections from other addresses, and every
		// sixteenth arrival comes from the node's address: that source stays within its
		// share, so the node still goes only after a whole set of newer connections
		w := newWaiting(limit)
		for i := 0; i < limit; i++ {
			w.admit(conn(fmt.Sprintf("[2001:db8:a:%x::1]:1024", i)))
		}
		w.admit(conn(node))
		at := untilEvicted(t, w, 100*limit, func(i int) string {
			if i%16 == 0 {
				return fmt.Sprintf("192.0.2.9:%d", 2000+i)
			}
			return fmt.Sprintf("[2001:db8:b:%x::1]:1024", i)
		})
		if at != limit {
			t.Fatalf("the node was evicted at admission %d, want exactly %d", at, limit)
		}
	})
	t.Run("the node opens several connections at once", func(t *testing.T) {
		// eight connections from the node (its share) into a set full of single
		// connections: none of them goes before a whole set of newer ones has arrived
		w := newWaiting(limit)
		for i := 0; i < limit; i++ {
			w.admit(conn(fmt.Sprintf("[2001:db8:a:%x::1]:1024", i)))
		}
		for i := 0; i < 8; i++ {
			w.admit(conn(fmt.Sprintf("192.0.2.9:%d", 3000+i)))
		}
		for i := 1; i <= limit-8; i++ {
			if _, ev := w.admit(conn(fmt.Sprintf("[2001:db8:b:%x::1]:1024", i))); ev != nil && strings.HasPrefix(ev.RemoteAddr().String(), "192.0.2.9:") {
				t.Fatalf("a connection of the node was evicted at admission %d, before a whole set of newer ones", i)
			}
		}
	})
	t.Run("someone at the node's own address goes over the share", func(t *testing.T) {
		// the stated exception: a share's worth of newer connections from the address
		// the node shares is enough. With the eighth of them the source is over its
		// share, and the arrival after that takes its oldest connection, the node's.
		w := newWaiting(limit)
		for i := 0; i < limit-1; i++ {
			w.admit(conn(fmt.Sprintf("[2001:db8:a:%x::1]:1024", i)))
		}
		w.admit(conn(node))
		at := untilEvicted(t, w, 100*limit, func(i int) string { return fmt.Sprintf("192.0.2.9:%d", 4000+i) })
		if at != 9 {
			t.Fatalf("the node was evicted at admission %d, want 9: one more than the share of its source", at)
		}
	})
}

// The bookkeeping agrees with a plain rewrite of the rule under random arrivals and
// departures.
func TestWaitingAgainstReference(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for round := 0; round < 20; round++ {
		limit := 8 + rng.Intn(40)
		w := newWaiting(limit)
		type entry struct {
			addr  string
			src   string
			leave func() bool
		}
		var ref []entry          // in order of arrival
		share := max(limit/8, 1) // the rule's own figure, not the implementation's
		refVictim := func() int {
			count := map[string]int{}
			for _, e := range ref {
				count[e.src]++
			}
			worst, at := 0, -1
			for i, e := range ref { // the first entry of a source is its oldest
				if n := count[e.src]; n > share && n > worst {
					worst, at = n, i
				}
			}
			if at >= 0 {
				return at
			}
			return 0
		}
		for step := 0; step < 4000; step++ {
			if len(ref) > 0 && rng.Intn(3) == 0 {
				i := rng.Intn(len(ref))
				if !ref[i].leave() {
					t.Fatalf("round %d step %d: leave says %s was not waiting", round, step, ref[i].addr)
				}
				ref = append(ref[:i], ref[i+1:]...)
			} else {
				addr := fmt.Sprintf("192.0.2.%d:%d", rng.Intn(1+limit/3), 1000+step)
				want := ""
				if len(ref) >= limit {
					i := refVictim()
					want = ref[i].addr
					ref = append(ref[:i], ref[i+1:]...)
				}
				leave, ev := w.admit(addrConn{addr: addr})
				got := ""
				if ev != nil {
					got = ev.RemoteAddr().String()
				}
				if got != want {
					t.Fatalf("round %d step %d: evicted %q, want %q", round, step, got, want)
				}
				ref = append(ref, entry{addr: addr, src: sourceOf(strAddr(addr)), leave: leave})
			}
			if w.n != len(ref) {
				t.Fatalf("round %d step %d: %d waiting, want %d", round, step, w.n, len(ref))
			}
			if len(w.queue) > 2*w.n+16 {
				t.Fatalf("round %d step %d: the queue holds %d entries for %d waiting", round, step, len(w.queue), w.n)
			}
			for src, s := range w.sources {
				if len(s.queue) > 2*s.n+16 {
					t.Fatalf("round %d step %d: the queue of %s holds %d entries for %d waiting", round, step, src, len(s.queue), s.n)
				}
			}
		}
		for _, e := range ref {
			e.leave()
		}
		if w.n != 0 || len(w.sources) != 0 || len(w.over) != 0 {
			t.Fatalf("round %d: after all left: %d waiting, %d sources, %d over their share", round, w.n, len(w.sources), len(w.over))
		}
	}
}

// Connections that leave out of order do not make the queues grow.
func TestWaitingQueueStaysSmall(t *testing.T) {
	conn := func(addr string) net.Conn { return addrConn{addr: addr} }
	w := newWaiting(1 << 20)
	w.admit(conn("192.0.2.1:1")) // stays at the front
	for i := 0; i < 10000; i++ {
		leave, _ := w.admit(conn("192.0.2.1:2"))
		leave()
	}
	if got := len(w.sources["192.0.2.1"].queue); got > 64 {
		t.Fatalf("the source's queue holds %d entries for one waiting connection", got)
	}
	if got := len(w.queue); got > 64 {
		t.Fatalf("the queue holds %d entries for one waiting connection", got)
	}
	if w.n != 1 {
		t.Fatalf("%d waiting, want 1", w.n)
	}
}

// An IPv6 host has a whole /64 to connect from: it counts as one source.
func TestSourceOf(t *testing.T) {
	for addr, want := range map[string]string{
		"192.0.2.7:4000":                "192.0.2.7",
		"[::ffff:192.0.2.7]:4000":       "192.0.2.7",
		"[2001:db8:1:2::1]:4000":        "2001:db8:1:2::/64",
		"[2001:db8:1:2:aaaa:bbbb::9]:1": "2001:db8:1:2::/64",
		"[2001:db8:1:3::1]:4000":        "2001:db8:1:3::/64",
		"[fe80::1%eth0]:4000":           "fe80::/64",
		"not an address":                "not an address",
	} {
		if got := sourceOf(strAddr(addr)); got != want {
			t.Errorf("sourceOf(%q) = %q, want %q", addr, got, want)
		}
	}
}

// Lines about refused connections cannot be multiplied by whoever causes them: one is
// written, the rest of that second are counted, and the count is written when the
// second ends.
func TestRefusalsAreLoggedSparingly(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	s := &Server{Logf: func(f string, args ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(f, args...))
		mu.Unlock()
	}}
	for i := 0; i < 1000; i++ {
		s.LogRefused("refused a connection from %s", "192.0.2.1:1")
	}
	mu.Lock()
	n := len(lines)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("%d lines for 1000 refusals within a second, want 1", n)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n = len(lines)
		last := lines[n-1]
		mu.Unlock()
		if n == 2 {
			if !strings.Contains(last, "999 more") {
				t.Fatalf("the count line is %q", last)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no count line after the second ended: %q", lines)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// the count does not open another silent second: the next refusal has its own line
	s.LogRefused("refused a connection from %s", "192.0.2.2:1")
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 3 || !strings.Contains(lines[2], "192.0.2.2:1") {
		t.Fatalf("the refusal after the count line was not written: %q", lines)
	}
}

// addrConn is a connection of which only the remote address is used.
type addrConn struct {
	net.Conn
	addr string
}

type strAddr string

func (a strAddr) Network() string { return "tcp" }
func (a strAddr) String() string  { return string(a) }

func (c addrConn) RemoteAddr() net.Addr { return strAddr(c.addr) }

// A connection that sends nothing is closed after FirstByteTimeout; one that has begun
// its handshake has HandshakeTimeout.
func TestSilentConnectionIsClosedSoon(t *testing.T) {
	a := startNode(t, "alpha", nil, func(s *Server) { s.FirstByteTimeout = 150 * time.Millisecond })
	silent, err := net.Dial("tcp", a.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	begun, err := net.Dial("tcp", a.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer begun.Close()
	// the first bytes of a TLS record: a handshake that has started and then stalls
	if _, err := begun.Write([]byte{0x16, 0x03, 0x01}); err != nil {
		t.Fatal(err)
	}
	if !closedSoon(silent) {
		t.Fatal("a silent connection was kept beyond the first-byte timeout")
	}
	begun.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := begun.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("a connection that began its handshake was closed with the silent ones: %v", err)
	}
}

// A peer that TLS has authenticated does not take a place: more peers' connections than
// the bound stay open side by side.
func TestAuthenticatedConnectionsAreNotBounded(t *testing.T) {
	a, b := startNode(t, "alpha", nil), startNode(t, "beta", nil)
	_, onB := pairNodes(t, a, b)
	hold := make(chan struct{})
	arrived := make(chan struct{}, 3)
	srv := &Server{
		Identity:           a.n.Identity,
		PeerByFingerprint:  a.n.PeerByFingerprint,
		MaxUnauthenticated: 1,
		HandlePeer: func(_ node.Peer, c net.Conn) {
			arrived <- struct{}{}
			<-hold
			c.Close()
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.ServeTLS(ctx, ln) }()
	path := onB.Paths[0]
	path.Addr = ln.Addr().String()
	// One after the other: each holds the one place only while its handshake runs, and
	// gives it back when the handler has it. All three then stay open side by side.
	for i := 0; i < 3; i++ {
		c, err := b.svc.Dialer.Dial(bg, onB, path)
		if err != nil {
			t.Fatalf("connection %d: %v", i+1, err)
		}
		defer c.Close()
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatalf("connection %d was not handed to the handler with a bound of one and %d held", i+1, i)
		}
	}
	// Wait reports connections still being handled, and their end
	if srv.Wait(50 * time.Millisecond) {
		t.Fatal("Wait reported no connection in progress while three are held")
	}
	close(hold)
	if !srv.Wait(5 * time.Second) {
		t.Fatal("Wait did not see the handlers end")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// The listener hands out no session tickets: every connection is a full handshake.
func TestNoSessionResumption(t *testing.T) {
	a, b := startNode(t, "alpha", nil), startNode(t, "beta", nil)
	_, onB := pairNodes(t, a, b)
	cache := tls.NewLRUClientSessionCache(4)
	for i := 0; i < 2; i++ {
		c, err := tls.Dial("tcp", a.addr, &tls.Config{
			MinVersion:         tls.VersionTLS13,
			Certificates:       []tls.Certificate{b.n.Identity.TLSCertificate()},
			NextProtos:         []string{ALPN},
			InsecureSkipVerify: true, // the listener's key is not what this test is about
			ClientSessionCache: cache,
			ServerName:         "node",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := checkConn(c, b.n.Identity.FingerprintBytes(), onB.PairSecret); err != nil {
			t.Fatal(err)
		}
		resumed := c.ConnectionState().DidResume
		c.Close()
		if resumed {
			t.Fatalf("connection %d resumed an earlier session", i+1)
		}
	}
}

// A join gives up when its time is over, however the other end stalls, and says so.
func TestJoinTimeLimit(t *testing.T) {
	a, b := startNode(t, "alpha", nil), startNode(t, "beta", nil)
	// something that takes connections and never says a word
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	go func() {
		for {
			c, err := silent.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	text, _, err := a.svc.CreatePairingCode([]string{silent.Addr().String(), silent.Addr().String()}, 0)
	if err != nil {
		t.Fatal(err)
	}
	b.svc.JoinTimeout = 300 * time.Millisecond
	start := time.Now()
	_, err = b.svc.Join(bg, text, "")
	took := time.Since(start)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v, want an error matching ErrTimeout", err)
	}
	if took < 250*time.Millisecond || took > 5*time.Second {
		t.Fatalf("the join took %v with a limit of 300ms", took)
	}
	if len(b.n.Peers()) != 0 {
		t.Fatal("a peer was stored by a join that timed out")
	}
	// the code was not used up: with time enough and the right address it still pairs
	b.svc.JoinTimeout = 0
	if _, err := b.svc.Join(bg, text, a.addr); err != nil {
		t.Fatalf("joining afterwards: %v", err)
	}
}

// A pairing that this node accepted is on record even when the answer never reached the
// other node, here because the connection was closed to make room right after the code
// was redeemed: the node has a new peer, and its log has to say so.
func TestAcceptedPairingIsLoggedWhenTheAnswerIsLost(t *testing.T) {
	a := startNode(t, "alpha", nil, func(s *Server) { s.MaxUnauthenticated = 1; s.FirstByteTimeout = time.Minute })
	b := startNode(t, "beta", nil)
	text, _, err := a.svc.CreatePairingCode(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Just before the code is redeemed, another connection arrives; with room for one
	// waiting connection, the pairing connection is the one closed. The handler only
	// learns of it when it sends the answer.
	var once sync.Once
	a.svc.Now = func() time.Time {
		once.Do(func() {
			c, err := net.Dial("tcp", a.addr)
			if err != nil {
				t.Error(err)
				return
			}
			t.Cleanup(func() { c.Close() })
			if !a.logs.has("to make room") { // waits for the line
				t.Error("the pairing connection was not closed to make room")
			}
		})
		return time.Now()
	}
	if _, err := b.svc.Join(bg, text, ""); err == nil {
		t.Fatal("the joining node reports success although the answer was cut off")
	}
	// The joining node has its answer (a failure) as soon as the connection is closed;
	// the node that made the code is still at work.
	if !a.logs.has(`paired with "beta"`) || !a.logs.has("but the answer did not reach it") {
		t.Fatalf("an accepted pairing whose answer was lost is not in the log:\n%s", a.logs)
	}
	if _, ok := a.n.PeerByFingerprint(b.n.Fingerprint()); !ok {
		t.Fatal("the log reports a new peer that is not stored")
	}
	if strings.Contains(a.logs.String(), "refused pairing code") {
		t.Fatalf("an accepted pairing is logged as refused:\n%s", a.logs)
	}
}

// echoByte reads one byte from conn, sends it back and closes conn.
func echoByte(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	var b [1]byte
	if _, err := io.ReadFull(conn, b[:]); err == nil {
		conn.Write(b[:])
	}
}

// askByte sends one byte on conn and waits for it to come back.
func askByte(conn net.Conn) error {
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	defer conn.SetDeadline(time.Time{})
	if _, err := conn.Write([]byte{0}); err != nil {
		return err
	}
	var b [1]byte
	_, err := io.ReadFull(conn, b[:])
	return err
}
