package node

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParsePortRange(t *testing.T) {
	for _, c := range []struct {
		in        string
		any       bool
		host      string
		first, n  int
		errSubstr string
	}{
		{in: "192.0.2.1:873", host: "192.0.2.1", first: 873, n: 1},
		{in: "0.0.0.0:50000-50050", any: true, host: "0.0.0.0", first: 50000, n: 51},
		{in: "[2001:db8::1]:22", host: "2001:db8::1", first: 22, n: 1},
		{in: ":5000", any: true, host: "", first: 5000, n: 1},
		{in: ":5000", errSubstr: "no IP address"},
		{in: "nas.example.net:873", errSubstr: "takes no host name"},
		{in: "192.0.2.1", errSubstr: "want IP:port"},
		{in: "192.0.2.1:0", errSubstr: "1 to 65535"},
		{in: "192.0.2.1:70000", errSubstr: "1 to 65535"},
		{in: "192.0.2.1:10-5", errSubstr: "below the first"},
		{in: "192.0.2.1:5-x", errSubstr: "last port"},
		{in: "192.0.2.1:1-1024", host: "192.0.2.1", first: 1, n: 1024},
		{in: "192.0.2.1:1-1025", errSubstr: "at most 1024"},
	} {
		p, err := ParsePortRange(c.in, c.any)
		if c.errSubstr != "" {
			if err == nil || !strings.Contains(err.Error(), c.errSubstr) {
				t.Errorf("%q: error %v, want one with %q", c.in, err, c.errSubstr)
			}
			continue
		}
		if err != nil || p.Host != c.host || p.First != c.first || p.Ports() != c.n {
			t.Errorf("%q: %+v, %v", c.in, p, err)
		}
	}
	// the ports of two ranges correspond one to one
	l, _ := ParsePortRange("0.0.0.0:50000-50002", true)
	d, _ := ParsePortRange("[2001:db8::7]:6000-6002", false)
	for i, want := range [][2]string{{"0.0.0.0:50000", "[2001:db8::7]:6000"}, {"0.0.0.0:50001", "[2001:db8::7]:6001"}, {"0.0.0.0:50002", "[2001:db8::7]:6002"}} {
		if l.Addr(i) != want[0] || d.Addr(i) != want[1] {
			t.Errorf("port %d: %s -> %s, want %s -> %s", i, l.Addr(i), d.Addr(i), want[0], want[1])
		}
	}
}

func TestRuleCheck(t *testing.T) {
	good := Rule{Name: "zfs", Listen: "0.0.0.0:50000-50050", Destination: "192.0.2.10:50000-50050", Peer: "beta", Enabled: true}
	if err := good.Check(); err != nil {
		t.Fatal(err)
	}
	tr := Rule{Name: "lan", Match: "198.51.100.0/24:22, [2001:db8::/32]", Peer: "beta"}
	if err := tr.Check(); err != nil || !tr.Transparent() || good.Transparent() {
		t.Fatalf("transparent rule: %v", err)
	}
	change := func(f func(*Rule)) Rule { r := good; f(&r); return r }
	for want, r := range map[string]Rule{
		"no name":               change(func(r *Rule) { r.Name = "" }),
		"space":                 change(func(r *Rule) { r.Name = "a b" }),
		"longer than":           change(func(r *Rule) { r.Name = strings.Repeat("x", MaxRuleNameLen+1) }),
		"names no peer":         change(func(r *Rule) { r.Peer = "" }),
		"same length":           change(func(r *Rule) { r.Destination = "192.0.2.10:50000-50049" }),
		"cannot be used":        change(func(r *Rule) { r.Destination = "nas..example.net:50000-50050" }),
		"takes no host name":    change(func(r *Rule) { r.Listen = "nas.example.net:50000-50050" }),
		"needs listen":          change(func(r *Rule) { r.Listen = "" }),
		"fixed rule needs":      change(func(r *Rule) { r.Destination = "" }),
		"not both":              change(func(r *Rule) { r.Match = "198.51.100.0/24" }),
		"destination":           change(func(r *Rule) { r.Destination = ":50000-50050" }),
		"match":                 {Name: "t", Match: "not a network", Peer: "beta"},
		"match names no networ": {Name: "t", Match: " , ", Peer: "beta"},
	} {
		if err := r.Check(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error with %q, got %v (%+v)", want, err, r)
		}
	}
}

func TestCheckRulesCollisions(t *testing.T) {
	r := func(name, listen string) Rule {
		p, _ := ParsePortRange(listen, true)
		dst := "192.0.2.10:" + strings.SplitN(listen[strings.LastIndex(listen, ":")+1:], "-", 2)[0]
		if p.Ports() > 1 {
			dst = "192.0.2.10:" + listen[strings.LastIndex(listen, ":")+1:]
		}
		return Rule{Name: name, Listen: listen, Destination: dst, Peer: "beta"}
	}
	for _, c := range []struct {
		a, b    string
		collide bool
	}{
		{"0.0.0.0:5000", "0.0.0.0:5001", false},
		{"0.0.0.0:5000", "0.0.0.0:5000", true},
		{"0.0.0.0:5000-5010", "0.0.0.0:5010-5020", true},
		{"0.0.0.0:5000-5010", "0.0.0.0:5011-5020", false},
		{"192.0.2.1:5000", "192.0.2.2:5000", false},
		{"192.0.2.1:5000", "0.0.0.0:5000", true},
		{"192.0.2.1:5000", ":4990-5005", true},
		{"[::]:5000", "192.0.2.1:5000", true},
		{"[2001:db8::1]:5000", "[2001:db8:0::1]:5000", true},
	} {
		err := CheckRules([]Rule{r("a", c.a), r("b", c.b)})
		if c.collide != (err != nil) {
			t.Errorf("%s and %s: %v", c.a, c.b, err)
		}
		if err != nil && !strings.Contains(err.Error(), "already takes") {
			t.Errorf("%s and %s: %v", c.a, c.b, err)
		}
	}
	if err := CheckRules([]Rule{r("a", "0.0.0.0:1"), r("a", "0.0.0.0:2")}); err == nil || !strings.Contains(err.Error(), "two rules are called") {
		t.Errorf("same name twice: %v", err)
	}
	// a transparent rule listens nowhere
	if err := CheckRules([]Rule{r("a", "0.0.0.0:22"), {Name: "t", Match: "0.0.0.0/0:22", Peer: "beta"}}); err != nil {
		t.Error(err)
	}
}

func TestRuleStore(t *testing.T) {
	n := openNode(t, "alpha")
	if _, err := n.PutPeer(Peer{Name: "beta", Fingerprint: strings.Repeat("b", 64), PairSecret: make([]byte, PairSecretLen), PairedAt: time.Unix(1000000000, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	r1 := Rule{Name: "r1", Listen: "0.0.0.0:5000", Destination: "192.0.2.10:873", Peer: "beta", Enabled: true}
	if err := n.AddRule(r1); err != nil {
		t.Fatal(err)
	}
	for want, r := range map[string]Rule{
		"two rules are called":      r1,
		"already takes":             {Name: "r2", Listen: ":5000", Destination: "192.0.2.10:874", Peer: "beta"},
		"no peer is called":         {Name: "r3", Listen: ":5001", Destination: "192.0.2.10:874", Peer: "gamma"},
		"neither an IP address nor": {Name: "r4", Listen: ":5002", Destination: "192.0.2:874", Peer: "beta"},
	} {
		if err := n.AddRule(r); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), want) {
			t.Errorf("AddRule(%+v): %v, want ErrInvalid with %q", r, err, want)
		}
	}
	if n.RuleCount() != 1 {
		t.Fatalf("%d rules after refused additions", n.RuleCount())
	}
	t1 := Rule{Name: "t1", Match: "198.51.100.0/24", Peer: "beta", Enabled: true}
	if err := n.AddRule(t1); err != nil {
		t.Fatal(err)
	}
	// replace: same place in the list; may be renamed; refused like an addition
	r1b := Rule{Name: "r1-new", Listen: "0.0.0.0:5005", Destination: "192.0.2.10:873", Peer: "beta"}
	if err := n.ReplaceRule("r1", r1b); err != nil {
		t.Fatal(err)
	}
	if got := n.Rules(); len(got) != 2 || got[0] != r1b || got[1] != t1 {
		t.Fatalf("rules after replace: %+v", got)
	}
	if err := n.ReplaceRule("r1-new", Rule{Name: "t1", Listen: ":1", Destination: "192.0.2.1:1", Peer: "beta"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("renaming onto another rule: %v", err)
	}
	if err := n.ReplaceRule("nope", r1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replacing a rule that is not there: %v", err)
	}
	r1b.Enabled = true
	if err := n.ReplaceRule("r1-new", r1b); err != nil {
		t.Fatal(err)
	}
	if got := n.Rules(); !got[0].Enabled {
		t.Fatal("the rule was not switched on")
	}
	// the rules are in the file, and a node that opens the directory has them
	dir := n.Dir
	n.Close()
	n2 := mustOpen(t, dir, "")
	if got := n2.Rules(); len(got) != 2 || got[0].Name != "r1-new" || got[1] != t1 {
		t.Fatalf("rules after reopening: %+v", got)
	}
	// removing the peer leaves the rules: they name a peer that is gone
	if err := n2.RemovePeer("beta"); err != nil {
		t.Fatal(err)
	}
	n2.Close()
	n3 := mustOpen(t, dir, "")
	if n3.RuleCount() != 2 || n3.PeerCount() != 0 {
		t.Fatalf("after removing the peer: %d rules, %d peers", n3.RuleCount(), n3.PeerCount())
	}
	if err := n3.RemoveRule("t1"); err != nil {
		t.Fatal(err)
	}
	if err := n3.RemoveRule("t1"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestPeerOptions(t *testing.T) {
	n := openNode(t, "alpha")
	if _, err := n.PutPeer(Peer{Name: "beta", Fingerprint: strings.Repeat("b", 64), PairSecret: make([]byte, PairSecretLen), PairedAt: time.Unix(1000000000, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	allow, udp := "198.51.100.0/24:873", "192.0.2.1:9852"
	p, err := n.SetPeerOptions("beta", &allow, nil)
	if err != nil || p.Allow != allow || p.UDPDial != "" {
		t.Fatalf("%+v, %v", p, err)
	}
	if p, err = n.SetPeerOptions("beta", nil, &udp); err != nil || p.Allow != allow || p.UDPDial != udp {
		t.Fatalf("%+v, %v", p, err)
	}
	bad := "not a network"
	if _, err := n.SetPeerOptions("beta", &bad, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a bad allow list: %v", err)
	}
	if _, err := n.SetPeerOptions("beta", nil, &bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a bad UDP address: %v", err)
	}
	if _, err := n.SetPeerOptions("nobody", &allow, nil); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if got, _ := n.Peer("beta"); got.Allow != allow || got.UDPDial != udp {
		t.Fatalf("after refused changes: %+v", got)
	}
	// pairing again keeps them
	if _, err := n.PutPeer(Peer{Name: "other-name", Fingerprint: strings.Repeat("b", 64), PairSecret: make([]byte, PairSecretLen), PairedAt: time.Unix(1000000001, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	if got, _ := n.Peer("beta"); got.Allow != allow || got.UDPDial != udp {
		t.Fatalf("after pairing again: %+v", got)
	}
}

// A state file of schema version 2 is brought to version 3 when the node opens the
// directory: the peers and pairing codes stay, the rule list is there and empty.
func TestSchemaTwoIsUpgraded(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	mustOpen(t, dir, "alpha").Close()
	path := filepath.Join(dir, StateFile)
	secret := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	v2 := `{"schema":2,"node":{"name":"alpha"},"peers":[` +
		`{"name":"one","fingerprint":"` + strings.Repeat("a", 64) + `","pair_secret":"` + secret + `","addresses":["192.0.2.1:9851"],"paired_at":"2001-02-03T04:05:06Z",` +
		`"paths":[{"name":"default","type":"tcp","addr":"192.0.2.1:9851","security":"tls","roles":["data"],"subs":8}]}],` +
		`"pairing_codes":[{"id":"0a0b0c0d","secret_hash":"` + secret + `","expires":"2001-02-03T05:05:06Z","used":false}],"rules":[]}`
	if err := os.WriteFile(path, []byte(v2), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadState(path)
	if err != nil || s.Schema != SchemaVersion || len(s.Peers) != 1 || len(s.PairingCodes) != 1 || s.Rules == nil || len(s.Rules) != 0 {
		t.Fatalf("LoadState of a version 2 file: %+v, %v", s, err)
	}
	if got := string(read(t, path)); got != v2 {
		t.Fatalf("LoadState changed the file to %s", got)
	}
	n := mustOpen(t, dir, "")
	if p, ok := n.Peer("one"); !ok || len(p.Paths) != 1 || p.Allow != "" {
		t.Fatalf("the peer after the upgrade: %+v", p)
	}
	var onDisk map[string]any
	if err := json.Unmarshal(read(t, path), &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk["schema"] != float64(SchemaVersion) {
		t.Fatalf("the file after Open: %s", read(t, path))
	}
	if err := n.AddRule(Rule{Name: "r", Listen: ":5000", Destination: "192.0.2.9:873", Peer: "one", Enabled: true}); err != nil {
		t.Fatal(err)
	}
}

// The destination of a rule takes an IP address or a host name, and nothing that is
// neither.
func TestParseDestination(t *testing.T) {
	long := strings.Repeat("a", 63)
	for _, c := range []struct {
		in        string
		host      string
		named     bool
		first, n  int
		errSubstr string
	}{
		{in: "192.0.2.1:873", host: "192.0.2.1", first: 873, n: 1},
		{in: "[2001:db8::1]:22", host: "2001:db8::1", first: 22, n: 1},
		{in: "nas.example.net:873", host: "nas.example.net", named: true, first: 873, n: 1},
		{in: "nas:50000-50010", host: "nas", named: true, first: 50000, n: 11},
		{in: "my_nas-2.lan:22", host: "my_nas-2.lan", named: true, first: 22, n: 1},
		{in: "NAS.Example.NET:22", host: "NAS.Example.NET", named: true, first: 22, n: 1},
		{in: "1st.example:22", host: "1st.example", named: true, first: 22, n: 1},
		{in: long + "." + long + "." + long + "." + strings.Repeat("a", 61) + ":22", host: long + "." + long + "." + long + "." + strings.Repeat("a", 61), named: true, first: 22, n: 1},
		{in: ":873", errSubstr: "no IP address and no host name"},
		{in: "nas.example.net", errSubstr: "want HOST:port"},
		{in: "nas.example.net:0", errSubstr: "1 to 65535"},
		{in: "nas.example.net:1-1025", errSubstr: "at most 1024"},
		{in: "192.0.2:873", errSubstr: "neither an IP address nor a host name"},
		{in: "192.0.2.999:873", errSubstr: "neither an IP address nor a host name"},
		{in: "12345:873", errSubstr: "neither an IP address nor a host name"},
		{in: "nas..example.net:873", errSubstr: "between its dots"},
		{in: "nas.example.net.:873", errSubstr: "between its dots"},
		{in: ".nas:873", errSubstr: "between its dots"},
		{in: "-nas.example:873", errSubstr: "hyphen"},
		{in: "nas-.example:873", errSubstr: "hyphen"},
		{in: "nas example:873", errSubstr: "letters, digits"},
		{in: "nas/example:873", errSubstr: "letters, digits"},
		{in: "n\u00e4s.example:873", errSubstr: "letters, digits"},
		{in: "user@nas.example:873", errSubstr: "letters, digits"},
		{in: strings.Repeat("a", 64) + ".example:873", errSubstr: "1 to 63"},
		{in: long + "." + long + "." + long + "." + strings.Repeat("a", 62) + ":22", errSubstr: "1 to 253"},
	} {
		p, err := ParseDestination(c.in)
		if c.errSubstr != "" {
			if err == nil || !strings.Contains(err.Error(), c.errSubstr) {
				t.Errorf("%q: error %v, want one with %q", c.in, err, c.errSubstr)
			}
			continue
		}
		if err != nil || p.Host != c.host || p.First != c.first || p.Ports() != c.n || p.Named() != c.named {
			t.Errorf("%q: %+v (named %v), %v", c.in, p, p.Named(), err)
		}
	}
	// a rule with a name for its destination is a rule; one with a name to listen on is not
	named := Rule{Name: "zfs", Listen: "0.0.0.0:50000-50010", Destination: "nas.example.net:50000-50010", Peer: "beta", Enabled: true}
	if err := named.Check(); err != nil {
		t.Errorf("a rule whose destination is a host name: %v", err)
	}
}
