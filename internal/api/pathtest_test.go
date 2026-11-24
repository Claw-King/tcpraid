package api

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/node"
)

// online waits until the node sees the peer on a control connection and has its
// hello: a path test asked for before that is refused.
func online(t *testing.T, pn *peerNode, peer string) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if p := decode[Peer](t, pn.send("GET", PathPeers+"/"+peer, ""), 200); p.Online && p.Version != "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is not connected", peer)
		}
	}
}

// noSecret fails the test if text holds the pair secret of the two nodes in a form a
// program would write it in, or the access token of either.
func noSecret(t *testing.T, what, text string, a, b *peerNode) {
	t.Helper()
	p, ok := b.n.Peer("alpha")
	if !ok {
		t.Fatal("beta has no record of alpha")
	}
	for _, s := range []string{hex.EncodeToString(p.PairSecret), base64.StdEncoding.EncodeToString(p.PairSecret),
		base64.RawURLEncoding.EncodeToString(p.PairSecret), a.cfg.Token, b.cfg.Token} {
		if strings.Contains(text, s) {
			t.Errorf("%s holds a secret: %s", what, text)
		}
	}
	if strings.Contains(text, "secret\"") || strings.Contains(text, "token\"") {
		t.Errorf("%s has a field for a secret: %s", what, text)
	}
}

// The path test through the API: the refusals, a test from the node with the path and
// one from the node without, and the fields of the answer.
func TestPeerTestRoute(t *testing.T) {
	a, b := newPeerNode(t, "alpha"), newPeerNode(t, "beta")
	pairOver(t, a, b)
	wantError(t, "unknown peer", b.send("POST", PathPeers+"/nobody/test", ""), 404)
	wantError(t, "too long", b.send("POST", PathPeers+"/alpha/test", `{"seconds":61}`), 400)
	wantError(t, "negative", b.send("POST", PathPeers+"/alpha/test", `{"seconds":-1}`), 400)
	wantError(t, "unknown field", b.send("POST", PathPeers+"/alpha/test", `{"minutes":1}`), 400)
	if w := b.send("GET", PathPeers+"/alpha/test", ""); w.Code != 405 {
		t.Errorf("GET: %d", w.Code)
	}
	online(t, b, "alpha")
	online(t, a, "beta")
	wantError(t, "unknown path", b.send("POST", PathPeers+"/alpha/test", `{"seconds":1,"path":"nope"}`), 404)

	w := b.send("POST", PathPeers+"/alpha/test", `{"seconds":1}`)
	res := decode[PeerTest](t, w, 200)
	if res.Peer != "alpha" || res.Seconds != 1 || res.PeerDials || len(res.Paths) != 1 {
		t.Fatalf("from the node with the path: %s", w.Body)
	}
	if p := res.Paths[0]; p.Name != "default" || p.Subs != 8 || p.Error != "" || p.One <= 0 || p.All <= 0 {
		t.Fatalf("the path: %+v", p)
	}
	var raw map[string]any
	json.Unmarshal(w.Body.Bytes(), &raw)
	if got, want := keys(raw), "paths peer peer_dials seconds"; got != want {
		t.Errorf("fields of the answer %q, want %q", got, want)
	}
	if got, want := keys(raw["paths"].([]any)[0].(map[string]any)), "all_bytes_per_second little_gain name one_bytes_per_second subs"; got != want {
		t.Errorf("fields of a path %q, want %q", got, want)
	}
	noSecret(t, "the answer of a test", w.Body.String(), a, b)

	// the other way round: alpha has no path, and the answer says whose paths these are
	w = a.send("POST", PathPeers+"/beta/test", `{"seconds":1,"path":"default"}`)
	back := decode[PeerTest](t, w, 200)
	if !back.PeerDials || len(back.Paths) != 1 || back.Paths[0].Error != "" || back.Paths[0].Subs != 8 || back.Paths[0].One <= 0 {
		t.Fatalf("from the node without a path: %s", w.Body)
	}
	noSecret(t, "the answer of a test", w.Body.String(), a, b)

	// a peer that is not connected: the test cannot start, which is the caller's to
	// try again and no error of this node
	if _, err := a.n.PutPeer(node.Peer{Name: "ghost", Fingerprint: strings.Repeat("c", 64), PairSecret: make([]byte, node.PairSecretLen)}); err != nil {
		t.Fatal(err)
	}
	if w := a.send("POST", PathPeers+"/ghost/test", ""); w.Code != 409 || !strings.Contains(w.Body.String(), "is not connected to this node") {
		t.Errorf("a test with a peer that is not connected: %d %s", w.Code, w.Body)
	}
}

// A rule says why its last connection failed and when, and the session says how it is
// doing; neither answer holds a secret.
func TestRuleErrorHasATime(t *testing.T) {
	a, b := newPeerNode(t, "alpha"), newPeerNode(t, "beta")
	pairOver(t, a, b)
	online(t, b, "alpha")
	in, nowhere := freePort(t), freePort(t)
	before := time.Now().Add(-time.Second)
	made := decode[Rule](t, b.send("POST", PathRules, fmt.Sprintf(`{"name":"r","listen":%q,"destination":%q,"peer":"alpha"}`, in, nowhere)), 201)
	if made.ErrorAt != nil {
		t.Fatalf("a new rule has a failure dated %v", made.ErrorAt)
	}
	c, err := net.Dial("tcp", in)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("x"))
	var r Rule
	var body string
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		w := b.send("GET", PathRules, "")
		body = w.Body.String()
		r = decode[[]Rule](t, w, 200)[0]
		if r.Error != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the rule does not say why its connection failed")
		}
	}
	want := fmt.Sprintf("peer \"alpha\" could not connect to %s: connect: connection refused", nowhere)
	if r.Error != want {
		t.Errorf("the rule says %q, want %q", r.Error, want)
	}
	if r.ErrorAt == nil || r.ErrorAt.Before(before) || r.ErrorAt.After(time.Now().Add(time.Second)) {
		t.Errorf("dated %v", r.ErrorAt)
	}
	if !r.Listening {
		t.Error("the rule stopped listening")
	}
	noSecret(t, "the rules", body, a, b)
	noSecret(t, "the sessions", b.send("GET", PathSessions, "").Body.String(), a, b)
}
