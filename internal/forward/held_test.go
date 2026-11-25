package forward

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/control"
	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/node"
)

// rare makes the two ends of a control connection say hello and then nothing for a
// long while: what a node hears from its peer after that is not a ping.
func rare(rt *Runtime) {
	rt.times = times{ping: 30 * time.Second, timeout: 90 * time.Second, wait: 300 * time.Millisecond}
}

// windowScene is two paired nodes, alpha and beta (beta holds the path, and the rule
// is on beta), with one application connection that beta carries to a destination
// next to alpha. The session began after alpha last heard from beta's process: alpha
// tells beta when it has connected a session, and beta says nothing after its hello.
type windowScene struct {
	a, b *testNode
	c    net.Conn
	got  chan arrival
	in   int
}

func newWindowScene(t *testing.T) *windowScene {
	t.Helper()
	sc := &windowScene{a: startNode(t, "alpha", rare), b: startNode(t, "beta", rare)}
	pairNodes(t, sc.a, sc.b, 2)
	helloed(t, sc.a, sc.b)
	in, out := freePorts(t, 1), freePorts(t, 1)
	sc.in = in
	sc.got = sinks(t, out, 1)
	if err := sc.b.rt.AddRule(node.Rule{Name: "r", Listen: loop(in), Destination: loop(out), Peer: "alpha", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", loop(in))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	sc.c = c
	c.Write([]byte("some bytes"))
	eventually(t, "the session is up on both nodes", func() bool {
		al, bl := sc.a.e.SessionList(), sc.b.e.SessionList()
		return len(al) == 1 && len(bl) == 1 && al[0].Subs == 2 && bl[0].Subs == 2
	})
	return sc
}

func (sc *windowScene) killBeta() {
	sc.b.crash()
	sc.a.dropAccepted()
}

// A session that began after a node last heard from its peer's process, and before
// that process was killed, is reset when the new process says that it does not hold
// it. Before version 0.11.0 such a session waited out the grace time.
func TestSessionOfTheLastSecondsIsResetAfterARestart(t *testing.T) {
	sc := newWindowScene(t)
	time.Sleep(time.Second) // the peer is killed a second after the session began
	sc.killBeta()
	time.Sleep(300 * time.Millisecond)
	if n := len(sc.a.e.SessionList()); n != 1 {
		t.Fatalf("with the peer away, alpha holds %d sessions, want the 1 it had", n)
	}
	startNodeIn(t, sc.b.dir, "beta", rare)
	eventually(t, "alpha resets the session that the new process does not hold", func() bool { return len(sc.a.e.SessionList()) == 0 })
	if arr := wait(t, sc.got); arr.err == nil {
		t.Fatalf("the destination's connection ended cleanly after %d bytes", arr.n)
	}
	if !strings.Contains(sc.a.logs.String(), "runs as a new process: 1 sessions") {
		t.Fatalf("alpha's log does not say why:\n%s", sc.a.logs)
	}
}

// The same question leaves the sessions alone that the new process began: a
// connection that the restarted node carries at once arrives whole, while the session
// of the old process is reset.
func TestSessionsOfTheNewProcessAreLeftAlone(t *testing.T) {
	for round := 1; round <= 20; round++ {
		sc := newWindowScene(t)
		sc.killBeta()
		b2 := startNodeIn(t, sc.b.dir, "beta", rare)
		// at once: the new session may reach alpha before the hello of the new process
		var sum [32]byte
		eventually(t, "the restarted node takes a connection of its rule", func() bool {
			var err error
			sum, err = send(t, loop(sc.in), 100<<10)
			return err == nil
		})
		whole, cut := 0, 0
		for whole == 0 || cut == 0 {
			switch arr := wait(t, sc.got); {
			case arr.err == nil && arr.n == 100<<10 && arr.sum == sum:
				whole++
			case arr.err != nil:
				cut++ // the old session, or a connection that came before the rule listened
			default:
				t.Fatalf("round %d: %d bytes arrived and a clean end, which is neither the new connection nor a cut-off one", round, arr.n)
			}
		}
		eventually(t, "no session is left", func() bool { return len(sc.a.e.SessionList())+len(b2.e.SessionList()) == 0 })
		b2.stop()
		sc.a.stop()
	}
}

// The answer to held counts only for the question that was asked, from the process
// that was asked, in time, and only for the sessions the question named.
func TestHeldAnswerIsChecked(t *testing.T) {
	sc := newWindowScene(t)
	id := sc.a.e.SessionList()[0].ID
	hexID := hex.EncodeToString(id[:])
	cc := sc.a.rt.liveControl("beta")
	if cc == nil {
		t.Fatal("alpha has no control connection with beta")
	}
	sc.a.rt.mu.Lock()
	instance := cc.instance
	sc.a.rt.mu.Unlock()
	ask := func(q *heldQuery) {
		sc.a.rt.mu.Lock()
		sc.a.rt.heldAsked["beta"] = q
		sc.a.rt.mu.Unlock()
	}
	alive := func(what string) {
		t.Helper()
		time.Sleep(200 * time.Millisecond)
		if n := len(sc.a.e.SessionList()); n != 1 {
			t.Fatalf("%s: alpha holds %d sessions, want the 1 it had", what, n)
		}
	}
	// no question was asked
	sc.a.rt.heldAnswered(cc, control.Message{Type: control.TypeHeldReply, Request: 7})
	alive("an answer without a question")
	// another request number
	ask(&heldQuery{request: 7, instance: instance, ids: map[string]bool{hexID: true}, at: time.Now()})
	sc.a.rt.heldAnswered(cc, control.Message{Type: control.TypeHeldReply, Request: 8})
	alive("an answer to another request")
	// from another process than the one that was asked
	ask(&heldQuery{request: 7, instance: "another", ids: map[string]bool{hexID: true}, at: time.Now()})
	sc.a.rt.heldAnswered(cc, control.Message{Type: control.TypeHeldReply, Request: 7})
	alive("an answer from another process")
	// too late
	ask(&heldQuery{request: 7, instance: instance, ids: map[string]bool{hexID: true}, at: time.Now().Add(-heldWait - time.Second)})
	sc.a.rt.heldAnswered(cc, control.Message{Type: control.TypeHeldReply, Request: 7})
	alive("an answer that came too late")
	// the question named another session: this one is not reset for being absent
	ask(&heldQuery{request: 7, instance: instance, ids: map[string]bool{strings.Repeat("ab", 16): true}, at: time.Now()})
	sc.a.rt.heldAnswered(cc, control.Message{Type: control.TypeHeldReply, Request: 7})
	alive("an answer to a question about another session")
	// the session is named as held, and as ended complete
	ask(&heldQuery{request: 7, instance: instance, ids: map[string]bool{hexID: true}, at: time.Now()})
	sc.a.rt.heldAnswered(cc, control.Message{Type: control.TypeHeldReply, Request: 7, Sessions: []string{strings.ToUpper(hexID)}})
	alive("an answer that names the session as held")
	ask(&heldQuery{request: 7, instance: instance, ids: map[string]bool{hexID: true}, at: time.Now()})
	sc.a.rt.heldAnswered(cc, control.Message{Type: control.TypeHeldReply, Request: 7, Done: []string{hexID}})
	alive("an answer that names the session as ended complete")
	// and the answer that counts
	ask(&heldQuery{request: 7, instance: instance, ids: map[string]bool{hexID: true}, at: time.Now()})
	sc.a.rt.heldAnswered(cc, control.Message{Type: control.TypeHeldReply, Request: 7})
	eventually(t, "the session that the peer does not hold is reset", func() bool { return len(sc.a.e.SessionList()) == 0 })
}

// A node answers held with the sessions it holds with the asking peer and the ones
// that ended complete at it, and with nothing for the others.
func TestHeldSaysWhatANodeHolds(t *testing.T) {
	sc := newWindowScene(t)
	live := sc.b.e.SessionList()[0].ID
	// a second connection that ends complete
	if _, err := send(t, loop(sc.in), 5000); err != nil {
		t.Fatal(err)
	}
	var ended [16]byte
	eventually(t, "beta remembers how the second session ended", func() bool {
		sc.b.rt.endMu.Lock()
		defer sc.b.rt.endMu.Unlock()
		for id, r := range sc.b.rt.ends {
			if r.clean && r.peer == "alpha" {
				ended = id
				return true
			}
		}
		return false
	})
	h := func(id [16]byte) string { return hex.EncodeToString(id[:]) }
	unknown := strings.Repeat("cd", 16)
	held, done := sc.b.rt.heldAnswer("alpha", []string{h(live), h(ended), unknown, "not an id", ""})
	if !slices.Equal(held, []string{h(live)}) || !slices.Equal(done, []string{h(ended)}) {
		t.Errorf("held %v, done %v, want the live session and the one that ended complete", held, done)
	}
	// the sessions are those with the asking peer only
	if held, done := sc.b.rt.heldAnswer("gamma", []string{h(live), h(ended)}); len(held)+len(done) != 0 {
		t.Errorf("for another peer: held %v, done %v, want none", held, done)
	}
	// more than MaxHeld are not looked at
	many := make([]string, control.MaxHeld, control.MaxHeld+1)
	for i := range many {
		many[i] = unknown
	}
	if held, _ := sc.b.rt.heldAnswer("alpha", append(many, h(live))); len(held) != 0 {
		t.Errorf("a session named after the first %d was answered", control.MaxHeld)
	}
}

// A node whose hello lists no version of the wire protocol that this node speaks gets
// no session: the rule and the peer's status say so, and the log says it once.
func TestNoProtocolVersionInCommon(t *testing.T) {
	a := startNode(t, "alpha")
	b := startNode(t, "beta", func(rt *Runtime) { rt.protocols = []int{2} })
	pairNodes(t, a, b, 2)
	helloed(t, a, b)
	dst, got := sink(t)
	// alpha has no path to beta: the rule on alpha would ask beta to open the connections
	in := freePorts(t, 1)
	if err := a.rt.AddRule(node.Rule{Name: "r", Listen: loop(in), Destination: loop(dst), Peer: "beta", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := send(t, loop(in), 5000); err == nil {
		t.Fatal("a connection was carried to a peer that lists no protocol version in common")
	}
	const says = "speaks no version of the wire protocol that this node speaks (the peer lists [2], this node [1])"
	eventually(t, "the rule says why", func() bool { return strings.Contains(statusOf(t, a, "r").Error, says) })
	if st := a.rt.PeerStatus("beta"); !strings.Contains(st.Problem, says) || !st.Online {
		t.Errorf("the peer's status: %+v", st)
	}
	// the log says once that nothing is carried, whatever else quotes the reason
	if n := strings.Count(a.logs.String(), "No connection is carried to or from this peer."); n != 1 {
		t.Errorf("the log says it %d times, want once:\n%s", n, a.logs)
	}
	// and beta, which holds the path, opens none for its own rule either
	in2 := freePorts(t, 1)
	if err := b.rt.AddRule(node.Rule{Name: "back", Listen: loop(in2), Destination: loop(dst), Peer: "alpha", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := send(t, loop(in2), 5000); err == nil {
		t.Fatal("beta carried a connection to a peer that lists no protocol version in common")
	}
	select {
	case arr := <-got:
		t.Errorf("a connection reached the destination: %+v", arr)
	default:
	}
	// two nodes of this version have one in common, and say what they can do
	c := startNode(t, "gamma")
	pairNodes(t, a, c, 2)
	helloed(t, a, c)
	if st := a.rt.PeerStatus("gamma"); st.Problem != "" || !slices.Contains(st.Features, control.FeatureHeld) || !slices.Contains(st.Features, control.FeatureSessionOK) {
		t.Errorf("the status of a peer of this version: %+v", st)
	}
}

// With a peer that tells when it has connected a session, an entry answers its client
// when that word comes and not when a time is over: the stream does not begin before
// the peer has said so.
func TestEntryWaitsForThePeersWord(t *testing.T) {
	rt := &Runtime{EntrySettle: 50 * time.Millisecond, EntryConfirm: 3 * time.Second}
	rt.ctl = map[string]*peerControl{"beta": {conns: []*ctlConn{{peer: "beta"}}, greeted: true, features: []string{control.FeatureSessionOK}}}
	rt.waiting = map[failKey]*openConn{}
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	began := make(chan string, 1)
	oc := rt.newOpenConn("beta", a, endReader{}, func(session string) error { began <- session; return nil })
	id := [16]byte{1, 2, 3}
	oc.SessionID(id)
	oc.SessionUp()
	select {
	case <-began:
		t.Fatal("the entry answered its client before the peer said that the session was connected")
	case <-time.After(time.Second):
	}
	// a word for another session, and one from another peer, change nothing
	rt.sessionOK("beta", control.Message{Session: strings.Repeat("00", 16)})
	rt.sessionOK("gamma", control.Message{Session: hex.EncodeToString(id[:])})
	select {
	case <-began:
		t.Fatal("the entry answered its client on a word that was not about its session")
	case <-time.After(300 * time.Millisecond):
	}
	rt.sessionOK("beta", control.Message{Session: strings.ToUpper(hex.EncodeToString(id[:]))})
	select {
	case session := <-began:
		if session != hex.EncodeToString(id[:]) {
			t.Errorf("the answer was given the session %q", session)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the entry did not answer its client after the peer's word")
	}
	// a client that ended its sending side has its end passed on now
	if _, err := oc.Read(make([]byte, 8)); err != io.EOF {
		t.Errorf("after the answer the read returned %v, want the end of the data", err)
	}
	oc.forget()
	if len(rt.waiting) != 0 {
		t.Errorf("%d entries still wait", len(rt.waiting))
	}

	// no word at all: the entry gives up, and an end that it read is no end
	oc = rt.newOpenConn("beta", a, endReader{}, func(string) error { return errors.New("no answer is written after the wait was given up") })
	oc.SessionID([16]byte{9})
	oc.SessionUp()
	res := make(chan error, 1)
	go func() {
		_, err := oc.Read(make([]byte, 8))
		res <- err
	}()
	select {
	case err := <-res:
		if err != io.ErrUnexpectedEOF {
			t.Errorf("the read of an entry that gave up returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the entry did not give the wait up")
	}
	if !oc.waited() || !oc.refused() {
		t.Error("the entry does not say that it gave the wait up")
	}
	oc.forget()

	// a peer that does not say the word: the time of before
	rt.ctl["beta"].features = nil
	oc = rt.newOpenConn("beta", a, endReader{}, func(session string) error { began <- session; return nil })
	oc.SessionUp()
	select {
	case <-began:
	case <-time.After(5 * time.Second):
		t.Fatal("with a peer that does not say the word, the entry did not answer after its own time")
	}
}

// How a session ended is kept for a while, and a caller can wait for it.
func TestSessionEndIsRemembered(t *testing.T) {
	sc := newWindowScene(t)
	id := sc.b.e.SessionList()[0].ID
	// the session is live: a short wait says that it has not ended
	ctx, cancel := context.WithTimeout(bg, 300*time.Millisecond)
	end, known := sc.b.rt.SessionEnd(ctx, id)
	cancel()
	if !known || end.Ended {
		t.Fatalf("a live session: %+v, known %v", end, known)
	}
	// a wait that began before the end returns with it
	res := make(chan SessionEnd, 1)
	go func() {
		end, _ := sc.b.rt.SessionEnd(bg, id)
		res <- end
	}()
	time.Sleep(200 * time.Millisecond)
	sc.c.(*net.TCPConn).CloseWrite()
	select {
	case end := <-res:
		if !end.Ended || !end.Complete {
			t.Errorf("a session that ended complete: %+v", end)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the wait did not return when the session ended")
	}
	wait(t, sc.got)
	// afterwards it is still known, on both nodes
	for _, tn := range []*testNode{sc.a, sc.b} {
		if end, known := tn.rt.SessionEnd(bg, id); !known || !end.Ended || !end.Complete {
			t.Errorf("after the end: %+v, known %v", end, known)
		}
	}
	// a session that was cut off
	c, err := net.Dial("tcp", loop(sc.in))
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("x"))
	var cut [16]byte
	eventually(t, "the second session is up", func() bool {
		l := sc.b.e.SessionList()
		if len(l) == 1 && l[0].Up {
			cut = l[0].ID
			return true
		}
		return false
	})
	c.(*net.TCPConn).SetLinger(0)
	c.Close()
	if end, known := sc.b.rt.SessionEnd(bg, cut); !known || !end.Ended || end.Complete || end.Reason == "" {
		t.Errorf("a session that was cut off: %+v, known %v", end, known)
	}
	// one that never was
	if _, known := sc.b.rt.SessionEnd(bg, [16]byte{7}); known {
		t.Error("a session that never was is known")
	}
	// the list is bounded
	for i := 0; i < 2*maxEnds; i++ {
		sc.b.rt.sessionEvent(engine.SessionEvent{Ended: true, ID: [16]byte{0xee, byte(i), byte(i >> 8)}})
	}
	sc.b.rt.endMu.Lock()
	n := len(sc.b.rt.ends)
	sc.b.rt.endMu.Unlock()
	if n > maxEnds {
		t.Errorf("the list holds %d ends, more than %d", n, maxEnds)
	}
}

// A session that the node neither holds nor remembers is unknown, also to one who asks
// with a context that is over already, as the route does for a request that waits 0
// seconds. (Found in review: the answer was "it has not ended", for ever.)
func TestSessionEndOfAnUnknownSessionWithASpentContext(t *testing.T) {
	b := startNode(t, "beta")
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if end, known := b.rt.SessionEnd(ctx, [16]byte{7}); known {
		t.Fatalf("a session that never was, asked with a context that is over: known, %+v", end)
	}
}

// Only an end that is clean and that the other node confirmed is complete.
func TestEndRecordSaysCompleteOnlyWhenConfirmed(t *testing.T) {
	for _, c := range []struct {
		r        endRecord
		complete bool
	}{
		{endRecord{clean: true, confirmed: true, why: "done"}, true},
		{endRecord{clean: true, why: "done"}, false},
		{endRecord{why: "peer aborted"}, false},
		{endRecord{confirmed: true, why: "peer aborted"}, false},
	} {
		end := c.r.end()
		if !end.Ended || end.Complete != c.complete || end.Reason == "" {
			t.Errorf("%+v: %+v, want complete %v and a reason", c.r, end, c.complete)
		}
	}
	if end := (endRecord{clean: true, why: "done"}).end(); end.Reason == "done" {
		t.Errorf("an end that was not confirmed gives the reason %q, which reads as complete", end.Reason)
	}
}
