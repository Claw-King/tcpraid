package engine

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"time"
)

type allowRule struct {
	nw     *net.IPNet
	lo, hi int
}

func parseAllow(spec string) ([]allowRule, error) {
	var rules []allowRule
	for _, f := range strings.Split(spec, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		// forms: CIDR, CIDR:port[-port] (IPv4), [CIDR]:port[-port] (IPv6), IPv6 CIDR without port
		r := allowRule{lo: 1, hi: 65535}
		cidr, portSpec := f, ""
		if strings.HasPrefix(f, "[") {
			j := strings.Index(f, "]")
			if j < 0 {
				return nil, fmt.Errorf("bad entry %q", f)
			}
			cidr = f[1:j]
			if rest := f[j+1:]; rest != "" {
				if !strings.HasPrefix(rest, ":") {
					return nil, fmt.Errorf("bad entry %q", f)
				}
				portSpec = rest[1:]
			}
		} else if i := strings.LastIndex(f, ":"); i >= 0 && strings.Count(f, ":") == 1 {
			cidr, portSpec = f[:i], f[i+1:]
		}
		if portSpec != "" {
			ps := strings.SplitN(portSpec, "-", 2)
			lo, err := strconv.Atoi(ps[0])
			if err != nil {
				return nil, fmt.Errorf("bad port in %q", f)
			}
			r.lo, r.hi = lo, lo
			if len(ps) == 2 {
				if r.hi, err = strconv.Atoi(ps[1]); err != nil {
					return nil, fmt.Errorf("bad port in %q", f)
				}
			}
		}
		_, nw, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, err
		}
		r.nw = nw
		rules = append(rules, r)
	}
	return rules, nil
}

func allowed(rules []allowRule, ip net.IP, port int) bool {
	if len(rules) == 0 {
		return true
	}
	for _, r := range rules {
		if r.nw.Contains(ip) && port >= r.lo && port <= r.hi {
			return true
		}
	}
	return false
}

// refuse answers a sub with a status, then half-closes and lingers briefly: a relay in
// between may drop the reply if the connection is torn down at once.
func refuse(c net.Conn, st byte) {
	c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	c.Write(reply(st, 0))
	if tc, ok := c.(*net.TCPConn); ok {
		tc.CloseWrite()
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	io.Copy(io.Discard, c)
	c.Close()
}

// endInfo is what the engine remembers of a session that has ended, for late subs.
type endInfo struct {
	at    time.Time
	clean bool
	peer  string // whose session it was (see session.ofPair)
	hdr   []byte
}

// AllowList is a list of destinations a session may be connected to.
type AllowList struct {
	rules []allowRule
}

// ParseAllow reads an allow list: entries separated by commas, each a network in CIDR
// form, optionally with a port or a port range (192.0.2.0/24:873, [2001:db8::/32]:22,
// 198.51.100.7/32:5000-5010). An empty spec is a list that allows everything.
func ParseAllow(spec string) (*AllowList, error) {
	rules, err := parseAllow(spec)
	if err != nil {
		return nil, err
	}
	return &AllowList{rules}, nil
}

// Allows reports whether ip:port is in the list. A nil or empty list allows everything.
func (l *AllowList) Allows(ip net.IP, port int) bool {
	return l == nil || allowed(l.rules, ip, port)
}

// Empty reports whether the list has no entry, and so allows everything.
func (l *AllowList) Empty() bool { return l == nil || len(l.rules) == 0 }

// Match reports whether ip:port is in an entry of the list; unlike Allows, an empty
// list matches nothing.
func (l *AllowList) Match(ip net.IP, port int) bool {
	return l != nil && len(l.rules) > 0 && allowed(l.rules, ip, port)
}

// SubOptions says how ServeSub treats a sub-connection.
type SubOptions struct {
	Keys Keys
	// Peer names the other end; its sessions belong to it (Owner.Peer).
	Peer string
	// Allow lists the destinations a session of this end may reach; nil: any.
	Allow *AllowList
	// AllowName is how the log names the list when it refuses; empty: "-allow".
	AllowName string
	// UDPDial is the host:port UDP feedback is sent to; empty: this end does not dial
	// out for it.
	UDPDial string
	// Valid, if not nil, is asked once, at the moment a session is created, whether it
	// may be (see Engine.AbortPeer).
	Valid func() bool
	// Sink, if not nil, gives the local end of a session whose destination is
	// SinkIP:SinkPort: nothing is dialed for it and the allow list is not asked, because
	// the session reaches nothing outside this process. Sink returns the connection, or
	// nil and why not. Without Sink such a session fails like one whose destination
	// cannot be reached.
	Sink func() (net.Conn, string)
	// DialFailed, if not nil, is told when a session could not be connected to its
	// destination, before the other end is: code is DialRefused or DialError.
	DialFailed func(id [16]byte, code, reason string)
	// Config, if not nil, are the settings a session that the call creates runs with,
	// instead of the engine's: the session takes a copy when it is created.
	Config *Config
}

// The codes DialFailed gives.
const (
	DialRefused = "allow" // the destination is not in the allow list
	DialError   = "dial"  // connecting to the destination failed; the reason is the system's error
)

// SinkPort and SinkIP are the destination of a session that ends inside the other end
// (SubOptions.Sink). No application connection can have it: port 0 is not a port.
const SinkPort = 0

// SinkIP is the unspecified IPv4 address.
var SinkIP = net.IPv4zero

// isSink reports whether ip:port is the destination of SubOptions.Sink.
func isSink(ip net.IP, port int) bool { return port == SinkPort && ip.IsUnspecified() }

// RunServer accepts sub-connections on Config.Listen and connects each session to its
// destination. It returns only when the allow list is invalid or it cannot listen.
func (e *Engine) RunServer() error {
	ks := e.ks
	rules, err := parseAllow(e.cfg.Allow)
	if err != nil {
		return fmt.Errorf("-allow: %w", err)
	}
	ln, err := net.Listen("tcp", e.cfg.Listen)
	if err != nil {
		return err
	}
	log.Printf("server %s %s window=%dMB target=%v allow=%q psk=%v udp=%s%s", e.cfg.Version, e.cfg.Listen, e.cfg.Window>>20, e.cfg.Target, e.cfg.Allow, ks.configured, e.cfg.UDPListen, e.cfg.UDPDial)
	opt := SubOptions{Keys: Keys{ks}, Allow: &AllowList{rules}, UDPDial: e.cfg.UDPDial}
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		go e.ServeSub(c, opt)
	}
}

// ServeSub takes one sub-connection that the other end opened: it reads the header, and
// adds the sub-connection to the session it names or creates that session and connects
// it to its destination. It closes c when it does not keep it. A call that creates a
// session returns when the session has ended; any other returns at once.
//
// A session belongs to the pair of ends that created it (the keys and the peer name of
// the options): a sub-connection of another pair that names it is refused like one for
// a session this end does not hold.
func (e *Engine) ServeSub(c net.Conn, opt SubOptions) {
	e.serveSub(c, opt, nil, nil, nil)
}

// ServeSpare is ServeSub for a connection that this end dialed ahead of any session, so
// that the other end, which carries the sessions, can start one on it at once: the
// other end sends the header when it has a session for it, and until then the call
// waits, however long that is. used, if not nil, is called when the header begins to
// arrive. Closing c ends the wait.
//
// A session that the call creates is not kept at any number of sub-connections by this
// end; it is once the other end asks for the rest (Route.ServeReverse).
func (e *Engine) ServeSpare(c net.Conn, opt SubOptions, used func()) {
	if used == nil {
		used = func() {}
	}
	e.serveSub(c, opt, nil, nil, used)
}

// ServeReverse is ServeSub for a connection that this end dialed because the other end,
// which carries the session, asked for it: the other end sends the header on it. When
// the call creates the session, this end keeps the session's sub-connections at the
// numbers the Route's groups ask for, by dialing (a group's Dial returns a connection
// on which the other end is about to send a header). The keys and the UDP address are
// the Route's.
//
// When the header names a session that this end holds already and that it does not
// keep at any number of sub-connections (the other end dialed its first sub-connection,
// or it began on a spare connection), the session is kept at the numbers of the
// Route's groups from then on, as if the call had created it.
func (r *Route) ServeReverse(c net.Conn, opt SubOptions) {
	opt.Keys, opt.UDPDial = Keys{r.ks}, r.udpDial
	r.e.serveSub(c, opt, r, nil, nil)
}

// reverseRedial returns the function that keeps the sub-connections of s, a session the
// other end carries, at the numbers the groups of r ask for, by dialing: the other end
// sends the header on each connection.
func (e *Engine) reverseRedial(s *session, opt SubOptions, r *Route) func() {
	return r.redialer(s, func(g DialGroup) func() {
		return func() {
			sc, err := g.Dial()
			if err != nil {
				s.mu.Lock()
				s.noteFlap(g.Path, g.Ctrl, time.Time{}, true)
				s.mu.Unlock()
				time.Sleep(time.Second)
				return
			}
			if !e.serveSub(sc, opt, nil, s, nil) {
				// the other end did not make it a sub-connection of this session
				s.mu.Lock()
				s.noteFlap(g.Path, g.Ctrl, time.Time{}, true)
				s.mu.Unlock()
				time.Sleep(time.Second)
			}
		}
	})
}

// serveSub is ServeSub. With a Route, a session it creates redials over the Route's
// groups, and so does one it finds that does not redial yet. With join, the header must
// name that session: anything else is refused. With spare, c is a spare connection: the
// header may take any time to begin, and spare is called when it does. It reports
// whether c became a sub-connection of a session.
func (e *Engine) serveSub(c net.Conn, opt SubOptions, r *Route, join *session, spare func()) bool {
	ks := opt.Keys.ks
	b := make([]byte, headerLen)
	rest := b
	if spare != nil {
		if _, err := io.ReadFull(c, b[:1]); err != nil {
			c.Close()
			return false
		}
		spare()
		rest = b[1:]
	}
	c.SetReadDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.ReadFull(c, rest); err != nil {
		c.Close()
		return false
	}
	c.SetReadDeadline(time.Time{})
	h, err := parseHeader(b, ks.hdr)
	if err != nil {
		log.Printf("refusing sub from %s: %v", c.RemoteAddr(), err)
		refuse(c, stBadHdr)
		return false
	}
	if join != nil && h.id != join.id {
		refuse(c, stBadHdr)
		return false
	}
	keepAlive(c)
	ok := func(s *session) bool {
		c.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := c.Write(reply(stOK, uint16(s.cfg.Window>>20))); err != nil {
			c.Close()
			return false
		}
		c.SetWriteDeadline(time.Time{})
		// this end dialed c when a session was asked for it, when it joins one that
		// this end redials, and when it is a spare
		s.addSub(c, h.idx, h.path, h.ctrl, h.repair, false, r != nil || join != nil || spare != nil)
		return true
	}
	e.smu.Lock()
	if s := e.sess[h.id]; s != nil {
		e.smu.Unlock()
		if !s.ofPair(opt.Peer, ks) {
			refuse(c, stGoneBad)
			return false
		}
		added := ok(s)
		if added && r != nil {
			// the other end asked for sub-connections of a session that began
			// another way: this end keeps its share of them up from here on
			if redial := e.reverseRedial(s, opt, r); s.adopt(redial) {
				redial()
			}
		}
		return added
	}
	end, done := e.ended[h.id]
	if done && !(end.peer == opt.Peer && bytes.Equal(end.hdr, ks.hdr)) {
		// another pair's session: this pair never had one with this id
		e.smu.Unlock()
		refuse(c, stGoneBad)
		return false
	}
	if !done && h.isNew && len(e.sess) >= e.cfg.MaxSess {
		n := len(e.sess)
		e.smu.Unlock()
		log.Printf("refusing session from %s: %d sessions", c.RemoteAddr(), n)
		refuse(c, stRefused)
		return false
	}
	if done || !h.isNew {
		// a late or rejoining sub of a session we do not hold: never invent one
		e.smu.Unlock()
		st := byte(stGoneBad)
		if done && end.clean {
			st = stGoneOK
		}
		refuse(c, st)
		return false
	}
	if e.stopping || (opt.Valid != nil && !opt.Valid()) {
		e.smu.Unlock()
		refuse(c, stRefused)
		return false
	}
	sink := opt.Sink != nil && isSink(h.dstIP, h.dstPort)
	s := e.newSessionWith(opt.Config, h.id, nil, false, net.JoinHostPort(h.dstIP.String(), strconv.Itoa(h.dstPort)), ks, Owner{Peer: opt.Peer, Test: sink})
	// the session is listed from the moment it exists (SessionList), so what a listing
	// reads is set under the session's lock
	s.mu.Lock()
	if pb := int(h.windowMB) << 20; pb > 0 {
		s.peerBudget = min(s.peerBudget, pb)
	}
	s.everUp = true
	s.redials = r != nil
	s.mu.Unlock()
	e.sess[h.id] = s
	e.smu.Unlock()
	s.udpSetup(ks, opt.UDPDial)
	log.Printf("session %x -> %s", h.id[:3], s.dstAddr)
	added := ok(s)
	// dial outside the server lock: a slow destination must not hold up others
	go func() {
		failed := func(code, why string) {
			if opt.DialFailed != nil {
				opt.DialFailed(h.id, code, why)
			}
			s.dialFail(why)
		}
		if sink {
			d, why := opt.Sink()
			if d == nil {
				failed(DialError, why)
				return
			}
			if !s.setLocal(d) {
				d.Close()
				return
			}
			go s.pumpLocal()
			go s.deliverLocal()
			return
		}
		if !opt.Allow.Allows(h.dstIP, h.dstPort) {
			name := opt.AllowName
			if name == "" {
				name = "-allow"
			}
			failed(DialRefused, "not in "+name)
			return
		}
		d, err := net.DialTimeout("tcp", s.dstAddr, 10*time.Second)
		if err != nil {
			failed(DialError, err.Error())
			return
		}
		// from here on, a process that dies resets the destination
		ArmReset(d)
		if !s.setLocal(d) {
			resetClose(d)
			return
		}
		// Shutdown may have walked the sessions while this dial was under way: it found
		// no connection to reset then, so the connection is reset here. Shutdown sets
		// stopping before it walks, so one of the two always sees the other.
		e.smu.Lock()
		stopping := e.stopping
		e.smu.Unlock()
		if stopping {
			resetClose(d)
			return
		}
		go s.pumpLocal()
		go s.deliverLocal()
	}()
	var redial func()
	if r != nil {
		redial = e.reverseRedial(s, opt, r)
		redial()
	}
	s.monitor(redial)
	e.smu.Lock()
	delete(e.sess, h.id)
	s.mu.Lock()
	e.ended[h.id] = endInfo{at: time.Now(), clean: s.cleanEnd, peer: opt.Peer, hdr: ks.hdr}
	s.mu.Unlock()
	for k, end := range e.ended {
		if time.Since(end.at) > 30*time.Minute {
			delete(e.ended, k)
		}
	}
	e.smu.Unlock()
	return added
}
