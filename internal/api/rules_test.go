package api

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// pairOver pairs b with a through the API: b joins a and gets a path to it.
func pairOver(t *testing.T, a, b *peerNode) {
	t.Helper()
	made := decode[PairingCode](t, a.send("POST", PathPairingCodes, jsonBody(PairingCodeRequest{Addresses: []string{a.addr}})), 201)
	decode[Peer](t, b.send("POST", PathPeers, jsonBody(JoinRequest{Code: made.Code})), 201)
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func TestRuleRoutes(t *testing.T) {
	a, b := newPeerNode(t, "alpha"), newPeerNode(t, "beta")
	pairOver(t, a, b)
	if got := strings.TrimSpace(b.send("GET", PathRules, "").Body.String()); got != "[]" {
		t.Fatalf("no rules is %q, want []", got)
	}

	// a destination that takes what arrives and says what it got
	dst, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	arrived := make(chan [sha256.Size]byte, 1)
	hold := make(chan struct{})
	go func() {
		c, err := dst.Accept()
		if err != nil {
			return
		}
		h := sha256.New()
		io.Copy(h, c)
		<-hold
		c.Close()
		var sum [sha256.Size]byte
		copy(sum[:], h.Sum(nil))
		arrived <- sum
	}()

	in := freePort(t)
	spec := fmt.Sprintf(`{"name":"zfs","listen":%q,"destination":%q,"peer":"alpha"}`, in, dst.Addr().String())
	made := decode[Rule](t, b.send("POST", PathRules, spec), 201)
	if made.Name != "zfs" || !made.Enabled || !made.Listening || made.Error != "" || made.Sessions != 0 || made.Listen != in {
		t.Fatalf("made %+v", made)
	}
	wantError(t, "same name", b.send("POST", PathRules, spec), 400)
	wantError(t, "unknown peer", b.send("POST", PathRules, `{"name":"x","listen":"127.0.0.1:1","destination":"192.0.2.1:1","peer":"nobody"}`), 400)
	wantError(t, "what is neither an address nor a host name", b.send("POST", PathRules, `{"name":"x","listen":"127.0.0.1:1","destination":"nas..example.net:1","peer":"alpha"}`), 400)
	wantError(t, "range lengths", b.send("POST", PathRules, `{"name":"x","listen":"127.0.0.1:1-3","destination":"192.0.2.1:1-2","peer":"alpha"}`), 400)
	wantError(t, "taken port", b.send("POST", PathRules, fmt.Sprintf(`{"name":"x","listen":%q,"destination":"192.0.2.1:1","peer":"alpha"}`, in)), 400)
	wantError(t, "unknown field", b.send("POST", PathRules, `{"name":"x","listen":"127.0.0.1:1","destination":"192.0.2.1:1","peer":"alpha","sessions":1}`), 400)
	wantError(t, "no such rule", b.send("PUT", PathRules+"/nope", spec), 404)
	wantError(t, "no such rule", b.send("DELETE", PathRules+"/nope", ""), 404)
	if got := decode[Status](t, b.send("GET", PathStatus, ""), 200); got.Rules != 1 || got.Peers != 1 {
		t.Fatalf("status: %d rules, %d peers", got.Rules, got.Peers)
	}

	// a transfer through the rule shows in the sessions, with the rule and the peer
	c, err := net.Dial("tcp", in)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	data := make([]byte, 2<<20)
	rand.Read(data)
	if _, err := c.Write(data); err != nil {
		t.Fatal(err)
	}
	c.(*net.TCPConn).CloseWrite()
	// the sending node can have written everything before the other node has even
	// taken the first sub-connection: wait for both
	var sess, far []Session
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		sess = decode[[]Session](t, b.send("GET", PathSessions, ""), 200)
		far = decode[[]Session](t, a.send("GET", PathSessions, ""), 200)
		if len(sess) == 1 && sess[0].TxBytes == int64(len(data)) && len(far) == 1 && far[0].RxBytes == int64(len(data)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sessions on beta: %+v; on alpha: %+v", sess, far)
		}
	}
	if sess[0].Rule != "zfs" || sess[0].Peer != "alpha" || sess[0].Role != "client" || sess[0].Destination != dst.Addr().String() {
		t.Fatalf("the session on beta: %+v", sess[0])
	}
	if far[0].Peer != "beta" || far[0].Rule != "" || far[0].Role != "server" {
		t.Fatalf("the session on alpha: %+v", far)
	}
	list := decode[[]Rule](t, b.send("GET", PathRules, ""), 200)
	if len(list) != 1 || list[0].Sessions != 1 {
		t.Fatalf("rules during the transfer: %+v", list)
	}

	// the rule is changed while its session runs: the session is not touched
	in2 := freePort(t)
	off := fmt.Sprintf(`{"listen":%q,"destination":%q,"peer":"alpha","enabled":false}`, in2, dst.Addr().String())
	changed := decode[Rule](t, b.send("PUT", PathRules+"/zfs", off), 200)
	if changed.Name != "zfs" || changed.Enabled || changed.Listening || changed.Listen != in2 {
		t.Fatalf("changed %+v", changed)
	}
	if _, err := net.DialTimeout("tcp", in, time.Second); err == nil {
		t.Fatal("the old listen address still takes connections")
	}
	close(hold)
	select {
	case sum := <-arrived:
		if sum != sha256.Sum256(data) {
			t.Fatal("what arrived differs from what was sent")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the transfer did not finish")
	}

	// a transparent rule is stored; without a transparent listener it says so
	tr := decode[Rule](t, b.send("POST", PathRules, `{"name":"lan","match":"198.51.100.0/24:22","peer":"alpha"}`), 201)
	if tr.Listening || !strings.Contains(tr.Error, "transparent listener") || tr.Match != "198.51.100.0/24:22" {
		t.Fatalf("transparent rule: %+v", tr)
	}
	if w := b.send("DELETE", PathRules+"/zfs", ""); w.Code != 204 {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body)
	}
	if list := decode[[]Rule](t, b.send("GET", PathRules, ""), 200); len(list) != 1 || list[0].Name != "lan" {
		t.Fatalf("rules after the removal: %+v", list)
	}
}

// PUT on a peer changes what the request names and nothing else; the allow list and
// the UDP address are checked like the paths.
func TestPeerOptionRoutes(t *testing.T) {
	a, b := newPeerNode(t, "alpha"), newPeerNode(t, "beta")
	pairOver(t, a, b)
	before := decode[Peer](t, b.send("GET", PathPeers+"/alpha", ""), 200)
	if before.Allow != "" || before.UDPDial != "" || len(before.Paths) != 1 {
		t.Fatalf("a fresh peer: %+v", before)
	}
	got := decode[Peer](t, b.send("PUT", PathPeers+"/alpha", `{"allow":"192.0.2.0/24:873","udp_dial":"192.0.2.7:9852"}`), 200)
	if got.Allow != "192.0.2.0/24:873" || got.UDPDial != "192.0.2.7:9852" || len(got.Paths) != 1 {
		t.Fatalf("after setting the options: %+v", got)
	}
	got = decode[Peer](t, b.send("PUT", PathPeers+"/alpha", `{"allow":""}`), 200)
	if got.Allow != "" || got.UDPDial != "192.0.2.7:9852" {
		t.Fatalf("after clearing the allow list: %+v", got)
	}
	wantError(t, "nothing to change", b.send("PUT", PathPeers+"/alpha", `{}`), 400)
	wantError(t, "bad allow", b.send("PUT", PathPeers+"/alpha", `{"allow":"not a network"}`), 400)
	wantError(t, "bad udp", b.send("PUT", PathPeers+"/alpha", `{"udp_dial":"no port"}`), 400)
	// one bad part: nothing of the request is stored
	wantError(t, "good paths, bad allow", b.send("PUT", PathPeers+"/alpha", `{"paths":[],"allow":"not a network"}`), 400)
	if got := decode[Peer](t, b.send("GET", PathPeers+"/alpha", ""), 200); len(got.Paths) != 1 || got.UDPDial != "192.0.2.7:9852" {
		t.Fatalf("after refused requests: %+v", got)
	}
	if strings.Contains(b.send("GET", PathPeers+"/alpha", "").Body.String(), "secret") {
		t.Fatal("the peer's details mention a secret")
	}
}

// The peer answers say what the control connections say: online once the two nodes are
// connected, with the version the peer reported and the time of its last message.
func TestPeerOnlineFields(t *testing.T) {
	a, b := newPeerNode(t, "alpha"), newPeerNode(t, "beta")
	pairOver(t, a, b)
	// beta has the path: it opens the control connection within a few seconds
	var got Peer
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		got = decode[Peer](t, a.send("GET", PathPeers+"/beta", ""), 200)
		if got.Online && got.Version != "" && got.LastMessage != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("alpha does not see beta online: %+v", got)
		}
	}
	if got.Version != "1.2.3-test" || time.Since(*got.LastMessage) > time.Minute {
		t.Fatalf("peer answer: %+v", got)
	}
	list := decode[[]Peer](t, b.send("GET", PathPeers, ""), 200)
	if len(list) != 1 || !list[0].Online {
		t.Fatalf("beta's list: %+v", list)
	}
	// a peer that is not connected is listed as offline, with the field present
	c := newPeerNode(t, "gamma")
	body := c.send("GET", PathPeers, "").Body.String()
	pairOver(t, c, a)
	raw := a.send("GET", PathPeers+"/gamma", "").Body.String()
	if !strings.Contains(raw, `"online":`) {
		t.Fatalf("no online field: %s (list before pairing: %s)", raw, body)
	}
}
