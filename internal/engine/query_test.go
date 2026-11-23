package engine

import (
	"encoding/json"
	"expvar"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionList(t *testing.T) {
	e := New(DefaultConfig())
	if l := e.SessionList(); l == nil || len(l) != 0 {
		t.Fatalf("no sessions: got %#v, want an empty list", l)
	}
	srv := e.newSession([16]byte{0xa1, 2}, nil, false, "192.0.2.10:873", keys{})
	live, gone, ctrl := testSub(1), testSub(2), testSub(3)
	gone.dead, ctrl.ctrl = true, true
	srv.subs = []*sub{live, gone, ctrl}
	srv.st.txB, srv.st.rxB, srv.unackedB = 300, 200, 100
	cli := e.newSession([16]byte{0x01}, nil, true, "[2001:db8::10]:22", keys{})
	cli.started = srv.started.Add(time.Second)

	l := e.SessionList()
	if len(l) != 2 {
		t.Fatalf("%d sessions listed, want 2", len(l))
	}
	want := SessionInfo{ID: [16]byte{0xa1, 2}, Destination: "192.0.2.10:873", Started: srv.started,
		TxBytes: 300, RxBytes: 200, UnackedBytes: 100, Subs: 2}
	if l[0] != want {
		t.Errorf("oldest session:\n got %+v\nwant %+v", l[0], want)
	}
	if !l[1].Client || l[1].Destination != "[2001:db8::10]:22" || l[1].Subs != 0 || l[1].ID != [16]byte{0x01} {
		t.Errorf("newer session: %+v", l[1])
	}
	// same start time: ordered by ID, so the order does not change between calls
	cli.started = srv.started
	if l = e.SessionList(); l[0].ID != [16]byte{0x01} {
		t.Errorf("equal start times: %x listed first", l[0].ID[:2])
	}
	if e.Sessions() != 2 {
		t.Fatalf("listing changed the session count to %d", e.Sessions())
	}
}

// published is the one engine of the test process that registers its counters with
// package expvar, whose names can be registered only once.
var (
	published   = New(DefaultConfig())
	publishOnce sync.Once
)

// Counters and PublishExpvar give the same names and the same values.
func TestCountersMatchExpvar(t *testing.T) {
	e := published
	publishOnce.Do(e.PublishExpvar)
	e.mSessions.Store(3)
	e.nSessions.Store(2)
	e.mBytesTx.Store(1 << 40)
	e.globalB.Store(5 << 20)
	e.writeUs.Store(7000)
	e.waitUs[wCap].Store(9000)
	e.resendBy[cDead].Store(11)
	t.Cleanup(func() { e.nSessions.Store(0); e.globalB.Store(0) })

	plain := map[string]bool{}
	keyed := map[string]int{}
	for _, c := range e.Counters() {
		v := expvar.Get(c.Name)
		if v == nil {
			t.Errorf("%s is not published by PublishExpvar", c.Name)
			continue
		}
		if (c.Label == "") != (c.Key == "") {
			t.Errorf("%s: label %q with key %q", c.Name, c.Label, c.Key)
		}
		if c.Key != "" {
			keyed[c.Name]++
			var m map[string]int64
			if err := json.Unmarshal([]byte(v.String()), &m); err != nil {
				t.Errorf("%s is not keyed in expvar: %s", c.Name, v)
			} else if got, ok := m[c.Key]; !ok || got != c.Value {
				t.Errorf("%s[%s] = %d, expvar has %s", c.Name, c.Key, c.Value, v)
			}
			continue
		}
		if plain[c.Name] {
			t.Errorf("%s is listed twice", c.Name)
		}
		plain[c.Name] = true
		// resident memory moves between the two readings
		if c.Name != "rss_mb" && v.String() != strconv.FormatInt(c.Value, 10) {
			t.Errorf("%s = %d, expvar has %s", c.Name, c.Value, v)
		}
	}
	expvar.Do(func(kv expvar.KeyValue) {
		switch {
		case kv.Key == "cmdline" || kv.Key == "memstats": // published by package expvar itself
		case keyed[kv.Key] > 0:
			var m map[string]int64
			json.Unmarshal([]byte(kv.Value.String()), &m)
			if len(m) != keyed[kv.Key] {
				t.Errorf("%s: %d keys in expvar, %d in Counters", kv.Key, len(m), keyed[kv.Key])
			}
		case !plain[kv.Key]:
			t.Errorf("%s is published by PublishExpvar and missing from Counters", kv.Key)
		}
	})
	for _, c := range e.Counters() {
		gauge := c.Name == "sessions_active" || c.Name == "unacked_mb" || c.Name == "rss_mb" || c.Name == "sojourn_ms_max"
		if c.Gauge != gauge {
			t.Errorf("%s: Gauge is %v", c.Name, c.Gauge)
		}
	}
}

// freeAddr returns a loopback address nothing listens on. The engine takes its listen
// address as text, so a test cannot hand it a listener.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// dialSoon connects to an address whose listener is being started by another goroutine.
func dialSoon(t *testing.T, addr string) net.Conn {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			return c
		}
		if time.Now().After(end) {
			t.Fatalf("nothing listens on %s: %v", addr, err)
		}
	}
}

// Sessions can be listed while a client and a server create them: the race detector
// finds no unsynchronised access, and a listed session never lacks its destination.
func TestSessionListWhileSessionsStart(t *testing.T) {
	dst, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	reached := make(chan struct{}, 64)
	go func() {
		for {
			c, err := dst.Accept()
			if err != nil {
				return
			}
			reached <- struct{}{}
			go func() { io.Copy(io.Discard, c); c.Close() }()
		}
	}()

	scfg := DefaultConfig()
	scfg.Listen = freeAddr(t)
	server := New(scfg)
	go server.RunServer()
	dialSoon(t, scfg.Listen).Close() // the server listens before the client dials its sub-connections

	ccfg := DefaultConfig()
	ccfg.Listen = freeAddr(t)
	ccfg.FixedDst = dst.Addr().String()
	client := New(ccfg)
	dial, err := NewDial(scfg.Listen, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	go client.RunClient([]DialGroup{{Name: "data", Want: 2, Dial: dial}})
	dialSoon(t, ccfg.Listen).Close()

	stop := make(chan struct{})
	var listers sync.WaitGroup
	var listed, noDst atomic.Int64
	for _, e := range []*Engine{client, server} {
		listers.Add(1)
		go func() {
			defer listers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, si := range e.SessionList() {
					listed.Add(1)
					if si.Destination == "" {
						noDst.Add(1)
					}
				}
			}
		}()
	}

	const conns = 24
	for i := 0; i < conns; i++ {
		app, err := net.Dial("tcp", ccfg.Listen)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := app.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		select {
		case <-reached:
		case <-time.After(20 * time.Second):
			t.Fatalf("connection %d did not reach the destination", i)
		}
		app.Close()
	}
	close(stop)
	listers.Wait()
	if listed.Load() == 0 {
		t.Fatal("no session was ever listed")
	}
	if noDst.Load() != 0 {
		t.Errorf("%d of %d listed sessions had no destination", noDst.Load(), listed.Load())
	}
}
