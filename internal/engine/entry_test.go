package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// sinkListener accepts connections and reports what it read on each when it ends.
func sinkListener(t *testing.T) (addr *net.TCPAddr, got chan []byte) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan []byte, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				b, _ := io.ReadAll(c)
				c.Close()
				got <- b
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr), got
}

// subListener hands every connection it accepts to e.ServeSub with opt.
func subListener(t *testing.T, e *Engine, opt SubOptions) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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
			go e.ServeSub(c, opt)
		}
	}()
	return ln.Addr().String()
}

func dialTo(addr string) func() (net.Conn, error) {
	return func() (net.Conn, error) { return net.Dial("tcp", addr) }
}

// sendHeader opens a connection to addr, sends the header and returns the status of the
// reply.
func sendHeader(t *testing.T, addr string, h subHeader, ks keys) byte {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	h.unix = time.Now().Unix()
	if _, err := c.Write(h.marshal(ks.hdr)); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	b := make([]byte, replyLen)
	if _, err := io.ReadFull(c, b); err != nil {
		t.Fatalf("no reply: %v", err)
	}
	return b[4]
}

// One engine carries a connection from a Route to ServeSub of another engine, with keys
// and an owner per pair, and the bytes arrive unchanged.
func TestCarryAndServeSub(t *testing.T) {
	dst, got := sinkListener(t)
	k := NewKeys([]byte("key material of the pair, 32 b.."))
	exit := New(DefaultConfig())
	addr := subListener(t, exit, SubOptions{Keys: k, Peer: "alpha"})
	entry := New(DefaultConfig())
	route := entry.NewRoute([]DialGroup{{Name: "p", Want: 3, Dial: dialTo(addr)}}, k, "")

	app, far := net.Pipe()
	done := make(chan struct{})
	go func() {
		route.Carry(far, dst.IP, dst.Port, Owner{Peer: "beta", Rule: "r1"}, nil)
		close(done)
	}()
	data := make([]byte, 3<<20)
	rand.Read(data)
	go func() { app.Write(data); app.Close() }()

	// the session is listed with its owner on both ends while it runs
	deadline := time.Now().Add(5 * time.Second)
	for {
		el, xl := entry.SessionList(), exit.SessionList()
		if len(el) == 1 && len(xl) == 1 {
			if el[0].Peer != "beta" || el[0].Rule != "r1" || xl[0].Peer != "alpha" || xl[0].Rule != "" {
				t.Fatalf("owners: entry %+v, exit %+v", el[0], xl[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sessions: entry %d, exit %d", len(el), len(xl))
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case b := <-got:
		if sha256.Sum256(b) != sha256.Sum256(data) {
			t.Fatalf("%d bytes arrived, %d sent, or they differ", len(b), len(data))
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the data did not arrive")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Carry did not return after the session ended")
	}
}

// A sub-connection can join only a session of its own pair: with another pair's keys,
// or another peer name, the session is not there for it -- while it lives and after it
// has ended.
func TestServeSubKeepsPairsApart(t *testing.T) {
	dst, _ := sinkListener(t)
	e := New(DefaultConfig())
	ka := deriveKeys([]byte("pair a: sixteen bytes or more"))
	kb := deriveKeys([]byte("pair b: sixteen bytes or more"))
	addrA := subListener(t, e, SubOptions{Keys: Keys{ka}, Peer: "a"})
	addrB := subListener(t, e, SubOptions{Keys: Keys{kb}, Peer: "b"})
	// same keys as pair a, under another peer name
	addrA2 := subListener(t, e, SubOptions{Keys: Keys{ka}, Peer: "a2"})

	id := [16]byte{7, 7, 7}
	first := subHeader{isNew: true, id: id, dstIP: dst.IP, dstPort: dst.Port, idx: 1, windowMB: 64}
	c, err := net.Dial("tcp", addrA)
	if err != nil {
		t.Fatal(err)
	}
	first.unix = time.Now().Unix()
	c.Write(first.marshal(ka.hdr))
	b := make([]byte, replyLen)
	if _, err := io.ReadFull(c, b); err != nil || b[4] != stOK {
		t.Fatalf("pair a could not start its session: %v, status %d", err, b[4])
	}

	join := subHeader{id: id, dstIP: dst.IP, dstPort: dst.Port, idx: 2}
	if st := sendHeader(t, addrB, join, kb); st != stGoneBad {
		t.Errorf("pair b joining pair a's session: status %d, want %d", st, stGoneBad)
	}
	if st := sendHeader(t, addrA2, join, ka); st != stGoneBad {
		t.Errorf("another peer with the same keys joining: status %d, want %d", st, stGoneBad)
	}
	again := first
	again.idx = 3
	if st := sendHeader(t, addrB, again, kb); st != stGoneBad {
		t.Errorf("pair b starting a session under the id of pair a's: status %d, want %d", st, stGoneBad)
	}
	if n := len(e.SessionList()); n != 1 {
		t.Fatalf("%d sessions, want the one of pair a", n)
	}
	if st := sendHeader(t, addrA, join, ka); st != stOK {
		t.Errorf("pair a joining its own session: status %d", st)
	}

	// end the session; what is remembered of it is pair a's too
	if n := e.AbortPeer("a"); n != 1 {
		t.Fatalf("AbortPeer found %d sessions", n)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(e.SessionList()) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the aborted session is still listed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.smu.Lock()
	_, remembered := e.ended[id]
	for !remembered {
		e.smu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("the ended session is not remembered")
		}
		time.Sleep(5 * time.Millisecond)
		e.smu.Lock()
		_, remembered = e.ended[id]
	}
	e.smu.Unlock()
	if st := sendHeader(t, addrB, again, kb); st != stGoneBad {
		t.Errorf("pair b asking about pair a's ended session: status %d, want %d", st, stGoneBad)
	}
	if n := len(e.SessionList()); n != 0 {
		t.Fatalf("pair b created a session under the id of an ended one of pair a")
	}
	c.Close()
}

// AbortPeer resets the application connection of the peer's sessions and leaves the
// sessions of other peers alone; a session that is not valid any more is not created.
func TestAbortPeerAndValid(t *testing.T) {
	dst, _ := sinkListener(t)
	k := NewKeys([]byte("key material of the pair, 32 b.."))
	exit := New(DefaultConfig())
	addr := subListener(t, exit, SubOptions{Keys: k, Peer: "x"})
	entry := New(DefaultConfig())
	route := entry.NewRoute([]DialGroup{{Name: "p", Want: 1, Dial: dialTo(addr)}}, k, "")

	start := func(peer string) (app net.Conn, done chan struct{}) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		app, err = net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		far, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		done = make(chan struct{})
		go func() { route.Carry(far, dst.IP, dst.Port, Owner{Peer: peer}, nil); close(done) }()
		return app, done
	}
	gone, goneDone := start("gone")
	stays, staysDone := start("stays")
	defer stays.Close()
	deadline := time.Now().Add(5 * time.Second)
	for len(entry.SessionList()) != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("%d sessions, want 2", len(entry.SessionList()))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := entry.AbortPeer("gone"); n != 1 {
		t.Fatalf("AbortPeer found %d sessions, want 1", n)
	}
	select {
	case <-goneDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the aborted session did not end")
	}
	gone.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := gone.Read(make([]byte, 1)); err == nil || err == io.EOF {
		t.Fatalf("the application connection of an aborted session ended with %v, want a reset", err)
	}
	select {
	case <-staysDone:
		t.Fatal("the session of another peer ended too")
	case <-time.After(300 * time.Millisecond):
	}
	if l := entry.SessionList(); len(l) != 1 || l[0].Peer != "stays" {
		t.Fatalf("sessions left: %+v", l)
	}

	// not valid: reset at once, no session
	a, b := net.Pipe()
	ended := make(chan struct{})
	go func() {
		route.Carry(b, dst.IP, dst.Port, Owner{Peer: "gone"}, func() bool { return false })
		close(ended)
	}()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("Carry did not return for a session that is not valid")
	}
	a.Close()
	if n := len(entry.SessionList()); n != 1 {
		t.Fatalf("%d sessions after a refused one, want 1", n)
	}
	// the exit end refuses a new session that is not valid, too
	refusing := subListener(t, exit, SubOptions{Keys: k, Peer: "x", Valid: func() bool { return false }})
	h := subHeader{isNew: true, id: [16]byte{9}, dstIP: dst.IP, dstPort: dst.Port, idx: 1}
	if st := sendHeader(t, refusing, h, k.ks); st != stRefused {
		t.Fatalf("a session that is not valid: status %d, want %d", st, stRefused)
	}
}

// A session whose sub-connections the exit end opens: the carrying end attaches what
// arrives, the exit end keeps their number up. Two of them are cut in the middle of a
// transfer; the exit end dials again and every byte arrives.
func TestReverseSession(t *testing.T) {
	dst, got := sinkListener(t)
	k := NewKeys([]byte("key material of the pair, 32 b.."))
	entry, exit := New(DefaultConfig()), New(DefaultConfig())

	app, far := net.Pipe()
	rv := entry.NewReverse(far, dst.IP, dst.Port, k, "", Owner{Peer: "exit", Rule: "r"}, nil)
	if rv == nil {
		t.Fatal("no session")
	}
	ran := make(chan struct{})
	go func() { rv.Run(); close(ran) }()

	// what the exit end dials arrives here and is attached to the session
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
			rv.Attach(c, 0, false, false)
		}
	}()
	var mu sync.Mutex
	var dialed []net.Conn
	dial := func() (net.Conn, error) {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err == nil {
			mu.Lock()
			dialed = append(dialed, c)
			mu.Unlock()
		}
		return c, err
	}
	dials := func() int { mu.Lock(); defer mu.Unlock(); return len(dialed) }
	route := exit.NewRoute([]DialGroup{{Name: "p", Want: 4, Dial: dial}}, k, "")
	first, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() { route.ServeReverse(first, SubOptions{Peer: "entry"}); close(served) }()

	until := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("it did not happen: %s", what)
			}
		}
	}
	subs := func(e *Engine) int {
		if l := e.SessionList(); len(l) == 1 {
			return l[0].Subs
		}
		return -1
	}
	until("four sub-connections at both ends", func() bool { return subs(entry) == 4 && subs(exit) == 4 })
	if !rv.Up() {
		t.Fatal("the session has sub-connections and does not say it is up")
	}
	if el, xl := entry.SessionList()[0], exit.SessionList()[0]; !el.Client || el.Peer != "exit" || el.Rule != "r" || xl.Client || xl.Peer != "entry" {
		t.Fatalf("sessions: carrying end %+v, exit end %+v", el, xl)
	}

	data := make([]byte, 6<<20)
	rand.Read(data)
	half := len(data) / 2
	if _, err := app.Write(data[:half]); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	dialed[0].Close()
	dialed[1].Close()
	mu.Unlock()
	if _, err := app.Write(data[half:]); err != nil {
		t.Fatal(err)
	}
	until("the exit end dials for the two that were cut", func() bool { return dials() >= 6 })
	until("four sub-connections again", func() bool { return subs(entry) == 4 && subs(exit) == 4 })
	app.Close()
	select {
	case b := <-got:
		if sha256.Sum256(b) != sha256.Sum256(data) {
			t.Fatalf("%d bytes arrived, %d sent, or they differ", len(b), len(data))
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the data did not arrive")
	}
	for _, c := range []chan struct{}{ran, served} {
		select {
		case <-c:
		case <-time.After(10 * time.Second):
			t.Fatal("an end did not return after the session ended")
		}
	}
}

// A connection that the exit end dialed for one session is not taken for another: the
// header on it must name the session it was dialed for.
func TestReverseJoinsOnlyItsSession(t *testing.T) {
	dst, _ := sinkListener(t)
	k := NewKeys([]byte("key material of the pair, 32 b.."))
	exit := New(DefaultConfig())
	s := exit.newSessionFor([16]byte{1}, nil, false, dst.String(), k.ks, Owner{Peer: "entry"})
	defer s.shutdown("test over", true)
	a, b := net.Pipe()
	defer a.Close()
	done := make(chan bool, 1)
	go func() { done <- exit.serveSub(b, SubOptions{Keys: k, Peer: "entry"}, nil, s) }()
	h := subHeader{isNew: true, id: [16]byte{2}, dstIP: dst.IP, dstPort: dst.Port, idx: 1, unix: time.Now().Unix()}
	go a.Write(h.marshal(k.ks.hdr))
	rp := make([]byte, replyLen)
	a.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(a, rp); err != nil || rp[4] != stBadHdr {
		t.Fatalf("a header for another session: reply %v, status %d, want status %d", err, rp[4], stBadHdr)
	}
	if <-done {
		t.Fatal("the connection was taken as a sub-connection")
	}
	if n := len(exit.SessionList()); n != 1 {
		t.Fatalf("%d sessions, want the one of the test", n)
	}
}

// After Shutdown no session begins at either end: it would never be reset, and the exit
// of the process would close its application connection like the complete end of a stream.
func TestNoSessionAfterShutdown(t *testing.T) {
	dst, _ := sinkListener(t)
	k := NewKeys([]byte("key material of the pair, 32 b.."))
	exit := New(DefaultConfig())
	addr := subListener(t, exit, SubOptions{Keys: k, Peer: "x"})
	entry := New(DefaultConfig())
	route := entry.NewRoute([]DialGroup{{Name: "p", Want: 1, Dial: dialTo(addr)}}, k, "")
	entry.Shutdown()
	exit.Shutdown()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	app, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	far, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	go func() { route.Carry(far, dst.IP, dst.Port, Owner{Peer: "x"}, nil); close(ended) }()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("Carry did not return after Shutdown")
	}
	app.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := app.Read(make([]byte, 1)); err == nil || err == io.EOF {
		t.Fatalf("the application connection ended with %v after Shutdown, want a reset", err)
	}
	if n := len(entry.SessionList()); n != 0 {
		t.Fatalf("%d sessions at the carrying end after Shutdown", n)
	}

	h := subHeader{isNew: true, id: [16]byte{5}, dstIP: dst.IP, dstPort: dst.Port, idx: 1}
	if st := sendHeader(t, addr, h, k.ks); st != stRefused {
		t.Fatalf("a new session after Shutdown: status %d, want %d", st, stRefused)
	}
	if n := len(exit.SessionList()); n != 0 {
		t.Fatalf("%d sessions at the exit end after Shutdown", n)
	}
}

// Shutdown of the carrying end alone: Carry resets the connection it is given and makes
// no session, whatever the other end would have answered.
func TestCarryAfterShutdown(t *testing.T) {
	dst, _ := sinkListener(t)
	k := NewKeys([]byte("key material of the pair, 32 b.."))
	exit := New(DefaultConfig())
	addr := subListener(t, exit, SubOptions{Keys: k, Peer: "x"})
	entry := New(DefaultConfig())
	route := entry.NewRoute([]DialGroup{{Name: "p", Want: 1, Dial: dialTo(addr)}}, k, "")
	entry.Shutdown()

	a, b := net.Pipe()
	defer a.Close()
	ended := make(chan struct{})
	go func() { route.Carry(b, dst.IP, dst.Port, Owner{Peer: "x"}, nil); close(ended) }()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("Carry did not return after Shutdown of the carrying end")
	}
	if n := len(entry.SessionList()); n != 0 {
		t.Fatalf("%d sessions at the carrying end after Shutdown", n)
	}
	if n := len(exit.SessionList()); n != 0 {
		t.Fatalf("%d sessions at the exit end, which was never asked", n)
	}
}

// A session at the exit end whose destination is still being dialed when Shutdown runs:
// the connection comes up afterwards and must be reset, not left to end like a complete
// stream when the process exits. The destination here takes no connection until
// Shutdown has returned: its accept queue is full, so the dial waits for a place.
func TestDestinationDialedDuringShutdownIsReset(t *testing.T) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Listen(fd, 0); err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fd), "destination")
	ln, err := net.FileListener(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dst := ln.Addr().(*net.TCPAddr)
	// fill the accept queue: connections that nobody accepts, until one more does not fit
	var fill []net.Conn
	defer func() {
		for _, c := range fill {
			c.Close()
		}
	}()
	for i := 0; i < 8; i++ {
		c, err := net.DialTimeout("tcp", dst.String(), 300*time.Millisecond)
		if err != nil {
			break
		}
		fill = append(fill, c)
	}
	if len(fill) == 8 {
		t.Skip("this system does not hold connections back when the accept queue is full")
	}

	k := NewKeys([]byte("key material of the pair, 32 b.."))
	exit := New(DefaultConfig())
	addr := subListener(t, exit, SubOptions{Keys: k, Peer: "x"})
	entry := New(DefaultConfig())
	route := entry.NewRoute([]DialGroup{{Name: "p", Want: 1, Dial: dialTo(addr)}}, k, "")
	al, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer al.Close()
	app, err := net.Dial("tcp", al.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	far, err := al.Accept()
	if err != nil {
		t.Fatal(err)
	}
	go route.Carry(far, dst.IP, dst.Port, Owner{Peer: "x"}, nil)
	app.Write([]byte("hello"))
	for deadline := time.Now().Add(5 * time.Second); len(exit.SessionList()) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no session at the exit end")
		}
	}
	time.Sleep(200 * time.Millisecond) // the exit end's dial is under way and held back
	exit.Shutdown()

	// make room: the held-back dial of the exit end now gets through
	tl := ln.(*net.TCPListener)
	var d net.Conn
	for n := 0; d == nil; n++ {
		tl.SetDeadline(time.Now().Add(10 * time.Second))
		c, err := tl.Accept()
		if err != nil {
			t.Fatalf("the destination got %d connections, then: %v", n, err)
		}
		if n < len(fill) {
			defer c.Close()
			continue
		}
		d = c
	}
	defer d.Close()
	// what the destination sees on the connection that came up after Shutdown
	d.SetReadDeadline(time.Now().Add(5 * time.Second))
	var got []byte
	buf := make([]byte, 16)
	for {
		n, err := d.Read(buf)
		got = append(got, buf[:n]...)
		if err == nil {
			continue
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatalf("the destination connection that came up after Shutdown is still open (read %q): nothing resets it", got)
		}
		if err == io.EOF {
			t.Fatalf("the destination connection ended like a complete stream (read %q)", got)
		}
		break // reset
	}
}

func TestAllowListMatch(t *testing.T) {
	l, err := ParseAllow("192.0.2.0/24:873, [2001:db8::/32]:5000-5010")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		ip   string
		port int
		ok   bool
	}{
		{"192.0.2.7", 873, true},
		{"192.0.2.7", 874, false},
		{"198.51.100.1", 873, false},
		{"2001:db8::1", 5005, true},
		{"2001:db8::1", 5011, false},
	} {
		if got := l.Match(net.ParseIP(c.ip), c.port); got != c.ok {
			t.Errorf("Match(%s, %d) = %v", c.ip, c.port, got)
		}
		if got := l.Allows(net.ParseIP(c.ip), c.port); got != c.ok {
			t.Errorf("Allows(%s, %d) = %v", c.ip, c.port, got)
		}
	}
	empty, err := ParseAllow("")
	if err != nil {
		t.Fatal(err)
	}
	if !empty.Empty() || !empty.Allows(net.ParseIP("192.0.2.1"), 1) || empty.Match(net.ParseIP("192.0.2.1"), 1) {
		t.Error("an empty list allows everything and matches nothing")
	}
	var none *AllowList
	if !none.Allows(net.ParseIP("192.0.2.1"), 1) || none.Match(net.ParseIP("192.0.2.1"), 1) || !none.Empty() {
		t.Error("a nil list allows everything and matches nothing")
	}
	if _, err := ParseAllow("not a network"); err == nil {
		t.Error("a bad entry was accepted")
	}
}

func TestDialContextCancelled(t *testing.T) {
	// a proxy that accepts and never answers
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
			defer c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = DialContext(ctx, "192.0.2.1:9", "socks5://"+ln.Addr().String())
	if err == nil {
		t.Fatal("dialing through a silent proxy succeeded")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("the dial took %v after its context ended", d)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("deadline")) {
		t.Fatalf("error %q does not say the context ended", err)
	}
}

// lingerOf reads SO_LINGER of a TCP connection.
func lingerOf(t *testing.T, c net.Conn) (on, sec int32) {
	t.Helper()
	rc, err := c.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var l syscall.Linger
	var gerr error
	if err := rc.Control(func(fd uintptr) {
		n := uint32(unsafe.Sizeof(l))
		if _, _, e := syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, syscall.SOL_SOCKET, syscall.SO_LINGER,
			uintptr(unsafe.Pointer(&l)), uintptr(unsafe.Pointer(&n)), 0); e != 0 {
			gerr = e
		}
	}); err != nil {
		t.Fatal(err)
	}
	if gerr != nil {
		t.Fatal(gerr)
	}
	return l.Onoff, l.Linger
}

// While a session runs, the application's connection at each end is set to be reset by
// any close -- the one the kernel does for a process that was killed included. A session
// that ends cleanly takes that back before it closes: the destination here has not yet
// read the end of the stream when the exit end closes its connection, and still gets
// every byte and then a clean end. (That part does not catch a clean end that resets:
// the last part fits into the destination's receive buffer, where a reset does not
// take it away. TestCleanCloseSendsWhatTheSocketHolds does; the socket buffers are left
// at the system's sizes here, because the stream ends only once all of it is written.)
func TestApplicationConnectionsAreArmedForReset(t *testing.T) {
	const size, held = 24 << 20, 256 << 10
	// the destination: it closes its own direction at once, reads all but the last
	// part of the stream, waits, reads the rest, and reports how the stream ended
	dln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer dln.Close()
	type result struct {
		data []byte
		err  error
	}
	arrived := make(chan result, 1)
	proceed := make(chan struct{})
	go func() {
		c, err := dln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.(*net.TCPConn).CloseWrite()
		b := make([]byte, size-held)
		if _, err := io.ReadFull(c, b); err != nil {
			arrived <- result{nil, err}
			return
		}
		<-proceed
		rest, err := io.ReadAll(c)
		arrived <- result{append(b, rest...), err}
	}()
	dst := dln.Addr().(*net.TCPAddr)

	k := NewKeys([]byte("key material of the pair, 32 b.."))
	exit := New(DefaultConfig())
	addr := subListener(t, exit, SubOptions{Keys: k, Peer: "alpha"})
	entry := New(DefaultConfig())
	route := entry.NewRoute([]DialGroup{{Name: "p", Want: 3, Dial: dialTo(addr)}}, k, "")

	// the application at the entry end, over real TCP
	aln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer aln.Close()
	app, err := net.Dial("tcp", aln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	far, err := aln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		route.Carry(far, dst.IP, dst.Port, Owner{Peer: "beta"}, nil)
		close(done)
	}()

	// the exit end's connection to the destination, once it is up
	var local net.Conn
	deadline := time.Now().Add(10 * time.Second)
	for local == nil {
		exit.registry.Range(func(key, _ any) bool {
			s := key.(*session)
			s.mu.Lock()
			local = s.local
			s.mu.Unlock()
			return false
		})
		if local == nil {
			if time.Now().After(deadline) {
				t.Fatal("the exit end did not connect to the destination")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	for name, c := range map[string]net.Conn{"entry": far, "exit": local} {
		if on, sec := lingerOf(t, c); on == 0 || sec != 0 {
			t.Errorf("%s end: SO_LINGER of the application connection is on=%d sec=%d, want on, 0 s", name, on, sec)
		}
	}

	data := make([]byte, size)
	rand.Read(data)
	go func() {
		app.Write(data)
		app.(*net.TCPConn).CloseWrite()
	}()
	// both ends finish cleanly while the destination still has the last part to read:
	// it sits in the sockets between the exit end and the destination
	select {
	case <-done:
	case r := <-arrived:
		t.Fatalf("the destination's stream ended early: %d bytes, %v", len(r.data), r.err)
	case <-time.After(60 * time.Second):
		t.Fatal("the session did not end")
	}
	for deadline := time.Now().Add(10 * time.Second); len(exit.SessionList()) > 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the session did not end at the exit end")
		}
	}
	time.Sleep(200 * time.Millisecond)
	close(proceed)
	select {
	case r := <-arrived:
		if r.err != nil {
			t.Fatalf("the destination's stream ended with %v after %d bytes, want a clean end", r.err, len(r.data))
		}
		if sha256.Sum256(r.data) != sha256.Sum256(data) {
			t.Fatalf("%d bytes arrived, %d sent, or they differ", len(r.data), len(data))
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the rest of the data did not arrive")
	}
	// the application at the entry end saw a clean end as well
	app.SetReadDeadline(time.Now().Add(15 * time.Second))
	if n, err := app.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("the application at the entry end read %d bytes, %v; want a clean end", n, err)
	}
}

// A connection that was armed for a reset and is then closed the clean way still
// delivers what its socket held at the close, followed by a clean end. The socket is
// filled until the kernel takes no more, so part of the data has certainly not left it
// when it is closed.
func TestCleanCloseSendsWhatTheSocketHolds(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	peer, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	ArmReset(c)
	// nobody reads: write until a write stalls
	block := make([]byte, 64<<10)
	rand.Read(block)
	h := sha256.New()
	var sent int64
	for {
		c.SetWriteDeadline(time.Now().Add(300 * time.Millisecond))
		n, err := c.Write(block)
		h.Write(block[:n])
		sent += int64(n)
		if err != nil {
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatal(err)
			}
			break
		}
	}
	if sent < int64(len(block)) {
		t.Fatalf("only %d bytes went into the socket", sent)
	}
	cleanClose(c)
	got := sha256.New()
	n, err := io.Copy(got, peer)
	if err != nil || n != sent {
		t.Fatalf("after a clean close the other side read %d of %d bytes and then %v, want all of them and a clean end", n, sent, err)
	}
	if !bytes.Equal(got.Sum(nil), h.Sum(nil)) {
		t.Fatal("the bytes differ")
	}
}

// AbortPeerBefore resets the sessions of the peer that began before the time given and
// leaves the later ones alone.
func TestAbortPeerBefore(t *testing.T) {
	dst, _ := sinkListener(t)
	k := NewKeys([]byte("key material of the pair, 32 b.."))
	exit := New(DefaultConfig())
	addr := subListener(t, exit, SubOptions{Keys: k, Peer: "alpha"})
	entry := New(DefaultConfig())
	route := entry.NewRoute([]DialGroup{{Name: "p", Want: 2, Dial: dialTo(addr)}}, k, "")
	carry := func(rule string) net.Conn {
		app, far := net.Pipe()
		t.Cleanup(func() { app.Close() })
		go route.Carry(far, dst.IP, dst.Port, Owner{Peer: "beta", Rule: rule}, nil)
		return app
	}
	sessions := func(n int) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); len(entry.SessionList()) != n; time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("the entry end has %d sessions, want %d", len(entry.SessionList()), n)
			}
		}
	}
	carry("early")
	sessions(1)
	time.Sleep(20 * time.Millisecond)
	between := time.Now()
	time.Sleep(20 * time.Millisecond)
	carry("late")
	sessions(2)
	if owners := entry.AbortPeerBefore("other", between, "test"); len(owners) != 0 {
		t.Fatalf("sessions of another peer were reset: %+v", owners)
	}
	owners := entry.AbortPeerBefore("beta", between, "test")
	if len(owners) != 1 || owners[0].Rule != "early" {
		t.Fatalf("reset %+v, want the one session that began before the time", owners)
	}
	sessions(1)
	if l := entry.SessionList(); l[0].Rule != "late" {
		t.Fatalf("the session that is left is %+v, want the late one", l[0])
	}
}
