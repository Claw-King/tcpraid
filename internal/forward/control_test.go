package forward

import (
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/control"
	"github.com/Claw-King/tcpraid/internal/link"
	"github.com/Claw-King/tcpraid/internal/node"
)

// Two paired nodes see each other online, whichever of them opened the control
// connection, with the version the other reported; a node that stops goes offline.
func TestPeersGoOnline(t *testing.T) {
	a, b := startNode(t, "alpha"), startNode(t, "beta")
	if st := a.rt.PeerStatus("beta"); st.Online || st.Version != "" || !st.LastMessage.IsZero() {
		t.Fatalf("before the pairing: %+v", st)
	}
	pairNodes(t, a, b, 2) // only beta has a path, so beta opens the control connection
	eventually(t, "both ends are online and have said hello", func() bool {
		x, y := a.rt.PeerStatus("beta"), b.rt.PeerStatus("alpha")
		return x.Online && y.Online && x.Version == "test-beta" && y.Version == "test-alpha" && !x.LastMessage.IsZero() && !y.LastMessage.IsZero()
	})
	b.stop()
	eventually(t, "alpha sees beta go", func() bool { return !a.rt.PeerStatus("beta").Online })
	if st := a.rt.PeerStatus("beta"); st.Version != "test-beta" || st.LastMessage.IsZero() {
		t.Fatalf("what was known of a peer that went offline is gone: %+v", st)
	}
}

// A rule on the node without a path: the peer, which has one, opens the sub-connections,
// and the bytes arrive unchanged. The session is listed on both ends with its owner.
func TestReverseRule(t *testing.T) {
	a, b := startNode(t, "alpha"), startNode(t, "beta")
	pairNodes(t, a, b, 3) // alpha has no path to beta
	in, out := freePorts(t, 1), freePorts(t, 1)
	got := sinks(t, out, 1)
	if err := a.rt.AddRule(node.Rule{Name: "back", Listen: loop(in), Destination: loop(out), Peer: "beta", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "beta connects to alpha", func() bool { return a.rt.PeerStatus("beta").Online })
	for i := 0; i < 2; i++ {
		sum, err := send(t, loop(in), 3<<20)
		if err != nil {
			t.Fatalf("transfer %d: the sending side ended with %v\n%s\n%s", i, err, a.logs, b.logs)
		}
		if arr := wait(t, got); arr.err != nil || arr.n != 3<<20 || arr.sum != sum {
			t.Fatalf("transfer %d: %d bytes arrived (%v), or they differ", i, arr.n, arr.err)
		}
	}
	if st := statusOf(t, a, "back"); st.Error != "" {
		t.Fatalf("the rule reports %q after two good transfers", st.Error)
	}

	// a connection that stays open: the session is there to look at
	c, err := net.Dial("tcp", loop(in))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("x"))
	eventually(t, "the session is listed on both ends", func() bool {
		al, bl := a.e.SessionList(), b.e.SessionList()
		return len(al) == 1 && len(bl) == 1 && al[0].Subs == 3 && bl[0].Subs == 3
	})
	al, bl := a.e.SessionList()[0], b.e.SessionList()[0]
	if !al.Client || al.Rule != "back" || al.Peer != "beta" || bl.Client || bl.Peer != "alpha" {
		t.Fatalf("sessions: carrying end %+v, exit end %+v", al, bl)
	}
	if statusOf(t, a, "back").Sessions != 1 {
		t.Fatal("the rule does not count its session")
	}
	// ending the pairing on the exit end resets the session on both
	if err := b.svc.RemovePeer("alpha"); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || err == io.EOF {
		t.Fatalf("the application connection ended with %v after the pairing ended, want a reset", err)
	}
	eventually(t, "no session is left", func() bool { return len(a.e.SessionList()) == 0 && len(b.e.SessionList()) == 0 })
}

// barePeer is a node that is paired with another and has a path to it, but runs
// nothing: the test plays its part on the connections.
type barePeer struct {
	n    *node.Node
	d    *link.Dialer
	peer node.Peer // its record of the node it joined
}

func joinBare(t *testing.T, maker *testNode, name string) *barePeer {
	t.Helper()
	n, err := node.Open(filepath.Join(t.TempDir(), "data"), name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	d := &link.Dialer{Identity: n.Identity}
	svc := &link.Service{Node: n, Dialer: d, Logf: func(string, ...any) {}}
	text, _, err := maker.svc.CreatePairingCode(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.Join(bg, text, "")
	if err != nil {
		t.Fatal(err)
	}
	peer, _ := n.Peer(p.Name)
	return &barePeer{n: n, d: d, peer: peer}
}

// control opens a control connection to the node that was joined.
func (b *barePeer) control(t *testing.T) (*control.Conn, net.Conn) {
	t.Helper()
	conn, err := b.d.OpenControl(bg, b.peer, b.peer.Paths[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	key, err := control.Key(b.peer.PairSecret)
	if err != nil {
		t.Fatal(err)
	}
	return control.NewConn(conn, key), conn
}

// expect reads messages until one of the type arrives and returns it.
func expect(t *testing.T, cc *control.Conn, typ string) control.Message {
	t.Helper()
	for {
		m, err := cc.Receive(5 * time.Second)
		if err != nil {
			t.Fatalf("waiting for a message of type %s: %v", typ, err)
		}
		if m.Type == typ {
			return m
		}
	}
}

func quick(rt *Runtime) {
	rt.times = times{ping: 40 * time.Millisecond, timeout: 400 * time.Millisecond, wait: 300 * time.Millisecond}
}

// The node says hello and pings; it answers a ping, ignores a type it does not know,
// answers a request it cannot serve with a failure, and takes a peer that falls silent
// for gone.
func TestControlConnection(t *testing.T) {
	a := startNode(t, "alpha", quick)
	b := joinBare(t, a, "bare")
	cc, _ := b.control(t)
	hello := expect(t, cc, control.TypeHello)
	if hello.Name != "alpha" || hello.Version != "test-alpha" || len(hello.Protocols) != 1 || hello.Protocols[0] != control.Protocol {
		t.Fatalf("hello: %+v", hello)
	}
	eventually(t, "the node counts the peer as online", func() bool { return a.rt.PeerStatus("bare").Online })
	expect(t, cc, control.TypePing)

	if err := cc.Send(control.Message{Type: control.TypeHello, Name: "bare", Version: "1.2\x1b[31m", Protocols: []int{1}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the version the peer reported is kept, without what a terminal would act on", func() bool {
		return a.rt.PeerStatus("bare").Version == "1.2?[31m"
	})
	if err := cc.Send(control.Message{Type: "from-the-future", Seq: 1}); err != nil {
		t.Fatal(err)
	}
	if err := cc.Send(control.Message{Type: control.TypePing, Seq: 77}); err != nil {
		t.Fatal(err)
	}
	if pong := expect(t, cc, control.TypePong); pong.Seq != 77 {
		t.Fatalf("pong: %+v", pong)
	}
	// the node has no path to this peer: it cannot open anything
	if err := cc.Send(control.Message{Type: control.TypeOpen, Request: 5, Session: "00"}); err != nil {
		t.Fatal(err)
	}
	if f := expect(t, cc, control.TypeOpenFail); f.Request != 5 || f.Reason == "" {
		t.Fatalf("open_fail: %+v", f)
	}

	// silence: after the timeout the node closes the connection and the peer is offline
	began := time.Now()
	for {
		if _, err := cc.Receive(5 * time.Second); err != nil {
			break
		}
		if time.Since(began) > 5*time.Second {
			t.Fatal("the node keeps a connection on which the peer says nothing")
		}
	}
	eventually(t, "the peer is offline", func() bool { return !a.rt.PeerStatus("bare").Online })
}

// A message that was not made with the pair secret ends the control connection.
func TestControlWrongKey(t *testing.T) {
	a := startNode(t, "alpha")
	b := joinBare(t, a, "bare")
	cc, raw := b.control(t)
	expect(t, cc, control.TypeHello)
	eventually(t, "online", func() bool { return a.rt.PeerStatus("bare").Online })
	other, err := control.Key(make([]byte, node.PairSecretLen))
	if err != nil {
		t.Fatal(err)
	}
	bad, err := control.Encode(control.Message{Type: control.TypePing, Seq: 1}, other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Write(bad); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the node dropped the connection", func() bool { return !a.rt.PeerStatus("bare").Online })
	if !strings.Contains(a.logs.String(), "control connection") {
		t.Fatalf("no line about the control connection:\n%s", a.logs)
	}
}

// A node that carries a connection for a peer it cannot dial asks the peer and keeps
// the request: connections for it are attached, connections for another number are
// closed, and when none arrives in time, or the peer says it cannot, the application
// connection is reset and the request is gone.
func TestReverseRequest(t *testing.T) {
	a := startNode(t, "alpha", quick)
	b := joinBare(t, a, "bare")
	cc, _ := b.control(t)
	expect(t, cc, control.TypeHello)
	eventually(t, "online", func() bool { return a.rt.PeerStatus("bare").Online })
	// the test's end of the control connection answers pings, or the node would drop it
	// (its timeout is short here), and passes on what else arrives
	msgs := make(chan control.Message, 16)
	go func() {
		for {
			m, err := cc.Receive(5 * time.Second)
			if err != nil {
				close(msgs)
				return
			}
			if m.Type == control.TypePing {
				cc.Send(control.Message{Type: control.TypePong, Seq: m.Seq})
				continue
			}
			msgs <- m
		}
	}()
	nextOpen := func() control.Message {
		t.Helper()
		select {
		case m, ok := <-msgs:
			if !ok || m.Type != control.TypeOpen || m.Request == 0 || len(m.Session) != 32 {
				t.Fatalf("want an open with a request and a session id, got %+v (connection up: %v)", m, ok)
			}
			return m
		case <-time.After(5 * time.Second):
			t.Fatal("the node did not ask for sub-connections")
			return control.Message{}
		}
	}
	in := freePorts(t, 1)
	if err := a.rt.AddRule(node.Rule{Name: "back", Listen: loop(in), Destination: "127.0.0.1:9", Peer: "bare", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	reverse := func(request uint64) net.Conn {
		t.Helper()
		conn, err := b.d.OpenReverse(bg, b.peer, b.peer.Paths[0], request, 0, link.RoleData)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		return conn
	}
	closed := func(conn net.Conn) bool {
		n, err := conn.Read(make([]byte, 1))
		return n == 0 && err != nil && !strings.Contains(err.Error(), "timeout")
	}
	ended := make(chan error, 1)
	app := func() { _, err := send(t, loop(in), 1000); ended <- err }
	reset := func(what string) {
		t.Helper()
		select {
		case err := <-ended:
			if err == nil {
				t.Fatalf("%s: the application connection ended cleanly, want a reset", what)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: the application connection was not reset", what)
		}
	}

	// 1. nothing arrives in time
	go app()
	first := nextOpen()
	reset("no sub-connection arrives")
	eventually(t, "the rule says why", func() bool { return strings.Contains(statusOf(t, a, "back").Error, "none arrived") })
	if !closed(reverse(first.Request)) {
		t.Fatal("a connection for a request that ran out was not closed")
	}
	eventually(t, "the session is gone", func() bool { return len(a.e.SessionList()) == 0 })

	// 2. the peer says it cannot
	go app()
	second := nextOpen()
	if second.Request == first.Request {
		t.Fatal("two requests with one number")
	}
	if err := cc.Send(control.Message{Type: control.TypeOpenFail, Request: second.Request, Reason: "not today\x07"}); err != nil {
		t.Fatal(err)
	}
	reset("the peer cannot open the connections")
	eventually(t, "the rule passes on the reason", func() bool {
		return strings.Contains(statusOf(t, a, "back").Error, "could not open the connections: not today?")
	})

	// 3. a connection for another number is closed; one for the request gets the header
	go app()
	third := nextOpen()
	if !closed(reverse(third.Request + 1)) {
		t.Fatal("a connection for a request the node did not make was not closed")
	}
	conn := reverse(third.Request)
	head := make([]byte, 69)
	if _, err := io.ReadFull(conn, head); err != nil || string(head[:4]) != "TCPR" {
		t.Fatalf("the node did not send the transport's header on the connection it asked for: %v, %q", err, head[:4])
	}
	// this end never answers the header: the session does not come up and is reset
	reset("the session never comes up")
}
