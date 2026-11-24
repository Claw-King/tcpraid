package api

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Removing a peer with the query "rules": the answer lists the rules that name it, and
// "rules=remove" takes them away with it. Without the query everything is as before.
func TestRemovePeerAndItsRules(t *testing.T) {
	a, b := newPeerNode(t, "alpha"), newPeerNode(t, "beta")
	rule := func(name string) {
		t.Helper()
		spec := fmt.Sprintf(`{"name":%q,"listen":%q,"destination":"192.0.2.1:873","peer":"alpha"}`, name, freePort(t))
		decode[Rule](t, b.send("POST", PathRules, spec), 201)
	}
	names := func() string {
		t.Helper()
		var out []string
		for _, r := range decode[[]Rule](t, b.send("GET", PathRules, ""), 200) {
			out = append(out, r.Name)
		}
		return strings.Join(out, ",")
	}

	// rules=keep: the peer goes, the rules stay and are named
	pairOver(t, a, b)
	rule("zfs")
	rule("ssh")
	wantError(t, "a value that is neither", b.send("DELETE", PathPeers+"/alpha?rules=maybe", ""), 400)
	wantError(t, "an unknown peer", b.send("DELETE", PathPeers+"/nobody?rules=remove", ""), 404)
	if got := names(); got != "zfs,ssh" {
		t.Fatalf("the rules after two requests that were refused: %s", got)
	}
	gone := decode[PeerRemoval](t, b.send("DELETE", PathPeers+"/alpha?rules=keep", ""), 200)
	if gone.Peer != "alpha" || !reflect.DeepEqual(gone.Rules, []string{"zfs", "ssh"}) || gone.RulesRemoved {
		t.Errorf("rules=keep answered %+v", gone)
	}
	if got := names(); got != "zfs,ssh" {
		t.Errorf("the rules after rules=keep: %s", got)
	}
	wantError(t, "the peer is gone", b.send("GET", PathPeers+"/alpha", ""), 404)

	// paired again under the same name, the rules are the new peer's; rules=remove
	// takes them with it and leaves a rule of another peer alone
	pairOver(t, a, b)
	c := newPeerNode(t, "gamma")
	pairOver(t, c, b)
	decode[Rule](t, b.send("POST", PathRules, fmt.Sprintf(`{"name":"other","listen":%q,"destination":"192.0.2.1:22","peer":"gamma"}`, freePort(t))), 201)
	gone = decode[PeerRemoval](t, b.send("DELETE", PathPeers+"/alpha?rules=remove", ""), 200)
	if !reflect.DeepEqual(gone.Rules, []string{"zfs", "ssh"}) || !gone.RulesRemoved {
		t.Errorf("rules=remove answered %+v", gone)
	}
	if got := names(); got != "other" {
		t.Errorf("the rules after rules=remove: %s", got)
	}

	if w := b.send("DELETE", PathPeers+"/gamma?rules=keep", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"rules":["other"]`) {
		t.Errorf("rules=keep for gamma: %d %s", w.Code, w.Body)
	}
	// a peer that no rule names: an empty list, not null
	pairOver(t, a, b)
	if w := b.send("DELETE", PathPeers+"/alpha?rules=keep", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"rules":[]`) {
		t.Errorf("a peer without rules: %d %s", w.Code, w.Body)
	}

	// without the query: 204 and no body, as in every version before
	pairOver(t, a, b)
	rule("again")
	if w := b.send("DELETE", PathPeers+"/alpha", ""); w.Code != 204 || w.Body.Len() != 0 {
		t.Errorf("DELETE without the query: %d %q", w.Code, w.Body)
	}
	if got := names(); got != "other,again" {
		t.Errorf("the rules after a DELETE without the query: %s", got)
	}
}
