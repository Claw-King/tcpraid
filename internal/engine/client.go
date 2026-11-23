package engine

import (
	"crypto/rand"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

// Route is where the sessions of this end go: the dial groups, the keys shared with the
// other end, and where UDP feedback is sent. Make one Route for a destination end and
// carry every application connection for it with the same Route: the limit on redials
// spans all its sessions.
type Route struct {
	e       *Engine
	groups  []DialGroup
	procCap []*bucket
	ks      keys
	udpDial string
}

// NewRoute returns a Route over the given dial groups. udpDial is the host:port UDP
// feedback is sent to; empty means the sessions do not dial out for it.
func (e *Engine) NewRoute(groups []DialGroup, k Keys, udpDial string) *Route {
	r := &Route{e: e, groups: groups, ks: k.ks, udpDial: udpDial}
	// hard redial limits per path / role, whatever the backoff says: 20 a minute in one session,
	// 60 a minute in all sessions together. A session's first fill (want subs per path / role)
	// does not count: new sessions are bounded by -maxsess, and limiting them starved short
	// connections (lab shortconns: ~140 sessions a minute)
	r.procCap = make([]*bucket, len(groups))
	for gi := range groups {
		r.procCap[gi] = newBucket(60, time.Minute)
	}
	return r
}

// RunClient accepts application connections on Config.Listen and carries each one over
// the given dial groups. It returns only when it cannot listen.
func (e *Engine) RunClient(groups []DialGroup) error {
	ks := e.ks
	ln, err := net.Listen("tcp", e.cfg.Listen)
	if err != nil {
		return err
	}
	route := e.NewRoute(groups, Keys{ks}, e.cfg.UDPDial)
	var gdesc []string
	for _, g := range groups {
		role := "data"
		if g.Ctrl {
			role = "control"
		} else if g.Repair {
			role = "repair"
		}
		gdesc = append(gdesc, fmt.Sprintf("%s/%s x%d", g.Name, role, g.Want))
	}
	log.Printf("client %s %s paths [%s] udp=%s%s chunk=%d window=%dMB target=%v/%v dead=%v grace=%v psk=%v timeline=%v",
		e.cfg.Version, e.cfg.Listen, strings.Join(gdesc, ", "), e.cfg.UDPListen, e.cfg.UDPDial, e.cfg.Chunk, e.cfg.Window>>20, e.cfg.Target, e.cfg.PigTarget, e.cfg.Dead, e.cfg.Grace, ks.configured, e.cfg.Timeline)
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		go func(c net.Conn) {
			var ip net.IP
			var port int
			if e.cfg.FixedDst != "" {
				a, err := net.ResolveTCPAddr("tcp", e.cfg.FixedDst)
				if err != nil {
					resetClose(c)
					return
				}
				ip, port = a.IP, a.Port
			} else if ip, port, err = origDst(c); err != nil {
				resetClose(c)
				return
			}
			route.Carry(c, ip, port, Owner{}, nil)
		}(c)
	}
}

// Carry carries the application connection c to ip:port at the other end of the Route,
// as one session, and returns when the session has ended. It closes c: with a reset
// unless the stream ended completely in both directions. owner says whom the session
// belongs to. valid, if not nil, is asked once, at the moment the session is created,
// whether it may be (see Engine.AbortPeer); a session that may not be is reset at once.
func (r *Route) Carry(c net.Conn, ip net.IP, port int, owner Owner, valid func() bool) {
	e, ks := r.e, r.ks
	s := e.begin(c, ip, port, ks, r.udpDial, owner, valid)
	if s == nil {
		return
	}
	id := s.id
	redial := r.redialer(s, func(g DialGroup) func() {
		s.mu.Lock()
		s.nextIdx++
		h := subHeader{isNew: !s.everUp, ctrl: g.Ctrl, repair: g.Repair, id: id, dstIP: ip, dstPort: port, path: g.Path,
			idx: s.nextIdx, windowMB: uint16(e.cfg.Window >> 20), unix: time.Now().Unix()}
		s.mu.Unlock()
		return func() {
			sc, err := g.Dial()
			if err != nil {
				s.mu.Lock()
				s.noteFlap(g.Path, g.Ctrl, time.Time{}, true)
				s.mu.Unlock()
				time.Sleep(time.Second)
				return
			}
			keepAlive(sc)
			sc.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := sc.Write(h.marshal(ks.hdr)); err != nil {
				sc.Close()
				return
			}
			sc.SetWriteDeadline(time.Time{})
			s.addSub(sc, h.idx, h.path, h.ctrl, h.repair, true)
		}
	})
	redial()
	log.Printf("session %x %s -> %s", id[:3], c.RemoteAddr(), s.dstAddr)
	go s.pumpLocal()
	go s.deliverLocal()
	s.monitor(redial)
}

// begin creates the session that carries the application connection c to ip:port, or
// resets c and returns nil: at the session limit, while the engine stops, or when valid
// says no.
func (e *Engine) begin(c net.Conn, ip net.IP, port int, ks keys, udpDial string, owner Owner, valid func() bool) *session {
	if e.nSessions.Load() >= int64(e.cfg.MaxSess) {
		log.Printf("refusing %s: %d sessions", c.RemoteAddr(), e.nSessions.Load())
		resetClose(c)
		return nil
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		resetClose(c)
		return nil
	}
	e.smu.Lock()
	if e.stopping || (valid != nil && !valid()) {
		e.smu.Unlock()
		resetClose(c)
		return nil
	}
	s := e.newSessionFor(id, c, true, fmt.Sprintf("%s:%d", ip, port), ks, owner)
	e.smu.Unlock()
	s.udpSetup(ks, udpDial)
	return s
}

// redialer returns the function that keeps the sub-connections of s at the numbers the
// Route's groups ask for; a session's monitor calls it. start is called for each
// sub-connection that is due, with its group: it prepares what must be prepared in the
// order of the calls and returns the function that opens the sub-connection and adds it
// to the session, which runs in a goroutine of its own. That function reports a dial
// that failed with noteFlap and waits a second before it returns.
func (r *Route) redialer(s *session, start func(g DialGroup) func()) func() {
	e, groups, procCap := r.e, r.groups, r.procCap
	var dmu sync.Mutex
	dialing := make([]int, len(groups))
	lastDial := make([]time.Time, len(groups))
	sessCap := make([]*bucket, len(groups))
	for gi := range groups {
		sessCap[gi] = newBucket(20, time.Minute)
	}
	dialed := make([]int, len(groups))
	var cappedLog time.Time
	return func() {
		s.mu.Lock()
		wait := make([]time.Duration, len(groups))
		for gi, g := range groups {
			wait[gi] = s.redialWait(g.Path, g.Ctrl)
		}
		have := make([]int, len(groups))
		for _, x := range s.subs {
			if x.dead {
				continue
			}
			for gi, g := range groups {
				if g.Path == x.path && g.Ctrl == x.ctrl {
					have[gi]++
				}
			}
		}
		s.mu.Unlock()
		for gi, g := range groups {
			dmu.Lock()
			need := max(g.Want-have[gi]-dialing[gi], 0)
			if need > 0 && wait[gi] > 0 {
				// subs on this path keep dying young or cannot be dialed: one at a
				// time, backing off (no redial storm during an outage)
				if time.Since(lastDial[gi]) < wait[gi] {
					need = 0
				} else {
					need = min(need, 1)
				}
			}
			if need > 0 {
				first := max(min(need, g.Want-dialed[gi]), 0)
				re := need - first
				got := sessCap[gi].take(re)
				all := procCap[gi].take(got)
				sessCap[gi].give(got - all)
				if all < re {
					e.mDialCapped.Add(int64(re - all))
					if time.Since(cappedLog) > 10*time.Second {
						cappedLog = time.Now()
						log.Printf("[%x] redial limit on %s: %d of %d redials now", s.id[:3], g.Name, all, re)
					}
				}
				need = first + all
				dialed[gi] += need
			}
			if need > 0 {
				lastDial[gi] = time.Now()
			}
			dialing[gi] += need
			dmu.Unlock()
			for i := 0; i < need; i++ {
				open := start(g)
				go func(gi int) {
					defer func() { dmu.Lock(); dialing[gi]--; dmu.Unlock() }()
					open()
				}(gi)
			}
		}
	}
}

// Reverse is a session that this end carries and whose sub-connections the other end
// opens: this end has no way to dial the other, so it asks for them (outside the
// engine) and attaches each one that arrives. It never redials; keeping the number of
// sub-connections up is the other end's business (Route.ServeReverse).
type Reverse struct {
	e    *Engine
	s    *session
	ks   keys
	ip   net.IP
	port int
}

// maxAttached bounds the live sub-connections of a Reverse: more than any set of paths
// asks for.
const maxAttached = 2048

// NewReverse creates the session that carries the application connection c to ip:port
// at the other end, like Route.Carry, but without sub-connections: Attach adds those
// that the other end opens. It returns nil, with c reset, where Carry would have reset
// c at once. Call Run next.
func (e *Engine) NewReverse(c net.Conn, ip net.IP, port int, k Keys, udpDial string, owner Owner, valid func() bool) *Reverse {
	s := e.begin(c, ip, port, k.ks, udpDial, owner, valid)
	if s == nil {
		return nil
	}
	log.Printf("session %x %s -> %s (sub-connections opened by the other end)", s.id[:3], c.RemoteAddr(), s.dstAddr)
	return &Reverse{e: e, s: s, ks: k.ks, ip: ip, port: port}
}

// ID returns the session's id.
func (rv *Reverse) ID() [16]byte { return rv.s.id }

// Attach makes sc, a connection that the other end opened for this session, one of its
// sub-connections on the path and in the role given: it sends the transport's header,
// as the dialing end of a sub-connection does. It closes sc when the session has ended
// or has too many sub-connections.
func (rv *Reverse) Attach(sc net.Conn, path uint8, ctrl, repair bool) {
	s := rv.s
	s.mu.Lock()
	data, ctl := s.counts()
	if s.closed || data+ctl >= maxAttached {
		s.mu.Unlock()
		sc.Close()
		return
	}
	s.nextIdx++
	h := subHeader{isNew: !s.everUp, ctrl: ctrl, repair: repair && !ctrl, id: s.id, dstIP: rv.ip, dstPort: rv.port, path: path,
		idx: s.nextIdx, windowMB: uint16(rv.e.cfg.Window >> 20), unix: time.Now().Unix()}
	s.mu.Unlock()
	keepAlive(sc)
	sc.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := sc.Write(h.marshal(rv.ks.hdr)); err != nil {
		sc.Close()
		return
	}
	sc.SetWriteDeadline(time.Time{})
	s.addSub(sc, h.idx, h.path, h.ctrl, h.repair, true)
}

// Up reports whether the other end has answered on a sub-connection: it holds the
// session.
func (rv *Reverse) Up() bool {
	rv.s.mu.Lock()
	defer rv.s.mu.Unlock()
	return rv.s.everUp
}

// Abort ends the session: the other end is told, if it can be, and the application
// connection is reset.
func (rv *Reverse) Abort(why string) { rv.s.abort(why) }

// Run carries the session and returns when it has ended. The application connection is
// closed then: with a reset unless the stream ended completely in both directions.
func (rv *Reverse) Run() {
	go rv.s.pumpLocal()
	go rv.s.deliverLocal()
	rv.s.monitor(nil)
}
