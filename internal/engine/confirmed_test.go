package engine

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// lastHeld is a destination's connection at the exit end that has ended its own side
// and takes what it is given, except that the write that would complete total bytes is
// held until the connection is closed, if hold is set: a destination that stopped
// reading just before the end. It stands in for the connection and the system's
// buffers behind it, whose sizes a test must not depend on.
type lastHeld struct {
	net.Conn // nil: only the methods below are used
	total    int
	hold     bool

	mu     sync.Mutex
	n      int
	held   chan struct{} // closed when the last write is being held
	closed chan struct{}
	honce  sync.Once
	conce  sync.Once
}

func newLastHeld(total int, hold bool) *lastHeld {
	return &lastHeld{total: total, hold: hold, held: make(chan struct{}), closed: make(chan struct{})}
}

func (c *lastHeld) Read([]byte) (int, error) { return 0, io.EOF }
func (c *lastHeld) Write(p []byte) (int, error) {
	c.mu.Lock()
	last := c.n+len(p) >= c.total
	c.mu.Unlock()
	if last && c.hold {
		c.honce.Do(func() { close(c.held) })
		<-c.closed
		return 0, io.ErrClosedPipe
	}
	c.mu.Lock()
	c.n += len(p)
	c.mu.Unlock()
	return len(p), nil
}
func (c *lastHeld) Close() error                     { c.conce.Do(func() { close(c.closed) }); return nil }
func (c *lastHeld) CloseWrite() error                { return nil }
func (c *lastHeld) SetReadDeadline(time.Time) error  { return nil }
func (c *lastHeld) SetWriteDeadline(time.Time) error { return nil }

// confirmedPair starts an exit end whose sessions end in dst and an entry end that
// tells every end of a session to ends. die closes everything the exit end accepted,
// as the system does when the exit end's process is killed.
func confirmedPair(t *testing.T, dst net.Conn) (route *Route, ends chan SessionEvent, die func()) {
	k := NewKeys([]byte("key material of the pair, 32 b.."))
	exit := New(DefaultConfig())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	var subs []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			subs = append(subs, c)
			mu.Unlock()
			go exit.ServeSub(c, SubOptions{Keys: k, Peer: "alpha", Sink: func() (net.Conn, string) { return dst, "" }})
		}
	}()
	die = func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range subs {
			c.(*net.TCPConn).SetLinger(0)
			c.Close()
		}
	}
	entry := New(DefaultConfig())
	ends = make(chan SessionEvent, 4)
	entry.Watch = func(ev SessionEvent) {
		if ev.Ended {
			ends <- ev
		}
	}
	return entry.NewRoute([]DialGroup{{Name: "p", Want: 2, Dial: dialTo(ln.Addr().String())}}, k, ""), ends, die
}

func awaitEnd(t *testing.T, ends chan SessionEvent) SessionEvent {
	t.Helper()
	select {
	case ev := <-ends:
		return ev
	case <-time.After(30 * time.Second):
		t.Fatal("the session did not end")
		return SessionEvent{}
	}
}

// The exit end counts a chunk as taken before it writes the chunk to its destination.
// So the entry end can see both directions done while the exit end still writes the
// last chunk. When the exit end's process dies then, the entry end's session ends in
// order, as it always did, but it must not be reported as confirmed: nobody said that
// the destination has everything. (Found in review with real processes: a command that
// asked how its session ended exited 0 while the last 160 KiB never left the exit end.)
func TestAnEndTheOtherEndDidNotSayIsNotConfirmed(t *testing.T) {
	const total = 1 << 20
	dst := newLastHeld(total, true)
	route, ends, die := confirmedPair(t, dst)
	app, _ := carry(route, SinkIP, SinkPort, Owner{Peer: "beta", Test: true})
	go func() {
		app.Write(make([]byte, total))
		app.Close()
	}()
	select {
	case <-dst.held:
	case <-time.After(20 * time.Second):
		t.Fatal("the exit end never came to its last write")
	}
	// the entry end has read the end of its input and of the destination's side, and
	// the exit end has taken every chunk: nothing is left to do at the entry end
	time.Sleep(time.Second)
	select {
	case ev := <-ends:
		t.Fatalf("the session ended while the exit end still wrote: %+v", ev)
	default:
	}
	die()
	ev := awaitEnd(t, ends)
	if ev.Confirmed {
		t.Errorf("the end is reported as confirmed although the exit end died in its last write: %+v", ev)
	}
	dst.Close()
}

// And the end of a session that both ends completed is confirmed.
func TestAnEndBothEndsCompletedIsConfirmed(t *testing.T) {
	const total = 1 << 20
	for round := 0; round < 10; round++ {
		dst := newLastHeld(total, false)
		route, ends, _ := confirmedPair(t, dst)
		app, _ := carry(route, SinkIP, SinkPort, Owner{Peer: "beta", Test: true})
		go func() {
			app.Write(make([]byte, total))
			app.Close()
		}()
		if ev := awaitEnd(t, ends); !ev.Clean || !ev.Confirmed {
			t.Fatalf("round %d: a session that both ends completed: %+v", round, ev)
		}
	}
}
