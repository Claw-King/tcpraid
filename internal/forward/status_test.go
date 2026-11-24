package forward

import (
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/control"
	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/node"
)

// failingRule makes the two nodes of a rule whose connections the exit end cannot
// connect: with reverse, the node with the rule has no path and its peer dials. It
// returns the node with the rule, the exit node, and the rule's listening port and
// destination port (nothing listens on the destination).
func failingRule(t *testing.T, reverse bool) (entry, exit *testNode, in, dst int) {
	t.Helper()
	a, b := startNode(t, "alpha"), startNode(t, "beta")
	pairNodes(t, a, b, 2) // beta has the path
	entry, exit = b, a
	if reverse {
		entry, exit = a, b
	}
	in, dst = freePorts(t, 1), freePorts(t, 1)
	rule := node.Rule{Name: "r", Listen: loop(in), Destination: loop(dst), Peer: exit.n.Name(), Enabled: true}
	if err := entry.rt.AddRule(rule); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the control connection is up", func() bool {
		return entry.rt.PeerStatus(exit.n.Name()).Online && exit.rt.PeerStatus(entry.n.Name()).Online
	})
	return entry, exit, in, dst
}

// The node with the rule says why its peer could not connect a session to the
// destination: nothing listens there. With a path of its own and with a peer that dials.
func TestRuleSaysWhyThePeerCouldNotConnect(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%v", reverse), func(t *testing.T) {
			entry, exit, in, dst := failingRule(t, reverse)
			before := time.Now()
			if _, err := send(t, loop(in), 5000); err == nil {
				t.Fatal("a connection to a destination that refuses ended cleanly")
			}
			// the reason after the colon is the system's own, and each system has
			// its words for a refused connection: all of them say "refused"
			want := fmt.Sprintf("peer %q could not connect to %s: ", exit.n.Name(), loop(dst))
			saysWhy := func(s string) bool {
				return strings.HasPrefix(s, want) && strings.Contains(s[len(want):], "refused")
			}
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("the rule says %q\nentry:\n%s\nexit:\n%s", statusOf(t, entry, "r").Error, entry.logs, exit.logs)
				}
			})
			eventually(t, "the rule says why", func() bool { return saysWhy(statusOf(t, entry, "r").Error) })
			st := statusOf(t, entry, "r")
			if st.ErrorAt.Before(before) || st.ErrorAt.After(time.Now()) {
				t.Errorf("the failure is dated %v, want between %v and now", st.ErrorAt, before)
			}
			if !st.Listening {
				t.Error("the rule stopped listening")
			}
			if !strings.Contains(entry.logs.String(), want) {
				t.Errorf("the log of the node with the rule does not say it:\n%s", entry.logs)
			}
		})
	}
}

// ... and when the peer's allow list for this node does not hold the destination.
func TestRuleSaysTheAllowListRefused(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%v", reverse), func(t *testing.T) {
			entry, exit, in, dst := failingRule(t, reverse)
			allow := "192.0.2.0/24:873"
			if _, err := exit.svc.SetPeerOptions(entry.n.Name(), &allow, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := send(t, loop(in), 5000); err == nil {
				t.Fatal("a connection to a destination outside the allow list ended cleanly")
			}
			want := fmt.Sprintf("peer %q refused the destination %s, which is not in the allow list it keeps for this node. Change the list on that node with \"tcpraid peers set\".", exit.n.Name(), loop(dst))
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("the rule says %q\nentry:\n%s\nexit:\n%s", statusOf(t, entry, "r").Error, entry.logs, exit.logs)
				}
			})
			eventually(t, "the rule says why", func() bool { return statusOf(t, entry, "r").Error == want })
		})
	}
}

// A connection that works clears what the rule said about the one before.
func TestGoodConnectionClearsTheReason(t *testing.T) {
	entry, _, in, dst := failingRule(t, false)
	if _, err := send(t, loop(in), 5000); err == nil {
		t.Fatal("a connection to a destination that refuses ended cleanly")
	}
	eventually(t, "the rule says why", func() bool { return statusOf(t, entry, "r").Error != "" })
	got := sinks(t, dst, 1)
	if _, err := send(t, loop(in), 5000); err != nil {
		t.Fatal(err)
	}
	wait(t, got)
	if st := statusOf(t, entry, "r"); st.Error != "" || !st.ErrorAt.IsZero() {
		t.Fatalf("after a good connection the rule still says %q (%v)", st.Error, st.ErrorAt)
	}
}

// The peer does not accept this node any more (the pairing was ended at its end): while
// a connection of the rule waits for its first sub-connection, the rule says what the
// attempts to open one run into.
func TestRuleSaysThePeerDoesNotAcceptIt(t *testing.T) {
	entry, exit, in, _ := failingRule(t, false)
	if err := exit.svc.RemovePeer(entry.n.Name()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := send(t, loop(in), 5000)
		done <- err
	}()
	// What the listener says before it closes the connection reaches this end on some
	// systems and is lost on others, where only the reset arrives. The rule names the
	// refusal in the first case and its possible causes in the second.
	said := `peer "alpha" does not answer on the connections this node opens: `
	refused := said + `the other node does not accept this node, which is not paired with it or no longer is`
	closed := said + `the other end closed the connection without answering. Either the address is not a tcpraid listener, or the other node is not paired with this one, or their pair secrets differ.`
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the rule says %q", statusOf(t, entry, "r").Error)
		}
	})
	eventually(t, "the rule says that the peer does not accept the connections", func() bool {
		got := statusOf(t, entry, "r").Error
		return got == refused || got == closed
	})
	// a state that holds now, not something that happened at a time
	if at := statusOf(t, entry, "r").ErrorAt; !at.IsZero() {
		t.Errorf("the state is dated %v", at)
	}
	// the session itself ends only after the transport's 30 seconds: not waited for here
	entry.stop()
	<-done
}

// The reason the peer gives and the end of the session arrive in either order, and the
// rule says the same in the end.
func TestPeerReasonAndSessionEndInEitherOrder(t *testing.T) {
	a := startNode(t, "alpha")
	id := [16]byte{1, 2, 3}
	end := engine.End{ID: id, Why: engine.WhyUnreachable + "192.0.2.9:22", Up: true}
	said := control.Message{Type: control.TypeSessionFail, Session: hex.EncodeToString(id[:]), Code: control.CodeDial, Reason: "dial tcp 192.0.2.9:22: connect: no route to host"}
	want := `peer "beta" could not connect to 192.0.2.9:22: connect: no route to host`
	a.rt.mu.Lock()
	a.rt.problems["first"], a.rt.problems["second"] = problem{}, problem{}
	a.rt.mu.Unlock()
	status := func(rule string) string {
		a.rt.mu.Lock()
		defer a.rt.mu.Unlock()
		return a.rt.problems[rule].text
	}

	// the reason first
	a.rt.sessionFailed("beta", said)
	a.rt.ended(a.rt.begin("first"), "beta", "192.0.2.9:22", end)
	if got := status("first"); got != want {
		t.Errorf("reason, then end:\n got %s\nwant %s", got, want)
	}
	// the end first: the rule says what it knows, then what the peer says
	at := a.rt.begin("second")
	a.rt.ended(at, "beta", "192.0.2.9:22", end)
	if got := status("second"); got != `peer "beta" could not connect to 192.0.2.9:22. Its log says why.` {
		t.Errorf("end without a reason: %s", got)
	}
	a.rt.sessionFailed("beta", said)
	if got := status("second"); got != want {
		t.Errorf("end, then reason:\n got %s\nwant %s", got, want)
	}
	// a sub-connection that came late found the session gone before this end heard that
	// the peer could not connect it: nothing to say until the peer says why
	other := [16]byte{7}
	a.rt.mu.Lock()
	a.rt.problems["third"] = problem{}
	a.rt.mu.Unlock()
	a.rt.ended(a.rt.begin("third"), "beta", "192.0.2.9:22", engine.End{ID: other, Why: engine.WhyNoSession, Up: true})
	if got := status("third"); got != "" {
		t.Errorf("a session the peer no longer holds, before the peer said why: %s", got)
	}
	said.Session = hex.EncodeToString(other[:])
	a.rt.sessionFailed("beta", said)
	if got := status("third"); got != want {
		t.Errorf("a session the peer no longer holds, after the peer said why:\n got %s\nwant %s", got, want)
	}
	said.Session = hex.EncodeToString(id[:])
	// another peer's word about the session counts for nothing
	a.rt.ended(a.rt.begin("first"), "beta", "192.0.2.9:22", end)
	a.rt.sessionFailed("gamma", said)
	if got := status("first"); strings.Contains(got, "no route") {
		t.Errorf("the word of a peer that does not hold the session was taken: %s", got)
	}
	a.rt.mu.Lock()
	kept := len(a.rt.details) + len(a.rt.recent)
	a.rt.mu.Unlock()
	if kept != 2 {
		t.Errorf("%d entries wait to be matched, want 2 (gamma's word, and beta's session that ended)", kept)
	}
}

// What a peer writes is shown cut to size and without anything that could break a line
// or hide; a message that names no session is dropped.
func TestPeerReasonIsMadePrintable(t *testing.T) {
	a := startNode(t, "alpha")
	id := [16]byte{9}
	at := a.rt.begin("r")
	a.rt.ended(at, "beta", "192.0.2.9:22", engine.End{ID: id, Why: engine.WhyUnreachable + "192.0.2.9:22"})
	a.rt.sessionFailed("beta", control.Message{Session: "zz", Code: control.CodeDial, Reason: "x"})
	a.rt.sessionFailed("beta", control.Message{Session: hex.EncodeToString(id[:]), Code: "new\ncode", Reason: "line one\nline two\x1b[31m " + strings.Repeat("A", 1000)})
	a.rt.mu.Lock()
	got := a.rt.problems["r"].text
	a.rt.mu.Unlock()
	if strings.ContainsAny(got, "\n\x1b") || len(got) > 300 || !strings.Contains(got, "line one?line two?[31m AAAA") {
		t.Fatalf("shown as %q", got)
	}
}

// For one connection the first reason stays; a report about an older connection does
// not replace one about a newer; a new connection clears what was said.
func TestRuleKeepsTheRightReason(t *testing.T) {
	a := startNode(t, "alpha")
	status := func() (string, time.Time) {
		a.rt.mu.Lock()
		defer a.rt.mu.Unlock()
		p := a.rt.problems["r"]
		return p.text, p.at
	}
	older, newer := a.rt.begin("r"), a.rt.begin("r")
	newer.fail("the newer connection failed")
	older.fail("the older connection failed late")
	if got, _ := status(); got != "the newer connection failed" {
		t.Errorf("after a late report about an older connection: %q", got)
	}
	newer.fail("a consequence of the first failure")
	got, at := status()
	if got != "the newer connection failed" {
		t.Errorf("after a second reason for the same connection: %q", got)
	}
	if time.Since(at) > time.Minute || at.IsZero() {
		t.Errorf("dated %v", at)
	}
	// the older one's failure is in the log all the same
	if !strings.Contains(a.logs.String(), "the older connection failed late") {
		t.Error("the late report is not in the log")
	}
	third := a.rt.begin("r")
	if got, _ := status(); got != "" {
		t.Errorf("a new connection left %q standing", got)
	}
	// a long connection that began before a short one, and fails after the short one
	// has come and gone, is the last failure there is
	older2 := third
	a.rt.begin("r")
	older2.fail("the long transfer was cut")
	if got, _ := status(); got != "the long transfer was cut" {
		t.Errorf("an older connection that fails when nothing newer has: %q", got)
	}
	// something that is not about a connection that just arrived counts as the newest
	a.rt.note("r", "the peer was restarted")
	a.rt.begin("r").fail("x")
	if got, _ := status(); got != "x" {
		t.Errorf("a connection after a note: %q", got)
	}
}
