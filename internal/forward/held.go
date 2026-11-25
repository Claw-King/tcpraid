package forward

// What two nodes of version 0.11.0 or later tell each other about single sessions:
//
//   - how the sessions of this node ended (a short list, for the connect route and for
//     the answer to held);
//   - held: after a peer was restarted, which of the sessions that began shortly
//     before the restart its new process holds;
//   - session_ok: that the exit end connected a session to its destination.
//
// Each is used with a peer only when the peer named the feature in its hello.

import (
	"context"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Claw-King/tcpraid/internal/control"
	"github.com/Claw-King/tcpraid/internal/engine"
)

const (
	// maxEnds and endAge bound the list of how sessions ended: the newest maxEnds, and
	// none older than endAge.
	maxEnds = 256
	endAge  = 5 * time.Minute
	// entryConfirm is how long an entry whose peer tells when it has connected a
	// session waits for that word after the peer took the session: the 10 seconds the
	// peer gives the connect to the destination, and a margin.
	entryConfirm = 12 * time.Second
)

// heldWait is how long the answer to a held counts. heldRecheck is how long a node
// that answers a held waits before it calls a session unknown that it does not hold:
// a session that ends at that moment is in neither of its lists for an instant.
var (
	heldWait    = 10 * time.Second
	heldRecheck = 100 * time.Millisecond
)

// endRecheck is how long SessionEnd looks again for the end of a session that the node
// neither holds nor remembers, before it says that it does not know the session.
const endRecheck = 100 * time.Millisecond

// endRecord is how one session of this node ended.
type endRecord struct {
	peer  string
	clean bool
	// confirmed: the other node said that it had written this node's whole stream to
	// the destination (engine.SessionEvent.Confirmed)
	confirmed bool
	why       string
	at        time.Time
}

// end is what the record says to one who asks how the session ended. A session that
// ended in order at this node, while the other node was gone before it said that the
// destination had everything, is not complete.
func (r endRecord) end() SessionEnd {
	if r.clean && !r.confirmed {
		return SessionEnd{Ended: true, Reason: "the other node was gone before it said that it had written everything to the destination"}
	}
	return SessionEnd{Ended: true, Complete: r.clean, Reason: r.why}
}

// SessionEnd says how a session ended. Ended is false while it has not.
type SessionEnd struct {
	Ended bool
	// Complete: the stream ended completely in both directions. The other node wrote
	// all of this end's data to the destination and ended that direction in order,
	// and all of the destination's data was handed over at this end.
	Complete bool
	// Reason is the reason as the log has it.
	Reason string
}

// sessionEvent is what the engine tells of every session (Engine.Watch): the end of
// each is kept for a while. It takes endMu and nothing else.
func (rt *Runtime) sessionEvent(ev engine.SessionEvent) {
	if !ev.Ended {
		return
	}
	now := time.Now()
	rt.endMu.Lock()
	if rt.ends == nil {
		rt.ends = map[[16]byte]endRecord{}
	}
	var oldest [16]byte
	var oldestAt time.Time
	for id, r := range rt.ends {
		if now.Sub(r.at) > endAge {
			delete(rt.ends, id)
			continue
		}
		if oldestAt.IsZero() || r.at.Before(oldestAt) {
			oldest, oldestAt = id, r.at
		}
	}
	if len(rt.ends) >= maxEnds {
		delete(rt.ends, oldest)
	}
	rt.ends[ev.ID] = endRecord{peer: ev.Owner.Peer, clean: ev.Clean, confirmed: ev.Confirmed, why: ev.Why, at: now}
	if rt.endCh != nil {
		close(rt.endCh)
	}
	rt.endCh = make(chan struct{})
	rt.endMu.Unlock()
}

// endOf returns how the session with the id ended, if the list still has it, and the
// channel that is closed when the list gets its next entry.
func (rt *Runtime) endOf(id [16]byte) (r endRecord, ok bool, next chan struct{}) {
	rt.endMu.Lock()
	defer rt.endMu.Unlock()
	if rt.endCh == nil {
		rt.endCh = make(chan struct{})
	}
	r, ok = rt.ends[id]
	return r, ok, rt.endCh
}

// holds reports whether this node holds a session with the id at this moment.
func (rt *Runtime) holds(id [16]byte) bool {
	return slices.ContainsFunc(rt.Engine.SessionList(), func(si engine.SessionInfo) bool { return si.ID == id })
}

// SessionEnd waits until the session with the id has ended at this node and says how.
// known is false when this node neither holds such a session nor remembers the end of
// one: the list of ends is short and forgets after a few minutes. When ctx ends first,
// the answer has Ended false.
func (rt *Runtime) SessionEnd(ctx context.Context, id [16]byte) (end SessionEnd, known bool) {
	for {
		r, ok, next := rt.endOf(id)
		if ok {
			return r.end(), true
		}
		if !rt.holds(id) {
			// The engine takes a session off its list a moment before it reports the
			// end, so one that ends right now is in neither place for an instant.
			for wait := endRecheck; ; wait -= endRecheck / 5 {
				if r, ok, _ = rt.endOf(id); ok {
					return r.end(), true
				}
				if wait <= 0 {
					return SessionEnd{}, false
				}
				// not cut short by ctx: the look is short, and one who asks with a
				// context that is over already must still hear "unknown"
				time.Sleep(endRecheck / 5)
			}
		}
		select {
		case <-next:
		case <-ctx.Done():
			return SessionEnd{}, true
		}
	}
}

// peerSays reports whether a control connection with the peer is up and the peer's
// hello named the feature.
func (rt *Runtime) peerSays(name, feature string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	pc := rt.ctl[name]
	return pc != nil && len(pc.conns) > 0 && pc.greeted && slices.Contains(pc.features, feature)
}

// speaks returns an error when the peer's hello listed no version of the wire protocol
// that this node speaks: no session is opened with such a peer.
func (rt *Runtime) speaks(name string) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if pc := rt.ctl[name]; pc != nil && pc.noCommon != "" {
		return fmt.Errorf("%s", pc.noCommon)
	}
	return nil
}

// noCommonProtocol returns what to say about a peer whose hello lists theirs when this
// node lists mine, or "" when the two have a version in common. A hello that lists
// nothing is one of a node that speaks version 1.
func noCommonProtocol(peer string, theirs, mine []int) string {
	if len(theirs) == 0 {
		theirs = []int{1}
	}
	for _, v := range theirs {
		if slices.Contains(mine, v) {
			return ""
		}
	}
	if len(theirs) > 8 {
		theirs = theirs[:8]
	}
	return fmt.Sprintf("peer %q speaks no version of the wire protocol that this node speaks (the peer lists %v, this node %v). Run versions of tcpraid on the two nodes that have one in common.", peer, theirs, mine)
}

// heldQuery is what this node asked a peer that was restarted.
type heldQuery struct {
	request  uint64
	instance string // of the hello that led to the question: the answer must come from that process
	ids      map[string]bool
	at       time.Time
}

// askHeld asks a peer that runs as a new process which of the sessions it holds that
// this node cannot place: those that began after the old process was last heard, so
// that either process may have them. A session of this node that the peer has not
// answered on any sub-connection is left out, because its next dial will tell. The
// sessions that began before were reset already (peerRestarted).
func (rt *Runtime) askHeld(cc *ctlConn, oldUntil time.Time) {
	if !rt.peerSays(cc.peer, control.FeatureHeld) {
		return
	}
	ids := map[string]bool{}
	var list []string
	for _, si := range rt.Engine.SessionList() {
		if si.Peer != cc.peer || si.Started.Before(oldUntil) || (si.Client && !si.Up) {
			continue
		}
		id := hex.EncodeToString(si.ID[:])
		ids[id] = true
		list = append(list, id)
		if len(list) == control.MaxHeld {
			break
		}
	}
	if len(list) == 0 {
		return
	}
	rt.mu.Lock()
	rt.heldSeq++
	q := &heldQuery{request: rt.heldSeq, instance: cc.instance, ids: ids, at: time.Now()}
	if rt.heldAsked != nil {
		rt.heldAsked[cc.peer] = q
	}
	rt.mu.Unlock()
	cc.Send(control.Message{Type: control.TypeHeld, Request: q.request, Sessions: list})
}

// heldAnswered handles a peer's answer to held: the sessions this node asked about
// that the peer neither holds nor saw end complete are reset. The answer counts only
// for the question this node asked last, for heldWait, and from the process that was
// asked. What it names beyond the question is passed over.
func (rt *Runtime) heldAnswered(cc *ctlConn, m control.Message) {
	rt.mu.Lock()
	q := rt.heldAsked[cc.peer]
	if q == nil || q.request != m.Request || q.instance != cc.instance || time.Since(q.at) > heldWait {
		rt.mu.Unlock()
		return
	}
	delete(rt.heldAsked, cc.peer)
	rt.mu.Unlock()
	known := map[string]bool{}
	for _, l := range [][]string{m.Sessions, m.Done} {
		for _, id := range l[:min(len(l), control.MaxHeld)] {
			if id = strings.ToLower(id); q.ids[id] {
				known[id] = true
			}
		}
	}
	var owners []engine.Owner
	for id := range q.ids {
		if known[id] {
			continue
		}
		raw, err := hex.DecodeString(id)
		if err != nil || len(raw) != 16 {
			continue
		}
		if o, ok := rt.Engine.AbortSession(cc.peer, [16]byte(raw), "the peer's node was restarted"); ok {
			owners = append(owners, o)
		}
	}
	rt.restarted(cc.peer, owners)
}

// serveHeld answers a peer's held. It runs in a goroutine of its own, one at a time for
// a peer.
func (rt *Runtime) serveHeld(cc *ctlConn, m control.Message) {
	if m.Request == 0 || len(m.Sessions) == 0 {
		return
	}
	held, done := rt.heldAnswer(cc.peer, m.Sessions)
	cc.Send(control.Message{Type: control.TypeHeldReply, Request: m.Request, Sessions: held, Done: done})
}

// heldAnswer says, of the sessions a peer names (their ids in hex, MaxHeld of them at
// most), which this node holds with that peer and which ended complete here. What is
// in neither list the node does not know, or it ended another way.
func (rt *Runtime) heldAnswer(peer string, asked []string) (held, done []string) {
	live := map[[16]byte]bool{}
	for _, si := range rt.Engine.SessionList() {
		if si.Peer == peer {
			live[si.ID] = true
		}
	}
	complete := func(id [16]byte) bool {
		r, ok, _ := rt.endOf(id)
		return ok && r.clean && r.peer == peer
	}
	var unknown [][16]byte
	for _, text := range asked[:min(len(asked), control.MaxHeld)] {
		raw, err := hex.DecodeString(text)
		if err != nil || len(raw) != 16 {
			continue
		}
		switch id := [16]byte(raw); {
		case live[id]:
			held = append(held, hex.EncodeToString(raw))
		case complete(id):
			done = append(done, hex.EncodeToString(raw))
		default:
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		// a session that ended at this very moment may be in neither list yet
		time.Sleep(heldRecheck)
		for _, id := range unknown {
			if complete(id) {
				done = append(done, hex.EncodeToString(id[:]))
			}
		}
	}
	return held, done
}

// tellConnected returns what the engine calls when this node has connected a session
// of the peer to its destination: the peer is told on a control connection, if one is
// up and the peer said that it listens for it.
func (rt *Runtime) tellConnected(peer string) func(id [16]byte) {
	return func(id [16]byte) {
		if !rt.peerSays(peer, control.FeatureSessionOK) {
			return
		}
		if cc := rt.liveControl(peer); cc != nil {
			// not in the goroutine that is about to carry the session's data
			go cc.Send(control.Message{Type: control.TypeSessionOK, Session: hex.EncodeToString(id[:])})
		}
	}
}

// sessionOK handles a peer's word that it has connected a session to its destination:
// an entry that waits to tell its client so does it now. The word counts only for a
// session that this node carries with that peer and whose entry is waiting.
func (rt *Runtime) sessionOK(peer string, m control.Message) {
	raw, err := hex.DecodeString(m.Session)
	if err != nil || len(raw) != 16 {
		return
	}
	rt.mu.Lock()
	oc := rt.waiting[failKey{peer, hex.EncodeToString(raw)}]
	rt.mu.Unlock()
	if oc != nil {
		go oc.open()
	}
}

// wait and unwait keep the entries whose peers tell when a session is connected. Each
// stands for a session of this node, so there are no more than the engine has sessions.
func (rt *Runtime) wait(peer, session string, oc *openConn) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.waiting != nil {
		rt.waiting[failKey{peer, session}] = oc
	}
}

func (rt *Runtime) unwait(peer, session string) {
	if session == "" {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	delete(rt.waiting, failKey{peer, session})
}
