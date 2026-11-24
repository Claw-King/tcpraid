package api

import (
	"fmt"
	"strings"
	"testing"
)

// The web page edits a peer and a rule with requests that hold only what its forms
// have. What it has no field for stays as it is: a peer's transport settings, a path's
// further addresses when the page sends them back as it got them, and a rule's paths,
// number of sub-connections, dial and transport settings.
func TestRequestsOfThePageKeepWhatItHasNoFieldFor(t *testing.T) {
	a, b := newPeerNode(t, "alpha"), newPeerNode(t, "beta")
	pairOver(t, a, b)
	p := decode[Peer](t, b.send("GET", PathPeers+"/alpha", ""), 200)
	if len(p.Paths) != 1 {
		t.Fatalf("paths after pairing: %+v", p.Paths)
	}
	first := p.Paths[0].Addr
	set := fmt.Sprintf(`{"addr":%q,"addrs":["192.0.2.8:9851","192.0.2.9:9851"]}`, first)
	decode[PathSet](t, b.send("PUT", PathPeers+"/alpha/paths/"+p.Paths[0].Name, set), 200)
	decode[Peer](t, b.send("PUT", PathPeers+"/alpha", `{"transport":{"window":"32MB"}}`), 200)

	// the form for a path: the whole list of paths, each with the further addresses it
	// came with, and nothing else of the peer
	body := fmt.Sprintf(`{"paths":[{"name":%q,"type":"tcp","addr":%q,"proxy":"","security":"tls","verify":"","pin":"","roles":["data"],"subs":4,"addrs":["192.0.2.8:9851","192.0.2.9:9851"]}]}`,
		p.Paths[0].Name, first)
	p = decode[Peer](t, b.send("PUT", PathPeers+"/alpha", body), 200)
	if p.Paths[0].Subs != 4 || strings.Join(p.Paths[0].Addrs, " ") != "192.0.2.8:9851 192.0.2.9:9851" || p.Transport["window"] != "32MB" {
		t.Errorf("after the page saved a path: %+v", p)
	}
	// the form for the peer's other settings
	p = decode[Peer](t, b.send("PUT", PathPeers+"/alpha", `{"allow":"192.0.2.0/24:873","udp_dial":""}`), 200)
	if p.Allow != "192.0.2.0/24:873" || len(p.Paths) != 1 || len(p.Paths[0].Addrs) != 2 || p.Transport["window"] != "32MB" {
		t.Errorf("after the page saved the peer's settings: %+v", p)
	}

	in := freePort(t)
	spec := fmt.Sprintf(`{"name":"zfs","listen":%q,"destination":"192.0.2.1:873","peer":"alpha"`, in)
	made := decode[Rule](t, b.send("POST", PathRules, spec+fmt.Sprintf(`,"paths":[%q],"subs":2,"dial":"self","transport":{"window":"16MB"}}`, p.Paths[0].Name)), 201)
	if len(made.Paths) != 1 || made.Subs != 2 || made.Dial != "self" || made.Transport["window"] != "16MB" {
		t.Fatalf("the rule as made: %+v", made)
	}
	// the form for a rule, and the switch in the list: name, peer, the addresses, on or off
	for _, enabled := range []string{"false", "true"} {
		r := decode[Rule](t, b.send("PUT", PathRules+"/zfs", spec+`,"enabled":`+enabled+`}`), 200)
		if len(r.Paths) != 1 || r.Subs != 2 || r.Dial != "self" || r.Transport["window"] != "16MB" || fmt.Sprint(r.Enabled) != enabled {
			t.Errorf("after the page saved the rule with enabled %s: %+v", enabled, r)
		}
	}
}
