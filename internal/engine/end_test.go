package engine

import (
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// carry starts route.Carry for a connection to ip:port and returns the application's
// end of it and where the outcome arrives.
func carry(route *Route, ip net.IP, port int, owner Owner) (app net.Conn, done chan End) {
	app, far := net.Pipe()
	done = make(chan End, 1)
	go func() {
		end, _ := route.Carry(far, ip, port, owner, nil)
		done <- end
	}()
	return app, done
}

func ended(t *testing.T, done chan End) End {
	t.Helper()
	select {
	case end := <-done:
		return end
	case <-time.After(20 * time.Second):
		t.Fatal("the session did not end")
		return End{}
	}
}

// Carry says how its session ended, and the end that could not connect a session to its
// destination is told why before the session is failed.
func TestCarrySaysHowTheSessionEnded(t *testing.T) {
	dst, got := sinkListener(t)
	k := NewKeys([]byte("key material of the pair, 32 b.."))
	type failure struct{ code, reason string }
	var mu sync.Mutex
	failures := map[[16]byte]failure{}
	allow, err := ParseAllow("127.0.0.1/32:" + strconv.Itoa(dst.Port) + ",127.0.0.1/32:1")
	if err != nil {
		t.Fatal(err)
	}
	exit := New(DefaultConfig())
	addr := subListener(t, exit, SubOptions{Keys: k, Peer: "alpha", Allow: allow, AllowName: "the allow list of the peer",
		DialFailed: func(id [16]byte, code, reason string) {
			mu.Lock()
			failures[id] = failure{code, reason}
			mu.Unlock()
		}})
	entry := New(DefaultConfig())
	route := entry.NewRoute([]DialGroup{{Name: "p", Want: 2, Dial: dialTo(addr)}}, k, "")
	owner := Owner{Peer: "beta", Rule: "r"}

	// a clean end
	app, done := carry(route, dst.IP, dst.Port, owner)
	app.Write([]byte("hello"))
	app.Close()
	<-got
	if end := ended(t, done); !end.Clean || !end.Up || !strings.HasPrefix(end.Why, "done") || end.NoReply != "" || end.ID == [16]byte{} {
		t.Errorf("a clean end: %+v", end)
	}
	// nothing listens at the destination (port 1 is in the allow list)
	app, done = carry(route, dst.IP, 1, owner)
	defer app.Close()
	end := ended(t, done)
	if end.Clean || end.Why != WhyUnreachable+"127.0.0.1:1" {
		// a second sub-connection can find the session gone before the first one
		// brought the news
		if end.Why != WhyNoSession {
			t.Errorf("a destination that refuses: %+v", end)
		}
	}
	mu.Lock()
	f := failures[end.ID]
	mu.Unlock()
	if f.code != DialError || !strings.Contains(f.reason, "refused") {
		t.Errorf("what the exit end was told about %x: %+v", end.ID[:3], f)
	}
	// the destination is not in the allow list
	app, done = carry(route, dst.IP, 2, owner)
	defer app.Close()
	end = ended(t, done)
	mu.Lock()
	f = failures[end.ID]
	mu.Unlock()
	if end.Clean || f.code != DialRefused || f.reason != "not in the allow list of the peer" {
		t.Errorf("a destination outside the allow list: %+v, told %+v", end, f)
	}
}

// countConn is a local end that counts what it is given and gives nothing.
type countConn struct {
	net.Conn // nil: only the methods below are used
	mu       sync.Mutex
	n        int
	closed   chan struct{}
	once     sync.Once
}

func (c *countConn) Read([]byte) (int, error) { <-c.closed; return 0, io.ErrClosedPipe }
func (c *countConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.n += len(p)
	c.mu.Unlock()
	return len(p), nil
}
func (c *countConn) Close() error                     { c.once.Do(func() { close(c.closed) }); return nil }
func (c *countConn) SetReadDeadline(time.Time) error  { return nil }
func (c *countConn) SetWriteDeadline(time.Time) error { return nil }
func (c *countConn) got() int                         { c.mu.Lock(); defer c.mu.Unlock(); return c.n }

// A session whose destination is the sink ends inside the other end: nothing is dialed,
// the allow list is not asked, and both ends mark it as a test. Without a sink the same
// destination is one that cannot be reached. AbortOwner ends the session at the end that
// carries it and leaves alone a session that this end only takes.
func TestSinkSession(t *testing.T) {
	k := NewKeys([]byte("key material of the pair, 32 b.."))
	allow, _ := ParseAllow("192.0.2.1/32:9") // nothing a test could reach
	sink := &countConn{closed: make(chan struct{})}
	exit := New(DefaultConfig())
	addr := subListener(t, exit, SubOptions{Keys: k, Peer: "alpha", Allow: allow, Sink: func() (net.Conn, string) { return sink, "" }})
	entry := New(DefaultConfig())
	route := entry.NewRoute([]DialGroup{{Name: "p", Want: 2, Dial: dialTo(addr)}}, k, "")
	owner := Owner{Peer: "beta", Test: true}
	app, done := carry(route, SinkIP, SinkPort, owner)
	defer app.Close()
	go app.Write(make([]byte, 1<<20))
	deadline := time.Now().Add(10 * time.Second)
	for sink.got() < 1<<20 {
		if time.Now().After(deadline) {
			t.Fatalf("%d bytes arrived in the sink", sink.got())
		}
		time.Sleep(5 * time.Millisecond)
	}
	el, xl := entry.SessionList(), exit.SessionList()
	if len(el) != 1 || len(xl) != 1 || !el[0].Test || !xl[0].Test || !el[0].Up || el[0].GraceLeft > testGrace || xl[0].GraceLeft > testGrace {
		t.Fatalf("sessions: entry %+v, exit %+v", el, xl)
	}
	// the exit end only takes the session: it is not its to end this way
	if n := exit.AbortOwner(Owner{Peer: "alpha", Test: true}, "x"); n != 0 {
		t.Errorf("AbortOwner ended %d sessions at the end that does not carry them", n)
	}
	if n := entry.AbortOwner(Owner{Peer: "beta"}, "x"); n != 0 {
		t.Errorf("AbortOwner ended %d sessions of another owner", n)
	}
	// nor is it another peer's to end, or a session that is no test's
	if exit.EndTest("gamma", xl[0].ID) || entry.EndTest("beta", el[0].ID) {
		t.Error("EndTest ended a session of another peer, or at the end that carries it")
	}
	if n := entry.AbortOwner(owner, "the test is over"); n != 1 {
		t.Fatalf("AbortOwner ended %d sessions, want 1", n)
	}
	if end := ended(t, done); end.Clean || end.Why != "the test is over" {
		t.Errorf("the end of an aborted test session: %+v", end)
	}
	select {
	case <-sink.closed:
	case <-time.After(10 * time.Second):
		t.Fatal("the sink was not closed when its session ended")
	}

	// the same destination where nothing takes it
	plain := New(DefaultConfig())
	var told string
	var mu sync.Mutex
	addr = subListener(t, plain, SubOptions{Keys: k, Peer: "alpha", DialFailed: func(_ [16]byte, code, _ string) { mu.Lock(); told = code; mu.Unlock() }})
	route = entry.NewRoute([]DialGroup{{Name: "p", Want: 1, Dial: dialTo(addr)}}, k, "")
	app, done = carry(route, SinkIP, SinkPort, owner)
	defer app.Close()
	if end := ended(t, done); end.Clean || !strings.HasPrefix(end.Why, WhyUnreachable) {
		t.Errorf("a test session to an end without a sink: %+v", end)
	}
	mu.Lock()
	if told != DialError {
		t.Errorf("the end without a sink was told %q", told)
	}
	mu.Unlock()
	if xl := plain.SessionList(); len(xl) != 0 {
		t.Errorf("sessions left at the end without a sink: %+v", xl)
	}
}

// Unreachable ends a session of the peer that this end carries, as the news on a
// sub-connection would have; it does not touch another peer's.
func TestUnreachable(t *testing.T) {
	dst, _ := sinkListener(t)
	k := NewKeys([]byte("key material of the pair, 32 b.."))
	exit := New(DefaultConfig())
	addr := subListener(t, exit, SubOptions{Keys: k, Peer: "alpha"})
	entry := New(DefaultConfig())
	route := entry.NewRoute([]DialGroup{{Name: "p", Want: 1, Dial: dialTo(addr)}}, k, "")
	app, done := carry(route, dst.IP, dst.Port, Owner{Peer: "beta", Rule: "r"})
	defer app.Close()
	var id [16]byte
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if l := entry.SessionList(); len(l) == 1 && l[0].Up {
			id = l[0].ID
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the session did not come up")
		}
	}
	if entry.Unreachable("gamma", id) || entry.Unreachable("beta", [16]byte{1}) || exit.Unreachable("alpha", id) {
		t.Fatal("Unreachable took a session of another peer, an id nobody has, or one this end only takes")
	}
	if !entry.Unreachable("beta", id) {
		t.Fatal("Unreachable did not find the session")
	}
	want := WhyUnreachable + dst.String()
	if end := ended(t, done); end.Clean || end.Why != want {
		t.Errorf("ended %+v, want %q", end, want)
	}
}

// The error with which a sub-connection ended that the other end never answered is kept
// for whoever asks why the session does not come up.
func TestNoReplyIsKept(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close() // takes the connection and says nothing
		}
	}()
	entry := New(DefaultConfig())
	route := entry.NewRoute([]DialGroup{{Name: "p", Want: 1, Dial: dialTo(ln.Addr().String())}}, NewKeys([]byte("key material of the pair, 32 b..")), "")
	app, done := carry(route, net.IPv4(192, 0, 2, 1), 9, Owner{Peer: "beta", Rule: "r"})
	defer app.Close()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if l := entry.SessionList(); len(l) == 1 && l[0].NoReply != "" {
			if l[0].Up {
				t.Fatalf("the session counts as answered: %+v", l[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no trace of the unanswered sub-connection: %+v", entry.SessionList())
		}
	}
	entry.AbortPeer("beta")
	if end := ended(t, done); end.Up || end.NoReply == "" {
		t.Errorf("the end: %+v", end)
	}
}

// A session that carries test data does not wait the grace time of an application's.
func TestGraceOfATestSession(t *testing.T) {
	e := New(DefaultConfig())
	test := e.newSessionFor([16]byte{1}, nil, true, "0.0.0.0:0", keys{}, Owner{Peer: "p", Test: true})
	if g := test.grace(); g != e.cfg.Grace {
		t.Errorf("a test session that the other end has not answered waits %v, want %v", g, e.cfg.Grace)
	}
	test.everUp = true
	if g := test.grace(); g != testGrace {
		t.Errorf("a test session waits %v", g)
	}
	if g := e.newSessionFor([16]byte{2}, nil, true, "192.0.2.1:9", keys{}, Owner{Peer: "p"}).grace(); g != e.cfg.Grace {
		t.Errorf("an application's session waits %v, want %v", g, e.cfg.Grace)
	}
	cfg := DefaultConfig()
	cfg.Grace = time.Second
	short := New(cfg)
	brief := short.newSessionFor([16]byte{3}, nil, true, "0.0.0.0:0", keys{}, Owner{Test: true})
	brief.everUp = true
	if g := brief.grace(); g != time.Second {
		t.Errorf("with a grace time of a second, a test session waits %v", g)
	}
}

// EndTest ends a test session at the end that takes it, when the end that carries it
// says, by another way, that it is over.
func TestEndTest(t *testing.T) {
	k := NewKeys([]byte("key material of the pair, 32 b.."))
	sink := &countConn{closed: make(chan struct{})}
	exit := New(DefaultConfig())
	addr := subListener(t, exit, SubOptions{Keys: k, Peer: "alpha", Sink: func() (net.Conn, string) { return sink, "" }})
	entry := New(DefaultConfig())
	route := entry.NewRoute([]DialGroup{{Name: "p", Want: 1, Dial: dialTo(addr)}}, k, "")
	app, done := carry(route, SinkIP, SinkPort, Owner{Peer: "beta", Test: true})
	defer app.Close()
	var id [16]byte
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if l := exit.SessionList(); len(l) == 1 && l[0].Test {
			id = l[0].ID
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the exit end did not take the test session")
		}
	}
	if !exit.EndTest("alpha", id) {
		t.Fatal("EndTest did not find the session")
	}
	select {
	case <-sink.closed:
	case <-time.After(10 * time.Second):
		t.Fatal("the sink was not closed")
	}
	if end := ended(t, done); end.Clean {
		t.Errorf("the carrying end: %+v", end)
	}
	if exit.EndTest("alpha", id) {
		t.Error("EndTest found the session a second time")
	}
}
