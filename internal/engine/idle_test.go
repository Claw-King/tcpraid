package engine

import (
	"io"
	"net"
	"testing"
	"time"
)

// The figure of the session list: when a session counts as waiting for its application.
func TestWhenASessionWaitsForItsApplication(t *testing.T) {
	const tick = 500 * time.Millisecond
	for _, c := range []struct {
		what       string
		ticks      int
		idle, read time.Duration // added at every tick, idle summed over two data subs
		tx, rx     int64
		unsent     bool
		repairOnly bool
		wantShare  float64 // -1: not known
		wantWait   bool
	}{
		{"an application that gives data piece by piece", 12, 950 * time.Millisecond, 480 * time.Millisecond, 1000, 10, false, false, 0.95, true},
		{"a full window: the subs are idle, the session does not wait for the application", 12, 950 * time.Millisecond, 10 * time.Millisecond, 1000, 10, false, false, 0.95, false},
		{"a path that is busy", 12, 100 * time.Millisecond, 10 * time.Millisecond, 1000, 10, false, false, 0.1, false},
		{"the end that receives", 12, 950 * time.Millisecond, 480 * time.Millisecond, 10, 1000, false, false, 0.95, false},
		{"an end that sends nothing", 12, 1000 * time.Millisecond, 500 * time.Millisecond, 0, 0, false, false, 1, false},
		{"data that is not sent yet", 12, 950 * time.Millisecond, 480 * time.Millisecond, 1000, 10, true, false, 0.95, false},
		{"only sub-connections of a repair path", 12, 0, 480 * time.Millisecond, 1000, 10, false, true, -1, false},
		{"a session of two seconds", 4, 950 * time.Millisecond, 480 * time.Millisecond, 1000, 10, false, false, -1, false},
	} {
		s := testSession(t)
		s.mu.Lock()
		s.everUp = true
		data := []*sub{testSub(1), testSub(2)}
		repair := testSub(3)
		repair.repair = true
		if c.repairOnly {
			data = nil
		}
		if c.unsent {
			s.newQ = []uint64{1}
		}
		t0 := time.Now()
		for i := 0; i <= c.ticks; i++ {
			if i > 0 {
				s.idle.acc += c.idle
				s.idle.read += c.read
				s.st.txB += c.tx
				s.st.rxB += c.rx
			}
			s.sampleIdle(append(data[:len(data):len(data)], repair), t0.Add(time.Duration(i)*tick))
		}
		share, wait := s.idleShare(), s.idle.appWait
		s.mu.Unlock()
		if (c.wantShare < 0) != (share < 0) || (c.wantShare >= 0 && (share < c.wantShare-0.02 || share > c.wantShare+0.02)) || wait != c.wantWait {
			t.Errorf("%s: share %.2f, waits for the application %v, want %.2f and %v", c.what, share, wait, c.wantShare, c.wantWait)
		}
	}
}

// idlePair starts an exit engine with the window given and an entry engine, and returns
// the two and a route from the entry to the exit.
func idlePair(t *testing.T, window int) (entry, exit *Engine, route *Route) {
	k := NewKeys([]byte("key material of the pair, 32 b.."))
	cfg := DefaultConfig()
	cfg.Window = window
	exit = New(cfg)
	addr := subListener(t, exit, SubOptions{Keys: k, Peer: "alpha"})
	entry = New(cfg)
	return entry, exit, entry.NewRoute([]DialGroup{{Name: "p", Want: 2, Dial: dialTo(addr)}}, k, "")
}

// appWaitOf returns what the engine's one session says about waiting for its
// application.
func appWaitOf(t *testing.T, e *Engine) (bool, float64) {
	t.Helper()
	l := e.SessionList()
	if len(l) != 1 {
		t.Fatalf("the engine has %d sessions, want 1", len(l))
	}
	return l[0].AppWait, l[0].IdleShare
}

// A client that asks for a file piece by piece, and asks for the next piece when it has
// the one before: the end that sends the file waits for its application, the end that
// receives it does not.
func TestAskAndAnswerWaitsForTheApplication(t *testing.T) {
	const piece = 32 << 10
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		ask, answer := make([]byte, 1), make([]byte, piece)
		for {
			if _, err := io.ReadFull(c, ask); err != nil {
				return
			}
			if _, err := c.Write(answer); err != nil {
				return
			}
		}
	}()
	dst := ln.Addr().(*net.TCPAddr)
	entry, exit, route := idlePair(t, 8<<20)
	app, _ := carry(route, dst.IP, dst.Port, Owner{Peer: "beta", Rule: "r"})
	defer app.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		buf := make([]byte, piece)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := app.Write([]byte{1}); err != nil {
				return
			}
			if _, err := io.ReadFull(app, buf); err != nil {
				return
			}
		}
	}()
	deadline := time.Now().Add(20 * time.Second)
	for {
		time.Sleep(500 * time.Millisecond)
		wait, share := appWaitOf(t, exit)
		if wait {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the end that sends the file never counted as waiting for its application (idle share %.2f)", share)
		}
	}
	if wait, _ := appWaitOf(t, entry); wait {
		t.Error("the end that receives the file counts as waiting for its application")
	}
}

// A destination that sends without pause to a client that reads slowly: the window of
// the end that sends is full, and that end does not count as waiting for its
// application, although its sub-connections have nothing to send most of the time.
func TestAFullWindowDoesNotWaitForTheApplication(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 64<<10)
		for {
			if _, err := c.Write(buf); err != nil {
				return
			}
		}
	}()
	dst := ln.Addr().(*net.TCPAddr)
	_, exit, route := idlePair(t, 4<<20)
	app, _ := carry(route, dst.IP, dst.Port, Owner{Peer: "beta", Rule: "r"})
	defer app.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		buf := make([]byte, 16<<10)
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			if _, err := io.ReadFull(app, buf); err != nil {
				return
			}
		}
	}()
	// the figure looks back over five seconds: from the seventh second on it speaks of
	// a time in which the window was full throughout
	time.Sleep(7 * time.Second)
	for i := 0; i < 6; i++ {
		if wait, share := appWaitOf(t, exit); wait {
			t.Fatalf("the end whose window is full counts as waiting for its application (idle share %.2f)", share)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
