package forward

import (
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// blackhole returns the port of a listener on the loopback address that answers no
// connect: its queue of connections that wait to be accepted is full, and nothing
// accepts. A connect to it gets no answer until whoever connects gives up.
func blackhole(t *testing.T) int {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Listen(fd, 0); err != nil {
		t.Fatal(err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	port := sa.(*syscall.SockaddrInet4).Port
	addr := "127.0.0.1:" + strconv.Itoa(port)
	for i := 0; i < 8; i++ {
		c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err != nil {
			return port
		}
		t.Cleanup(func() { c.Close() })
	}
	t.Skip("this system answers the connects to a listener whose queue is full")
	return 0
}

// A SOCKS5 client is not told that its connection began while the destination has not
// answered the peer's connect: with a destination that never answers it gets the
// reply for a host that cannot be reached, when the peer has given up. Before version
// 0.11.0 it was told "succeeded" a moment after the peer took the session.
func TestSocksSaysNothingBeforeTheDestinationAnswers(t *testing.T) {
	_, b, proxy := socksPair(t)
	port := blackhole(t)
	start := time.Now()
	_, rep, err := socksAsk(t, proxy, 1, 1, []byte{127, 0, 0, 1}, port)
	if err != nil {
		t.Fatalf("no reply: %v", err)
	}
	if rep != socksHostUnreachable {
		t.Errorf("reply %d after %v, want %d. The rule says: %q", rep, time.Since(start), socksHostUnreachable, statusOf(t, b, "s").Error)
	}
	if d := time.Since(start); d < 5*time.Second {
		t.Errorf("the reply came after %v, before the peer can have given the destination up", d)
	}
}
