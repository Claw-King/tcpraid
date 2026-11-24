package forward

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/node"
)

// The tests give the peer a second to report that it could not connect a session,
// where a node waits 150 ms: a busy machine must not turn a refusal into a success.
func init() { entrySettle = time.Second }

// socksAsk connects to a SOCKS5 rule at proxy, offers "no authentication" and sends
// the request built from cmd, atyp, the address bytes and the port. It returns the
// connection and the reply code; 0xFF with an error when no reply came.
func socksAsk(t *testing.T, proxy string, cmd, atyp byte, addr []byte, port int) (net.Conn, byte, error) {
	t.Helper()
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(40 * time.Second))
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		return c, 0xFF, err
	}
	var m [2]byte
	if _, err := io.ReadFull(c, m[:]); err != nil {
		return c, 0xFF, err
	}
	if m != [2]byte{5, 0} {
		t.Fatalf("the rule answered the methods with %v, want 5 0", m)
	}
	req := append([]byte{5, cmd, 0, atyp}, addr...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		return c, 0xFF, err
	}
	var rep [10]byte
	if _, err := io.ReadFull(c, rep[:]); err != nil {
		return c, 0xFF, err
	}
	if rep[0] != 5 || rep[2] != 0 || rep[3] != 1 {
		t.Fatalf("a reply of %v: want version 5, a reserved byte of 0 and an IPv4 address", rep)
	}
	c.SetDeadline(time.Time{})
	return c, rep[1], nil
}

// socksName is the address bytes of a domain name in a request.
func socksName(name string) []byte { return append([]byte{byte(len(name))}, name...) }

// through sends size random bytes on c, closes its side, reads to the end and returns
// the hash of what was sent.
func through(t *testing.T, c net.Conn, size int) [sha256.Size]byte {
	t.Helper()
	data := make([]byte, size)
	rand.Read(data)
	if _, err := c.Write(data); err != nil {
		t.Fatal(err)
	}
	c.(*net.TCPConn).CloseWrite()
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.Copy(io.Discard, c); err != nil {
		t.Fatalf("the connection through the SOCKS5 rule ended with %v", err)
	}
	return sha256.Sum256(data)
}

// socksPair starts alpha, where the destinations are, and beta, which holds a path to
// alpha and has a SOCKS5 rule called s. It returns the address of the rule.
func socksPair(t *testing.T, configureAlpha ...func(*Runtime)) (a, b *testNode, proxy string) {
	t.Helper()
	return socksPairWith(t, configureAlpha, nil)
}

// socksPairWith is socksPair with something to set on beta as well.
func socksPairWith(t *testing.T, configureAlpha, configureBeta []func(*Runtime)) (a, b *testNode, proxy string) {
	t.Helper()
	a, b = startNode(t, "alpha", configureAlpha...), startNode(t, "beta", configureBeta...)
	pairNodes(t, a, b, 2)
	helloed(t, a, b)
	proxy = loop(freePorts(t, 1))
	if err := b.rt.AddRule(node.Rule{Name: "s", Socks: proxy, Peer: "alpha", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, b, "s"); !st.Listening || st.Error != "" {
		t.Fatalf("the SOCKS5 rule after adding: %+v", st)
	}
	return a, b, proxy
}

// A SOCKS5 rule carries each connection to the destination its client asks for: by
// address, and by a name that only the peer can look up.
func TestSocksRuleCarriesWhereTheClientAsks(t *testing.T) {
	res := &names{known: map[string][]net.IP{"files.site-a.example": {net.IPv4(127, 0, 0, 1)}}}
	a, b, proxy := socksPair(t, func(rt *Runtime) { rt.Lookup = res.lookup })
	dst, got := sink(t)

	c, rep, err := socksAsk(t, proxy, 1, 1, []byte{127, 0, 0, 1}, dst)
	if err != nil || rep != 0 {
		t.Fatalf("a request by address: reply %d, %v\n%s", rep, err, b.logs)
	}
	sum := through(t, c, 1<<20)
	if arr := wait(t, got); arr.n != 1<<20 || arr.sum != sum || arr.err != nil {
		t.Fatalf("%d bytes arrived (%v), or they differ from what was sent", arr.n, arr.err)
	}

	c, rep, err = socksAsk(t, proxy, 1, 3, socksName("files.site-a.example"), dst)
	if err != nil || rep != 0 {
		t.Fatalf("a request by name: reply %d, %v\n%s", rep, err, b.logs)
	}
	sum = through(t, c, 1<<20)
	if arr := wait(t, got); arr.n != 1<<20 || arr.sum != sum || arr.err != nil {
		t.Fatalf("by name, %d bytes arrived (%v), or they differ from what was sent", arr.n, arr.err)
	}
	if res.count() != 1 {
		t.Errorf("alpha looked up %d names, want the one of the request", res.count())
	}
	eventually(t, "the sessions have ended", func() bool { return len(a.e.SessionList())+len(b.e.SessionList()) == 0 })
}

// A destination that speaks first is heard only after the reply: the client gets the
// reply, then the stream.
func TestSocksReplyComesBeforeTheStream(t *testing.T) {
	_, _, proxy := socksPair(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			d, err := ln.Accept()
			if err != nil {
				return
			}
			d.Write([]byte("hello from the destination\n"))
			d.Close()
		}
	}()
	c, rep, err := socksAsk(t, proxy, 1, 1, []byte{127, 0, 0, 1}, ln.Addr().(*net.TCPAddr).Port)
	if err != nil || rep != 0 {
		t.Fatalf("reply %d, %v", rep, err)
	}
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	all, err := io.ReadAll(c)
	if err != nil || string(all) != "hello from the destination\n" {
		t.Fatalf("after the reply the client read %q (%v)", all, err)
	}
}

// What the peer does not let through and what it cannot connect to is answered with
// the reply code for it, and the peer's allow list holds for a SOCKS5 rule as for any.
func TestSocksReplySaysWhyNot(t *testing.T) {
	res := &names{known: map[string][]net.IP{"db.site-a.example": {net.IPv4(127, 0, 0, 1)}}}
	a, b, proxy := socksPair(t, func(rt *Runtime) { rt.Lookup = res.lookup })
	dst, got := sink(t)
	_, closed, _ := net.SplitHostPort(refusing(t))
	refused, _ := strconv.Atoi(closed)

	// alpha lets beta reach one port only, which is not the sink's
	allow := "127.0.0.1/32:" + strconv.Itoa(refused)
	if _, err := a.svc.SetPeerOptions("beta", &allow, nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		what string
		atyp byte
		addr []byte
		port int
		want byte
		says string
	}{
		{"an address the allow list does not hold", 1, []byte{127, 0, 0, 1}, dst, 2, "not in the allow list"},
		{"a name whose address the allow list does not hold", 3, socksName("db.site-a.example"), dst, 2, "allow list"},
		{"a destination that refuses", 1, []byte{127, 0, 0, 1}, refused, 4, "refused"},
		{"a name the peer does not know", 3, socksName("nothing.site-a.example"), dst, 4, "no such host"},
	} {
		var rep byte
		eventually(t, c.what+" gets its reply code", func() bool {
			conn, r, err := socksAsk(t, proxy, 1, c.atyp, c.addr, c.port)
			if err != nil {
				t.Fatalf("%s: no reply: %v", c.what, err)
			}
			rep = r
			// the reply is the last thing on the connection
			conn.SetReadDeadline(time.Now().Add(30 * time.Second))
			if n, err := io.Copy(io.Discard, conn); n != 0 || err != nil {
				t.Fatalf("%s: after the reply came %d bytes and %v, want a plain end", c.what, n, err)
			}
			// a control connection that is being opened afresh gives another reason
			return r != 1
		})
		if rep != c.want {
			t.Errorf("%s: reply %d, want %d. The rule says: %q", c.what, rep, c.want, statusOf(t, b, "s").Error)
		}
		eventually(t, c.what+": the rule says why", func() bool { return strings.Contains(statusOf(t, b, "s").Error, c.says) })
	}
	select {
	case arr := <-got:
		t.Errorf("a connection reached the destination that the allow list does not hold: %+v", arr)
	default:
	}
	eventually(t, "no session is left", func() bool { return len(a.e.SessionList())+len(b.e.SessionList()) == 0 })
}

// What is no CONNECT request for an address the rule can read is refused with the code
// for it, and what is no SOCKS5 gets no reply at all.
func TestSocksRefusesWhatItDoesNotCarry(t *testing.T) {
	_, _, proxy := socksPair(t)
	if _, rep, err := socksAsk(t, proxy, 2, 1, []byte{127, 0, 0, 1}, 80); err != nil || rep != 7 {
		t.Errorf("BIND: reply %d, %v, want 7", rep, err)
	}
	if _, rep, err := socksAsk(t, proxy, 3, 1, []byte{127, 0, 0, 1}, 80); err != nil || rep != 7 {
		t.Errorf("UDP ASSOCIATE: reply %d, %v, want 7", rep, err)
	}
	if _, rep, err := socksAsk(t, proxy, 1, 9, []byte{1, 2, 3, 4}, 80); err != nil || rep != 8 {
		t.Errorf("an unknown address type: reply %d, %v, want 8", rep, err)
	}
	if _, rep, err := socksAsk(t, proxy, 1, 3, socksName("not a name"), 80); err != nil || rep != 1 {
		t.Errorf("a name with a space in it: reply %d, %v, want 1", rep, err)
	}
	if _, rep, err := socksAsk(t, proxy, 1, 1, []byte{127, 0, 0, 1}, 0); err != nil || rep != 1 {
		t.Errorf("port 0: reply %d, %v, want 1", rep, err)
	}

	// a client that offers only methods with authentication is told that none fits
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(30 * time.Second))
	c.Write([]byte{5, 2, 1, 2})
	answer, err := io.ReadAll(c)
	if err != nil || !bytes.Equal(answer, []byte{5, 0xFF}) {
		t.Errorf("methods 1 and 2 only: the rule answered %v (%v), want 5 255 and the end", answer, err)
	}

	// another protocol: closed without a word
	c2, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	c2.SetDeadline(time.Now().Add(30 * time.Second))
	c2.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
	if answer, _ := io.ReadAll(c2); len(answer) != 0 {
		t.Errorf("an HTTP request got %q, want nothing", answer)
	}
}

// A client that does not finish its request is closed when its time is up, and only so
// many may be in that state at once.
func TestSocksOpeningIsBounded(t *testing.T) {
	const opening = 2 * time.Second
	_, _, proxy := socksPairWith(t, nil, []func(*Runtime){func(rt *Runtime) { rt.socksOpening = opening }})

	began := time.Now()
	var idle []net.Conn
	for i := 0; i < maxSocksOpening; i++ {
		c, err := net.Dial("tcp", proxy)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.Write([]byte{5}) // half of the first two bytes
		idle = append(idle, c)
	}
	// one more than the rule keeps waiting: closed at once, long before the time is up
	extra, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	extra.SetReadDeadline(time.Now().Add(30 * time.Second))
	var ne net.Error
	if _, err := extra.Read(make([]byte, 1)); err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("the client beyond the bound read %v, want the end of the connection", err)
	}
	if waited := time.Since(began); waited >= opening {
		t.Skipf("the machine took %v to open the connections, which is the time the rule gives a client", waited)
	}
	// the ones that wait are closed when their time is up
	for _, c := range idle {
		c.SetReadDeadline(time.Now().Add(30 * time.Second))
		if n, err := c.Read(make([]byte, 1)); n != 0 || err == nil {
			t.Fatalf("an idle client read %d bytes and %v, want the end of the connection", n, err)
		}
	}
	if time.Since(began) < opening {
		t.Errorf("the idle clients were closed after %v, before their time of %v was up", time.Since(began), opening)
	}
	// and the rule takes clients again
	dst, _ := sink(t)
	if _, rep, err := socksAsk(t, proxy, 1, 1, []byte{127, 0, 0, 1}, dst); err != nil || rep != 0 {
		t.Errorf("a client after the idle ones were closed: reply %d, %v", rep, err)
	}
}

// A name needs a peer that looks names up. With an older peer the client gets the
// general failure, and the rule says what the peer lacks.
func TestSocksNameNeedsAPeerThatLooksUp(t *testing.T) {
	_, b, proxy := socksPair(t, func(rt *Runtime) { rt.noResolve = true })
	_, rep, err := socksAsk(t, proxy, 1, 3, socksName("files.site-a.example"), 873)
	if err != nil || rep != 1 {
		t.Fatalf("a name through a peer that looks up no names: reply %d, %v, want 1", rep, err)
	}
	if st := statusOf(t, b, "s"); !strings.Contains(st.Error, "does not look up host names") || !strings.Contains(st.Error, "0.4.0") {
		t.Errorf("the rule says %q", st.Error)
	}
}

// A SOCKS5 rule is one kind of rule: it cannot also be another, it listens on one
// port, and it collides with a fixed rule on the same port.
func TestSocksRuleIsChecked(t *testing.T) {
	a, b := startNode(t, "alpha"), startNode(t, "beta")
	pairNodes(t, a, b, 2)
	port := freePorts(t, 1)
	for _, r := range []node.Rule{
		{Name: "x", Socks: loop(port), Listen: loop(port), Destination: "192.0.2.1:80", Peer: "alpha", Enabled: true},
		{Name: "x", Socks: loop(port), Match: "192.0.2.0/24", Peer: "alpha", Enabled: true},
		{Name: "x", Socks: "127.0.0.1:1080-1081", Peer: "alpha", Enabled: true},
		{Name: "x", Socks: ":1080", Peer: "alpha", Enabled: true}, // stored with its address
		{Name: "x", Socks: "localhost:1080", Peer: "alpha", Enabled: true},
	} {
		if err := b.rt.AddRule(r); !errors.Is(err, node.ErrInvalid) {
			t.Errorf("rule %+v: %v, want it refused", r, err)
		}
	}
	if err := b.rt.AddRule(node.Rule{Name: "s", Socks: loop(port), Peer: "alpha", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	err := b.rt.AddRule(node.Rule{Name: "f", Listen: ":" + strconv.Itoa(port), Destination: "192.0.2.1:80", Peer: "alpha", Enabled: true})
	if !errors.Is(err, node.ErrInvalid) || !strings.Contains(err.Error(), "already takes") {
		t.Errorf("a fixed rule on the port of a SOCKS5 rule: %v", err)
	}
	if got, err := node.SocksAddr("1080"); got != "127.0.0.1:1080" || err != nil {
		t.Errorf("SocksAddr(1080) = %q, %v", got, err)
	}
	if got, err := node.SocksAddr(":1080"); got != "127.0.0.1:1080" || err != nil {
		t.Errorf("SocksAddr(:1080) = %q, %v", got, err)
	}
	if got, err := node.SocksAddr("[::1]:1080"); got != "[::1]:1080" || err != nil || !node.SocksLoopback(got) {
		t.Errorf("SocksAddr([::1]:1080) = %q, %v", got, err)
	}
	if node.SocksLoopback("192.0.2.7:1080") || node.SocksLoopback("0.0.0.0:1080") {
		t.Error("an address other hosts reach counts as loopback")
	}
	// a rule on an address that others reach says so when it begins to listen
	open := "0.0.0.0:" + strconv.Itoa(freePorts(t, 1))
	if err := b.rt.AddRule(node.Rule{Name: "open", Socks: open, Peer: "alpha", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.logs.String(), "is not a loopback address, and a SOCKS5 rule asks no client who it is") {
		t.Errorf("no warning in the log:\n%s", b.logs)
	}
}

// A SOCKS5 rule on the node without a path: the peer opens the sub-connections, as for
// a fixed rule there, and the rule's dial is kept to.
func TestSocksRuleWhereThePeerDials(t *testing.T) {
	a, b := startNode(t, "alpha"), startNode(t, "beta")
	pairNodes(t, a, b, 2) // beta holds the path, alpha has none
	helloed(t, a, b)
	dst, got := sink(t)
	proxy := loop(freePorts(t, 2))
	if err := a.rt.AddRule(node.Rule{Name: "s", Socks: proxy, Peer: "beta", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	c, rep, err := socksAsk(t, proxy, 1, 1, []byte{127, 0, 0, 1}, dst)
	if err != nil || rep != 0 {
		t.Fatalf("a request on the node without a path: reply %d, %v. The rule says: %q\n%s", rep, err, statusOf(t, a, "s").Error, a.logs)
	}
	sum := through(t, c, 1<<20)
	if arr := wait(t, got); arr.n != 1<<20 || arr.sum != sum || arr.err != nil {
		t.Fatalf("%d bytes arrived (%v), or they differ from what was sent", arr.n, arr.err)
	}
	if !strings.Contains(a.logs.String(), "sub-connections opened by the other end") && !strings.Contains(a.logs.String(), "begins on a spare connection") {
		t.Logf("alpha's log does not show that beta opened the connections:\n%s", a.logs)
	}
	// a rule that lets only this node dial cannot be carried from here, and says so
	_, port, _ := net.SplitHostPort(proxy)
	n, _ := strconv.Atoi(port)
	self := loop(n + 1)
	if err := a.rt.AddRule(node.Rule{Name: "self", Socks: self, Peer: "beta", Enabled: true, Dial: node.DialSelf}); err != nil {
		t.Fatal(err)
	}
	if _, rep, err := socksAsk(t, self, 1, 1, []byte{127, 0, 0, 1}, dst); err != nil || rep != 1 {
		t.Errorf("a rule with dial self on the node without a path: reply %d, %v, want 1", rep, err)
	}
	if st := statusOf(t, a, "self"); !strings.Contains(st.Error, "dial self") {
		t.Errorf("the rule says %q", st.Error)
	}
}
