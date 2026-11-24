package node

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/engine"
)

func str(s string) *string { return &s }

// Every setting reads back from the defaults as a value it accepts, written the way it
// is stored: what is shown can be typed in again.
func TestSettingsReadTheDefaults(t *testing.T) {
	d := engine.DefaultConfig()
	seen := map[string]bool{}
	for _, s := range Settings() {
		if seen[s.Name] {
			t.Errorf("two settings are called %q", s.Name)
		}
		seen[s.Name] = true
		v := s.Value(d)
		stored, err := ParseSetting(s.Name, v)
		if err != nil || stored != v {
			t.Errorf("%s: the default reads as %q, which parses to %q, %v", s.Name, v, stored, err)
		}
		cfg := d
		Transport{s.Name: v}.Apply(&cfg)
		if !reflect.DeepEqual(cfg, d) {
			t.Errorf("%s: applying the default changed the settings", s.Name)
		}
		if s.Help == "" {
			t.Errorf("%s has no help text", s.Name)
		}
	}
	// the chunk size is not a setting: every node uses the engine's default
	if d.Chunk != transportChunk {
		t.Errorf("the engine's chunk is %d, the settings assume %d", d.Chunk, transportChunk)
	}
	if _, ok := SettingByName("chunk"); ok {
		t.Error("chunk is a setting, and both ends would have to agree on it")
	}
}

// Each setting takes the values in its range, stores them in one form, and refuses the
// rest with a message that names it.
func TestSettingValues(t *testing.T) {
	for _, c := range []struct {
		name, in, stored string
	}{
		{"window", "256MB", "256MB"}, {"window", "268435456", "256MB"}, {"window", "1gb", "1GB"},
		{"grace", "3600s", "1h0m0s"}, {"grace", "1ms", "1ms"},
		{"hol", "0.5s", "500ms"},
		{"target", "20ms", "20ms"}, {"target", "5s", "5s"},
		{"pigtarget", "30s", "30s"},
		{"dead", "5s", "5s"}, {"dead", "10m", "10m0s"},
		{"kickratio", "1.5", "1.5"}, {"kickratio", "100", "100"}, {"kickratio", "5.0", "5"},
		{"kickfloor", "1KB", "1KB"}, {"kickfloor", "262144", "256KB"}, {"kickfloor", "1GB", "1GB"},
		{"share", "false", "false"}, {"share", "TRUE", "true"},
		{"fbsubs", "1", "1"}, {"fbsubs", "16", "16"},
		{"fbevery", "5ms", "5ms"}, {"fbevery", "1s", "1s"},
		{"coalesce", "0", "0s"}, {"coalesce", "1s", "1s"},
		{"budget", "1MB", "1MB"}, {"budget", "1536KB", "1536KB"},
		{"timeline", "0", "0s"}, {"timeline", "2s", "2s"},
	} {
		if got, err := ParseSetting(c.name, c.in); err != nil || got != c.stored {
			t.Errorf("%s %q: stored as %q, %v, want %q", c.name, c.in, got, err, c.stored)
		}
	}
	for _, c := range [][2]string{
		{"window", "0"}, {"window", "512KB"}, {"window", "1500KB"}, {"window", "65536MB"}, {"window", "plenty"}, {"window", "-128MB"},
		{"grace", "0"}, {"grace", "-5s"}, {"grace", "long"},
		{"hol", "0"}, {"hol", "-1s"},
		{"target", "19ms"}, {"target", "6s"}, {"target", "0"},
		{"pigtarget", "10ms"}, {"pigtarget", "31s"},
		{"dead", "4s"}, {"dead", "11m"}, {"dead", "0"},
		{"kickratio", "1.4"}, {"kickratio", "101"}, {"kickratio", "NaN"}, {"kickratio", "fast"},
		{"kickfloor", "100"}, {"kickfloor", "2GB"},
		{"share", "yes"}, {"share", ""},
		{"fbsubs", "0"}, {"fbsubs", "17"}, {"fbsubs", "2.5"},
		{"fbevery", "4ms"}, {"fbevery", "2s"}, {"fbevery", "0"},
		{"coalesce", "-1ms"}, {"coalesce", "2s"},
		{"budget", "0"}, {"budget", "512KB"}, {"budget", "much"},
		{"timeline", "-2s"},
		{"chunk", "256KB"}, {"maxrss", "900"}, {"", "1"},
	} {
		if got, err := ParseSetting(c[0], c[1]); err == nil {
			t.Errorf("%s %q was taken, as %q", c[0], c[1], got)
		} else if c[0] != "" && !strings.Contains(err.Error(), c[0]) && !strings.Contains(err.Error(), c[1]) {
			t.Errorf("%s %q: the message %q names neither the setting nor the value", c[0], c[1], err)
		}
	}
}

// A rule's setting goes before a peer's, a peer's before the node's, and the node's
// before the default. A setting that the command line gave is not changed by the node's
// stored layer, and still gives way to a peer and a rule. A setting that all sessions
// share is taken from the node layer only.
func TestResolveTransportLayers(t *testing.T) {
	base := engine.DefaultConfig()
	base.Dead = 40 * time.Second // as if the command line said -dead 40s
	pinned := map[string]bool{"dead": true}
	nodeLayer := Transport{"window": "64MB", "grace": "10m0s", "target": "400ms", "dead": "50s", "budget": "512MB", "timeline": "2s"}
	peerLayer := Transport{"window": "32MB", "grace": "5m0s", "share": "false"}
	ruleLayer := Transport{"window": "16MB", "fbsubs": "5"}
	for _, l := range []struct {
		t    Transport
		node bool
	}{{nodeLayer, true}, {peerLayer, false}, {ruleLayer, false}} {
		if err := l.t.Check(l.node); err != nil {
			t.Fatal(err)
		}
	}

	got := ResolveTransport(base, pinned, nodeLayer, peerLayer, ruleLayer)
	want := base
	want.Window = 16 << 20               // rule
	want.FbSubs = 5                      // rule
	want.Grace = 5 * time.Minute         // peer
	want.Share = false                   // peer
	want.Target = 400 * time.Millisecond // node
	want.Budget = 512 << 20              // node
	want.Timeline = 2 * time.Second      // node
	want.Dead = 40 * time.Second         // the command line, not the node's stored 50s
	if !reflect.DeepEqual(got, want) {
		t.Errorf("all three layers:\n got %+v\nwant %+v", got, want)
	}

	// without the rule the peer decides, without the peer the node, without all the base
	if got := ResolveTransport(base, pinned, nodeLayer, peerLayer, nil); got.Window != 32<<20 || got.FbSubs != base.FbSubs {
		t.Errorf("node and peer: window %d, fbsubs %d", got.Window, got.FbSubs)
	}
	if got := ResolveTransport(base, pinned, nodeLayer, nil, nil); got.Window != 64<<20 || got.Grace != 10*time.Minute || !got.Share {
		t.Errorf("node only: window %d, grace %v, share %v", got.Window, got.Grace, got.Share)
	}
	if got := ResolveTransport(base, pinned, nil, nil, nil); !reflect.DeepEqual(got, base) {
		t.Errorf("no layer: %+v", got)
	}
	// a peer or a rule goes before a setting of the command line too
	if got := ResolveTransport(base, pinned, nodeLayer, Transport{"dead": "25s"}, nil); got.Dead != 25*time.Second {
		t.Errorf("a peer's dead under a command line that gives it: %v", got.Dead)
	}
	if got := ResolveTransport(base, pinned, nodeLayer, Transport{"dead": "25s"}, Transport{"dead": "15s"}); got.Dead != 15*time.Second {
		t.Errorf("a rule's dead: %v", got.Dead)
	}
	// a layer that should not hold a shared setting does not apply one
	if got := ResolveTransport(base, nil, nil, Transport{"budget": "1GB"}, Transport{"timeline": "1s"}); got.Budget != base.Budget || got.Timeline != base.Timeline {
		t.Errorf("a shared setting came from a peer or a rule: budget %d, timeline %v", got.Budget, got.Timeline)
	}
}

// Merge sets and takes out by name, stores each value in one form, leaves the receiver
// as it was, and refuses what cannot be a setting of that layer.
func TestTransportMerge(t *testing.T) {
	old := Transport{"window": "64MB", "grace": "10m0s"}
	next, err := old.Merge(map[string]*string{"window": nil, "target": str("0.5s"), "kickfloor": str("131072")}, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Transport{"grace": "10m0s", "target": "500ms", "kickfloor": "128KB"}); !reflect.DeepEqual(next, want) {
		t.Errorf("merged: %v, want %v", next, want)
	}
	if len(old) != 2 || old["window"] != "64MB" {
		t.Errorf("the receiver changed: %v", old)
	}
	if got := next.String(); got != "grace=10m0s target=500ms kickfloor=128KB" {
		t.Errorf("shown as %q", got)
	}
	// taking out the last one leaves no layer at all
	if next, err = (Transport{"window": "64MB"}).Merge(map[string]*string{"window": nil}, false); err != nil || next != nil {
		t.Errorf("after the last setting is taken out: %v, %v", next, err)
	}
	for _, change := range []map[string]*string{
		{"budget": str("512MB")}, // shared by the node
		{"timeline": str("1s")},  //
		{"chunk": str("128KB")},  // not a setting
		{"target": str("1ms")},   // out of range
		{"window": str("lots")},  // not a size
	} {
		if got, err := old.Merge(change, false); err == nil {
			t.Errorf("%v was taken for a peer or a rule: %v", change, got)
		}
	}
	if next, err = old.Merge(map[string]*string{"budget": str("512MB"), "timeline": str("1s")}, true); err != nil || next["budget"] != "512MB" || next["timeline"] != "1s" {
		t.Errorf("the node layer: %v, %v", next, err)
	}
	// a stored layer is written the way Merge writes it
	if err := (Transport{"target": "0.5s"}).Check(false); err == nil {
		t.Error("a value in another form than the stored one passed the check")
	}
	if err := (Transport{"budget": "512MB"}).Check(false); err == nil {
		t.Error("a shared setting passed the check of a peer or a rule")
	}
}

// The three layers are stored in the state file and read back; a node, a peer and a
// rule without settings are written exactly as before, so that a version from before
// 0.7.0 reads the file.
func TestTransportInTheStateFile(t *testing.T) {
	dir := t.TempDir()
	n, err := Open(dir, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	peer := Peer{Name: "beta", Fingerprint: strings.Repeat("ab", 32), PairSecret: make([]byte, PairSecretLen), PairedAt: time.Now()}
	if _, err := n.PutPeer(peer); err != nil {
		t.Fatal(err)
	}
	rule := Rule{Name: "r", Listen: "127.0.0.1:5000", Destination: "192.0.2.1:873", Peer: "beta", Enabled: true}
	if err := n.AddRule(rule); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "state.json")
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(before), "transport") {
		t.Fatalf("a state without transport settings names them:\n%s", before)
	}

	if _, err := n.SetTransport(map[string]*string{"window": str("64MB"), "budget": str("300MB")}, nil); err != nil {
		t.Fatal(err)
	}
	stored, err := n.UpdatePeer("beta", nil, nil, nil, map[string]*string{"grace": str("300s")})
	if err != nil || stored.Transport["grace"] != "5m0s" {
		t.Fatalf("the peer: %v, %v", stored.Transport, err)
	}
	rule.Transport = Transport{"target": "500ms"}
	if err := n.ReplaceRule("r", rule); err != nil {
		t.Fatal(err)
	}
	// what cannot be stored changes nothing and is the caller's doing
	if _, err := n.UpdatePeer("beta", nil, nil, nil, map[string]*string{"budget": str("1GB")}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a shared setting for a peer: %v", err)
	}
	if _, err := n.SetTransport(map[string]*string{"dead": str("1s")}, nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("a value out of range for the node: %v", err)
	}
	refuse := errors.New("does not fit")
	if _, err := n.SetTransport(map[string]*string{"window": str("32MB")}, func(Transport) error { return refuse }); !errors.Is(err, ErrInvalid) || !errors.Is(err, refuse) {
		t.Errorf("settings the caller says do not fit: %v", err)
	}
	bad := rule
	bad.Transport = Transport{"timeline": "1s"}
	if err := n.ReplaceRule("r", bad); !errors.Is(err, ErrInvalid) {
		t.Errorf("a shared setting for a rule: %v", err)
	}
	n.Close()

	n, err = Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	if got := n.Transport(); !reflect.DeepEqual(got, Transport{"window": "64MB", "budget": "300MB"}) {
		t.Errorf("the node's settings after a restart: %v", got)
	}
	if p, _ := n.Peer("beta"); !reflect.DeepEqual(p.Transport, Transport{"grace": "5m0s"}) {
		t.Errorf("the peer's settings after a restart: %v", p.Transport)
	}
	if got := n.RuleTransport("r"); !reflect.DeepEqual(got, Transport{"target": "500ms"}) {
		t.Errorf("the rule's settings after a restart: %v", got)
	}
	// taken out again, the file is what it was before any setting
	if _, err := n.SetTransport(map[string]*string{"window": nil, "budget": nil}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := n.UpdatePeer("beta", nil, nil, nil, map[string]*string{"grace": nil}); err != nil {
		t.Fatal(err)
	}
	if err := n.ReplaceRule("r", rule.Clone()); err != nil {
		t.Fatal(err)
	}
	rule.Transport = nil
	if err := n.ReplaceRule("r", rule); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("with every setting taken out the file differs from the one before:\n%s\n---\n%s", before, after)
	}
}

// A state file with a setting that is none, or a value out of range, is refused and
// left as it is.
func TestStateFileWithBadTransport(t *testing.T) {
	for _, node := range []string{
		`{"name":"alpha","transport":{"chunk":"128KB"}}`,
		`{"name":"alpha","transport":{"dead":"1s"}}`,
		`{"name":"alpha","transport":{"target":"0.5s"}}`,
	} {
		path := filepath.Join(t.TempDir(), "state.json")
		content := `{"schema":3,"node":` + node + `,"peers":[],"pairing_codes":[],"rules":[]}`
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadState(path); err == nil {
			t.Errorf("%s was read", node)
		}
	}
}
