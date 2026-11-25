package engine

import (
	"io"
	"net"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

// localEnd is the application's connection as a node finds it when a write to the
// application and the end of the application's data come together. Its first read gives
// first. Its next read waits until a write is in flight and then returns the end of the
// data. That write returns hold later, with werr.
//
// With werr set this is what Linux shows when the application resets the connection
// while the node writes to it: the reset is reported to the write, and the read sees
// the end of the data. With werr nil it is an application that ended its sending side
// in order and reads slowly.
type localEnd struct {
	net.Conn // nil: only the methods below are used
	first    []byte
	hold     time.Duration
	werr     error

	mu      sync.Mutex
	gave    bool
	held    bool
	n       int
	writing chan struct{} // closed when the first write has begun
	readEnd chan struct{} // closed when a read has returned the end
	closed  chan struct{}
	wonce   sync.Once
	ronce   sync.Once
	conce   sync.Once
}

func newLocalEnd(first string, hold time.Duration, werr error) *localEnd {
	return &localEnd{first: []byte(first), hold: hold, werr: werr,
		writing: make(chan struct{}), readEnd: make(chan struct{}), closed: make(chan struct{})}
}

func (c *localEnd) Read(p []byte) (int, error) {
	c.mu.Lock()
	if !c.gave {
		c.gave = true
		c.mu.Unlock()
		return copy(p, c.first), nil
	}
	c.mu.Unlock()
	select {
	case <-c.writing:
	case <-c.closed:
		return 0, io.ErrClosedPipe
	}
	c.ronce.Do(func() { close(c.readEnd) })
	return 0, io.EOF
}

func (c *localEnd) Write(p []byte) (int, error) {
	c.wonce.Do(func() { close(c.writing) })
	c.mu.Lock()
	first := !c.held
	c.held = true
	c.mu.Unlock()
	if first {
		select {
		case <-c.readEnd:
		case <-c.closed:
			return 0, io.ErrClosedPipe
		}
		select {
		case <-time.After(c.hold):
		case <-c.closed:
			return 0, io.ErrClosedPipe
		}
		if c.werr != nil {
			return 0, c.werr
		}
	}
	c.mu.Lock()
	c.n += len(p)
	c.mu.Unlock()
	return len(p), nil
}

func (c *localEnd) Close() error                     { c.conce.Do(func() { close(c.closed) }); return nil }
func (c *localEnd) SetReadDeadline(time.Time) error  { return nil }
func (c *localEnd) SetWriteDeadline(time.Time) error { return nil }
func (c *localEnd) got() int                         { c.mu.Lock(); defer c.mu.Unlock(); return c.n }
func (c *localEnd) LocalAddr() net.Addr              { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1} }
func (c *localEnd) RemoteAddr() net.Addr             { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2} }

// what a destination read from one connection, after it wrote all it writes
type destReading struct {
	n    int64
	werr error
	rerr error
}

// talkingDest is a destination that writes amount bytes to each connection and then
// reads it to its end. It uses one goroutine for a connection, so that no other call of
// its own can be the one that a reset is reported to: an end that its read returns was
// an end.
func talkingDest(t *testing.T, amount int) (*net.TCPAddr, chan destReading) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	ch := make(chan destReading, 16)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, werr := c.Write(make([]byte, amount))
				n, rerr := io.Copy(io.Discard, c)
				ch <- destReading{n, werr, rerr}
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr), ch
}

func localEndRoute(t *testing.T, dst *net.TCPAddr) *Route {
	k := NewKeys([]byte("key material of the pair, 32 b.."))
	allow, err := ParseAllow("127.0.0.1/32:" + strconv.Itoa(dst.Port))
	if err != nil {
		t.Fatal(err)
	}
	exit := New(DefaultConfig())
	addr := subListener(t, exit, SubOptions{Keys: k, Peer: "alpha", Allow: allow, AllowName: "the allow list of the peer"})
	entry := New(DefaultConfig())
	return entry.NewRoute([]DialGroup{{Name: "p", Want: 2, Dial: dialTo(addr)}}, k, "")
}

// A reset that the write to the application took must not reach the other end as the
// end of a complete stream: the read of the application's data returns the end, and
// the write that is in flight returns the reset a moment later.
func TestAnEndReadWhileAWriteTakesTheResetIsNoEnd(t *testing.T) {
	dst, readings := talkingDest(t, 64<<10)
	route := localEndRoute(t, dst)
	local := newLocalEnd("hello", 300*time.Millisecond, syscall.ECONNRESET)
	done := make(chan End, 1)
	go func() {
		end, _ := route.Carry(local, dst.IP, dst.Port, Owner{Peer: "beta", Rule: "r"}, nil)
		done <- end
	}()
	select {
	case r := <-readings:
		if r.werr == nil && r.rerr == nil {
			t.Errorf("the application was reset while the node wrote to it, and the destination read %d bytes and then the end of a complete stream", r.n)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the destination's connection did not end")
	}
	if end := ended(t, done); end.Clean {
		t.Errorf("the session ended as a complete one: %+v", end)
	}
}

// An application that ends its sending side in order while a write to it is held has
// its end passed on, after localEndWait at most, and the session ends complete.
func TestAnEndReadWhileAWriteIsHeldIsPassedOn(t *testing.T) {
	const amount = 64 << 10
	dst, readings := talkingDest(t, amount)
	route := localEndRoute(t, dst)
	local := newLocalEnd("hello", localEndWait+3*time.Second, nil)
	done := make(chan End, 1)
	t0 := time.Now()
	go func() {
		end, _ := route.Carry(local, dst.IP, dst.Port, Owner{Peer: "beta", Rule: "r"}, nil)
		done <- end
	}()
	select {
	case r := <-readings:
		if r.werr != nil || r.rerr != nil || r.n != 5 {
			t.Errorf("the destination read %d bytes, write error %v, read error %v, want 5 bytes and the end", r.n, r.werr, r.rerr)
		}
		if d := time.Since(t0); d > localEndWait+2*time.Second {
			t.Errorf("the end reached the destination after %v, which is later than the wait for a write in flight", d)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the destination's connection did not end")
	}
	if end := ended(t, done); !end.Clean {
		t.Errorf("the session did not end complete: %+v", end)
	}
	if n := local.got(); n != amount {
		t.Errorf("the application was given %d bytes of %d", n, amount)
	}
}
