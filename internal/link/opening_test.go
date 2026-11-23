package link

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/node"
)

func testSecret(b byte) []byte { return bytes.Repeat([]byte{b}, node.PairSecretLen) }

func TestOpeningRoundTrip(t *testing.T) {
	key, err := OpeningKey(testSecret(1))
	if err != nil {
		t.Fatal(err)
	}
	o := &Opening{Type: TypeSub, Time: time.Unix(1700000000, 0), Request: 0x0102030405060708, Path: 3, Role: 2}
	for i := range o.Fingerprint {
		o.Fingerprint[i] = byte(i)
	}
	for i := range o.Nonce {
		o.Nonce[i] = byte(0xf0 + i)
	}
	b := o.marshal(key)
	if len(b) != OpeningLen || string(b[:4]) != "TCPL" || b[4] != 1 || b[5] != TypeSub {
		t.Fatalf("opening starts % x, %d bytes", b[:6], len(b))
	}
	got, err := parseOpening(b)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Time.Equal(o.Time) || got.Type != o.Type || got.Fingerprint != o.Fingerprint || got.Nonce != o.Nonce || got.Request != o.Request || got.Path != o.Path || got.Role != o.Role {
		t.Fatalf("parsed %+v, want %+v", got, o)
	}
	if err := verifyOpening(b, got, key, o.Time.Add(time.Minute)); err != nil {
		t.Fatalf("a good opening: %v", err)
	}

	// every byte is under the MAC
	for i := 0; i < OpeningLen; i++ {
		bad := bytes.Clone(b)
		bad[i] ^= 1
		po, err := parseOpening(bad)
		if err != nil {
			continue // magic or version: refused before the MAC
		}
		if verifyOpening(bad, po, key, o.Time) == nil {
			t.Fatalf("byte %d changed and the opening still verifies", i)
		}
	}
	// another pair's key
	other, _ := OpeningKey(testSecret(2))
	if err := verifyOpening(b, got, other, o.Time); err == nil {
		t.Fatal("an opening verifies under another pair's key")
	}
	// too old, too new
	for _, d := range []time.Duration{OpeningSkew + time.Second, -OpeningSkew - time.Second} {
		if err := verifyOpening(b, got, key, o.Time.Add(d)); err == nil || !strings.Contains(err.Error(), "ten minutes") {
			t.Fatalf("an opening %v from now: %v", d, err)
		}
	}
	for _, d := range []time.Duration{OpeningSkew - time.Second, -OpeningSkew + time.Second} {
		if err := verifyOpening(b, got, key, o.Time.Add(d)); err != nil {
			t.Fatalf("an opening %v from now: %v", d, err)
		}
	}
	// form
	if _, err := parseOpening(b[:OpeningLen-1]); err == nil {
		t.Fatal("a short opening parses")
	}
	wrong := bytes.Clone(b)
	wrong[4] = 2
	if _, err := parseOpening(wrong); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("an opening of another version: %v", err)
	}
	if _, err := OpeningKey([]byte("short")); err == nil {
		t.Fatal("a short pair secret gives an opening key")
	}
	tk, _ := TransportKey(testSecret(1))
	if bytes.Equal(tk, key) {
		t.Fatal("the opening key is the transport key")
	}
}

// rawTLS opens a TLS connection to a as the node b, without sending anything.
func rawTLS(t *testing.T, a, b *testNode) *tls.Conn {
	t.Helper()
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", a.addr, &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{b.n.Identity.TLSCertificate()},
		NextProtos:         []string{ALPN},
		InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// closedUnanswered reports whether the other end closes conn without sending anything.
func closedUnanswered(conn net.Conn) bool {
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := conn.Read(make([]byte, 1))
	return n == 0 && err != nil && !strings.Contains(err.Error(), "timeout")
}

// The listener takes an opening only from the peer it names, made with the pair secret
// of that pairing; anything else is closed without an answer.
func TestAcceptorRefuses(t *testing.T) {
	a, b, c := startNode(t, "alpha", nil), startNode(t, "beta", nil), startNode(t, "gamma", nil)
	_, bOfA := pairNodes(t, a, b)
	_, cOfA := pairNodes(t, a, c)
	subs := make(chan string, 4)
	a.acc.HandleSub = func(peer node.Peer, conn net.Conn) { subs <- peer.Name; conn.Close() }
	bfp, cfp := b.n.Identity.FingerprintBytes(), c.n.Identity.FingerprintBytes()

	// the opening names gamma, TLS authenticated beta (gamma's secret would even verify)
	conn := rawTLS(t, a, b)
	if _, _, err := sendOpening(conn, TypeSub, cfp, cOfA.PairSecret, nil); err != nil {
		t.Fatal(err)
	}
	if !closedUnanswered(conn) {
		t.Fatal("an opening that names another node than TLS authenticated was not refused")
	}
	if !a.logs.has("names another node") {
		t.Fatalf("no line about it:\n%s", a.logs)
	}
	// beta with the wrong pair secret
	conn = rawTLS(t, a, b)
	sendOpening(conn, TypeCheck, bfp, testSecret(9), nil)
	if !closedUnanswered(conn) {
		t.Fatal("an opening made with another secret was answered")
	}
	// types that this acceptor has no handler for, and an unknown one, are closed
	for _, typ := range []byte{TypeReverse, TypeControl, 9} {
		conn = rawTLS(t, a, b)
		sendOpening(conn, typ, bfp, bOfA.PairSecret, nil)
		if !closedUnanswered(conn) {
			t.Fatalf("an opening of type %d was not closed", typ)
		}
	}
	// something that is not an opening
	conn = rawTLS(t, a, b)
	conn.Write(bytes.Repeat([]byte{0}, OpeningLen))
	if !closedUnanswered(conn) {
		t.Fatal("88 zero bytes were answered")
	}
	select {
	case name := <-subs:
		t.Fatalf("a refused connection reached the transport as %q", name)
	default:
	}

	// the plain listener: the opening is all there is
	plain := func(typ byte, fp [32]byte, secret []byte) net.Conn {
		conn, err := net.Dial("tcp", a.plain)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		sendOpening(conn, typ, fp, secret, nil)
		return conn
	}
	if !closedUnanswered(plain(TypeSub, bfp, testSecret(9))) {
		t.Fatal("plain: an opening made with another secret was not refused")
	}
	if !closedUnanswered(plain(TypeSub, bfp, cOfA.PairSecret)) {
		t.Fatal("plain: beta's name with gamma's secret was not refused")
	}
	var nobody [32]byte
	if !closedUnanswered(plain(TypeCheck, nobody, bOfA.PairSecret)) {
		t.Fatal("plain: an opening from a node that is not a peer was not refused")
	}
	select {
	case name := <-subs:
		t.Fatalf("a refused plain connection reached the transport as %q", name)
	default:
	}
	// and the good ones arrive, with the peer they come from
	good := plain(TypeSub, bfp, bOfA.PairSecret)
	select {
	case name := <-subs:
		if name != "beta" {
			t.Fatalf("plain sub-connection of beta arrived as %q", name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a good plain opening did not reach the transport")
	}
	good.Close()
	opened, err := b.svc.Dialer.Open(bg, bOfA, bOfA.Paths[0])
	if err != nil {
		t.Fatal(err)
	}
	opened.Close()
	select {
	case name := <-subs:
		if name != "beta" {
			t.Fatalf("sub-connection of beta arrived as %q", name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Open did not reach the transport")
	}
}

// A check passes only when both nodes hold the same pair secret, in every tier; in the
// off tier it does not pass against something that merely answers.
func TestCheckProvesThePairSecret(t *testing.T) {
	a, b := startNode(t, "alpha", nil), startNode(t, "beta", nil)
	_, bOfA := pairNodes(t, a, b)
	off := node.Path{Name: "off", Type: "tcp", Addr: a.plain, Security: node.SecurityOff, Roles: []string{"data"}, Subs: 1}
	if err := check(b, bOfA, bOfA.Paths[0]); err != nil {
		t.Fatalf("tls: %v", err)
	}
	if err := check(b, bOfA, off); err != nil {
		t.Fatalf("off: %v", err)
	}
	// beta's record of alpha with another secret: what a pairing whose answer was lost
	// and that was then repeated one-sidedly leaves behind
	stale := bOfA
	stale.PairSecret = testSecret(5)
	for _, path := range []node.Path{bOfA.Paths[0], off} {
		err := check(b, stale, path)
		if err == nil || !strings.Contains(err.Error(), "pair them again") {
			t.Fatalf("%s: a check with a pair secret alpha does not hold: %v", path.Security, err)
		}
	}
	// off tier against a listener that sends back whatever it gets
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
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	echo := off
	echo.Addr = ln.Addr().String()
	if err := check(b, bOfA, echo); err == nil {
		t.Fatal("off: a check passed against an echo")
	}
}

// A dial that its context ends gives up at once, wherever it is.
func TestDialCancelled(t *testing.T) {
	a, b := startNode(t, "alpha", nil), startNode(t, "beta", nil)
	_, bOfA := pairNodes(t, a, b)
	// a listener that accepts and stays silent: the TLS handshake hangs
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
	silent := bOfA.Paths[0]
	silent.Addr = ln.Addr().String()
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() {
		_, err := b.svc.Dialer.Open(ctx, bOfA, silent)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Open succeeded against a silent listener")
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("Open returned %v after its context ended", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Open did not return after its context ended")
	}
}

// Openings of the reverse and control types reach their handlers with the peer that
// was authenticated and the fields of the opening; a role that does not exist is closed.
func TestAcceptorReverseAndControl(t *testing.T) {
	a, b := startNode(t, "alpha", nil), startNode(t, "beta", nil)
	_, aOfB := pairNodes(t, a, b)
	type rev struct {
		peer       string
		request    uint64
		path, role byte
	}
	revs, ctls := make(chan rev, 2), make(chan string, 2)
	a.acc.HandleReverse = func(peer node.Peer, request uint64, path, role byte, conn net.Conn) {
		revs <- rev{peer.Name, request, path, role}
		conn.Close()
	}
	a.acc.HandleControl = func(peer node.Peer, conn net.Conn) { ctls <- peer.Name; conn.Close() }
	path := aOfB.Paths[0]

	conn, err := b.svc.Dialer.OpenReverse(bg, aOfB, path, 0x1122334455667788, 3, RoleRepair)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	select {
	case got := <-revs:
		if want := (rev{"beta", 0x1122334455667788, 3, RoleRepair}); got != want {
			t.Fatalf("the reverse handler got %+v, want %+v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an opening of the reverse type did not reach its handler")
	}
	conn, err = b.svc.Dialer.OpenReverse(bg, aOfB, path, 1, 0, RoleControl+1)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if !closedUnanswered(conn) {
		t.Fatal("an opening with a role that does not exist was not closed")
	}
	select {
	case got := <-revs:
		t.Fatalf("it reached the handler: %+v", got)
	default:
	}

	conn, err = b.svc.Dialer.OpenControl(bg, aOfB, path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	select {
	case name := <-ctls:
		if name != "beta" {
			t.Fatalf("the control handler got peer %q", name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an opening of the control type did not reach its handler")
	}
}
