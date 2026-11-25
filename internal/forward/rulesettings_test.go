package forward

import (
	"strings"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/control"
	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/node"
)

// knowsSet reports whether tn holds a set of settings from the peer called peer in
// which the setting name has the value.
func knowsSet(tn *testNode, peer, name, value string) bool {
	tn.rt.mu.Lock()
	defer tn.rt.mu.Unlock()
	for _, t := range tn.rt.inSets[peer] {
		if t[name] == value {
			return true
		}
	}
	return false
}

// onlySession waits until tn has one session that the peer has answered and returns it.
func onlySession(t *testing.T, tn *testNode) engine.SessionInfo {
	t.Helper()
	var si engine.SessionInfo
	eventually(t, tn.n.Name()+" has one session that runs", func() bool {
		list := tn.e.SessionList()
		if len(list) != 1 || !list[0].Up {
			return false
		}
		si = list[0]
		return true
	})
	return si
}

// A rule's transport settings that each session has for itself act at the peer too:
// the session at the peer takes them in the place of what the peer keeps for the rule's
// node. The window does not travel. A change of the rule reaches the next session at
// the peer. Both when the node with the rule opens the sub-connections (forward) and
// when the peer does (reverse).
func TestRuleSettingsActAtThePeer(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		maker, joiner := startNode(t, "alpha"), startNode(t, "beta")
		pairNodes(t, maker, joiner, 2)
		helloed(t, maker, joiner)
		// forward: the rule is on beta, which holds the path; reverse: on alpha, which
		// holds none, so beta opens the sub-connections
		own, peer := joiner, maker
		if reverse {
			own, peer = maker, joiner
		}
		dst, got := sink(t)
		in := freePorts(t, 1)
		rule := node.Rule{Name: "r", Listen: loop(in), Destination: loop(dst), Peer: peer.n.Name(), Enabled: true,
			Transport: node.Transport{"coalesce": "0s", "hol": "700ms", "window": "16MB"}}
		if err := own.rt.AddRule(rule); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the peer holds the rule's settings", func() bool { return knowsSet(peer, own.n.Name(), "hol", "700ms") })
		h := hold(t, loop(in), 1<<20)
		sp, so := onlySession(t, peer), onlySession(t, own)
		if sp.Config.Coalesce != 0 || sp.Config.Hol != 700*time.Millisecond {
			t.Errorf("reverse %v: the session at the peer has coalesce %v and hol %v, want 0s and 700ms from the rule", reverse, sp.Config.Coalesce, sp.Config.Hol)
		}
		if sp.Config.Window == 16<<20 {
			t.Errorf("reverse %v: the window of the rule reached the peer", reverse)
		}
		if so.Config.Window != 16<<20 || so.Config.Hol != 700*time.Millisecond {
			t.Errorf("reverse %v: the session at the node with the rule has window %d and hol %v, want the rule's", reverse, so.Config.Window, so.Config.Hol)
		}
		if err := h.finish(); err != nil {
			t.Fatal(err)
		}
		if arr := wait(t, got); arr.n != 1<<20 || arr.sum != h.hash || arr.err != nil {
			t.Fatalf("reverse %v: %d bytes arrived (%v), or they differ from what was sent", reverse, arr.n, arr.err)
		}
		eventually(t, "the session has ended on both nodes", func() bool { return len(own.e.SessionList())+len(peer.e.SessionList()) == 0 })

		// the rule changes: the next session at the peer takes the new value
		rule.Transport = node.Transport{"hol": "300ms"}
		if err := own.rt.ReplaceRule("r", rule); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the peer holds the changed settings", func() bool { return knowsSet(peer, own.n.Name(), "hol", "300ms") })
		h = hold(t, loop(in), 1<<20)
		if sp := onlySession(t, peer); sp.Config.Hol != 300*time.Millisecond || sp.Config.Coalesce != engine.DefaultConfig().Coalesce {
			t.Errorf("reverse %v: after the change the session at the peer has hol %v and coalesce %v, want 300ms and the default", reverse, sp.Config.Hol, sp.Config.Coalesce)
		}
		if err := h.finish(); err != nil {
			t.Fatal(err)
		}
		wait(t, got)
		maker.stop()
		joiner.stop()
	}
}

// A setting that the peer's command line gives stays as it is there. A value that the
// peer's own rules for the setting do not take, and a setting that does not travel,
// are left out and logged.
func TestRuleSettingsThePeerDoesNotTake(t *testing.T) {
	pin := func(rt *Runtime) { rt.Pinned = map[string]bool{"hol": true} }
	maker, joiner := startNode(t, "alpha", pin), startNode(t, "beta")
	pairNodes(t, maker, joiner, 2)
	helloed(t, maker, joiner)
	dst, got := sink(t)
	in := freePorts(t, 1)
	if err := joiner.rt.AddRule(node.Rule{Name: "r", Listen: loop(in), Destination: loop(dst), Peer: "alpha", Enabled: true,
		Transport: node.Transport{"hol": "700ms", "coalesce": "0s"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the peer holds the rule's settings", func() bool { return knowsSet(maker, "beta", "coalesce", "0s") })
	h := hold(t, loop(in), 1<<20)
	if sp := onlySession(t, maker); sp.Config.Hol != engine.DefaultConfig().Hol || sp.Config.Coalesce != 0 {
		t.Errorf("the session at the peer has hol %v and coalesce %v, want the hol of its command line and coalesce 0s", sp.Config.Hol, sp.Config.Coalesce)
	}
	if err := h.finish(); err != nil {
		t.Fatal(err)
	}
	wait(t, got)

	// what a peer may send and this node does not take
	maker.rt.setsNamed("beta", control.Message{Type: control.TypeRuleSettings, Sets: []control.RuleSet{
		{ID: 7, Settings: map[string]string{"hol": "-1s", "window": "16MB", "coalesce": "5ms"}},
		{ID: 0, Settings: map[string]string{"hol": "500ms"}},
	}})
	maker.rt.mu.Lock()
	sets := maker.rt.inSets["beta"]
	maker.rt.mu.Unlock()
	if len(sets) != 1 || len(sets[7]) != 1 || sets[7]["coalesce"] != "5ms" {
		t.Errorf("of a set with a bad hol, a window and a good coalesce, and a set without a number, this node keeps %v, want only coalesce=5ms under 7", sets)
	}
	if logs := maker.logs.String(); !strings.Contains(logs, "these are not taken: hol=-1s, window") {
		t.Errorf("the log does not say what was not taken:\n%s", logs)
	}
	// more sets than a node keeps
	many := make([]control.RuleSet, control.MaxRuleSets+10)
	for i := range many {
		many[i] = control.RuleSet{ID: uint64(i + 1), Settings: map[string]string{"hol": "500ms"}}
	}
	maker.rt.setsNamed("beta", control.Message{Type: control.TypeRuleSettings, Sets: many})
	maker.rt.mu.Lock()
	n := len(maker.rt.inSets["beta"])
	maker.rt.mu.Unlock()
	if n != control.MaxRuleSets {
		t.Errorf("of %d sets this node keeps %d, want %d", len(many), n, control.MaxRuleSets)
	}
}

// A peer that does not take the settings of rules (a version before 0.12.0) gets
// sessions with its own settings, and the rule says that its settings act at its own
// node only.
func TestRuleSettingsWithAPeerThatDoesNotTakeThem(t *testing.T) {
	maker, joiner := startNode(t, "alpha", func(rt *Runtime) { rt.noRuleSettings = true }), startNode(t, "beta")
	pairNodes(t, maker, joiner, 2)
	helloed(t, maker, joiner)
	dst, got := sink(t)
	in := freePorts(t, 1)
	if err := joiner.rt.AddRule(node.Rule{Name: "r", Listen: loop(in), Destination: loop(dst), Peer: "alpha", Enabled: true,
		Transport: node.Transport{"coalesce": "0s"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the rule says that its settings act here only", func() bool { return statusOf(t, joiner, "r").SettingsHereOnly })
	h := hold(t, loop(in), 1<<20)
	if sp := onlySession(t, maker); sp.Config.Coalesce != engine.DefaultConfig().Coalesce {
		t.Errorf("the session at a peer that does not take the rule's settings has coalesce %v", sp.Config.Coalesce)
	}
	if err := h.finish(); err != nil {
		t.Fatal(err)
	}
	wait(t, got)
	maker.rt.mu.Lock()
	n := len(maker.rt.inSets)
	maker.rt.mu.Unlock()
	if n != 0 {
		t.Errorf("a node that does not take the settings of rules was sent some")
	}
	// a rule without settings that travel says nothing of the kind
	if err := joiner.rt.ReplaceRule("r", node.Rule{Name: "r", Listen: loop(in), Destination: loop(dst), Peer: "alpha", Enabled: true,
		Transport: node.Transport{"window": "16MB"}}); err != nil {
		t.Fatal(err)
	}
	if statusOf(t, joiner, "r").SettingsHereOnly {
		t.Error("a rule whose only setting is the window says that its settings act here only")
	}
}

// A session that the peer opens and that takes the rule's settings there does not
// begin on a spare connection, which the peer opened with its own settings; a session
// of a rule whose settings do not travel still does.
func TestRuleSettingsAndSpares(t *testing.T) {
	a, b := sparePair(t, 2, 2)
	eventually(t, "the spares are kept", func() bool { return sparesOf(a, "beta") == 2 && sparesOf(b, "alpha") == 2 })
	dst, got := sink(t)
	in := freePorts(t, 1)
	if err := a.rt.AddRule(node.Rule{Name: "r", Listen: loop(in), Destination: loop(dst), Peer: "beta", Enabled: true,
		Transport: node.Transport{"coalesce": "0s"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the peer holds the rule's settings", func() bool { return knowsSet(b, "alpha", "coalesce", "0s") })
	h := hold(t, loop(in), 1<<20)
	if sp := onlySession(t, b); sp.Config.Coalesce != 0 {
		t.Errorf("the session at the peer has coalesce %v, want 0s from the rule", sp.Config.Coalesce)
	}
	if n := used(a); n != 0 {
		t.Errorf("%d sessions of a rule whose settings the peer takes began on a spare", n)
	}
	if err := h.finish(); err != nil {
		t.Fatal(err)
	}
	wait(t, got)
}
