package link

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/node"
	"github.com/Claw-King/tcpraid/internal/pair"
)

// testNode is a node with its listeners on loopback.
type testNode struct {
	n     *node.Node
	svc   *Service
	srv   *Server
	acc   *Acceptor
	addr  string // TLS listener
	plain string // plain listener
	logs  *logBuffer
}

type logBuffer struct {
	mu    sync.Mutex
	lines []string
}

func (l *logBuffer) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// has reports whether a logged line contains want. The listener logs a refusal after it
// has closed the connection, so the line can arrive a moment after the dialing end saw
// the failure: has waits for it, up to two seconds.
func (l *logBuffer) has(want string) bool {
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if strings.Contains(l.String(), want) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// startNode opens a fresh node called name and serves its two listeners until the test
// ends. ownCert, if not nil, is the operator's certificate. Each configure changes the
// server before it serves.
func startNode(t *testing.T, name string, ownCert *tls.Certificate, configure ...func(*Server)) *testNode {
	t.Helper()
	n, err := node.Open(filepath.Join(t.TempDir(), "data"), name)
	if err != nil {
		t.Fatal(err)
	}
	logs := &logBuffer{}
	tn := &testNode{n: n, logs: logs}
	tn.svc = &Service{Node: n, Dialer: &Dialer{Identity: n.Identity}, Logf: logs.logf}
	tn.srv = &Server{
		Identity:          n.Identity,
		OwnCert:           ownCert,
		PeerByFingerprint: n.PeerByFingerprint,
		HandlePair:        tn.svc.HandlePair,
		Logf:              logs.logf,
	}
	tn.acc = &Acceptor{PeerByFingerprint: n.PeerByFingerprint, LogRefused: tn.srv.LogRefused}
	tn.srv.HandlePeer, tn.srv.HandlePlain = tn.acc.HandlePeer, tn.acc.HandlePlain
	tn.svc.RefusedLogf = tn.srv.LogRefused
	for _, f := range configure {
		f(tn.srv)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	listen := func(serve func(context.Context, net.Listener) error) string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := serve(ctx, ln); err != nil {
				t.Errorf("listener of %s: %v", name, err)
			}
		}()
		return ln.Addr().String()
	}
	tn.addr = listen(tn.srv.ServeTLS)
	tn.plain = listen(tn.srv.ServePlain)
	tn.svc.Advertise = []string{tn.addr}
	t.Cleanup(func() { cancel(); wg.Wait(); n.Close() })
	return tn
}

var bg = context.Background()

// pairNodes pairs joiner with maker (maker hands out the code) and returns the peer
// record each holds of the other.
func pairNodes(t *testing.T, maker, joiner *testNode) (onMaker, onJoiner node.Peer) {
	t.Helper()
	text, _, err := maker.svc.CreatePairingCode(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	p, err := joiner.svc.Join(bg, text, "")
	if err != nil {
		t.Fatal(err)
	}
	onJoiner, ok := joiner.n.Peer(p.Name)
	if !ok {
		t.Fatalf("the joining node has no peer %q", p.Name)
	}
	onMaker, ok = maker.n.PeerByFingerprint(joiner.n.Fingerprint())
	if !ok {
		t.Fatal("the node that made the code has no peer with the joining node's fingerprint")
	}
	return onMaker, onJoiner
}

func TestPairing(t *testing.T) {
	a, b := startNode(t, "alpha", nil), startNode(t, "beta", nil)
	text, rec, err := a.svc.CreatePairingCode(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rec.SecretHash != nil || len(rec.ID) != 8 || rec.Used {
		t.Fatalf("returned record %+v", rec)
	}
	got, err := b.svc.Join(bg, text, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.PairSecret != nil {
		t.Fatal("Join returned the pair secret")
	}
	onB, _ := b.n.Peer("alpha")
	onA, _ := a.n.Peer("beta")
	if onB.Fingerprint != a.n.Fingerprint() || onA.Fingerprint != b.n.Fingerprint() {
		t.Fatalf("fingerprints: beta holds %s, alpha holds %s", onB.Fingerprint, onA.Fingerprint)
	}
	if len(onA.PairSecret) != node.PairSecretLen || !bytes.Equal(onA.PairSecret, onB.PairSecret) {
		t.Fatal("the two nodes do not hold the same pair secret")
	}
	if bytes.Equal(onA.PairSecret, make([]byte, node.PairSecretLen)) {
		t.Fatal("the pair secret is all zero")
	}
	// the joining node knows the way; the other does not
	if len(onB.Paths) != 1 || onB.Paths[0].Addr != a.addr || onB.Paths[0].Security != node.SecurityTLS ||
		onB.Paths[0].Subs != 8 || len(onB.Paths[0].Roles) != 1 || onB.Paths[0].Roles[0] != "data" || onB.Addresses[0] != a.addr {
		t.Fatalf("the joining node's record: %+v", onB)
	}
	if len(onA.Paths) != 0 || len(onA.Addresses) != 0 {
		t.Fatalf("the code maker's record has a way to a node it was not told how to reach: %+v", onA)
	}
	// the code is used: listed as such, and good for nothing more
	if list := a.svc.PairingCodes(); len(list) != 1 || !list[0].Used || list[0].ID != rec.ID {
		t.Fatalf("codes after the pairing: %+v", list)
	}
	c := startNode(t, "gamma", nil)
	if _, err := c.svc.Join(bg, text, ""); !errors.Is(err, pair.ErrRefused) {
		t.Fatalf("a used code: %v, want pair.ErrRefused", err)
	}
	if c.n.PeerCount() != 0 || a.n.PeerCount() != 1 {
		t.Fatal("a refused pairing left a peer behind")
	}
	// one line per pairing, and no secret in any line
	for _, tn := range []*testNode{a, b, c} {
		logged := tn.logs.String()
		if strings.Contains(logged, text) || strings.Contains(logged, text[len(text)-40:]) {
			t.Fatalf("a log holds the pairing code:\n%s", logged)
		}
	}
	if !a.logs.has(`paired with "beta"`) || !b.logs.has(`paired with "alpha"`) || !a.logs.has("refused pairing code "+rec.ID) {
		t.Fatalf("logs:\nalpha:\n%s\nbeta:\n%s", a.logs, b.logs)
	}
}

// The transport key derived from the pair secret is the same at both ends of a pairing
// and differs between pairings.
func TestTransportKey(t *testing.T) {
	a, b, c := startNode(t, "alpha", nil), startNode(t, "beta", nil), startNode(t, "gamma", nil)
	onA, onB := pairNodes(t, a, b)
	onA2, _ := pairNodes(t, a, c)
	key := func(secret []byte) []byte {
		t.Helper()
		k, err := TransportKey(secret)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	ka, kb := key(onA.PairSecret), key(onB.PairSecret)
	if len(ka) != 32 || !bytes.Equal(ka, kb) {
		t.Fatal("the two ends derive different transport keys")
	}
	if bytes.Equal(ka, onA.PairSecret) {
		t.Fatal("the transport key is the pair secret itself")
	}
	if bytes.Equal(ka, key(onA2.PairSecret)) {
		t.Fatal("two pairings give the same transport key")
	}
	// no key from a secret that is not one: it would be the same key for everyone
	for _, bad := range [][]byte{nil, {}, onA.PairSecret[:31], append(bytes.Clone(onA.PairSecret), 0)} {
		if k, err := TransportKey(bad); err == nil || k != nil {
			t.Errorf("a pair secret of %d bytes gave a key", len(bad))
		}
	}
}

func TestPairingRefused(t *testing.T) {
	a, b := startNode(t, "alpha", nil), startNode(t, "beta", nil)
	clock := time.Now()
	a.svc.Now = func() time.Time { return clock }
	b.svc.Now = func() time.Time { return clock }

	// expired for the joining node: it does not even connect
	text, _, err := a.svc.CreatePairingCode(nil, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Minute)
	if _, err := b.svc.Join(bg, text, ""); !errors.Is(err, pair.ErrExpired) || !errors.Is(err, ErrInvalid) {
		t.Fatalf("an expired code on the joining node: %v", err)
	}
	// expired for the node that made it, while the joining node's clock is behind
	text, _, _ = a.svc.CreatePairingCode(nil, time.Minute)
	b.svc.Now = func() time.Time { return clock }
	a.svc.Now = func() time.Time { return clock.Add(2 * time.Minute) }
	if _, err := b.svc.Join(bg, text, ""); !errors.Is(err, pair.ErrRefused) {
		t.Fatalf("a code expired on the node that made it: %v, want pair.ErrRefused", err)
	}
	a.svc.Now = func() time.Time { return clock }

	// revoked
	text, rec, _ := a.svc.CreatePairingCode(nil, 0)
	if err := a.svc.RevokePairingCode(rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.svc.Join(bg, text, ""); !errors.Is(err, pair.ErrRefused) {
		t.Fatalf("a revoked code: %v", err)
	}

	// not a code, and the node's own code
	if _, err := b.svc.Join(bg, "hello", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("not a code: %v", err)
	}
	text, _, _ = a.svc.CreatePairingCode(nil, 0)
	if _, err := a.svc.Join(bg, text, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a node joining with its own code: %v", err)
	}
	if _, err := b.svc.Join(bg, text, "no port"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a bad address: %v", err)
	}

	// The code is right, but the address leads to another node: the joining node stops
	// before it sends anything about the code, and the code stays good.
	c := startNode(t, "gamma", nil)
	_, err = b.svc.Join(bg, text, c.addr)
	if err == nil || !strings.Contains(err.Error(), "not the one that made the pairing code") {
		t.Fatalf("joining at another node's address: %v", err)
	}
	if strings.Contains(err.Error(), "no connection was made") {
		t.Fatalf("a join that connected and was then given up says that it could not connect: %v", err)
	}
	if b.n.PeerCount() != 0 || c.n.PeerCount() != 0 || a.n.PeerCount() != 0 {
		t.Fatal("a pairing that stopped at the fingerprint left a peer behind")
	}
	// Nothing listens at the address: the answer says so in words, after the error itself,
	// and the node's log has the error alone.
	ln, lerr := net.Listen("tcp", "127.0.0.1:0")
	if lerr != nil {
		t.Fatal(lerr)
	}
	closed := ln.Addr().String()
	ln.Close()
	_, err = b.svc.Join(bg, text, closed)
	if err == nil || !strings.Contains(err.Error(), "dial tcp "+closed) || !strings.HasSuffix(err.Error(), "can this node reach that port?") {
		t.Fatalf("joining where nothing listens: %v", err)
	}
	if got := b.logs.String(); !strings.Contains(got, "dial tcp "+closed) || strings.Contains(got, "no connection was made") {
		t.Fatalf("the log of the failed join:\n%s", got)
	}
	if _, err := b.svc.Join(bg, text, ""); err != nil {
		t.Fatalf("the code after the attempt at the wrong address: %v", err)
	}
}

// A wrong proof is refused; the fifth in a row revokes the code, so that even the right
// code is refused afterwards.
func TestPairingWrongProof(t *testing.T) {
	a, b := startNode(t, "alpha", nil), startNode(t, "beta", nil)
	text, rec, err := a.svc.CreatePairingCode(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	code, err := pair.Parse(text, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	forged := *code
	forged.Secret[0] ^= 1 // the right id and fingerprint, the wrong secret
	for i := 1; i <= node.MaxProofFailures; i++ {
		if _, err := b.svc.Join(bg, forged.Text(), ""); !errors.Is(err, pair.ErrRefused) {
			t.Fatalf("wrong proof %d: %v, want pair.ErrRefused", i, err)
		}
		left := len(a.svc.PairingCodes())
		if want := 1; i == node.MaxProofFailures {
			if left != 0 {
				t.Fatalf("after %d wrong proofs the code is still there", i)
			}
		} else if left != want {
			t.Fatalf("after %d wrong proofs: %d codes", i, left)
		}
	}
	if _, err := b.svc.Join(bg, text, ""); !errors.Is(err, pair.ErrRefused) {
		t.Fatalf("the right code after %d wrong proofs: %v, want it refused", node.MaxProofFailures, err)
	}
	if a.n.PeerCount() != 0 || b.n.PeerCount() != 0 {
		t.Fatal("a peer was stored")
	}
	if !a.logs.has("refused pairing code " + rec.ID) {
		t.Fatalf("the refusals are not logged:\n%s", a.logs)
	}
}

func TestPairingNames(t *testing.T) {
	a := startNode(t, "alpha", nil)
	b1, b2 := startNode(t, "beta", nil), startNode(t, "beta", nil)
	on1, _ := pairNodes(t, a, b1)
	on2, _ := pairNodes(t, a, b2)
	if on1.Name != "beta" || on2.Name != "beta-2" {
		t.Fatalf("two nodes called beta are stored as %q and %q", on1.Name, on2.Name)
	}
	// pairing the same two nodes again replaces the secret and adds no record
	again, _ := pairNodes(t, a, b1)
	if again.Name != "beta" || bytes.Equal(again.PairSecret, on1.PairSecret) || a.n.PeerCount() != 2 || b1.n.PeerCount() != 1 {
		t.Fatalf("paired again: %q, %d peers on alpha, %d on beta", again.Name, a.n.PeerCount(), b1.n.PeerCount())
	}
}

func TestCreatePairingCode(t *testing.T) {
	a := startNode(t, "alpha", nil)
	a.svc.Advertise = nil
	if _, _, err := a.svc.CreatePairingCode(nil, 0); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "-advertise") {
		t.Fatalf("no address at all: %v", err)
	}
	if _, _, err := a.svc.CreatePairingCode([]string{"no port"}, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a bad address: %v", err)
	}
	if _, _, err := a.svc.CreatePairingCode([]string{"nas.example.org:9851"}, -time.Second); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a negative ttl: %v", err)
	}
	if len(a.svc.PairingCodes()) != 0 {
		t.Fatal("a refused request left a code")
	}
	before := time.Now()
	text, rec, err := a.svc.CreatePairingCode([]string{"nas.example.org:9851", "192.0.2.5:9851"}, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	code, err := pair.Parse(text, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(code.Addresses) != 2 || code.IDString() != rec.ID || code.Fingerprint != a.n.Identity.FingerprintBytes() {
		t.Fatalf("code %+v", code)
	}
	if d := rec.Expires.Sub(before); d < 9*time.Minute || d > 11*time.Minute {
		t.Fatalf("valid for %v, want 10 minutes", d)
	}
	// the default is an hour
	_, rec, _ = a.svc.CreatePairingCode([]string{"nas.example.org:9851"}, 0)
	if d := rec.Expires.Sub(before); d < 59*time.Minute || d > 61*time.Minute {
		t.Fatalf("valid for %v by default, want an hour", d)
	}
}

// check runs the check of the first path of peer on node tn.
func check(tn *testNode, peer node.Peer, path node.Path) error {
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	return tn.svc.Dialer.Check(ctx, peer, path)
}

func tlsPath(addr string) node.Path {
	return node.Path{Name: "p", Type: "tcp", Addr: addr, Security: node.SecurityTLS, Roles: []string{"data"}, Subs: 8}
}

func TestTierTLS(t *testing.T) {
	a, b := startNode(t, "alpha", nil), startNode(t, "beta", nil)
	onA, onB := pairNodes(t, a, b)
	if err := check(b, onB, onB.Paths[0]); err != nil {
		t.Fatalf("beta to alpha: %v", err)
	}
	if err := check(a, onA, tlsPath(b.addr)); err != nil {
		t.Fatalf("alpha to beta: %v", err)
	}
	// through the service, as the API does it
	res, err := b.svc.CheckPeer(bg, "alpha")
	if err != nil || len(res) != 1 || !res[0].OK || res[0].Error != "" || res[0].Addr != a.addr || res[0].Security != "tls" {
		t.Fatalf("CheckPeer: %+v, %v", res, err)
	}
	if res, err := a.svc.CheckPeer(bg, "beta"); err != nil || len(res) != 0 {
		t.Fatalf("CheckPeer of a peer without paths: %+v, %v", res, err)
	}
	if _, err := a.svc.CheckPeer(bg, "nobody"); !errors.Is(err, node.ErrNotFound) {
		t.Fatalf("CheckPeer of an unknown peer: %v", err)
	}

	// An impostor: a node with another identity key.
	x := startNode(t, "alpha", nil)
	// as the listener: beta dials what it believes is alpha and finds another key
	if err := check(b, onB, tlsPath(x.addr)); err == nil || !strings.Contains(err.Error(), "another fingerprint") {
		t.Fatalf("beta accepted an impostor as alpha: %v", err)
	}
	// as the dialer: it knows alpha's fingerprint and address, but alpha does not know it
	if err := check(x, onB, tlsPath(a.addr)); err == nil {
		t.Fatal("alpha accepted a node it is not paired with")
	}
	if !a.logs.has("not a peer of this node") {
		t.Fatalf("alpha did not log why it refused:\n%s", a.logs)
	}

	// after alpha ends the pairing, beta is refused
	if err := a.svc.RemovePeer("beta"); err != nil {
		t.Fatal(err)
	}
	if err := check(b, onB, onB.Paths[0]); err == nil {
		t.Fatal("alpha still accepts a peer it removed")
	}
	if err := a.svc.RemovePeer("beta"); !errors.Is(err, node.ErrNotFound) {
		t.Fatalf("removing twice: %v", err)
	}
}

// What is not a paired tcpraid node gets nowhere at the TLS listener.
func TestTLSListenerRefuses(t *testing.T) {
	a, b := startNode(t, "alpha", nil), startNode(t, "beta", nil)
	_, onB := pairNodes(t, a, b)
	dial := func(cfg *tls.Config) error {
		cfg.InsecureSkipVerify = true
		cfg.MinVersion = tls.VersionTLS13
		c, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", a.addr, cfg)
		if err != nil {
			return err
		}
		defer c.Close()
		return checkConn(c, b.n.Identity.FingerprintBytes(), onB.PairSecret)
	}
	cert := b.n.Identity.TLSCertificate()
	if err := dial(&tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{ALPN}}); err != nil {
		t.Fatalf("the peer itself, by hand: %v", err)
	}
	for name, cfg := range map[string]*tls.Config{
		"another protocol": {Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2"}},
		"no protocol":      {Certificates: []tls.Certificate{cert}},
		"no certificate":   {NextProtos: []string{ALPN}},
		"own, without one": {Certificates: []tls.Certificate{cert}, NextProtos: []string{ALPNOwn}},
	} {
		if err := dial(cfg); err == nil {
			t.Errorf("%s: the listener answered", name)
		}
	}
	// a pairing connection that sends garbage is closed and leaves nothing
	c, err := tls.Dial("tcp", a.addr, &tls.Config{InsecureSkipVerify: true, Certificates: []tls.Certificate{cert}, NextProtos: []string{pair.ALPN}})
	if err != nil {
		t.Fatal(err)
	}
	c.Write(bytes.Repeat([]byte{0xff}, 64))
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var one [1]byte
	if n, _ := c.Read(one[:]); n == 1 && one[0] == 0 {
		t.Fatal("garbage was accepted as a pairing")
	}
	c.Close()
	if a.n.PeerCount() != 1 {
		t.Fatal("garbage on a pairing connection changed the peers")
	}
}

// testCA is an authority made for a test, and a certificate it issued.
type testCA struct {
	pool *x509.CertPool
	leaf tls.Certificate
}

func newTestCA(t *testing.T, hosts ...string) testCA {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test authority"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "test listener"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			leafTmpl.IPAddresses = append(leafTmpl.IPAddresses, ip)
		} else {
			leafTmpl.DNSNames = append(leafTmpl.DNSNames, h)
		}
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(leafDER)
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	return testCA{pool: pool, leaf: tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey, Leaf: leaf}}
}

func TestTierOwnCert(t *testing.T) {
	ca := newTestCA(t, "127.0.0.1")
	a, b := startNode(t, "alpha", &ca.leaf), startNode(t, "beta", nil)
	_, onB := pairNodes(t, a, b)
	ownFP := node.Fingerprint(ca.leaf.Leaf)
	if ownFP == a.n.Fingerprint() {
		t.Fatal("the test certificate has the identity key")
	}
	path := func(verify, pin string) node.Path {
		return node.Path{Name: "p", Type: "tcp", Addr: a.addr, Security: node.SecurityOwnCert, Verify: verify, Pin: pin,
			Roles: []string{"data"}, Subs: 8}
	}

	// verify pin: the fingerprint of the operator's certificate key
	if err := check(b, onB, path(node.VerifyPin, ownFP)); err != nil {
		t.Fatalf("pin, the right fingerprint: %v", err)
	}
	if err := check(b, onB, path("", ownFP)); err != nil {
		t.Fatalf("pin is the default way to verify: %v", err)
	}
	// the identity fingerprint is not the certificate's: in this tier it is refused
	if err := check(b, onB, path(node.VerifyPin, a.n.Fingerprint())); err == nil || !strings.Contains(err.Error(), "another fingerprint") {
		t.Fatalf("pin, a wrong fingerprint: %v", err)
	}

	// verify ca: by authority and host name
	b.svc.Dialer.RootCAs = ca.pool
	if err := check(b, onB, path(node.VerifyCA, "")); err != nil {
		t.Fatalf("ca, the right authority: %v", err)
	}
	other := newTestCA(t, "127.0.0.1")
	b.svc.Dialer.RootCAs = other.pool
	if err := check(b, onB, path(node.VerifyCA, "")); err == nil {
		t.Fatal("ca: a certificate of an unknown authority was accepted")
	}
	// the right authority, but the certificate is for another host name
	wrongHost := newTestCA(t, "nas.example.org")
	c := startNode(t, "gamma", &wrongHost.leaf)
	_, onB2 := pairNodes(t, c, b)
	b.svc.Dialer.RootCAs = wrongHost.pool
	p := path(node.VerifyCA, "")
	p.Addr = c.addr
	if err := check(b, onB2, p); err == nil {
		t.Fatal("ca: a certificate for another host name was accepted")
	}

	// The listening end still wants to know the dialer: a node that is not a peer is
	// refused in this tier too.
	x := startNode(t, "xi", nil)
	if err := check(x, onB, path(node.VerifyPin, ownFP)); err == nil {
		t.Fatal("alpha accepted a node it is not paired with in the owncert tier")
	}

	// The tls tier keeps working at the same listener: it is shown the identity.
	if err := check(b, onB, onB.Paths[0]); err != nil {
		t.Fatalf("tls tier at a listener that has an operator's certificate: %v", err)
	}
	// A listener without an operator's certificate refuses the owncert tier.
	d := startNode(t, "delta", nil)
	_, onB3 := pairNodes(t, d, b)
	p = path(node.VerifyPin, ownFP)
	p.Addr = d.addr
	if err := check(b, onB3, p); err == nil {
		t.Fatal("owncert at a listener that has no operator's certificate succeeded")
	}
}

func TestTierOff(t *testing.T) {
	a, b := startNode(t, "alpha", nil), startNode(t, "beta", nil)
	onA, onB := pairNodes(t, a, b)
	off := func(addr string) node.Path {
		return node.Path{Name: "p", Type: "tcp", Addr: addr, Security: node.SecurityOff, Roles: []string{"data"}, Subs: 8}
	}
	if err := check(b, onB, off(a.plain)); err != nil {
		t.Fatalf("beta to alpha's plain listener: %v", err)
	}
	if err := check(a, onA, off(b.plain)); err != nil {
		t.Fatalf("alpha to beta's plain listener: %v", err)
	}
	// plain against the TLS listener, and TLS against the plain listener, both fail
	if err := check(b, onB, off(a.addr)); err == nil {
		t.Fatal("a plain connection to the TLS listener succeeded")
	}
	if err := check(b, onB, tlsPath(a.plain)); err == nil {
		t.Fatal("a TLS connection to the plain listener succeeded")
	}
	// nothing listens
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	if err := check(b, onB, off(dead)); err == nil {
		t.Fatal("a check of a closed port succeeded")
	}
}

func TestDialRefusesBadPath(t *testing.T) {
	a := startNode(t, "alpha", nil)
	peer := node.Peer{Name: "x", Fingerprint: strings.Repeat("a", 64), PairSecret: make([]byte, node.PairSecretLen)}
	for name, p := range map[string]node.Path{
		"unknown tier":        {Type: "tcp", Addr: a.addr, Security: "rot13"},
		"unknown type":        {Type: "carrier pigeon", Addr: a.addr, Security: node.SecurityOff},
		"pin without a pin":   {Type: "tcp", Addr: a.addr, Security: node.SecurityOwnCert},
		"socks5 no proxy":     {Type: "socks5", Addr: a.addr, Security: node.SecurityOff},
		"ca without a port":   {Type: "tcp", Addr: "nas.example.org", Security: node.SecurityOwnCert, Verify: node.VerifyCA},
		"no address at all":   {Type: "tcp", Security: node.SecurityOff},
		"pin on the tls tier": {Type: "tcp", Addr: a.addr, Security: node.SecurityTLS, Pin: strings.Repeat("a", 64)},
	} {
		if _, err := a.svc.Dialer.Dial(bg, peer, p); err == nil {
			t.Errorf("%s: Dial went ahead", name)
		}
	}
}
