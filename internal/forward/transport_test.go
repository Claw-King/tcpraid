package forward

import (
	"errors"
	"strings"
	"testing"

	"github.com/Claw-King/tcpraid/internal/node"
)

func text(s string) *string { return &s }

// valueOf returns the node's setting called name as Transport lists it.
func valueOf(t *testing.T, tn *testNode, name string) TransportValue {
	t.Helper()
	for _, v := range tn.rt.Transport() {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no setting called %q", name)
	return TransportValue{}
}

// A session whose sub-connections the peer opens takes the settings of its rule at the
// node that has the rule, and the settings of the peer at the node that opens the
// connections, whether it begins on a spare connection or with the request. A setting
// that changes while a spare connection waits reaches the session that begins on it. A
// session that runs keeps what it began with.
func TestSettingsOfSessionsThePeerOpens(t *testing.T) {
	for _, spare := range []int{2, 0} {
		a, b := sparePair(t, spare, spare)
		eventually(t, "the spares are kept", func() bool { return sparesOf(a, "beta") == spare && sparesOf(b, "alpha") == spare })
		dst, got := sink(t)
		in := freePorts(t, 1)
		rule := node.Rule{Name: "r", Listen: loop(in), Destination: loop(dst), Peer: "beta", Enabled: true, Transport: node.Transport{"window": "16MB"}}
		if err := a.rt.AddRule(rule); err != nil {
			t.Fatal(err)
		}
		// beta's setting for its sessions with alpha, made after the spares were opened
		if _, err := b.svc.UpdatePeer("alpha", nil, nil, nil, map[string]*string{"window": text("8MB")}); err != nil {
			t.Fatal(err)
		}
		first := hold(t, loop(in), 1<<20)
		if sa, sb := session(t, a, 2, 0), session(t, b, 2, 2); sa.Window != 16<<20 || sb.Window != 8<<20 {
			t.Errorf("%d spares: the session has a window of %d MB at alpha and %d MB at beta, want 16 and 8", spare, sa.Window>>20, sb.Window>>20)
		}
		if n := used(a); n != min(spare, 1) {
			t.Errorf("%d spares: %d sessions began on a spare", spare, n)
		}
		// everything changes while the session runs: the rule gives its window up, the
		// node layer of alpha gets one, beta's peer layer another
		rule.Transport = nil
		if err := a.rt.ReplaceRule("r", rule); err != nil {
			t.Fatal(err)
		}
		if _, err := a.rt.SetTransport(map[string]*string{"window": text("32MB")}); err != nil {
			t.Fatal(err)
		}
		if _, err := b.svc.UpdatePeer("alpha", nil, nil, nil, map[string]*string{"window": text("4MB")}); err != nil {
			t.Fatal(err)
		}
		if sa, sb := session(t, a, 2, 0), session(t, b, 2, 2); sa.Window != 16<<20 || sb.Window != 8<<20 {
			t.Errorf("%d spares: after the settings changed the running session has %d MB at alpha and %d MB at beta, want 16 and 8 still", spare, sa.Window>>20, sb.Window>>20)
		}
		if err := first.finish(); err != nil {
			t.Fatal(err)
		}
		if arr := wait(t, got); arr.n != 1<<20 || arr.sum != first.hash || arr.err != nil {
			t.Fatalf("%d bytes arrived (%v), or they differ from what was sent", arr.n, arr.err)
		}
		eventually(t, "the session has ended on both nodes", func() bool { return len(a.e.SessionList())+len(b.e.SessionList()) == 0 })
		eventually(t, "the spares are kept", func() bool { return sparesOf(a, "beta") == spare && sparesOf(b, "alpha") == spare })

		second := hold(t, loop(in), 1<<20)
		if sa, sb := session(t, a, 2, 0), session(t, b, 2, 2); sa.Window != 32<<20 || sb.Window != 4<<20 {
			t.Errorf("%d spares: the next session has a window of %d MB at alpha and %d MB at beta, want 32 and 4", spare, sa.Window>>20, sb.Window>>20)
		}
		if err := second.finish(); err != nil {
			t.Fatal(err)
		}
		wait(t, got)
		a.stop()
		b.stop()
	}
}

// The node's settings are listed with where each comes from. One that the command line
// gave cannot be set while the node runs, and a stored value of it can still be taken
// off. What cannot be a setting is refused and changes nothing.
func TestNodeSettingsAndTheCommandLine(t *testing.T) {
	a := startNode(t, "alpha", func(rt *Runtime) { rt.Pinned = map[string]bool{"grace": true} })
	if v := valueOf(t, a, "grace"); v.Source != SourceCommandLine || v.Value != "15m0s" || v.Stored != "" {
		t.Errorf("grace, which the command line gave: %+v", v)
	}
	if v := valueOf(t, a, "window"); v.Source != SourceDefault || v.Value != "128MB" || v.Default != "128MB" || v.Shared {
		t.Errorf("window: %+v", v)
	}
	if v := valueOf(t, a, "budget"); !v.Shared {
		t.Errorf("budget is not marked as shared by all sessions: %+v", v)
	}
	_, err := a.rt.SetTransport(map[string]*string{"window": text("64MB"), "grace": text("1m")})
	if !errors.Is(err, node.ErrInvalid) || !strings.Contains(err.Error(), "-grace 15m0s on its command line") {
		t.Errorf("setting what the command line gave: %v", err)
	}
	if v := valueOf(t, a, "window"); v.Value != "128MB" {
		t.Errorf("a refused change was stored in part: %+v", v)
	}
	for _, change := range []map[string]*string{
		{"window": text("100KB")}, {"chunk": text("128KB")}, {"maxrss": text("900")},
	} {
		if _, err := a.rt.SetTransport(change); !errors.Is(err, node.ErrInvalid) {
			t.Errorf("%v: %v, want an error of the caller", change, err)
		}
	}
	values, err := a.rt.SetTransport(map[string]*string{"window": text("64MB"), "timeline": text("2s"), "grace": nil})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != len(node.Settings()) {
		t.Errorf("%d settings came back, want all %d", len(values), len(node.Settings()))
	}
	if v := valueOf(t, a, "window"); v.Value != "64MB" || v.Source != SourceStored || v.Stored != "64MB" {
		t.Errorf("window after it was set: %+v", v)
	}
	if v := valueOf(t, a, "timeline"); v.Value != "2s" || v.Source != SourceStored {
		t.Errorf("timeline after it was set: %+v", v)
	}
	if !strings.Contains(a.logs.String(), "settings of the node changed (grace, timeline, window)") {
		t.Errorf("the change is not in the log:\n%s", a.logs)
	}
	if _, err := a.rt.SetTransport(map[string]*string{"window": nil, "timeline": nil}); err != nil {
		t.Fatal(err)
	}
	if v := valueOf(t, a, "window"); v.Value != "128MB" || v.Source != SourceDefault || v.Stored != "" {
		t.Errorf("window after it was taken off: %+v", v)
	}
}
