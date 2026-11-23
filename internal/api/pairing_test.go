package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/forward"
	"github.com/Claw-King/tcpraid/internal/link"
	"github.com/Claw-King/tcpraid/internal/node"
)

// peerNode is an API server whose node also has its two listeners up on loopback.
type peerNode struct {
	*Server
	n     *node.Node
	addr  string // TLS listener
	plain string // plain listener
}

func newPeerNode(t *testing.T, name string) *peerNode {
	t.Helper()
	n, err := node.Open(t.TempDir(), name)
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(engine.DefaultConfig())
	dialer := &link.Dialer{Identity: n.Identity}
	rules := &forward.Runtime{Node: n, Engine: e, Dialer: dialer, Version: "1.2.3-test"}
	svc := &link.Service{Node: n, Dialer: dialer, PeerRemoved: rules.PeerRemoved}
	srv := &link.Server{
		Identity:          n.Identity,
		PeerByFingerprint: n.PeerByFingerprint,
		HandlePair:        svc.HandlePair,
	}
	acc := &link.Acceptor{PeerByFingerprint: n.PeerByFingerprint, HandleSub: rules.HandleSub, HandleReverse: rules.HandleReverse, HandleControl: rules.HandleControl}
	srv.HandlePeer, srv.HandlePlain = acc.HandlePeer, acc.HandlePlain
	ctx, cancel := context.WithCancel(context.Background())
	rules.Start(ctx)
	var wg sync.WaitGroup
	listen := func(serve func(context.Context, net.Listener) error) string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() { defer wg.Done(); serve(ctx, ln) }()
		return ln.Addr().String()
	}
	pn := &peerNode{n: n, addr: listen(srv.ServeTLS), plain: listen(srv.ServePlain)}
	t.Cleanup(func() { cancel(); wg.Wait(); e.Shutdown(); n.Close() })
	pn.Server = New(Config{Version: "1.2.3-test", Token: n.Token, Node: svc, Engine: e, Rules: rules})
	return pn
}

// send sends a request with a body and the node's token straight to the handler.
func (pn *peerNode) send(method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+pn.cfg.Token)
	w := httptest.NewRecorder()
	pn.ServeHTTP(w, r)
	return w
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder, code int) T {
	t.Helper()
	var v T
	if w.Code != code {
		t.Fatalf("status %d, want %d; body %s", w.Code, code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("body %q: %v", w.Body, err)
	}
	return v
}

func jsonBody(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestPairingCodeRoutes(t *testing.T) {
	a := newPeerNode(t, "alpha")

	// no address in the request and none given to the node
	wantError(t, "no address", a.send("POST", PathPairingCodes, ""), 400)
	wantError(t, "bad address", a.send("POST", PathPairingCodes, `{"addresses":["no port"]}`), 400)
	wantError(t, "negative ttl", a.send("POST", PathPairingCodes, `{"addresses":["nas.example.org:9851"],"ttl_seconds":-1}`), 400)
	wantError(t, "unknown field", a.send("POST", PathPairingCodes, `{"addresses":["nas.example.org:9851"],"extra":1}`), 400)
	wantError(t, "not JSON", a.send("POST", PathPairingCodes, `{`), 400)
	wantError(t, "two values", a.send("POST", PathPairingCodes, `{"addresses":["nas.example.org:9851"]} {}`), 400)
	wantError(t, "too large", a.send("POST", PathPairingCodes, `{"addresses":["`+strings.Repeat("a", MaxBody)+`"]}`), 413)
	if got := decode[[]PairingCode](t, a.send("GET", PathPairingCodes, ""), 200); len(got) != 0 {
		t.Fatalf("refused requests left codes: %+v", got)
	}
	if got := strings.TrimSpace(a.send("GET", PathPairingCodes, "").Body.String()); got != "[]" {
		t.Fatalf("no codes is %q, want []", got)
	}

	w := a.send("POST", PathPairingCodes, `{"addresses":["nas.example.org:9851"],"ttl_seconds":600}`)
	made := decode[PairingCode](t, w, 201)
	if !strings.HasPrefix(made.Code, "tcpraid-pair-1-") || len(made.ID) != 8 || made.Used || made.Expires.IsZero() {
		t.Fatalf("made %+v", made)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q on the answer that carries the code", cc)
	}
	// the list has the code's id and not the code
	lw := a.send("GET", PathPairingCodes, "")
	list := decode[[]PairingCode](t, lw, 200)
	if len(list) != 1 || list[0].ID != made.ID || list[0].Code != "" || !list[0].Expires.Equal(made.Expires) {
		t.Fatalf("list %+v", list)
	}
	if strings.Contains(lw.Body.String(), "code\"") || strings.Contains(lw.Body.String(), "secret") {
		t.Fatalf("the list shows more than id, expiry and use: %s", lw.Body)
	}

	if w := a.send("DELETE", PathPairingCodes+"/"+made.ID, ""); w.Code != 204 || w.Body.Len() != 0 {
		t.Fatalf("revoke: status %d, body %q", w.Code, w.Body)
	}
	wantError(t, "revoke twice", a.send("DELETE", PathPairingCodes+"/"+made.ID, ""), 404)
	if got := decode[[]PairingCode](t, a.send("GET", PathPairingCodes, ""), 200); len(got) != 0 {
		t.Fatalf("after the revocation: %+v", got)
	}
}

func TestPeerRoutes(t *testing.T) {
	a, b := newPeerNode(t, "alpha"), newPeerNode(t, "beta")
	made := decode[PairingCode](t, a.send("POST", PathPairingCodes, jsonBody(PairingCodeRequest{Addresses: []string{a.addr}})), 201)

	wantError(t, "no code", b.send("POST", PathPeers, `{}`), 400)
	wantError(t, "empty body", b.send("POST", PathPeers, ``), 400)
	wantError(t, "not a code", b.send("POST", PathPeers, `{"code":"hello"}`), 400)
	wantError(t, "bad address", b.send("POST", PathPeers, jsonBody(JoinRequest{Code: made.Code, Address: "no port"})), 400)
	// nothing listens at this address: the pairing did not complete
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	wantError(t, "nobody there", b.send("POST", PathPeers, jsonBody(JoinRequest{Code: made.Code, Address: dead})), 502)

	w := b.send("POST", PathPeers, jsonBody(JoinRequest{Code: made.Code}))
	peer := decode[Peer](t, w, 201)
	if peer.Name != "alpha" || peer.Fingerprint != a.n.Fingerprint() || len(peer.Paths) != 1 || peer.Paths[0].Addr != a.addr ||
		peer.Paths[0].Security != "tls" || peer.PairedAt.IsZero() {
		t.Fatalf("joined %+v", peer)
	}
	// the code is used up
	wantError(t, "a used code", b.send("POST", PathPeers, jsonBody(JoinRequest{Code: made.Code})), 502)

	// both nodes list each other, and no answer holds a secret
	secretA, _ := a.n.Peer("beta")
	for _, pn := range []*peerNode{a, b} {
		lw := pn.send("GET", PathPeers, "")
		list := decode[[]Peer](t, lw, 200)
		if len(list) != 1 {
			t.Fatalf("%s lists %+v", pn.n.Name(), list)
		}
		for _, body := range []string{lw.Body.String(), w.Body.String()} {
			if strings.Contains(body, "secret") || strings.Contains(body, jsonBody(secretA.PairSecret)) {
				t.Fatalf("an answer holds the pair secret: %s", body)
			}
		}
		st := decode[Status](t, pn.send("GET", PathStatus, ""), 200)
		if st.Peers != 1 {
			t.Fatalf("%s: status counts %d peers", pn.n.Name(), st.Peers)
		}
	}
	if got := decode[[]Peer](t, a.send("GET", PathPeers, ""), 200)[0]; got.Name != "beta" || len(got.Paths) != 0 || got.Addresses == nil || got.Paths == nil {
		t.Fatalf("alpha's record of beta: %+v", got)
	}
	if got := decode[[]PairingCode](t, a.send("GET", PathPairingCodes, ""), 200); len(got) != 1 || !got[0].Used {
		t.Fatalf("the code after the pairing: %+v", got)
	}

	// check: beta has a path to alpha, alpha has none to beta
	chk := decode[PeerCheck](t, b.send("POST", PathPeers+"/alpha/check", ""), 200)
	if chk.Peer != "alpha" || len(chk.Paths) != 1 || !chk.Paths[0].OK || chk.Paths[0].Error != "" || chk.Paths[0].Security != "tls" {
		t.Fatalf("check %+v", chk)
	}
	if chk := decode[PeerCheck](t, a.send("POST", PathPeers+"/beta/check", ""), 200); chk.Paths == nil || len(chk.Paths) != 0 {
		t.Fatalf("check of a peer without paths: %+v", chk)
	}
	wantError(t, "check an unknown peer", b.send("POST", PathPeers+"/nobody/check", ""), 404)
	wantError(t, "check with a body it does not take", b.send("POST", PathPeers+"/alpha/check", `{"x":1}`), 400)

	// alpha ends the pairing: beta's check now fails, alpha no longer knows beta
	if w := a.send("DELETE", PathPeers+"/beta", ""); w.Code != 204 {
		t.Fatalf("remove: status %d, body %s", w.Code, w.Body)
	}
	wantError(t, "remove twice", a.send("DELETE", PathPeers+"/beta", ""), 404)
	wantError(t, "check a removed peer", a.send("POST", PathPeers+"/beta/check", ""), 404)
	chk = decode[PeerCheck](t, b.send("POST", PathPeers+"/alpha/check", ""), 200)
	if len(chk.Paths) != 1 || chk.Paths[0].OK || chk.Paths[0].Error == "" {
		t.Fatalf("check after the other node ended the pairing: %+v", chk)
	}
}

// Paths, methods and the token on the routes that take them.
func TestRouting(t *testing.T) {
	a := newPeerNode(t, "alpha")
	for _, c := range []struct {
		method, path string
		code         int
		allow        string
	}{
		{"PUT", PathPeers, 405, "POST, GET"},
		{"DELETE", PathPeers, 405, "POST, GET"},
		{"POST", PathPeers + "/beta", 405, "GET, PUT, DELETE"},
		{"GET", PathPeers + "/beta/check", 405, "POST"},
		{"DELETE", PathPairingCodes, 405, "POST, GET"},
		{"POST", PathPairingCodes + "/0a0b0c0d", 405, "DELETE"},
		{"GET", PathPeers + "/", 404, ""},
		{"GET", PathPeers + "/beta/check/more", 404, ""},
		{"DELETE", PathPeers + "//check", 404, ""},
		{"GET", "/api/v1", 404, ""},
		{"GET", "/api/v1/pairing-code", 404, ""},
	} {
		w := a.send(c.method, c.path, "")
		wantError(t, c.method+" "+c.path, w, c.code)
		if got := w.Header().Get("Allow"); got != c.allow {
			t.Errorf("%s %s: Allow %q, want %q", c.method, c.path, got, c.allow)
		}
	}
	// every new route wants the token, existing path or not
	for _, c := range [][2]string{{"POST", PathPairingCodes}, {"GET", PathPairingCodes}, {"DELETE", PathPairingCodes + "/00000000"},
		{"POST", PathPeers}, {"GET", PathPeers}, {"GET", PathPeers + "/x"}, {"PUT", PathPeers + "/x"}, {"DELETE", PathPeers + "/x"},
		{"POST", PathPeers + "/x/check"}} {
		for _, token := range []string{"", "wrong"} {
			wantError(t, c[0]+" "+c[1]+" without the token", do(a.Server, c[0], c[1], token), 401)
		}
	}

	// a peer name with a slash or a space in it is one path segment when escaped
	odd := "the office/2"
	if _, err := a.n.PutPeer(node.Peer{Name: odd, Fingerprint: strings.Repeat("c", 64), PairSecret: make([]byte, node.PairSecretLen)}); err != nil {
		t.Fatal(err)
	}
	chk := decode[PeerCheck](t, a.send("POST", PathPeers+"/"+url.PathEscape(odd)+"/check", ""), 200)
	if chk.Peer != odd {
		t.Fatalf("checked %q, want %q", chk.Peer, odd)
	}
	if w := a.send("DELETE", PathPeers+"/"+url.PathEscape(odd), ""); w.Code != 204 {
		t.Fatalf("removing %q: status %d", odd, w.Code)
	}
	// unescaped, the slash makes it another path
	wantError(t, "unescaped slash", a.send("DELETE", PathPeers+"/the%20office/2", ""), 404)
}

// A proxy's user and password are not shown.
func TestPeerProxyIsShownWithoutPassword(t *testing.T) {
	a := newPeerNode(t, "alpha")
	_, err := a.n.PutPeer(node.Peer{Name: "beta", Fingerprint: strings.Repeat("c", 64), PairSecret: make([]byte, node.PairSecretLen),
		Paths: []node.Path{{Name: "p", Type: "socks5", Addr: "192.0.2.9:9851", Proxy: "socks5://user:hunter2@192.0.2.1:1080",
			Security: node.SecurityOff, Roles: []string{"data"}, Subs: 4}}})
	if err != nil {
		t.Fatal(err)
	}
	w := a.send("GET", PathPeers, "")
	list := decode[[]Peer](t, w, 200)
	if got := list[0].Paths[0].Proxy; got != "socks5://192.0.2.1:1080" {
		t.Fatalf("proxy shown as %q", got)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("hunter2")) {
		t.Fatal("the proxy password is in the answer")
	}
}

var _ http.Handler = (*Server)(nil)

// The time to live is checked as a number of seconds: one too large for a duration is
// refused, not wrapped around into a short one.
func TestPairingCodeTTLRange(t *testing.T) {
	a := newPeerNode(t, "alpha")
	for _, ttl := range []string{"18446744074", "9223372036854775807", "2592001", "-5"} {
		wantError(t, "ttl "+ttl, a.send("POST", PathPairingCodes, `{"addresses":["nas.example.org:9851"],"ttl_seconds":`+ttl+`}`), 400)
	}
	if got := decode[[]PairingCode](t, a.send("GET", PathPairingCodes, ""), 200); len(got) != 0 {
		t.Fatalf("refused requests left codes: %+v", got)
	}
	before := time.Now()
	for ttl, want := range map[string]time.Duration{"1": time.Second, "2592000": 30 * 24 * time.Hour, "0": time.Hour} {
		made := decode[PairingCode](t, a.send("POST", PathPairingCodes, `{"addresses":["nas.example.org:9851"],"ttl_seconds":`+ttl+`}`), 201)
		if d := made.Expires.Sub(before); d < want-2*time.Second || d > want+5*time.Second {
			t.Errorf("ttl_seconds %s: valid for %v, want about %v", ttl, d, want)
		}
	}
}

// One peer is shown and its paths are replaced as a whole through the API; what cannot
// be stored is refused and changes nothing; a proxy keeps its password and never shows it.
func TestPeerPathRoutes(t *testing.T) {
	a, b := newPeerNode(t, "alpha"), newPeerNode(t, "beta")
	made := decode[PairingCode](t, a.send("POST", PathPairingCodes, jsonBody(PairingCodeRequest{Addresses: []string{a.addr}})), 201)
	decode[Peer](t, b.send("POST", PathPeers, jsonBody(JoinRequest{Code: made.Code})), 201)

	one := decode[Peer](t, b.send("GET", PathPeers+"/alpha", ""), 200)
	if one.Name != "alpha" || len(one.Paths) != 1 || one.Paths[0].Security != node.SecurityTLS {
		t.Fatalf("the peer: %+v", one)
	}
	if w := b.send("GET", PathPeers+"/alpha", ""); strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("the answer has a secret: %s", w.Body)
	}
	wantError(t, "unknown peer", b.send("GET", PathPeers+"/nobody", ""), 404)
	wantError(t, "unknown peer", b.send("PUT", PathPeers+"/nobody", `{"paths":[]}`), 404)
	wantError(t, "no paths field", b.send("PUT", PathPeers+"/alpha", `{}`), 400)
	wantError(t, "no body", b.send("PUT", PathPeers+"/alpha", ``), 400)
	wantError(t, "unknown field", b.send("PUT", PathPeers+"/alpha", `{"paths":[],"name":"x"}`), 400)

	// switch the path to the off tier, at the plain listener, and add a second one
	paths := []Path{
		{Name: "default", Type: "tcp", Addr: a.plain, Security: node.SecurityOff, Roles: []string{"data"}, Subs: 4},
		{Name: "side", Type: "socks5", Addr: "192.0.2.9:9851", Proxy: "socks5://user:hunter2@192.0.2.1:1080", Security: node.SecurityTLS,
			Roles: []string{"repair", "control"}, Subs: 2},
	}
	w := b.send("PUT", PathPeers+"/alpha", jsonBody(PeerUpdate{Paths: &paths}))
	got := decode[Peer](t, w, 200)
	if len(got.Paths) != 2 || got.Paths[0].Security != node.SecurityOff || got.Paths[0].Addr != a.plain || got.Paths[0].Subs != 4 ||
		got.Paths[1].Proxy != "socks5://192.0.2.1:1080" {
		t.Fatalf("after the change: %+v", got.Paths)
	}
	if strings.Contains(w.Body.String(), "hunter2") {
		t.Fatal("the proxy password is in the answer")
	}
	// the off path works: the check goes to the plain listener
	chk := decode[PeerCheck](t, b.send("POST", PathPeers+"/alpha/check", ""), 200)
	if len(chk.Paths) != 2 || !chk.Paths[0].OK || chk.Paths[0].Security != node.SecurityOff {
		t.Fatalf("check: %+v", chk)
	}

	// sending back what was shown (the proxy without its user) keeps the password
	shown := decode[Peer](t, b.send("GET", PathPeers+"/alpha", ""), 200).Paths
	shown[1].Subs = 3
	decode[Peer](t, b.send("PUT", PathPeers+"/alpha", jsonBody(PeerUpdate{Paths: &shown})), 200)
	stored, _ := b.n.Peer("alpha")
	if stored.Paths[1].Proxy != "socks5://user:hunter2@192.0.2.1:1080" || stored.Paths[1].Subs != 3 {
		t.Fatalf("stored after sending back what was shown: %+v", stored.Paths[1])
	}
	// another proxy host does not inherit it
	shown[1].Proxy = "socks5://192.0.2.2:1080"
	decode[Peer](t, b.send("PUT", PathPeers+"/alpha", jsonBody(PeerUpdate{Paths: &shown})), 200)
	if stored, _ = b.n.Peer("alpha"); stored.Paths[1].Proxy != "socks5://192.0.2.2:1080" {
		t.Fatalf("a new proxy took over the old password: %q", stored.Paths[1].Proxy)
	}

	// refused as a whole, with nothing changed and no proxy in the error
	before, _ := b.n.Peer("alpha")
	for name, body := range map[string]string{
		"bad proxy":       `{"paths":[{"name":"p","type":"socks5","addr":"192.0.2.9:9851","proxy":"socks://user:hunter2@192.0.2.1:1080","security":"off","roles":["data"],"subs":4}]}`,
		"no port":         `{"paths":[{"name":"p","type":"tcp","addr":"nas.example.org","security":"off","roles":["data"],"subs":4}]}`,
		"two of one name": `{"paths":[{"name":"p","type":"tcp","addr":"192.0.2.9:1","security":"off","roles":["data"],"subs":4},{"name":"p","type":"tcp","addr":"192.0.2.9:2","security":"off","roles":["data"],"subs":4}]}`,
		"no subs":         `{"paths":[{"name":"p","type":"tcp","addr":"192.0.2.9:1","security":"off","roles":["data"]}]}`,
		"unknown role":    `{"paths":[{"name":"p","type":"tcp","addr":"192.0.2.9:1","security":"off","roles":["bulk"],"subs":4}]}`,
		"unknown tier":    `{"paths":[{"name":"p","type":"tcp","addr":"192.0.2.9:1","security":"none","roles":["data"],"subs":4}]}`,
		"unknown field":   `{"paths":[{"name":"p","type":"tcp","addr":"192.0.2.9:1","security":"off","roles":["data"],"subs":4,"mark":7}]}`,
	} {
		w := b.send("PUT", PathPeers+"/alpha", body)
		wantError(t, name, w, 400)
		if strings.Contains(w.Body.String(), "hunter2") {
			t.Errorf("%s: the error repeats the proxy: %s", name, w.Body)
		}
	}
	if after, _ := b.n.Peer("alpha"); !reflect.DeepEqual(after, before) {
		t.Fatalf("refused requests changed the peer:\n%+v\n%+v", after, before)
	}

	// an empty list leaves the peer without a path
	if got := decode[Peer](t, b.send("PUT", PathPeers+"/alpha", `{"paths":[]}`), 200); len(got.Paths) != 0 {
		t.Fatalf("after removing every path: %+v", got.Paths)
	}
}
