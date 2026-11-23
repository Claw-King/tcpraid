package engine

// Tests that need Linux: they read a socket option through the raw system call, or
// rely on how Linux fills the accept queue of a listener and sizes socket buffers.

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

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
