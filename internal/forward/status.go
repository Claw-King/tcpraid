package forward

// What a rule's status says about the last connection it could not carry.
//
// Each connection of a rule gets a number when it arrives (begin). What goes wrong with
// it is recorded once: the first reason is the cause, and what follows from it (a
// timer that runs out later, the reset) does not replace it. A report about an older
// connection never replaces one about a newer connection, and a new connection clears
// what is recorded about the ones before it.
//
// Three kinds of reason are told:
//
//   - what this node finds itself: no path, the peer is not connected, the peer did not
//     open the connections it was asked for;
//   - how the session ended (ended): no sub-connection was answered, the peer could not
//     connect it to the destination, the peer was silent for the grace time;
//   - why the peer could not connect a session to its destination. The engine tells
//     this end only that it failed; the peer sends the reason on the control connection
//     (session_fail). The two arrive in either order.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Claw-King/tcpraid/internal/control"
	"github.com/Claw-King/tcpraid/internal/engine"
)

const (
	// dialTroubleAge is how long a failed dial of a sub-connection counts as the reason
	// that a connection of a rule is still waiting for its first one.
	dialTroubleAge = time.Minute
	// failAge is how long this node keeps what a peer said about a session that has not
	// ended here yet, and how long it waits, after a session ended, for the peer to say
	// why. maxFails bounds each of the two lists.
	failAge  = time.Minute
	maxFails = 256
	// maxReason is how much of a reason that a peer wrote is shown.
	maxReason = 200
)

// problem is what went wrong with a rule.
type problem struct {
	text string
	at   time.Time
	seq  uint64 // the number of the connection or event it is about; 0: the rule does not listen
}

// attempt is one connection of a rule, as far as the rule's status goes. A connection
// that came through the API (Connect) belongs to no rule: its attempt has an empty rule
// and is kept out of the rules' status.
type attempt struct {
	rt   *Runtime
	rule string
	seq  uint64
	said bool // fail has recorded the cause (rt.mu)
	// What an entry that answers its client with a reason reads (entry.go), all under
	// rt.mu: text is the cause as fail recorded it. code is the peer's code for a
	// session it could not connect to its destination (control.CodeAllow,
	// control.CodeDial), or what a lookup at the peer answered. pending: the peer may
	// still say why; known is closed when it has.
	text    string
	code    string
	kind    string // of a failed connect at the peer, as far as the peer said (control.KindRefused, ...)
	pending bool
	known   chan struct{}
}

// dialTrouble is the last dial of a sub-connection to a peer that failed.
type dialTrouble struct {
	text string
	at   time.Time
}

// failKey names a session of this node by the peer at its other end and its id in hex.
type failKey struct {
	peer, session string
}

// failDetail is why a peer could not connect a session to its destination, as the peer
// said it: the text is already made printable.
type failDetail struct {
	code, reason string
	kind         string // one of the kinds of package control, or empty
	at           time.Time
}

// recentEnd is a session that ended here because the peer could not connect it, kept
// for the reason the peer may still send.
type recentEnd struct {
	a   *attempt
	dst string
	at  time.Time
}

// begin starts the record of one connection of the rule: what was said about earlier
// connections is cleared.
func (rt *Runtime) begin(rule string) *attempt {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.seq++
	if rt.problems != nil && rule != "" {
		delete(rt.problems, rule)
	}
	return &attempt{rt: rt, rule: rule, seq: rt.seq, known: make(chan struct{})}
}

// setCode records the peer's code for the failure of the connection (rt.mu held).
func (a *attempt) setCode(code string) {
	a.code = code
	if a.pending {
		a.pending = false
		close(a.known)
	}
}

// reason returns why the connection was not carried, as fail recorded it, and the code
// that goes with it. Where the peer may still say why its end could not connect the
// session, it waits for that, wait at most.
func (a *attempt) reason(wait time.Duration) (text, code, kind string) {
	rt := a.rt
	rt.mu.Lock()
	pending := a.pending
	rt.mu.Unlock()
	if pending {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-a.known:
		case <-t.C:
		}
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return a.text, a.code, a.kind
}

// fail records what went wrong with the connection. Only the first call counts.
func (a *attempt) fail(text string) {
	if a.record(text) {
		a.rt.notify(Notice{Kind: NoticeFailure, Rule: a.rule, Reason: text})
	}
}

// record is fail without the notice; it reports whether this call was the first.
func (a *attempt) record(text string) bool {
	rt := a.rt
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if a.said || rt.problems == nil {
		return false
	}
	a.said = true
	a.text = text
	if a.rule == "" {
		rt.logf("connect: %s", text)
		return true
	}
	// connection after connection failing the same way is logged once, until one gets
	// through (ended)
	if rt.logged[a.rule] != text {
		rt.logged[a.rule] = text
		rt.logf("rule %q: %s", a.rule, text)
	}
	if p, ok := rt.problems[a.rule]; ok && p.seq > a.seq {
		return true // something newer is on record
	}
	rt.problems[a.rule] = problem{text: text, at: time.Now(), seq: a.seq}
	return true
}

// refine records the fuller reason that came after the connection ended: in the place
// of the text fail recorded for it, if that record is still the rule's, or as the
// connection's reason, if it has none yet.
func (a *attempt) refine(text string) {
	rt := a.rt
	rt.mu.Lock()
	said := a.said
	if said {
		a.text = text
	}
	if p, ok := rt.problems[a.rule]; ok && a.rule != "" && said && p.seq == a.seq && p.text != text {
		rt.logf("rule %q: %s", a.rule, text)
		p.text = text
		rt.problems[a.rule] = p
	}
	rt.mu.Unlock()
	if !said {
		a.fail(text)
	}
}

// note records something that went wrong with a rule and is not about one connection
// that just arrived: it counts as the newest.
func (rt *Runtime) note(rule, text string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.problems == nil {
		return
	}
	rt.seq++
	if rt.problems[rule].text != text {
		rt.logf("rule %q: %s", rule, text)
	}
	rt.problems[rule] = problem{text: text, at: time.Now(), seq: rt.seq}
}

// ended records, for the status of the rule, how the session of a connection to dst at
// the peer ended, when that is something the user can act on.
func (rt *Runtime) ended(a *attempt, peer, dst string, end engine.End) {
	if end.Clean {
		rt.mu.Lock()
		if rt.logged != nil && a.rule != "" {
			delete(rt.logged, a.rule) // a connection got through: what fails next is news again
		}
		rt.mu.Unlock()
		return
	}
	switch {
	case strings.HasPrefix(end.Why, engine.WhyUnreachable), end.Why == engine.WhyNoSession:
		// The peer could not connect the session, or a sub-connection that came late
		// found the session gone at the peer before this end was told why (a peer that
		// fails at once can be that fast). Either way the peer may say why, or has.
		key := failKey{peer, hex.EncodeToString(end.ID[:])}
		rt.mu.Lock()
		d, told := rt.details[key]
		if told {
			delete(rt.details, key)
			a.kind = d.kind
			a.setCode(d.code)
		} else if rt.recent != nil {
			prune(rt.recent, func(r recentEnd) time.Time { return r.at })
			rt.recent[key] = recentEnd{a: a, dst: dst, at: time.Now()}
			a.pending = true
		}
		rt.mu.Unlock()
		switch {
		case told:
			a.fail(unreachable(peer, dst, d))
		case end.Why != engine.WhyNoSession:
			a.fail(fmt.Sprintf("peer %q could not connect to %s. Its log says why.", peer, dst))
		}
	case end.Why == engine.WhyNoAnswer:
		text := fmt.Sprintf("peer %q answered on none of the connections within 30 seconds, so the application's connection was reset.", peer)
		rt.mu.Lock()
		d, ok := rt.dialErr[peer]
		rt.mu.Unlock()
		if ok && time.Since(d.at) < dialTroubleAge {
			text += " The last attempt to open one failed with: " + d.text
		} else if why := noReply(end.NoReply); why != "" {
			text += " The last one ended like this: " + why
		}
		a.fail(text)
	case end.Why == engine.WhyGrace:
		a.fail(fmt.Sprintf("nothing arrived from peer %q for the whole grace time, so the application's connection was reset.", peer))
	case end.Why == engine.WhyRefused:
		a.fail(fmt.Sprintf("peer %q refused the connection because it carries as many as it takes.", peer))
	}
}

// noReply says what the error means with which a sub-connection ended that the peer
// never answered. The connection was made and the peer closed it, or stayed silent: this
// end can only say what that usually stands for.
func noReply(err string) string {
	switch {
	case err == "":
		return ""
	case strings.Contains(err, "bad certificate"):
		// what a TLS server answers when it refuses the client's certificate
		return "the other node does not accept this node, which is not paired with it or no longer is"
	case err == "EOF" || strings.Contains(err, "unexpected EOF") || strings.Contains(err, "connection reset"):
		return "the other end closed the connection without answering. Either the address is not a tcpraid listener, or the other node is not paired with this one, or their pair secrets differ."
	case strings.Contains(err, "i/o timeout"):
		return "the other end took the connection and did not answer within 30 seconds"
	}
	return err
}

// unreachable says, for a rule's status, why the peer could not connect a session to dst.
func unreachable(peer, dst string, d failDetail) string {
	switch d.code {
	case control.CodeAllow:
		return fmt.Sprintf("peer %q refused the destination %s, which is not in the allow list it keeps for this node. Change the list on that node with \"tcpraid peers set\".", peer, dst)
	case control.CodeDial:
		// the system's error starts with what was dialed, which the sentence says already
		why := strings.TrimPrefix(d.reason, "dial tcp "+dst+": ")
		return fmt.Sprintf("peer %q could not connect to %s: %s", peer, dst, why)
	}
	return fmt.Sprintf("peer %q could not connect to %s: %s", peer, dst, d.reason)
}

// prune drops the entries of m that are older than failAge and, if it is full then, all
// of them: the lists hold what may still be matched in the next seconds, and a full one
// means somebody sends more than can be meant.
func prune[K comparable, V any](m map[K]V, at func(V) time.Time) {
	for k, v := range m {
		if time.Since(at(v)) > failAge {
			delete(m, k)
		}
	}
	if len(m) >= maxFails {
		clear(m)
	}
}

// tellFailure returns what the engine calls when this node could not connect a session
// of the peer to its destination: the peer is told why on a control connection, if one
// is up. The engine tells it afterwards that the session failed.
func (rt *Runtime) tellFailure(peer string) func(id [16]byte, code, kind, reason string) {
	return func(id [16]byte, code, kind, reason string) {
		cc := rt.liveControl(peer)
		if cc == nil {
			return
		}
		if len(reason) > maxReason {
			reason = reason[:maxReason]
		}
		cc.Send(control.Message{Type: control.TypeSessionFail, Session: hex.EncodeToString(id[:]), Code: code, Kind: kind, Reason: reason})
	}
}

// sessionFailed handles a peer's word that it could not connect a session of this node
// to its destination. The session may have ended here already, or not yet; if not, it
// is ended now.
func (rt *Runtime) sessionFailed(peer string, m control.Message) {
	raw, err := hex.DecodeString(m.Session)
	if err != nil || len(raw) != 16 {
		return
	}
	key := failKey{peer, strings.ToLower(m.Session)}
	d := failDetail{code: printable(m.Code, 16), reason: printable(m.Reason, maxReason), at: time.Now()}
	switch m.Kind {
	case control.KindRefused, control.KindTimeout, control.KindUnreachable:
		d.kind = m.Kind
	}
	rt.mu.Lock()
	if rt.details == nil {
		rt.mu.Unlock()
		return
	}
	r, ended := rt.recent[key]
	if ended {
		delete(rt.recent, key)
		// the text first: setCode wakes whoever waits for the reason
		r.a.text = unreachable(peer, r.dst, d)
		r.a.kind = d.kind
		r.a.setCode(d.code)
	} else {
		prune(rt.details, func(d failDetail) time.Time { return d.at })
		rt.details[key] = d
	}
	rt.mu.Unlock()
	if ended {
		r.a.refine(unreachable(peer, r.dst, d))
		return
	}
	// The session is told on a sub-connection as well, but the peer closes its
	// connections right after that, and what it wrote last can be lost with them. A
	// session whose sub-connections the peer opens would then wait for the grace time.
	rt.Engine.Unreachable(peer, [16]byte(raw))
}

// dialed notes how a dial of a sub-connection to the peer went. A dial that was given
// up because the pairing ended or the node stops is not a failure of the path.
func (rt *Runtime) dialed(ps *peerState, peer string, err error) {
	if err != nil && (ps.ctx.Err() != nil || errors.Is(err, context.Canceled)) {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.dialErr == nil {
		return
	}
	if err == nil {
		delete(rt.dialErr, peer)
		return
	}
	rt.dialErr[peer] = dialTrouble{text: err.Error(), at: time.Now()}
}
