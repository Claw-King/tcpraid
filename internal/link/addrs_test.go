package link

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/node"
)

// deadAddr returns a loopback address that refuses connections: a port that was just
// closed. Should another listener take it meanwhile, a dial of a path still fails
// there, because that listener is not the peer.
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// silentAddr returns the address of a listener that accepts connections and never says
// anything: a handshake with it does not end by itself.
func silentAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var held []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, c)
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		<-done
		for _, c := range held {
			c.Close()
		}
	})
	return ln.Addr().String()
}

// A path with several addresses is dialed address by address: a first one that refuses
// or does not answer does not stop the path, and the one that worked comes first the
// next time.
func TestDialTriesTheAddressesOfAPath(t *testing.T) {
	a, b := startNode(t, "alpha", nil), startNode(t, "beta", nil)
	_, alpha := pairNodes(t, a, b) // beta's record of alpha
	d := b.svc.Dialer

	dead := deadAddr(t)
	path := tlsPath(dead)
	path.Addrs = []string{a.addr}
	conn, err := d.Dial(bg, alpha, path)
	if err != nil {
		t.Fatalf("a path whose first address refuses: %v", err)
	}
	conn.Close()
	if got := d.order(alpha, path); !slices.Equal(got, []string{a.addr, dead}) {
		t.Errorf("after the second address worked the order is %v", got)
	}
	// with the address that works first, nothing is remembered and nothing changes
	good := tlsPath(a.addr)
	good.Addrs = []string{dead}
	if conn, err = d.Dial(bg, alpha, good); err != nil {
		t.Fatalf("a path whose first address works: %v", err)
	}
	conn.Close()
	if got := d.order(alpha, good); !slices.Equal(got, []string{a.addr, dead}) {
		t.Errorf("the order of a path whose first address works: %v", got)
	}
	// Open and Check go the same way
	if conn, err = d.Open(bg, alpha, path); err != nil {
		t.Fatalf("Open over a path whose first address refuses: %v", err)
	}
	conn.Close()
	if err := d.Check(bg, alpha, good); err != nil {
		t.Errorf("Check of a path at its first address: %v", err)
	}
	if err := d.CheckAt(bg, alpha, path, dead); err == nil {
		t.Error("CheckAt of the address that refuses passed")
	}
	if err := d.CheckAt(bg, alpha, path, a.addr); err != nil {
		t.Errorf("CheckAt of the address that works: %v", err)
	}

	// an address that accepts and stays silent costs AddrTimeout and no more
	defer func(was time.Duration) { addrTimeout = was }(addrTimeout)
	addrTimeout = time.Second
	silent := tlsPath(silentAddr(t))
	silent.Addrs = []string{a.addr}
	began := time.Now()
	if conn, err = d.Dial(bg, alpha, silent); err != nil {
		t.Fatalf("a path whose first address stays silent: %v", err)
	}
	conn.Close()
	if took := time.Since(began); took < addrTimeout || took > 20*time.Second {
		t.Errorf("the dial took %v, want the time of one address and then the next", took)
	}
}

// When no address of a path gives a connection, the error says what happened at each,
// and the causes can still be told.
func TestDialSaysWhatEveryAddressDid(t *testing.T) {
	a, b := startNode(t, "alpha", nil), startNode(t, "beta", nil)
	_, alpha := pairNodes(t, a, b)
	d := b.svc.Dialer
	one, two := deadAddr(t), deadAddr(t)
	path := tlsPath(one)
	path.Addrs = []string{two}
	_, err := d.Dial(bg, alpha, path)
	if err == nil {
		t.Fatal("a path with two addresses that refuse gave a connection")
	}
	text := err.Error()
	if !strings.Contains(text, "none of the path's 2 addresses gave a connection") || !strings.Contains(text, one+": ") || !strings.Contains(text, two+": ") {
		t.Errorf("the error does not name both addresses: %v", err)
	}
	var op *net.OpError
	if !errors.As(err, &op) || op.Op != "dial" {
		t.Errorf("the cause is not to be found in the error: %v", err)
	}
	// a time that runs out before the second address: it is said, and tried first next
	defer func(was time.Duration) { addrTimeout = was }(addrTimeout)
	addrTimeout = time.Minute
	silent := tlsPath(silentAddr(t))
	silent.Addrs = []string{a.addr}
	ctx, cancel := context.WithTimeout(bg, time.Second)
	_, err = d.Dial(ctx, alpha, silent)
	cancel()
	if err == nil || !strings.Contains(err.Error(), a.addr+" was not tried, because the time was up") {
		t.Fatalf("a dial that ran out of time: %v", err)
	}
	if got := d.order(alpha, silent); got[0] != a.addr {
		t.Errorf("after a dial that did not get to the second address the order is %v", got)
	}
	conn, err := d.Dial(bg, alpha, silent)
	if err != nil {
		t.Fatalf("the next dial, which begins at the second address: %v", err)
	}
	conn.Close()
}

// A pairing code with several addresses gives the new path all of them, the one that
// worked first, and the check of the peer says how each address does.
func TestJoinKeepsEveryAddressOfTheCode(t *testing.T) {
	a, b := startNode(t, "alpha", nil), startNode(t, "beta", nil)
	dead := deadAddr(t)
	text, _, err := a.svc.CreatePairingCode([]string{dead, a.addr}, 0)
	if err != nil {
		t.Fatal(err)
	}
	p, err := b.svc.Join(bg, text, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Paths) != 1 || p.Paths[0].Addr != a.addr || !slices.Equal(p.Paths[0].Addrs, []string{dead}) || !slices.Equal(p.Addresses, []string{a.addr, dead}) {
		t.Fatalf("the peer after joining with a code of two addresses: %+v", p)
	}
	results, err := b.svc.CheckPeer(bg, p.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Addr != a.addr || !results[0].OK || results[1].Addr != dead || results[1].OK || results[1].Error == "" ||
		results[0].Name != node.DefaultPathName || results[1].Name != node.DefaultPathName {
		t.Errorf("the check of a path with two addresses: %+v", results)
	}
	// an address given by hand is the only one the path gets
	c := startNode(t, "gamma", nil)
	text, _, err = a.svc.CreatePairingCode([]string{dead, a.addr}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p, err = c.svc.Join(bg, text, a.addr); err != nil || p.Paths[0].Addr != a.addr || len(p.Paths[0].Addrs) != 0 {
		t.Errorf("joining at an address given by hand: %+v, %v", p.Paths, err)
	}
}

// A path with a socket mark: where the node may not set it, the dial says which
// capability is missing; where the system has no mark, the path is refused.
func TestDialWithAMark(t *testing.T) {
	a, b := startNode(t, "alpha", nil), startNode(t, "beta", nil)
	_, alpha := pairNodes(t, a, b)
	path := tlsPath(a.addr)
	path.Mark = 7
	conn, err := b.svc.Dialer.Dial(bg, alpha, path)
	switch {
	case !engine.MarkSupported:
		if err == nil || !strings.Contains(err.Error(), "only be set on Linux") {
			t.Errorf("a path with a mark on a system without one: %v", err)
		}
	case err == nil:
		conn.Close() // the tests run with the capability
	case !strings.Contains(err.Error(), "CAP_NET_ADMIN") || !strings.Contains(err.Error(), "mark 7"):
		t.Errorf("a mark that could not be set: %v", err)
	}
}

// In the owncert tier with verify ca, the host name the certificate must be for is the
// one of the address that is being dialed, not the one of the path's first address.
func TestOwnCertChecksTheHostOfTheAddressDialed(t *testing.T) {
	ca := newTestCA(t, "127.0.0.1")
	a, b := startNode(t, "alpha", &ca.leaf), startNode(t, "beta", nil)
	_, alpha := pairNodes(t, a, b)
	b.svc.Dialer.RootCAs = ca.pool
	_, port, err := net.SplitHostPort(a.addr)
	if err != nil {
		t.Fatal(err)
	}
	byName := net.JoinHostPort("localhost", port) // the same listener under a name the certificate is not for
	path := node.Path{Name: "p", Type: "tcp", Addr: byName, Addrs: []string{a.addr}, Security: node.SecurityOwnCert, Verify: node.VerifyCA,
		Roles: []string{"data"}, Subs: 8}
	if err := b.svc.Dialer.CheckAt(bg, alpha, path, byName); err == nil {
		t.Error("the certificate for 127.0.0.1 was accepted at the address localhost")
	}
	if err := b.svc.Dialer.CheckAt(bg, alpha, path, a.addr); err != nil {
		t.Errorf("the certificate for 127.0.0.1 at the address 127.0.0.1: %v", err)
	}
	// the path as a whole: the first address fails on the name, the second works
	conn, err := b.svc.Dialer.Dial(bg, alpha, path)
	if err != nil {
		t.Fatalf("a path whose second address has the name the certificate is for: %v", err)
	}
	conn.Close()
}
