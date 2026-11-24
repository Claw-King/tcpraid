package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A path may have further addresses and a mark: what Check takes and what it refuses.
func TestPathAddressesAndMark(t *testing.T) {
	defer func(was bool) { MarkSupported = was }(MarkSupported)
	MarkSupported = true
	with := func(change func(*Path)) Path {
		p := goodPath()
		change(&p)
		return p
	}
	eight := []string{"192.0.2.2:1", "192.0.2.3:1", "192.0.2.4:1", "192.0.2.5:1", "192.0.2.6:1", "192.0.2.7:1", "192.0.2.8:1"}
	for name, p := range map[string]Path{
		"one address":            goodPath(),
		"two addresses":          with(func(p *Path) { p.Addrs = []string{"[2001:db8::1]:9851"} }),
		"a name and an address":  with(func(p *Path) { p.Addrs = []string{"nas.example.net:9851"} }),
		"as many as a code has":  with(func(p *Path) { p.Addrs = eight }),
		"a mark":                 with(func(p *Path) { p.Mark = 7 }),
		"the largest mark":       with(func(p *Path) { p.Mark = MaxMark }),
		"an empty list of addrs": with(func(p *Path) { p.Addrs = []string{} }),
	} {
		if err := p.Check(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, c := range map[string]struct {
		p    Path
		want string
	}{
		"one too many":      {with(func(p *Path) { p.Addrs = append(eight, "192.0.2.9:1") }), "9 addresses (at most 8)"},
		"the address twice": {with(func(p *Path) { p.Addrs = []string{p.Addr} }), "is there twice"},
		"a further twice":   {with(func(p *Path) { p.Addrs = []string{"192.0.2.2:1", "192.0.2.2:1"} }), "is there twice"},
		"no port":           {with(func(p *Path) { p.Addrs = []string{"192.0.2.2"} }), "want host:port"},
		"an empty address":  {with(func(p *Path) { p.Addrs = []string{""} }), "want host:port"},
		"a negative mark":   {with(func(p *Path) { p.Mark = -1 }), "mark -1"},
	} {
		if err := c.p.Check(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error with %q", name, err, c.want)
		}
	}
	// one more than the largest mark, where an int can hold it
	if big := int64(MaxMark) + 1; int64(int(big)) == big {
		if err := with(func(p *Path) { p.Mark = int(big) }).Check(); err == nil || !strings.Contains(err.Error(), "mark 2147483648") {
			t.Errorf("a mark too large: %v", err)
		}
	}
	if got := with(func(p *Path) { p.Addrs = []string{"192.0.2.2:1"} }).Addresses(); !slices.Equal(got, []string{goodPath().Addr, "192.0.2.2:1"}) {
		t.Errorf("Addresses: %v", got)
	}
	// where the system has no socket mark, a path cannot have one
	MarkSupported = false
	if err := with(func(p *Path) { p.Mark = 7 }).Check(); err == nil || !strings.Contains(err.Error(), "only be set on Linux") {
		t.Errorf("a mark where there is none: %v", err)
	}
	if err := goodPath().Check(); err != nil {
		t.Errorf("a path without a mark where there is none: %v", err)
	}
}

// A change of a path's address takes its further addresses away unless it names them.
func TestPathChangeAddresses(t *testing.T) {
	str := func(s string) *string { return &s }
	list := func(s ...string) *[]string { return &s }
	num := func(n int) *int { return &n }
	p := goodPath()
	PathChange{Addrs: list("192.0.2.2:1", "192.0.2.3:1"), Mark: num(5)}.Apply(&p)
	if p.Addr != goodPath().Addr || !slices.Equal(p.Addrs, []string{"192.0.2.2:1", "192.0.2.3:1"}) || p.Mark != 5 {
		t.Fatalf("further addresses and a mark set: %+v", p)
	}
	PathChange{Subs: num(3)}.Apply(&p)
	if len(p.Addrs) != 2 || p.Mark != 5 || p.Subs != 3 {
		t.Fatalf("another setting changed: %+v", p)
	}
	PathChange{Addr: str("192.0.2.9:1")}.Apply(&p)
	if p.Addr != "192.0.2.9:1" || p.Addrs != nil || p.Mark != 5 {
		t.Fatalf("the address alone set: %+v", p)
	}
	PathChange{Addr: str("192.0.2.9:2"), Addrs: list("192.0.2.9:3")}.Apply(&p)
	if p.Addr != "192.0.2.9:2" || !slices.Equal(p.Addrs, []string{"192.0.2.9:3"}) {
		t.Fatalf("both set: %+v", p)
	}
	PathChange{Addrs: list(), Mark: num(0)}.Apply(&p)
	if p.Addrs != nil || p.Mark != 0 {
		t.Fatalf("the further addresses and the mark taken away: %+v", p)
	}
	if !(PathChange{}).Empty() || (PathChange{Addrs: list()}).Empty() || (PathChange{Mark: num(0)}).Empty() {
		t.Error("Empty does not tell a change of addrs or mark from none")
	}
}

// The new settings of paths and rules are in the state file only when they are set, so
// a file written without them reads as before and one with them survives a restart.
func TestStateKeepsAddressesMarkAndRulePaths(t *testing.T) {
	defer func(was bool) { MarkSupported = was }(MarkSupported)
	MarkSupported = true
	n := openNode(t, "alpha")
	peer := testPeer("beta", 1)
	if _, err := n.PutPeer(peer); err != nil {
		t.Fatal(err)
	}
	plain := Rule{Name: "plain", Listen: ":5000", Destination: "192.0.2.10:873", Peer: "beta", Enabled: true}
	if err := n.AddRule(plain); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(n.Dir, StateFile))
	if err != nil {
		t.Fatal(err)
	}
	// the peer's list of paths is in the file; the fields that are new are not
	var st struct {
		Rules []map[string]json.RawMessage `json:"rules"`
		Peers []struct {
			Paths []map[string]json.RawMessage `json:"paths"`
		} `json:"peers"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"paths", "subs"} {
		if _, ok := st.Rules[0][field]; ok {
			t.Errorf("a rule that uses every path has %q in the file", field)
		}
	}
	for _, field := range []string{"addrs", "mark"} {
		if _, ok := st.Peers[0].Paths[0][field]; ok {
			t.Errorf("a path with one address and no mark has %q in the file", field)
		}
	}
	path := peer.Paths[0]
	path.Addrs, path.Mark = []string{"[2001:db8::1]:9851"}, 9
	if _, err := n.SetPeerPaths("beta", []Path{path}); err != nil {
		t.Fatal(err)
	}
	narrow := Rule{Name: "narrow", Listen: ":5001", Destination: "192.0.2.10:873", Peer: "beta", Enabled: true, Paths: []string{"default"}, Subs: 3}
	if err := n.AddRule(narrow); err != nil {
		t.Fatal(err)
	}
	dir := n.Dir
	n.Close()
	n2 := mustOpen(t, dir, "")
	p, _ := n2.Peer("beta")
	if got := p.Paths[0]; !slices.Equal(got.Addrs, path.Addrs) || got.Mark != 9 {
		t.Errorf("the path after a restart: %+v", got)
	}
	rules := n2.Rules()
	if len(rules) != 2 || !rules[0].Equal(plain) || !rules[1].Equal(narrow) || rules[0].Narrowed() || !rules[1].Narrowed() {
		t.Errorf("the rules after a restart: %+v", rules)
	}
	// what a caller gets is its own: changing it changes nothing in the node
	rules[1].Paths[0] = "other"
	if again := n2.Rules(); again[1].Paths[0] != "default" {
		t.Error("the rules a caller got share memory with the node's")
	}
}

// What a rule may say about paths and sub-connections.
func TestRuleNarrowingCheck(t *testing.T) {
	base := Rule{Name: "r", Listen: ":5000", Destination: "192.0.2.10:873", Peer: "beta", Enabled: true}
	with := func(change func(*Rule)) Rule {
		r := base
		change(&r)
		return r
	}
	for name, r := range map[string]Rule{
		"two paths":           with(func(r *Rule) { r.Paths = []string{"main", "side"} }),
		"a number":            with(func(r *Rule) { r.Subs = MaxSubs }),
		"both":                with(func(r *Rule) { r.Paths, r.Subs = []string{"main"}, 1 }),
		"a transparent rule":  {Name: "t", Match: "192.0.2.0/24", Peer: "beta", Enabled: true, Paths: []string{"main"}, Subs: 2},
		"an empty list":       with(func(r *Rule) { r.Paths = []string{} }),
		"as many as there is": with(func(r *Rule) { r.Paths = strings.Fields(strings.Repeat("p ", MaxPaths-1) + "q") }),
	} {
		if name == "as many as there is" {
			for i := range r.Paths {
				r.Paths[i] += string(rune('a' + i))
			}
		}
		if err := r.Check(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, c := range map[string]struct {
		r    Rule
		want string
	}{
		"a path twice":      {with(func(r *Rule) { r.Paths = []string{"main", "main"} }), `"main" is there twice`},
		"an empty name":     {with(func(r *Rule) { r.Paths = []string{""} }), "the path has no name"},
		"a name with space": {with(func(r *Rule) { r.Paths = []string{"a b"} }), "holds a space"},
		"a long name":       {with(func(r *Rule) { r.Paths = []string{strings.Repeat("x", MaxPathNameLen+1)} }), "longer than"},
		"a negative number": {with(func(r *Rule) { r.Subs = -1 }), "-1 sub-connections"},
		"a number too big":  {with(func(r *Rule) { r.Subs = MaxSubs + 1 }), "65 sub-connections"},
	} {
		if err := c.r.Check(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error with %q", name, err, c.want)
		}
	}
	a := with(func(r *Rule) { r.Paths, r.Subs = []string{"main"}, 2 })
	b := a.Clone()
	if !a.Equal(b) || a.Equal(base) || a.Equal(with(func(r *Rule) { r.Paths, r.Subs = []string{"side"}, 2 })) || a.Equal(with(func(r *Rule) { r.Paths, r.Subs = []string{"main"}, 3 })) {
		t.Error("Equal does not go by the paths and the number")
	}
	b.Paths[0] = "changed"
	if a.Paths[0] != "main" {
		t.Error("Clone shares the list of paths")
	}
}

// Pairing again with a peer that is there gives its default path the new addresses, all
// of them.
func TestPutPeerAgainKeepsEveryAddress(t *testing.T) {
	n := openNode(t, "alpha")
	first := testPeer("beta", 1)
	if _, err := n.PutPeer(first); err != nil {
		t.Fatal(err)
	}
	again := testPeer("beta", 1)
	again.Addresses = []string{"192.0.2.20:9851", "[2001:db8::20]:9851"}
	stored, err := n.PutPeer(again)
	if err != nil {
		t.Fatal(err)
	}
	if p := stored.Paths[0]; p.Addr != "192.0.2.20:9851" || !slices.Equal(p.Addrs, []string{"[2001:db8::20]:9851"}) {
		t.Errorf("the default path after pairing again: %+v", p)
	}
	again.Addresses = []string{"192.0.2.30:9851"}
	if stored, err = n.PutPeer(again); err != nil || stored.Paths[0].Addr != "192.0.2.30:9851" || stored.Paths[0].Addrs != nil {
		t.Errorf("the default path after pairing again with one address: %+v, %v", stored.Paths, err)
	}
}

// What a rule may say about who dials: one of three words, or nothing for auto. A rule
// that says nothing is the same rule as before the option was there.
func TestRuleDialCheck(t *testing.T) {
	base := Rule{Name: "r", Listen: ":5000", Destination: "192.0.2.10:873", Peer: "beta", Enabled: true}
	for _, dial := range []string{"", DialSelf, DialPeer, DialBoth} {
		r := base
		r.Dial = dial
		if err := r.Check(); err != nil {
			t.Errorf("dial %q: %v", dial, err)
		}
		if want := dial; want == "" && r.DialMode() != DialAuto || want != "" && r.DialMode() != want {
			t.Errorf("dial %q is the mode %q", dial, r.DialMode())
		}
	}
	for _, dial := range []string{"auto", "Both", "me", " self"} {
		r := base
		r.Dial = dial
		if err := r.Check(); err == nil {
			t.Errorf("a stored rule with dial %q passed", dial)
		}
	}
	for in, want := range map[string]string{"": "", "auto": "", "self": "self", "peer": "peer", "both": "both"} {
		if got, err := CheckDial(in); err != nil || got != want {
			t.Errorf("CheckDial(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := CheckDial("neither"); err == nil || !strings.Contains(err.Error(), "want auto, self, peer or both") {
		t.Errorf("CheckDial of a word that is none of the four: %v", err)
	}
	with := base
	with.Dial = DialBoth
	if base.Equal(with) || !with.Equal(with.Clone()) {
		t.Error("Equal does not tell a rule with dial from one without")
	}
	// a rule without the option is written as it always was
	b, _ := json.Marshal(base)
	if strings.Contains(string(b), "dial") {
		t.Errorf("a rule that says nothing about dial is written with it: %s", b)
	}
}
