package forward

// Spare connections.
//
// A session whose sub-connections the peer opens starts later than one this node dials
// for: the request goes over the control connection, the peer dials, the two nodes
// authenticate, and only then can this node send the header of the session. A spare
// connection is one the peer opened and authenticated ahead of any session. The node
// that carries a new session sends the header on a spare at once, and asks for the rest
// of the sub-connections as before.
//
// The node that holds no path to a peer asks that peer for spares (the control message
// spares) when both name the feature in their hello. The peer keeps that many, on its
// first path that carries data, and opens another when one is used or ends. A node
// takes spares only from a peer it asked, and never more than it asked for.

import (
	"context"
	"errors"
	"net"
	"os"
	"slices"
	"time"

	"github.com/Claw-King/tcpraid/internal/control"
	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/node"
)

// spareConn is a spare connection that a peer keeps to this node. A goroutine reads on
// it while it waits (watchSpare): nothing is to arrive on it before this node sends a
// header, so the read ends only when the connection does, or when takeSpare ends it.
type spareConn struct {
	conn  net.Conn
	path  byte          // the number the peer gives the path it came over
	taken bool          // takeSpare took it out of the peer's list (rt.mu)
	idle  chan struct{} // closed when watchSpare has returned
	ok    bool          // the watch ended because the spare was taken; read after idle
}

// spareYoung is how soon a spare connection must not end, unused, for the next one to
// be opened without a pause.
const spareYoung = 5 * time.Second

// sparesFor returns how many spare connections this node wants the peer to keep to it:
// Spares, if the peer said it keeps them and this node has no path to dial it by.
func (rt *Runtime) sparesFor(peer node.Peer, pc *peerControl) int {
	if rt.noSpare || rt.noDialBoth || rt.noFeatures || len(peer.Paths) > 0 || !slices.Contains(pc.features, control.FeatureSpare) {
		return 0
	}
	return rt.Spares
}

// askSpares tells the peer how many spare connections this node wants. From the
// supervisor (on is nil) it says so only when that is not what it said last. When a
// hello has arrived on the control connection on, it says the number again over that
// connection: the peer drops the number when it has lost every control connection,
// which it may notice long before this node does, and the hello is the first this
// node hears of the peer's new connection.
func (rt *Runtime) askSpares(peer node.Peer, on *ctlConn) {
	rt.mu.Lock()
	pc := rt.ctl[peer.Name]
	if pc == nil || len(pc.conns) == 0 || !pc.greeted {
		rt.mu.Unlock()
		return
	}
	want := rt.sparesFor(peer, pc)
	// a peer that was not asked for any, or was told that none are wanted, is not
	// told that none are wanted
	if (want == 0 && pc.spareAsked <= 0) || (on == nil && want == pc.spareAsked) {
		rt.mu.Unlock()
		return
	}
	pc.spareAsked = want
	cc := on
	if cc == nil {
		cc = pc.conns[len(pc.conns)-1] // the newest: an older one may have ended unseen
	}
	if want == 0 {
		rt.dropSparesLocked(peer.Name)
	}
	rt.mu.Unlock()
	// Spares 0 is left out of the JSON, which says the same
	cc.Send(control.Message{Type: control.TypeSpares, Spares: want})
}

// HandleSpare takes a spare connection that a peer opened (link.Acceptor.HandleSpare).
// It is kept only if this node asked the peer for spares and has fewer than it asked
// for. It returns at once.
func (rt *Runtime) HandleSpare(peer node.Peer, path byte, conn net.Conn) {
	rt.mu.Lock()
	pc := rt.ctl[peer.Name]
	if rt.ctx == nil || rt.ctx.Err() != nil || !rt.validFor(peer)() || pc == nil || len(rt.spares[peer.Name]) >= pc.spareAsked {
		rt.mu.Unlock()
		conn.Close()
		return
	}
	sp := &spareConn{conn: conn, path: path, idle: make(chan struct{})}
	rt.spares[peer.Name] = append(rt.spares[peer.Name], sp)
	rt.mu.Unlock()
	go rt.watchSpare(peer.Name, sp)
}

// watchSpare waits on a spare connection until it ends or is taken.
func (rt *Runtime) watchSpare(peer string, sp *spareConn) {
	var b [1]byte
	n, err := sp.conn.Read(b[:])
	rt.mu.Lock()
	taken := sp.taken
	if !taken {
		rt.spares[peer] = slices.DeleteFunc(rt.spares[peer], func(x *spareConn) bool { return x == sp })
	}
	rt.mu.Unlock()
	// taken, and the read ended only because takeSpare ended it: the connection is
	// as it was. Anything else (it closed, or the peer sent something) ends it.
	sp.ok = taken && n == 0 && errors.Is(err, os.ErrDeadlineExceeded)
	if !sp.ok {
		sp.conn.Close()
	}
	close(sp.idle)
}

// takeSpare returns a spare connection that the peer called name keeps to this node,
// for a session to begin on, or nil if there is none that still works.
func (rt *Runtime) takeSpare(name string) *spareConn {
	for {
		rt.mu.Lock()
		list := rt.spares[name]
		if len(list) == 0 {
			rt.mu.Unlock()
			return nil
		}
		sp := list[0]
		rt.spares[name] = list[1:]
		sp.taken = true
		rt.mu.Unlock()
		sp.conn.SetReadDeadline(time.Now()) // ends the read of watchSpare
		<-sp.idle
		if sp.ok {
			sp.conn.SetReadDeadline(time.Time{})
			return sp
		}
	}
}

// dropSparesLocked closes the spare connections the peer called name keeps to this node
// (rt.mu held).
func (rt *Runtime) dropSparesLocked(name string) {
	for _, sp := range rt.spares[name] {
		sp.conn.Close() // watchSpare sees it
	}
}

// sparesAsked handles a peer's word on how many spare connections it wants this node to
// keep to it: at most Spares, and none without a path to the peer.
func (rt *Runtime) sparesAsked(pc *peerControl, name string, n int) {
	if rt.noSpare || rt.noDialBoth || rt.noFeatures {
		return
	}
	n = max(0, min(n, rt.Spares))
	rt.mu.Lock()
	changed := pc.spareWant != n
	pc.spareWant = n
	// more than are wanted now are closed; the session of one that is in use goes on
	for len(pc.spareIdle) > n {
		last := len(pc.spareIdle) - 1
		pc.spareIdle[last].Close()
		pc.spareIdle = pc.spareIdle[:last]
	}
	start := n > 0 && !pc.spareKeeping
	if start {
		pc.spareKeeping = true
	}
	rt.mu.Unlock()
	if changed {
		rt.logf("peer %q asked for %d spare connections", name, n)
	}
	if start {
		go rt.keepSpares(name, pc)
	}
	rt.kickSpares(pc)
}

func (rt *Runtime) kickSpares(pc *peerControl) {
	select {
	case pc.spareKick <- struct{}{}:
	default:
	}
}

// dataPath returns the first path of the peer that carries data, and its number.
func dataPath(peer node.Peer) (int, node.Path, bool) {
	for i, p := range peer.Paths {
		if slices.Contains(p.Roles, node.RoleData) {
			return i, p, true
		}
	}
	return 0, node.Path{}, false
}

// keepSpares keeps as many spare connections to the peer called name as it asked for,
// while a control connection with it is up: it opens one, waits until one is missing,
// and opens the next, after a pause that grows while they end young and unused or
// cannot be opened. It ends when the peer wants none any more, when this node has no
// path that carries data to it, or when the pairing ends.
func (rt *Runtime) keepSpares(name string, pc *peerControl) {
	defer func() {
		rt.mu.Lock()
		pc.spareKeeping = false
		for _, c := range pc.spareIdle {
			c.Close()
		}
		pc.spareIdle = nil
		rt.mu.Unlock()
	}()
	for pc.ctx.Err() == nil {
		rt.mu.Lock()
		want, have, online, fails := pc.spareWant, len(pc.spareIdle), len(pc.conns) > 0, pc.spareFails
		rt.mu.Unlock()
		if want == 0 {
			return
		}
		if !online || have >= want {
			select {
			case <-pc.spareKick:
			case <-time.After(superviseInterval):
			case <-pc.ctx.Done():
				return
			}
			continue
		}
		if fails > 0 {
			select {
			case <-time.After(controlBackoff[min(fails-1, len(controlBackoff)-1)]):
			case <-pc.ctx.Done():
				return
			}
		}
		ps, err := rt.peerFor(name)
		if err != nil {
			return // no path any more, or the pairing has ended
		}
		peer := ps.peer
		index, path, ok := dataPath(peer)
		if !ok {
			return
		}
		conn, err := ps.dial(func(ctx context.Context) (net.Conn, error) {
			return rt.Dialer.OpenSpare(ctx, peer, path, uint8(index))
		})
		if err != nil {
			rt.mu.Lock()
			pc.spareFails++
			rt.mu.Unlock()
			continue
		}
		rt.mu.Lock()
		if pc.ctx.Err() != nil || len(pc.spareIdle) >= pc.spareWant {
			rt.mu.Unlock()
			conn.Close()
			continue
		}
		pc.spareIdle = append(pc.spareIdle, conn)
		rt.mu.Unlock()
		go rt.serveSpare(pc, ps, conn)
	}
}

// serveSpare waits on a spare connection this node opened until the peer begins a
// session on it or it ends, and serves the session.
func (rt *Runtime) serveSpare(pc *peerControl, ps *peerState, conn net.Conn) {
	peer := ps.peer
	began := time.Now()
	stop := context.AfterFunc(pc.ctx, func() { conn.Close() })
	defer stop()
	gone := func() bool { // conn is no spare any more; reports whether it was one still
		rt.mu.Lock()
		defer rt.mu.Unlock()
		n := len(pc.spareIdle)
		pc.spareIdle = slices.DeleteFunc(pc.spareIdle, func(c net.Conn) bool { return c == conn })
		return len(pc.spareIdle) < n
	}
	used := false
	keys, err := pairKeys(peer)
	allow, aerr := engine.ParseAllow(peer.Allow) // a stored allow list has passed this parse
	if err != nil || aerr != nil {
		gone()
		conn.Close()
		return
	}
	rt.Engine.ServeSpare(conn, engine.SubOptions{
		Keys:       keys,
		Peer:       peer.Name,
		Allow:      allow,
		AllowName:  "the allow list of the peer",
		UDPDial:    peer.UDPDial,
		Valid:      ps.valid,
		Sink:       rt.sink,
		DialFailed: rt.tellFailure(peer.Name),
	}, func() {
		// the peer begins a session on it: the next spare is due
		used = gone()
		rt.mu.Lock()
		pc.spareFails = 0
		rt.mu.Unlock()
		rt.kickSpares(pc)
	})
	if !used && gone() {
		// it ended as a spare: the peer closed it, or the way to the peer broke
		rt.mu.Lock()
		if time.Since(began) < spareYoung {
			pc.spareFails++
		} else {
			pc.spareFails = 0
		}
		rt.mu.Unlock()
		rt.kickSpares(pc)
	}
}
