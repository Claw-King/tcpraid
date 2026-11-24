package api

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Claw-King/tcpraid/internal/node"
)

// One path of a peer is added, changed and removed through its own route: only what
// the request names changes, a proxy keeps its password and never shows it, and what
// cannot be stored is refused and changes nothing.
func TestOnePathRoutes(t *testing.T) {
	a, b := newPeerNode(t, "alpha"), newPeerNode(t, "beta")
	pairOver(t, a, b)
	path := func(name string) string { return PathPeers + "/alpha/paths/" + name }

	wantError(t, "unknown peer", b.send("PUT", PathPeers+"/nobody/paths/p", `{"subs":2}`), 404)
	wantError(t, "nothing to set", b.send("PUT", path("default"), `{}`), 400)
	wantError(t, "no body", b.send("PUT", path("default"), ``), 400)
	wantError(t, "unknown field", b.send("PUT", path("default"), `{"name":"x"}`), 400)
	wantError(t, "a new path without an address", b.send("PUT", path("side"), `{"subs":2}`), 400)
	wantError(t, "a number of sub-connections that cannot be", b.send("PUT", path("default"), `{"subs":65}`), 400)
	before, _ := b.n.Peer("alpha")
	if len(before.Paths) != 1 || before.Paths[0].Subs != 8 {
		t.Fatalf("the paths after requests that were refused: %+v", before.Paths)
	}

	// change one setting of the path that is there: the rest stays
	set := decode[PathSet](t, b.send("PUT", path("default"), `{"subs":3}`), 200)
	if set.Created || len(set.Peer.Paths) != 1 || set.Peer.Paths[0].Subs != 3 || set.Peer.Paths[0].Addr != a.addr || set.Peer.Paths[0].Security != node.SecurityTLS {
		t.Fatalf("after subs=3: %+v", set)
	}
	// a new path: what the request does not name is tcp, tls, data, 8
	w := b.send("PUT", path("side"), `{"addr":"192.0.2.9:9851"}`)
	set = decode[PathSet](t, w, 201)
	if !set.Created || len(set.Peer.Paths) != 2 {
		t.Fatalf("after adding side: %+v", set)
	}
	if p := set.Peer.Paths[1]; p.Name != "side" || p.Type != "tcp" || p.Security != node.SecurityTLS || p.Subs != 8 || len(p.Roles) != 1 || p.Roles[0] != node.RoleData {
		t.Fatalf("the new path: %+v", p)
	}
	// through a proxy with a password: stored, never shown
	w = b.send("PUT", path("side"), `{"type":"socks5","proxy":"socks5://user:hunter2@192.0.2.1:1080","roles":["repair","control"],"subs":2}`)
	set = decode[PathSet](t, w, 200)
	if strings.Contains(w.Body.String(), "hunter2") || set.Peer.Paths[1].Proxy != "socks5://192.0.2.1:1080" {
		t.Fatalf("the proxy as shown: %s", w.Body)
	}
	// a change that does not name the proxy keeps it, password and all; so does one
	// that names it as it was shown
	decode[PathSet](t, b.send("PUT", path("side"), `{"subs":4}`), 200)
	decode[PathSet](t, b.send("PUT", path("side"), `{"proxy":"socks5://192.0.2.1:1080"}`), 200)
	if stored, _ := b.n.Peer("alpha"); stored.Paths[1].Proxy != "socks5://user:hunter2@192.0.2.1:1080" || stored.Paths[1].Subs != 4 {
		t.Fatalf("stored after two more changes: %+v", stored.Paths[1])
	}
	// what goes with a setting leaves with it: back to tcp, the proxy is gone
	set = decode[PathSet](t, b.send("PUT", path("side"), `{"type":"tcp"}`), 200)
	if set.Peer.Paths[1].Proxy != "" {
		t.Fatalf("a tcp path kept its proxy: %+v", set.Peer.Paths[1])
	}
	pin := strings.Repeat("ab", 32)
	set = decode[PathSet](t, b.send("PUT", path("side"), fmt.Sprintf(`{"security":"owncert","pin":%q}`, pin)), 200)
	if set.Peer.Paths[1].Pin != pin {
		t.Fatalf("owncert with a pin: %+v", set.Peer.Paths[1])
	}
	set = decode[PathSet](t, b.send("PUT", path("side"), `{"security":"tls"}`), 200)
	if set.Peer.Paths[1].Pin != "" || set.Peer.Paths[1].Verify != "" {
		t.Fatalf("a tls path kept its pin: %+v", set.Peer.Paths[1])
	}
	// refused: a bad proxy is not repeated in the error, and nothing changes
	w = b.send("PUT", path("side"), `{"type":"socks5","proxy":"socks://user:hunter2@192.0.2.1:1080"}`)
	if w.Code != 400 || strings.Contains(w.Body.String(), "hunter2") {
		t.Fatalf("a bad proxy: %d %s", w.Code, w.Body)
	}
	if w := b.send("GET", PathPeers+"/alpha", ""); strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "hunter2") {
		t.Fatalf("the answer has a secret: %s", w.Body)
	}

	// remove: the path goes, the other stays; twice is 404
	left := decode[Peer](t, b.send("DELETE", path("side"), ""), 200)
	if len(left.Paths) != 1 || left.Paths[0].Name != "default" {
		t.Fatalf("after removing side: %+v", left.Paths)
	}
	wantError(t, "removing twice", b.send("DELETE", path("side"), ""), 404)
	wantError(t, "unknown peer", b.send("DELETE", PathPeers+"/nobody/paths/default", ""), 404)
	if left = decode[Peer](t, b.send("DELETE", path("default"), ""), 200); len(left.Paths) != 0 {
		t.Fatalf("after removing the last path: %+v", left.Paths)
	}
	wantError(t, "a method the route does not have", b.send("POST", path("default"), `{}`), 405)
}

// Changes to two paths of one peer, and to its allow list, that are made at the same
// time all take effect: none is lost to another that read the peer before it and wrote
// it after.
func TestChangesAtTheSameTimeAllCount(t *testing.T) {
	a, b := newPeerNode(t, "alpha"), newPeerNode(t, "beta")
	pairOver(t, a, b)
	decode[PathSet](t, b.send("PUT", PathPeers+"/alpha/paths/side", `{"addr":"192.0.2.9:9851"}`), 201)
	for round := 1; round <= 40; round++ {
		one, two := 1+round%60, 1+(round*7)%60
		allow := fmt.Sprintf("192.0.2.0/24:%d", 1000+round)
		var wg sync.WaitGroup
		for _, req := range [][2]string{
			{PathPeers + "/alpha/paths/default", fmt.Sprintf(`{"subs":%d}`, one)},
			{PathPeers + "/alpha/paths/side", fmt.Sprintf(`{"subs":%d}`, two)},
			{PathPeers + "/alpha", fmt.Sprintf(`{"allow":%q}`, allow)},
		} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if w := b.send("PUT", req[0], req[1]); w.Code != 200 {
					t.Errorf("round %d: PUT %s: %d %s", round, req[0], w.Code, w.Body)
				}
			}()
		}
		wg.Wait()
		got := decode[Peer](t, b.send("GET", PathPeers+"/alpha", ""), 200)
		if len(got.Paths) != 2 || got.Paths[0].Subs != one || got.Paths[1].Subs != two || got.Allow != allow {
			t.Fatalf("round %d: the peer is %+v, want subs %d and %d and allow %s", round, got, one, two, allow)
		}
	}
}
