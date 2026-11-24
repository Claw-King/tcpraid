package api

import (
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/node"
)

// A path gets further addresses and a mark through its route, shows them, and a check
// of the peer answers for every address.
func TestPathAddressesAndMarkRoutes(t *testing.T) {
	a, b := newPeerNode(t, "alpha"), newPeerNode(t, "beta")
	pairOver(t, a, b)
	path := PathPeers + "/alpha/paths/default"
	// an address that refuses: a port that was just closed
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()

	set := decode[PathSet](t, b.send("PUT", path, fmt.Sprintf(`{"addrs":[%q]}`, dead)), 200)
	if p := set.Peer.Paths[0]; p.Addr != a.addr || !slices.Equal(p.Addrs, []string{dead}) || p.Mark != 0 {
		t.Fatalf("after a further address: %+v", p)
	}
	if raw := b.send("GET", PathPeers+"/alpha", "").Body.String(); !strings.Contains(raw, `"addrs":[`) || strings.Contains(raw, `"mark"`) {
		t.Errorf("the peer as shown: %s", raw)
	}
	chk := decode[PeerCheck](t, b.send("POST", PathPeers+"/alpha/check", ""), 200)
	if len(chk.Paths) != 2 || chk.Paths[0].Name != "default" || chk.Paths[0].Addr != a.addr || !chk.Paths[0].OK ||
		chk.Paths[1].Name != "default" || chk.Paths[1].Addr != dead || chk.Paths[1].OK || chk.Paths[1].Error == "" {
		t.Errorf("the check of a path with two addresses: %+v", chk.Paths)
	}
	wantError(t, "the address again", b.send("PUT", path, fmt.Sprintf(`{"addrs":[%q]}`, a.addr)), 400)
	wantError(t, "an address without a port", b.send("PUT", path, `{"addrs":["192.0.2.1"]}`), 400)
	wantError(t, "a negative mark", b.send("PUT", path, `{"mark":-1}`), 400)
	// the address alone takes the further ones away; with them it does not
	set = decode[PathSet](t, b.send("PUT", path, fmt.Sprintf(`{"addr":%q}`, a.addr)), 200)
	if p := set.Peer.Paths[0]; p.Addr != a.addr || len(p.Addrs) != 0 {
		t.Fatalf("after the address alone: %+v", p)
	}
	if raw := b.send("GET", PathPeers+"/alpha", "").Body.String(); strings.Contains(raw, `"addrs"`) {
		t.Errorf("a path with one address shows addrs: %s", raw)
	}
	set = decode[PathSet](t, b.send("PUT", path, fmt.Sprintf(`{"addr":%q,"addrs":[%q]}`, dead, a.addr)), 200)
	if p := set.Peer.Paths[0]; p.Addr != dead || !slices.Equal(p.Addrs, []string{a.addr}) {
		t.Fatalf("after both: %+v", p)
	}
	set = decode[PathSet](t, b.send("PUT", path, `{"addrs":[]}`), 200)
	if p := set.Peer.Paths[0]; p.Addr != dead || len(p.Addrs) != 0 {
		t.Fatalf("after an empty list: %+v", p)
	}
	decode[PathSet](t, b.send("PUT", path, fmt.Sprintf(`{"addr":%q}`, a.addr)), 200)

	// the mark: stored where the system has one, refused where it has none
	w := b.send("PUT", path, `{"mark":7}`)
	if !engine.MarkSupported {
		wantError(t, "a mark on a system without one", w, 400)
		return
	}
	if p := decode[PathSet](t, w, 200).Peer.Paths[0]; p.Mark != 7 {
		t.Fatalf("after mark=7: %+v", p)
	}
	if stored, _ := b.n.Peer("alpha"); stored.Paths[0].Mark != 7 {
		t.Fatalf("stored: %+v", stored.Paths[0])
	}
	// the whole list keeps what it names and drops what it does not
	whole := fmt.Sprintf(`{"paths":[{"name":"default","type":"tcp","addr":%q,"addrs":[%q],"security":"tls","roles":["data"],"subs":8,"mark":9}]}`, a.addr, dead)
	if p := decode[Peer](t, b.send("PUT", PathPeers+"/alpha", whole), 200).Paths[0]; p.Mark != 9 || !slices.Equal(p.Addrs, []string{dead}) {
		t.Fatalf("after the whole list: %+v", p)
	}
	if p := decode[PathSet](t, b.send("PUT", path, `{"mark":0}`), 200).Peer.Paths[0]; p.Mark != 0 {
		t.Fatalf("after mark=0: %+v", p)
	}
}

// A rule names paths and a number of sub-connections; a request that does not say
// anything about them, as a caller that does not know them sends it, leaves them.
func TestRulePathsRoutes(t *testing.T) {
	a, b := newPeerNode(t, "alpha"), newPeerNode(t, "beta")
	pairOver(t, a, b)
	in := freePort(t)
	spec := fmt.Sprintf(`{"name":"zfs","listen":%q,"destination":"192.0.2.1:873","peer":"alpha","paths":["default"],"subs":3}`, in)
	made := decode[Rule](t, b.send("POST", PathRules, spec), 201)
	if !slices.Equal(made.Paths, []string{"default"}) || made.Subs != 3 || made.PeerDials {
		t.Fatalf("made %+v", made)
	}
	base := fmt.Sprintf(`"name":"zfs","listen":%q,"destination":"192.0.2.1:873","peer":"alpha"`, in)
	wantError(t, "a path the node does not have", b.send("PUT", PathRules+"/zfs", `{`+base+`,"paths":["nope"]}`), 400)
	wantError(t, "a path twice", b.send("PUT", PathRules+"/zfs", `{`+base+`,"paths":["default","default"]}`), 400)
	wantError(t, "too many sub-connections", b.send("PUT", PathRules+"/zfs", `{`+base+`,"subs":65}`), 400)
	wantError(t, "a negative number", b.send("PUT", PathRules+"/zfs", `{`+base+`,"subs":-1}`), 400)
	stored := func() node.Rule {
		t.Helper()
		rules := b.n.Rules()
		if len(rules) != 1 {
			t.Fatalf("%d rules", len(rules))
		}
		return rules[0]
	}
	if r := stored(); !slices.Equal(r.Paths, []string{"default"}) || r.Subs != 3 {
		t.Fatalf("the rule after requests that were refused: %+v", r)
	}
	// switched off by a request that knows nothing of paths: they stay
	got := decode[Rule](t, b.send("PUT", PathRules+"/zfs", `{`+base+`,"enabled":false}`), 200)
	if got.Enabled || !slices.Equal(got.Paths, []string{"default"}) || got.Subs != 3 {
		t.Fatalf("after a request without paths and subs: %+v", got)
	}
	// one of the two named: the other stays
	got = decode[Rule](t, b.send("PUT", PathRules+"/zfs", `{`+base+`,"subs":5}`), 200)
	if !slices.Equal(got.Paths, []string{"default"}) || got.Subs != 5 {
		t.Fatalf("after subs alone: %+v", got)
	}
	// taken away by naming them empty
	got = decode[Rule](t, b.send("PUT", PathRules+"/zfs", `{`+base+`,"paths":[],"subs":0}`), 200)
	if len(got.Paths) != 0 || got.Subs != 0 {
		t.Fatalf("after an empty list and 0: %+v", got)
	}
	if raw := b.send("GET", PathRules, "").Body.String(); strings.Contains(raw, `"paths"`) || strings.Contains(raw, `"subs"`) || strings.Contains(raw, `"peer_dials"`) {
		t.Errorf("a rule that uses everything shows the new fields: %s", raw)
	}
	if r := stored(); r.Narrowed() {
		t.Fatalf("stored: %+v", r)
	}
	// on the node without a path the names are the peer's: stored without a check
	far := fmt.Sprintf(`{"name":"far","listen":%q,"destination":"192.0.2.1:873","peer":"beta","paths":["whatever"],"subs":1}`, freePort(t))
	if r := decode[Rule](t, a.send("POST", PathRules, far), 201); !slices.Equal(r.Paths, []string{"whatever"}) || r.Subs != 1 {
		t.Fatalf("made on the node without a path: %+v", r)
	}
}

// The dial of a rule over the API: named when the rule is made, kept by a request that
// does not name it, changed by one that does, and absent from a rule that has none.
func TestRuleDialRoutes(t *testing.T) {
	a, b := newPeerNode(t, "alpha"), newPeerNode(t, "beta")
	pairOver(t, a, b)
	in := freePort(t)
	base := fmt.Sprintf(`"name":"zfs","listen":%q,"destination":"192.0.2.1:873","peer":"alpha"`, in)
	made := decode[Rule](t, b.send("POST", PathRules, `{`+base+`,"dial":"both"}`), 201)
	if made.Dial != "both" {
		t.Fatalf("made %+v", made)
	}
	wantError(t, "a word that is none of the four", b.send("PUT", PathRules+"/zfs", `{`+base+`,"dial":"neither"}`), 400)
	if got := decode[Rule](t, b.send("PUT", PathRules+"/zfs", `{`+base+`,"enabled":false}`), 200); got.Dial != "both" || got.Enabled {
		t.Fatalf("after a request that does not name dial: %+v", got)
	}
	if got := decode[Rule](t, b.send("PUT", PathRules+"/zfs", `{`+base+`,"dial":"peer"}`), 200); got.Dial != "peer" {
		t.Fatalf("after dial peer: %+v", got)
	}
	if r := b.n.Rules()[0]; r.Dial != "peer" {
		t.Fatalf("stored: %+v", r)
	}
	if got := decode[Rule](t, b.send("PUT", PathRules+"/zfs", `{`+base+`,"dial":"auto"}`), 200); got.Dial != "" {
		t.Fatalf("after dial auto: %+v", got)
	}
	if raw := b.send("GET", PathRules, "").Body.String(); strings.Contains(raw, `"dial"`) {
		t.Errorf("a rule with dial auto shows the field: %s", raw)
	}
	if r := b.n.Rules()[0]; r.Dial != "" {
		t.Fatalf("stored after auto: %+v", r)
	}
}
