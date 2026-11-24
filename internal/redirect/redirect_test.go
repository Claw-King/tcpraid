package redirect

import (
	"strings"
	"testing"

	"github.com/Claw-King/tcpraid/internal/node"
)

func TestScript(t *testing.T) {
	rules := []node.Rule{
		{Name: "lan", Match: "192.0.2.0/24:873,[2001:db8::/32]:5000-5010", Peer: "b", Enabled: true},
		{Name: "all", Match: "198.51.100.7/32", Peer: "b", Enabled: true},
		{Name: "off", Match: "203.0.113.0/24", Peer: "b"},
		{Name: "fixed", Listen: ":5000", Destination: "192.0.2.1:5000", Peer: "b", Enabled: true},
		{Name: "socks", Socks: "127.0.0.1:1080", Peer: "b", Enabled: true},
	}
	got, err := Script(rules, 9800, Both, 1000)
	if err != nil {
		t.Fatal(err)
	}
	want := `table inet tcpraid
delete table inet tcpraid
table inet tcpraid {
	chain prerouting {
		type nat hook prerouting priority -110; policy accept;
		ip daddr 192.0.2.0/24 tcp dport 873 redirect to :9800
		ip6 daddr 2001:db8::/32 tcp dport 5000-5010 redirect to :9800
		ip daddr 198.51.100.7/32 meta l4proto tcp redirect to :9800
	}
	chain output {
		type nat hook output priority -110; policy accept;
		meta skuid 1000 return
		ip daddr 192.0.2.0/24 tcp dport 873 redirect to :9800
		ip6 daddr 2001:db8::/32 tcp dport 5000-5010 redirect to :9800
		ip daddr 198.51.100.7/32 meta l4proto tcp redirect to :9800
	}
}
`
	if got != want {
		t.Errorf("the script for both:\n%s\nwant:\n%s", got, want)
	}
	// a rule that is switched off and rules of other kinds give no line
	if strings.Contains(got, "203.0.113") || strings.Contains(got, "1080") {
		t.Error("a rule that is off, or one that is not transparent, is in the script")
	}
	fwd, _ := Script(rules, 9800, Forwarded, 0)
	if strings.Contains(fwd, "hook output") || !strings.Contains(fwd, "hook prerouting") {
		t.Errorf("forwarded:\n%s", fwd)
	}
	loc, _ := Script(rules, 9800, Local, 0)
	if strings.Contains(loc, "hook prerouting") || !strings.Contains(loc, "meta skuid 0 return") {
		t.Errorf("local:\n%s", loc)
	}
	// no transparent rule: the table is there, with empty chains
	none, err := Script(nil, 9800, Forwarded, 0)
	if err != nil || !strings.Contains(none, "chain prerouting") || strings.Contains(none, "redirect to") {
		t.Errorf("no rules: %v\n%s", err, none)
	}
	if _, err := Script(rules, 9800, "all", 0); err == nil {
		t.Error("a mode that is none of the three was taken")
	}
	if _, err := Script(rules, 0, Forwarded, 0); err == nil {
		t.Error("port 0 was taken")
	}
}

// With Print set nothing is run: the script goes to Print, once for each change.
// A match can hold ports that no connection has, which nft would refuse and with them
// the whole table. Such numbers are brought into the range of ports, and an entry that
// then holds none gets no line.
func TestScriptKeepsToPortsThatExist(t *testing.T) {
	rules := []node.Rule{
		{Name: "backwards", Match: "192.0.2.0/24:5-3", Peer: "b", Enabled: true},
		{Name: "wide", Match: "198.51.100.0/24:0-70000", Peer: "b", Enabled: true},
		{Name: "high", Match: "203.0.113.0/24:60000-70000", Peer: "b", Enabled: true},
	}
	got, err := Script(rules, 9800, Forwarded, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ip daddr 198.51.100.0/24 meta l4proto tcp redirect to :9800",
		"ip daddr 203.0.113.0/24 tcp dport 60000-65535 redirect to :9800",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the script lacks %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"192.0.2.0/24", "70000", "dport 0", "5-3"} {
		if strings.Contains(got, not) {
			t.Errorf("the script holds %q:\n%s", not, got)
		}
	}
}

func TestInstallerPrints(t *testing.T) {
	var printed []string
	in := &Installer{Nft: "/nonexistent/nft", Port: 9800, Mode: Forwarded, Print: func(s string) { printed = append(printed, s) }}
	rules := []node.Rule{{Name: "lan", Match: "192.0.2.0/24", Peer: "b", Enabled: true}}
	for i := 0; i < 2; i++ {
		if err := in.Apply(func() []node.Rule { return rules }); err != nil {
			t.Fatal(err)
		}
	}
	if len(printed) != 1 || !strings.Contains(printed[0], "192.0.2.0/24") {
		t.Fatalf("the same rules twice were printed %d times", len(printed))
	}
	if err := in.Apply(func() []node.Rule { return nil }); err != nil || len(printed) != 2 {
		t.Fatalf("a change: %v, printed %d times", err, len(printed))
	}
	if err := in.Remove(); err != nil {
		t.Fatal(err)
	}
	// without Print and without the program: said plainly
	in = &Installer{Nft: "/nonexistent/nft", Port: 9800, Mode: Forwarded}
	if err := in.Apply(func() []node.Rule { return rules }); err == nil || !strings.Contains(err.Error(), "the program nft was not found") {
		t.Errorf("no nft: %v", err)
	}
}
