package node

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// openNode opens a fresh node that stays open (and locked) until the test ends.
func openNode(t *testing.T, name string) *Node {
	t.Helper()
	n, err := Open(filepath.Join(t.TempDir(), "data"), name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	return n
}

// testPeer returns a peer record whose fingerprint and secret are made from seed.
func testPeer(name string, seed byte) Peer {
	return Peer{
		Name:        name,
		Fingerprint: strings.Repeat(fmt.Sprintf("%02x", seed), 32),
		PairSecret:  bytes.Repeat([]byte{seed}, PairSecretLen),
		Addresses:   []string{"192.0.2.10:9851"},
		PairedAt:    time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC),
		Paths: []Path{{Name: DefaultPathName, Type: "tcp", Addr: "192.0.2.10:9851", Security: SecurityTLS,
			Roles: []string{"data"}, Subs: 8}},
	}
}

// stateOnDisk reads the node's state file as LoadState sees it.
func stateOnDisk(t *testing.T, n *Node) *State {
	t.Helper()
	s, err := LoadState(filepath.Join(n.Dir, StateFile))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A state file of schema version 1 is brought to the current version when the node
// opens the directory, keeping the name.
func TestSchemaOneIsUpgraded(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	mustOpen(t, dir, "alpha")
	path := filepath.Join(dir, StateFile)
	v1 := `{"schema":1,"node":{"name":"alpha"},"peers":[],"rules":[]}`
	if err := os.WriteFile(path, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}
	// LoadState alone converts in memory and does not write
	s, err := LoadState(path)
	if err != nil || s.Schema != SchemaVersion || s.Node.Name != "alpha" || len(s.Rules) != 0 || s.PairingCodes == nil {
		t.Fatalf("LoadState of a version 1 file: %+v, %v", s, err)
	}
	if got := string(read(t, path)); got != v1 {
		t.Fatalf("LoadState changed the file to %s", got)
	}
	n := mustOpen(t, dir, "")
	if n.Name() != "alpha" || n.RuleCount() != 0 {
		t.Fatalf("after the upgrade: name %q, %d rules", n.Name(), n.RuleCount())
	}
	var onDisk map[string]any
	if err := json.Unmarshal(read(t, path), &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk["schema"] != float64(SchemaVersion) || onDisk["pairing_codes"] == nil {
		t.Fatalf("the file after Open: %s", read(t, path))
	}
}

func TestPutPeer(t *testing.T) {
	n := openNode(t, "alpha")
	a, err := n.PutPeer(testPeer("office", 1))
	if err != nil || a.Name != "office" {
		t.Fatalf("first peer: %+v, %v", a, err)
	}
	// another node with the same name gets a suffix
	b, err := n.PutPeer(testPeer("office", 2))
	if err != nil || b.Name != "office-2" {
		t.Fatalf("second peer called office: %+v, %v", b, err)
	}
	c, err := n.PutPeer(testPeer("office", 3))
	if err != nil || c.Name != "office-3" {
		t.Fatalf("third peer called office: %+v, %v", c, err)
	}
	if n.PeerCount() != 3 || len(stateOnDisk(t, n).Peers) != 3 {
		t.Fatalf("%d peers in memory, %d on disk", n.PeerCount(), len(stateOnDisk(t, n).Peers))
	}

	// the same node paired again: one record, new secret and address, name and paths kept
	again := testPeer("renamed", 1)
	again.PairSecret = bytes.Repeat([]byte{9}, PairSecretLen)
	again.Addresses = []string{"192.0.2.99:9851"}
	again.Paths = nil
	got, err := n.PutPeer(again)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "office" || !bytes.Equal(got.PairSecret, again.PairSecret) || got.Addresses[0] != "192.0.2.99:9851" {
		t.Fatalf("paired again: %+v", got)
	}
	if len(got.Paths) != 1 || got.Paths[0].Addr != "192.0.2.99:9851" {
		t.Fatalf("the default path did not follow the new address: %+v", got.Paths)
	}
	if n.PeerCount() != 3 {
		t.Fatalf("%d peers after pairing one again", n.PeerCount())
	}

	// look-ups return copies
	p, ok := n.Peer("office")
	if !ok {
		t.Fatal("peer office not found")
	}
	p.PairSecret[0] ^= 0xff
	p.Paths[0].Roles[0] = "changed"
	if q, _ := n.PeerByFingerprint(a.Fingerprint); q.PairSecret[0] != 9 || q.Paths[0].Roles[0] != "data" {
		t.Fatal("a returned peer shares memory with the node's state")
	}
	if _, ok := n.Peer("nobody"); ok {
		t.Fatal("found a peer that is not there")
	}

	// a very long name still gets its suffix within the limit
	long := strings.Repeat("x", MaxNameLen)
	if _, err := n.PutPeer(testPeer(long, 4)); err != nil {
		t.Fatal(err)
	}
	d, err := n.PutPeer(testPeer(long, 5))
	if err != nil || len(d.Name) != MaxNameLen || !strings.HasSuffix(d.Name, "-2") {
		t.Fatalf("suffix on a name of full length: %q, %v", d.Name, err)
	}
}

func TestPutPeerRefuses(t *testing.T) {
	n := openNode(t, "alpha")
	self := testPeer("me", 1)
	self.Fingerprint = n.Fingerprint()
	if _, err := n.PutPeer(self); !errors.Is(err, ErrSelf) {
		t.Fatalf("the node as its own peer: %v", err)
	}
	bad := testPeer("two\nlines", 2)
	if _, err := n.PutPeer(bad); err == nil {
		t.Fatal("a peer name with a line break was stored")
	}
	short := testPeer("short", 3)
	short.PairSecret = short.PairSecret[:8]
	if _, err := n.PutPeer(short); err == nil {
		t.Fatal("a peer with a short secret was stored")
	}
	badPath := testPeer("path", 4)
	badPath.Paths[0].Security = "rot13"
	if _, err := n.PutPeer(badPath); err == nil {
		t.Fatal("a path of an unknown security tier was stored")
	}
	if n.PeerCount() != 0 || len(stateOnDisk(t, n).Peers) != 0 {
		t.Fatal("a refused peer left something behind")
	}
}

func TestRemovePeer(t *testing.T) {
	n := openNode(t, "alpha")
	n.PutPeer(testPeer("one", 1))
	n.PutPeer(testPeer("two", 2))
	if err := n.RemovePeer("one"); err != nil {
		t.Fatal(err)
	}
	if err := n.RemovePeer("one"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removing twice: %v, want ErrNotFound", err)
	}
	ps := stateOnDisk(t, n).Peers
	if len(ps) != 1 || ps[0].Name != "two" || n.PeerCount() != 1 {
		t.Fatalf("after the removal: %+v", ps)
	}
}

// When the state file cannot be written, the change is not made in memory either.
func TestChangeIsUndoneWhenTheFileCannotBeWritten(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("the super-user writes to a read-only directory")
	}
	n := openNode(t, "alpha")
	if _, err := n.PutPeer(testPeer("one", 1)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(n.Dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(n.Dir, 0o700)
	if _, err := n.PutPeer(testPeer("two", 2)); err == nil {
		t.Fatal("PutPeer succeeded without being able to write the state file")
	}
	if err := n.RemovePeer("one"); err == nil {
		t.Fatal("RemovePeer succeeded without being able to write the state file")
	}
	if ps := n.Peers(); len(ps) != 1 || ps[0].Name != "one" {
		t.Fatalf("memory after two failed writes: %+v", ps)
	}
	os.Chmod(n.Dir, 0o700)
	if ps := stateOnDisk(t, n).Peers; len(ps) != 1 || ps[0].Name != "one" {
		t.Fatalf("file after two failed writes: %+v", ps)
	}
}

func testCode(id string, secret string, expires time.Time) PairingCode {
	h := sha256.Sum256([]byte(secret))
	return PairingCode{ID: id, SecretHash: h[:], Expires: expires}
}

func TestPairingCodeRecords(t *testing.T) {
	n := openNode(t, "alpha")
	now := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := n.AddPairingCode(testCode("00000001", "s1", now.Add(time.Hour)), now); err != nil {
		t.Fatal(err)
	}
	if err := n.AddPairingCode(testCode("00000002", "s2", now.Add(time.Minute)), now); err != nil {
		t.Fatal(err)
	}
	if err := n.AddPairingCode(testCode("00000001", "s3", now.Add(time.Hour)), now); err == nil {
		t.Fatal("a second code with the same id was accepted")
	}
	if err := n.AddPairingCode(PairingCode{ID: "xyz", SecretHash: make([]byte, 32), Expires: now.Add(time.Hour)}, now); err == nil {
		t.Fatal("a code with a malformed id was accepted")
	}
	list := n.PairingCodes(now)
	if len(list) != 2 || list[0].ID != "00000001" || list[1].ID != "00000002" {
		t.Fatalf("list %+v", list)
	}
	for _, pc := range list {
		if pc.SecretHash != nil {
			t.Fatal("the list carries a secret hash")
		}
	}
	// after two minutes the second has expired: not listed, and dropped at the next change
	later := now.Add(2 * time.Minute)
	if list := n.PairingCodes(later); len(list) != 1 || list[0].ID != "00000001" {
		t.Fatalf("list after expiry %+v", list)
	}
	if err := n.AddPairingCode(testCode("00000003", "s3", later.Add(time.Hour)), later); err != nil {
		t.Fatal(err)
	}
	if got := len(stateOnDisk(t, n).PairingCodes); got != 2 {
		t.Fatalf("%d codes on disk, want the expired one dropped", got)
	}
	if err := n.RevokePairingCode("00000001"); err != nil {
		t.Fatal(err)
	}
	if err := n.RevokePairingCode("00000001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking twice: %v, want ErrNotFound", err)
	}
	if list := n.PairingCodes(later); len(list) != 1 || list[0].ID != "00000003" {
		t.Fatalf("list after revoking %+v", list)
	}
}

func TestRedeemPairingCode(t *testing.T) {
	n := openNode(t, "alpha")
	now := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	want := sha256.Sum256([]byte("right"))
	right := func(h []byte) bool { return bytes.Equal(h, want[:]) }
	wrong := func([]byte) bool { return false }
	add := func(id string, ttl time.Duration) {
		t.Helper()
		if err := n.AddPairingCode(testCode(id, "right", now.Add(ttl)), now); err != nil {
			t.Fatal(err)
		}
	}
	add("0000000a", time.Hour)

	if _, err := n.RedeemPairingCode("ffffffff", now, right, testPeer("b", 2)); !errors.Is(err, ErrCodeUnknown) {
		t.Fatalf("unknown id: %v", err)
	}
	p, err := n.RedeemPairingCode("0000000a", now, right, testPeer("b", 2))
	if err != nil || p.Name != "b" {
		t.Fatalf("redeem: %+v, %v", p, err)
	}
	st := stateOnDisk(t, n)
	if len(st.Peers) != 1 || !st.PairingCodes[0].Used {
		t.Fatalf("after redeeming: %d peers, used %v", len(st.Peers), st.PairingCodes[0].Used)
	}
	if _, err := n.RedeemPairingCode("0000000a", now, right, testPeer("c", 3)); !errors.Is(err, ErrCodeUsed) {
		t.Fatalf("a used code: %v", err)
	}

	add("0000000b", time.Minute)
	if _, err := n.RedeemPairingCode("0000000b", now.Add(2*time.Minute), right, testPeer("c", 3)); !errors.Is(err, ErrCodeExpired) {
		t.Fatalf("an expired code: %v", err)
	}

	// a peer that cannot be stored leaves the code unused
	add("0000000c", time.Hour)
	if _, err := n.RedeemPairingCode("0000000c", now, right, testPeer("bad\nname", 4)); err == nil {
		t.Fatal("a peer with a bad name was stored")
	}
	if _, err := n.RedeemPairingCode("0000000c", now, right, testPeer("d", 4)); err != nil {
		t.Fatalf("the code after a refused peer: %v", err)
	}

	// wrong proofs: the fifth in a row removes the code
	add("0000000d", time.Hour)
	for i := 1; i < MaxProofFailures; i++ {
		if _, err := n.RedeemPairingCode("0000000d", now, wrong, testPeer("e", 5)); !errors.Is(err, ErrCodeProof) {
			t.Fatalf("wrong proof %d: %v", i, err)
		}
	}
	if _, err := n.RedeemPairingCode("0000000d", now, wrong, testPeer("e", 5)); !errors.Is(err, ErrCodeProof) {
		t.Fatalf("wrong proof %d: %v", MaxProofFailures, err)
	}
	if _, err := n.RedeemPairingCode("0000000d", now, right, testPeer("e", 5)); !errors.Is(err, ErrCodeUnknown) {
		t.Fatalf("the right proof after %d wrong ones: %v, want the code gone", MaxProofFailures, err)
	}

	// a right proof in between starts the count again; here it also uses the code up
	add("0000000e", time.Hour)
	for i := 1; i < MaxProofFailures; i++ {
		n.RedeemPairingCode("0000000e", now, wrong, testPeer("f", 6))
	}
	if _, err := n.RedeemPairingCode("0000000e", now, right, testPeer("f", 6)); err != nil {
		t.Fatalf("the right proof after %d wrong ones: %v", MaxProofFailures-1, err)
	}
}

// Readers and writers of the state from many goroutines: for the race detector.
func TestStateFromManyGoroutines(t *testing.T) {
	n := openNode(t, "alpha")
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("peer%d", i)
			if _, err := n.PutPeer(testPeer(name, byte(10+i))); err != nil {
				t.Error(err)
			}
			n.AddPairingCode(testCode(fmt.Sprintf("%08x", i), "s", now.Add(time.Hour)), now)
			n.Peers()
			n.PeerCount()
			n.PairingCodes(now)
			n.Name()
			if i%2 == 0 {
				n.RemovePeer(name)
			}
		}(i)
	}
	wg.Wait()
	if n.PeerCount() != 4 || len(stateOnDisk(t, n).Peers) != 4 {
		t.Fatalf("%d peers in memory, %d on disk, want 4", n.PeerCount(), len(stateOnDisk(t, n).Peers))
	}
}

// A peer whose name is taken gets a suffix; a long name of multi-byte characters gives up
// whole characters for it, never part of one.
func TestPeerNameSuffixKeepsCharactersWhole(t *testing.T) {
	for _, name := range []string{
		strings.Repeat("\u00e9", MaxNameLen/2),       // 32 characters of 2 bytes
		strings.Repeat("\u20ac", MaxNameLen/3) + "x", // 21 characters of 3 bytes and one of 1
		strings.Repeat("\U0001d11e", MaxNameLen/4),   // 16 characters of 4 bytes
		strings.Repeat("x", MaxNameLen),
	} {
		n := openNode(t, "alpha")
		if _, err := n.PutPeer(testPeer(name, 1)); err != nil {
			t.Fatal(err)
		}
		for seed := byte(2); seed <= 12; seed++ { // suffixes of two and of three bytes
			p, err := n.PutPeer(testPeer(name, seed))
			if err != nil {
				t.Fatalf("peer %d called %q: %v", seed, name, err)
			}
			want := fmt.Sprintf("-%d", seed)
			if !strings.HasSuffix(p.Name, want) || len(p.Name) > MaxNameLen || CheckName(p.Name) != nil {
				t.Fatalf("peer %d called %q was stored as %q", seed, name, p.Name)
			}
			if !strings.HasPrefix(name, strings.TrimSuffix(p.Name, want)) {
				t.Fatalf("%q does not start like %q", p.Name, name)
			}
		}
	}
}

func goodPath() Path {
	return Path{Name: "main", Type: "tcp", Addr: "192.0.2.10:9851", Security: SecurityTLS, Roles: []string{RoleData}, Subs: 8}
}

func TestPathCheck(t *testing.T) {
	pin := strings.Repeat("ab", 32)
	good := map[string]func(*Path){
		"as made by a pairing": func(p *Path) {},
		"host name":            func(p *Path) { p.Addr = "nas.example.org:9851" },
		"IPv6":                 func(p *Path) { p.Addr = "[2001:db8::7]:9851" },
		"repair and control":   func(p *Path) { p.Roles = []string{RoleRepair, RoleControl}; p.Subs = 2 },
		"one sub":              func(p *Path) { p.Subs = 1 },
		"most subs":            func(p *Path) { p.Subs = MaxSubs },
		"off":                  func(p *Path) { p.Security = SecurityOff },
		"owncert pin":          func(p *Path) { p.Security, p.Pin = SecurityOwnCert, pin },
		"owncert ca":           func(p *Path) { p.Security, p.Verify = SecurityOwnCert, VerifyCA },
		"socks5":               func(p *Path) { p.Type, p.Proxy = "socks5", "socks5://192.0.2.1:1080" },
		"socks5 with a user":   func(p *Path) { p.Type, p.Proxy = "socks5", "socks5://user:hunter2@proxy.example.org:1080" },
		"longest name":         func(p *Path) { p.Name = strings.Repeat("n", MaxPathNameLen) },
	}
	for name, change := range good {
		p := goodPath()
		change(&p)
		if err := p.Check(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]func(*Path){
		"no name":                func(p *Path) { p.Name = "" },
		"name too long":          func(p *Path) { p.Name = strings.Repeat("n", MaxPathNameLen+1) },
		"name with a space":      func(p *Path) { p.Name = "the main" },
		"name with a line break": func(p *Path) { p.Name = "a\nb" },
		"unknown type":           func(p *Path) { p.Type = "udp" },
		"no address":             func(p *Path) { p.Addr = "" },
		"address without a port": func(p *Path) { p.Addr = "nas.example.org" },
		"port out of range":      func(p *Path) { p.Addr = "192.0.2.10:70000" },
		"address with a space":   func(p *Path) { p.Addr = "nas example:9851" },
		"proxy on tcp":           func(p *Path) { p.Proxy = "socks5://192.0.2.1:1080" },
		"socks5 without a proxy": func(p *Path) { p.Type = "socks5" },
		"unknown tier":           func(p *Path) { p.Security = "rot13" },
		"pin on tls":             func(p *Path) { p.Pin = pin },
		"owncert without a pin":  func(p *Path) { p.Security = SecurityOwnCert },
		"ca with a pin":          func(p *Path) { p.Security, p.Verify, p.Pin = SecurityOwnCert, VerifyCA, pin },
		"unknown verify":         func(p *Path) { p.Security, p.Verify = SecurityOwnCert, "trust" },
		"no role":                func(p *Path) { p.Roles = nil },
		"unknown role":           func(p *Path) { p.Roles = []string{"bulk"} },
		"a role twice":           func(p *Path) { p.Roles = []string{RoleData, RoleData} },
		"data and repair":        func(p *Path) { p.Roles = []string{RoleData, RoleRepair} },
		"no subs":                func(p *Path) { p.Subs = 0 },
		"too many subs":          func(p *Path) { p.Subs = MaxSubs + 1 },
	}
	for name, change := range bad {
		p := goodPath()
		change(&p)
		if err := p.Check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A proxy that cannot be used is refused without being repeated: it may hold a password.
func TestPathCheckDoesNotEchoTheProxy(t *testing.T) {
	for _, proxy := range []string{
		"socks://user:hunter2@192.0.2.1:1080",       // another scheme
		"socks5h://user:hunter2@192.0.2.1:1080",     //
		"socks5://user:hunter2@192.0.2.1",           // no port
		"socks5://user:hunter2@192.0.2.1:1080/path", // more than a proxy
		"socks5://user:hunter2@:1080",               // no host
		"user:hunter2@192.0.2.1:1080",               // no scheme
		"socks5://user:hunter2@192.0.2.1:1080?x=1",  //
		"://hunter2",
	} {
		p := goodPath()
		p.Type, p.Proxy = "socks5", proxy
		for _, err := range []error{p.Check(), CheckPaths([]Path{p})} {
			if err == nil {
				t.Errorf("proxy %q: accepted", proxy)
			} else if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "192.0.2.1") {
				t.Errorf("the error repeats the proxy: %v", err)
			}
		}
	}
}

func TestCheckPathsRefusesTwoOfAName(t *testing.T) {
	a, b := goodPath(), goodPath()
	b.Addr = "192.0.2.11:9851"
	if err := CheckPaths([]Path{a, b}); err == nil {
		t.Fatal("two paths of one name were accepted")
	}
	b.Name = "second"
	if err := CheckPaths([]Path{a, b}); err != nil {
		t.Fatal(err)
	}
	if err := CheckPaths(nil); err != nil {
		t.Fatalf("no paths: %v", err)
	}
}

// SetPeerPaths replaces a peer's paths as a whole, in memory and in the file, or not at
// all.
func TestSetPeerPaths(t *testing.T) {
	n := openNode(t, "alpha")
	if _, err := n.PutPeer(testPeer("one", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := n.PutPeer(testPeer("two", 2)); err != nil {
		t.Fatal(err)
	}
	main, side := goodPath(), goodPath()
	side.Name, side.Addr, side.Security, side.Roles, side.Subs = "side", "192.0.2.11:9852", SecurityOff, []string{RoleRepair, RoleControl}, 2
	want := []Path{main, side}
	got, err := n.SetPeerPaths("one", want)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Paths, want) || got.Name != "one" || len(got.PairSecret) != PairSecretLen {
		t.Fatalf("stored %+v", got)
	}
	// the caller's slice is not the node's
	want[0].Addr = "192.0.2.99:1"
	if p, _ := n.Peer("one"); p.Paths[0].Addr != "192.0.2.10:9851" {
		t.Fatal("the stored paths share memory with the caller's")
	}
	want[0].Addr = "192.0.2.10:9851"
	if ps := stateOnDisk(t, n).Peers; !reflect.DeepEqual(ps[0].Paths, want) {
		t.Fatalf("file: %+v", ps[0].Paths)
	}
	if other, _ := n.Peer("two"); len(other.Paths) != 1 || other.Paths[0].Name != DefaultPathName {
		t.Fatalf("another peer's paths changed: %+v", other.Paths)
	}

	// refused as a whole: the second path is not one
	broken := []Path{main, {Name: "bad", Type: "tcp", Addr: "nowhere", Security: SecurityTLS, Roles: []string{RoleData}, Subs: 8}}
	if _, err := n.SetPeerPaths("one", broken); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a path without a port: %v, want an error matching ErrInvalid", err)
	}
	if _, err := n.SetPeerPaths("one", []Path{main, main}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("two paths of one name: %v, want an error matching ErrInvalid", err)
	}
	if _, err := n.SetPeerPaths("nobody", want); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown peer: %v, want an error matching ErrNotFound", err)
	}
	if p, _ := n.Peer("one"); !reflect.DeepEqual(p.Paths, want) {
		t.Fatalf("refused paths changed the peer: %+v", p.Paths)
	}

	// no paths at all is allowed: the peer can then only be reached by it
	if got, err = n.SetPeerPaths("one", nil); err != nil || len(got.Paths) != 0 {
		t.Fatalf("no paths: %+v, %v", got.Paths, err)
	}
	if ps := stateOnDisk(t, n).Peers; len(ps[0].Paths) != 0 {
		t.Fatalf("file: %+v", ps[0].Paths)
	}
}

// When the state file cannot be written, the peer keeps the paths it had.
func TestSetPeerPathsIsUndoneWhenTheFileCannotBeWritten(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("the super-user writes to a read-only directory")
	}
	n := openNode(t, "alpha")
	if _, err := n.PutPeer(testPeer("one", 1)); err != nil {
		t.Fatal(err)
	}
	before, _ := n.Peer("one")
	if err := os.Chmod(n.Dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(n.Dir, 0o700)
	p := goodPath()
	p.Security = SecurityOff
	if _, err := n.SetPeerPaths("one", []Path{p}); err == nil {
		t.Fatal("SetPeerPaths succeeded without being able to write the state file")
	}
	if after, _ := n.Peer("one"); !reflect.DeepEqual(after.Paths, before.Paths) {
		t.Fatalf("memory after the failed write: %+v", after.Paths)
	}
	os.Chmod(n.Dir, 0o700)
	if ps := stateOnDisk(t, n).Peers; !reflect.DeepEqual(ps[0].Paths, before.Paths) {
		t.Fatalf("file after the failed write: %+v", ps[0].Paths)
	}
}

// A peer has at most MaxPaths paths.
func TestTooManyPaths(t *testing.T) {
	var paths []Path
	for i := 0; i <= MaxPaths; i++ {
		paths = append(paths, Path{Name: fmt.Sprintf("p%d", i), Type: "tcp", Addr: "192.0.2.1:9000", Security: SecurityTLS,
			Roles: []string{"data"}, Subs: 1})
	}
	if err := CheckPaths(paths[:MaxPaths]); err != nil {
		t.Fatalf("%d paths refused: %v", MaxPaths, err)
	}
	if err := CheckPaths(paths); err == nil {
		t.Fatalf("%d paths accepted", MaxPaths+1)
	}
}
