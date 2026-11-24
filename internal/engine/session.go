package engine

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type sample struct {
	t time.Time
	b int64
}

// sub is one TCP sub-connection of a session.
type sub struct {
	c        net.Conn
	w        *bufio.Writer
	wmu      sync.Mutex    // serializes writes
	wdead    time.Duration // Config.Dead: a write blocked this long fails
	idx      uint16        // same number on both ends (header), names the sub in snapshots
	path     uint8
	ctrl     bool // control sub: no DATA, carries snapshots
	repair   bool // repair sub: re-sent chunks; new data only while no other data sub is up
	name     string
	dead     bool
	dialed   bool // this end opened the connection
	replied  bool // the server answered the header (always true on the server side)
	born     time.Time
	lastRecv time.Time // any frame from the peer on it
	// send side (session lock)
	sentB, rcvdB int64 // DATA bytes written to it / the peer reports received on it
	samples      []sample
	rate, avg    float64 // delivery rate (bytes/s): max of 1 s windows / 2 s average
	loops        []loopSample
	loopBase     time.Duration // lowest feedback loop this sub has seen (see addLoop)
	lastProg     time.Time     // the peer last reported more data received on it
	busySince    time.Time     // it last went from nothing in flight to something
	hist         [10]int64
	histN        int
	rate5        float64
	slowTicks    int
	snapGen      uint64 // last snapshot generation it wrote
	// receive side (session lock)
	inB  int64  // DATA bytes received on it
	echo uint64 // send time of the latest DATA received on it (peer clock)
}

type loopSample struct {
	t time.Time
	d time.Duration
}

const loopWin = 10 * time.Second

// chunk: one unit of the send stream, kept until the peer reports it received.
type chunk struct {
	data    []byte
	carrier *sub // last sub it was written to (nil: not sent yet)
	avoid   *sub // a re-send should not use this sub
	sentAt  time.Time
	due     time.Time // when its carrier should have reported it: max(one feedback loop, queue ahead of it at the carrier's rate)
	tries   int       // re-sends so far
	queued  bool      // waiting in newQ or urgQ
	resend  bool
	firstAt time.Time
	gapAt   time.Time     // when the peer's gap report made us re-send it (repair diagnostics)
	gapAge  time.Duration // how long the peer had waited for it then
	gapSent bool          // the gap re-send has been written to a sub
}

type session struct {
	e *Engine
	// cfg are the settings the session runs with: a copy taken when it was created,
	// of the engine's settings or of the ones its creator gave. It never changes, so
	// a session keeps what it began with when the settings of later ones change.
	cfg      Config
	id       [16]byte
	local    net.Conn // LAN side (nil on the server until the destination answers)
	isClient bool
	dstAddr  string
	owner    Owner  // whom the session belongs to; never changes
	hdrKey   []byte // the header key of the pair that created it; never changes
	mu       sync.Mutex
	cond     *sync.Cond
	subs     []*sub
	keyOut   []byte // MAC key for snapshots we send (nil = none)
	keyIn    []byte // ... for snapshots we receive
	started  time.Time
	// send side
	chunks     map[uint64]*chunk // sent or queued, not yet reported received
	unackedB   int               // bytes in chunks
	sackSize   map[uint64]int    // reported received above the peer's cumulative point
	sackB      int
	newQ, urgQ []uint64
	nextSeq    uint64
	peerCum    uint64
	sendWin    int // memory for unacked data
	peerBudget int // the peer's reorder budget (flow control)
	sendFin    int64
	// receive side
	pending    map[uint64][]byte
	pendAt     map[uint64]time.Duration // arrival, virtual clock
	pendingB   int
	cum        uint64 // next chunk to deliver
	highest    uint64 // one past the highest chunk received
	recvBudget int
	peerFin    int64
	rxDone     bool
	blocked    time.Duration // time blocked writing to the LAN side
	writingSet time.Time
	// feedback
	snapSeq     uint64
	snapGen     uint64
	snapAt      time.Time
	snapDirty   bool
	genData     int       // data subs that wrote the current generation
	pigAt       time.Time // last piggyback snapshot while a control sub is up
	lastPeerSeq uint64
	peerT0      time.Time
	peerTs0     uint64
	lastFb      time.Time
	fbStallTill time.Time // feedback stalled recently (a gap over 1 s with data in flight): deeper queues until then
	peerAllDone bool
	changeN     uint64    // bumped whenever our state changes (the UDP sender follows it)
	udp         *udpState // UDP feedback channel (nil = none)
	// lifecycle
	closed   bool
	everUp   bool
	cleanEnd bool
	endWhy   string // why the session ended, as shutdown was told
	// redials: the end that takes this session keeps the sub-connections it dials for
	// it at their numbers (it created the session for a request that asked for them,
	// or adopt took a function later). adopted is the one adopt took. A session this
	// end carries leaves it false and gives its function to the monitor.
	redials   bool
	adopted   func()
	attached  bool   // a connection that the other end opened became a sub-connection
	noReply   string // how the last sub-connection that the other end never answered ended
	lastPeer  time.Time
	nextIdx   uint16
	lastKick  time.Time
	peerGone  bool
	flaps     map[int]int       // per path / role: subs in a row that died young or could not be dialed (redial backoff)
	flapAt    map[int]time.Time // ... and when the last of them did
	tl        timeline
	st        sessStats
	lastStats time.Time
}

type timeline struct {
	at                   time.Time
	tx, re, ack, rx, dlv int64
	fbIn, fbOut, fbCtrl  int
	fbUDP                int
	pumpWin              time.Duration
	starve               time.Duration // data subs below their cap with nothing to send (summed over subs)
	loops                []time.Duration
}

type sessStats struct {
	txB, rxB, chunks, resends, kicks int64
	resentB                          int64 // bytes of the chunks queued for re-sending
	subsUp                           int64
	stall, pumpWin, write            time.Duration
	died                             [kChaos + 1]int64
	fbGapMax                         time.Duration
	fbGaps                           int64
	resentBy                         [nCause]int64
	// gap repairs: peer's wait when we decided, time in urgQ, decision -> release
	repAge, repWait, repDone []time.Duration
	repRetry                 int64 // gap re-sends that were not the first for their chunk
	truncSnaps               int64 // snapshots we built that could not hold every range
}

const (
	kLost = iota
	kSilent
	kWrite
	kSlow
	kChaos
)

const (
	cGap   = iota // reported missing in a snapshot
	cStall        // oldest chunk not reported received for long
	cDead         // its sub died
	nCause
)

var causeNames = [nCause]string{"gap", "stall", "dead"}

// newSession creates a session and registers it. dst is its destination address: it is
// set here, before the session can be found in the registry, because readers of the
// registry (the statistics log, SessionList) read it.
func (e *Engine) newSession(id [16]byte, local net.Conn, isClient bool, dst string, ks keys) *session {
	return e.newSessionFor(id, local, isClient, dst, ks, Owner{})
}

// newSessionFor is newSession for a session that belongs to owner. The owner and the
// header key are set here for the same reason as dst.
func (e *Engine) newSessionFor(id [16]byte, local net.Conn, isClient bool, dst string, ks keys, owner Owner) *session {
	return e.newSessionWith(nil, id, local, isClient, dst, ks, owner)
}

// newSessionWith is newSessionFor for a session that runs with the settings cfg; nil:
// the engine's.
func (e *Engine) newSessionWith(cfg *Config, id [16]byte, local net.Conn, isClient bool, dst string, ks keys, owner Owner) *session {
	if cfg == nil {
		cfg = &e.cfg
	}
	now := time.Now()
	if local != nil {
		ArmReset(local)
	}
	s := &session{e: e, cfg: *cfg, id: id, local: local, isClient: isClient, dstAddr: dst, owner: owner, hdrKey: ks.hdr, chunks: map[uint64]*chunk{}, sackSize: map[uint64]int{},
		pending: map[uint64][]byte{}, pendAt: map[uint64]time.Duration{}, sendWin: cfg.Window, peerBudget: cfg.Window,
		recvBudget: cfg.Window, sendFin: -1, peerFin: -1, started: now, lastPeer: now, snapAt: now}
	if ks.configured {
		if isClient {
			s.keyOut, s.keyIn = ks.snapC2S, ks.snapS2C
		} else {
			s.keyOut, s.keyIn = ks.snapS2C, ks.snapC2S
		}
	}
	s.cond = sync.NewCond(&s.mu)
	e.nSessions.Add(1)
	e.mSessions.Add(1)
	e.registry.Store(s, true)
	return s
}

// ofPair reports whether the session belongs to the pair of ends that the peer name and
// the keys stand for.
func (s *session) ofPair(peer string, ks keys) bool {
	return s.owner.Peer == peer && bytes.Equal(s.hdrKey, ks.hdr)
}

func (s *session) logf(f string, a ...interface{}) {
	if s.cfg.Verbose {
		log.Printf("[%x] "+f, append([]interface{}{s.id[:3]}, a...)...)
	}
}

// vnow: the receive side's virtual clock -- wall time minus time spent blocked writing
// to the LAN side, so a slow destination never looks like a late chunk.
func (s *session) vnow() time.Duration {
	v := time.Since(s.started) - s.blocked
	if !s.writingSet.IsZero() {
		v -= time.Since(s.writingSet)
	}
	return v
}

func nowNs() uint64 { return uint64(time.Since(t0)) }

// mark: our state changed, a new snapshot is due (lock held).
func (s *session) mark() {
	s.snapDirty = true
	s.changeN++
}

// ---------- subs ----------

// addSub makes c a sub-connection of the session. dialed says which end opened the
// connection: this one, or the other.
func (s *session) addSub(c net.Conn, idx uint16, path uint8, ctrl, repair bool, awaitReply, dialed bool) {
	now := time.Now()
	sb := &sub{c: c, w: bufio.NewWriterSize(c, 64<<10), wdead: s.cfg.Dead, idx: idx, path: path, ctrl: ctrl, repair: repair && !ctrl,
		name: c.LocalAddr().String(), born: now, lastRecv: now, replied: !awaitReply, dialed: dialed}
	if ctrl {
		sb.name += " (ctrl)"
	} else if sb.repair {
		sb.name += " (repair)"
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		c.Close()
		return
	}
	s.subs = append(s.subs, sb)
	s.st.subsUp++
	if !dialed {
		s.attached = true
	}
	s.cond.Broadcast()
	s.mu.Unlock()
	s.e.mSubsUp.Add(1)
	s.logf("sub up %s path %d (%d alive)", sb.name, path, s.alive())
	go s.reader(sb, awaitReply)
	go s.writer(sb)
}

func (s *session) counts() (data, ctrl int) {
	for _, x := range s.subs {
		if x.dead {
			continue
		}
		if x.ctrl {
			ctrl++
		} else {
			data++
		}
	}
	return
}

func (s *session) alive() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, c := s.counts()
	return d + c
}

func (s *session) killSub(sb *sub, why string, kind int) {
	s.mu.Lock()
	if sb.dead {
		s.mu.Unlock()
		return
	}
	sb.dead = true
	// a sub the server never answered was a failed dial, however long it hung (a relay that
	// accepts while its far side is cut)
	s.noteFlap(sb.path, sb.ctrl, sb.born, time.Since(sb.born) < 3*time.Second || !sb.replied)
	// everything it carried is gone with the connection: queue it again
	var re []uint64
	for q, c := range s.chunks {
		if c.carrier == sb && !c.queued {
			re = append(re, q)
		}
	}
	sort.Slice(re, func(i, j int) bool { return re[i] < re[j] })
	if !s.closed {
		s.requeue(re, nil, cDead)
	}
	s.st.died[kind]++
	if kind == kSlow {
		s.st.kicks++
	}
	d, c := s.counts()
	s.cond.Broadcast()
	s.mu.Unlock()
	sb.c.Close()
	switch kind {
	case kLost:
		s.e.mSubsLost.Add(1)
	case kSilent:
		s.e.mKickSilent.Add(1)
	case kWrite:
		s.e.mKickWrite.Add(1)
	case kSlow:
		s.e.mKickSlow.Add(1)
	}
	s.logf("sub down %s (%s), requeued %d, %d+%d alive", sb.name, why, len(re), d, c)
}

// noteFlap records how a sub of path / role ended: dying within 3 s of birth (or a failed
// dial) counts toward the redial backoff, a sub that lived resets it -- but only one dialed
// after the last flap: an old sub dying late says nothing about the path now (seen in a drill:
// old subs dying every few seconds kept resetting the backoff while every new one was refused
// at once, 388 dials a minute) (lock held).
func (s *session) noteFlap(path uint8, ctrl bool, born time.Time, young bool) {
	if s.flaps == nil {
		s.flaps, s.flapAt = map[int]int{}, map[int]time.Time{}
	}
	k := int(path) << 1
	if ctrl {
		k |= 1
	}
	if young {
		// subs that fail together (a relay dropping all of them, a refused session) are one
		// failure, not eight: count a flap only a second or more after the previous one
		if time.Since(s.flapAt[k]) >= time.Second {
			s.flaps[k]++
		}
		s.flapAt[k] = time.Now()
	} else if born.After(s.flapAt[k]) {
		s.flaps[k] = 0
	}
}

// redialWait: how long to wait between dials on path / role after flaps in a row:
// 0, then 1, 2, 4, 8 s (lock held). A sub of that path / role dialed after the last flap and
// alive for 5 s clears it; older subs do not (seen on a real path: while old subs kept running, every new
// one was reset at once by the relay, 2-4 dials a second for 20 s).
func (s *session) redialWait(path uint8, ctrl bool) time.Duration {
	k := int(path) << 1
	if ctrl {
		k |= 1
	}
	if s.flaps == nil {
		s.flaps, s.flapAt = map[int]int{}, map[int]time.Time{}
	}
	for _, x := range s.subs {
		if !x.dead && x.path == path && x.ctrl == ctrl && x.born.After(s.flapAt[k]) && time.Since(x.born) > 5*time.Second {
			s.flaps[k] = 0 // the path works again
		}
	}
	n := s.flaps[k]
	if n == 0 {
		return 0
	}
	return min(time.Second<<min(n-1, 3), 8*time.Second)
}

// bucket: a token bucket for redials, a hard limit on top of redialWait that does not depend on
// its bookkeeping (corner cases have reset the backoff before, once with 388 dials a minute
// to one relay).
type bucket struct {
	mu   sync.Mutex
	max  float64
	rate float64 // tokens per second
	n    float64
	at   time.Time
}

func newBucket(n int, per time.Duration) *bucket {
	return &bucket{max: float64(n), rate: float64(n) / per.Seconds(), n: float64(n), at: time.Now()}
}

// take grants up to want tokens and returns how many it granted.
func (b *bucket) take(want int) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.n = min(b.max, b.n+now.Sub(b.at).Seconds()*b.rate)
	b.at = now
	got := min(want, int(b.n))
	b.n -= float64(got)
	return got
}

// give returns tokens granted but not used.
func (b *bucket) give(k int) {
	b.mu.Lock()
	b.n = min(b.max, b.n+float64(k))
	b.mu.Unlock()
}

// requeue puts chunks at the front for re-sending (lock held).
func (s *session) requeue(qs []uint64, avoid *sub, cause int) int {
	var add []uint64
	for _, q := range qs {
		c := s.chunks[q]
		if c == nil || c.queued {
			continue
		}
		c.queued, c.resend, c.avoid = true, true, avoid
		c.tries++
		add = append(add, q)
	}
	if len(add) > 0 {
		s.urgQ = append(add, s.urgQ...)
		s.st.resends += int64(len(add))
		for _, q := range add {
			s.st.resentB += int64(len(s.chunks[q].data))
		}
		s.st.resentBy[cause] += int64(len(add))
		s.e.mResend.Add(int64(len(add)))
		s.e.resendBy[cause].Add(int64(len(add)))
	}
	return len(add)
}

// ---------- frames ----------

func (sb *sub) write(parts ...[]byte) error {
	sb.wmu.Lock()
	defer sb.wmu.Unlock()
	sb.c.SetWriteDeadline(time.Now().Add(sb.wdead))
	for _, p := range parts {
		if _, err := sb.w.Write(p); err != nil {
			return err
		}
	}
	return sb.w.Flush()
}

func dataHeader(seq, ts uint64, n int) []byte {
	h := make([]byte, 21)
	h[0] = fData
	binary.BigEndian.PutUint64(h[1:9], seq)
	binary.BigEndian.PutUint64(h[9:17], ts)
	binary.BigEndian.PutUint32(h[17:21], uint32(n))
	return h
}

func (s *session) snapFrame(sn *snapshot) []byte {
	body := sn.marshal()
	f := make([]byte, 3, 3+len(body)+macLen)
	f[0] = fSnap
	binary.BigEndian.PutUint16(f[1:3], uint16(len(body)+macLen))
	f = append(f, body...)
	if s.keyOut != nil {
		return append(f, mac(s.keyOut, body)...)
	}
	return append(f, make([]byte, macLen)...)
}

func (s *session) reader(sb *sub, awaitReply bool) {
	r := bufio.NewReaderSize(sb.c, 64<<10)
	if awaitReply {
		var rp [replyLen]byte
		sb.c.SetReadDeadline(time.Now().Add(30 * time.Second))
		if _, err := io.ReadFull(r, rp[:]); err != nil || string(rp[0:4]) != magic {
			// kept for whoever asks why the session does not come up (SessionInfo.NoReply)
			why := "the answer was not one of tcpraid"
			if err != nil {
				why = err.Error()
				if isReset(err) {
					// the same words on every system, for whoever reads them
					// (the systems word a reset differently)
					why = "connection reset by peer"
				}
			}
			s.mu.Lock()
			s.noReply = why
			s.mu.Unlock()
			s.killSub(sb, "no reply", kLost)
			return
		}
		sb.c.SetReadDeadline(time.Time{})
		if rp[4] != stOK {
			s.onRefused(sb, rp[4])
			return
		}
		s.mu.Lock()
		sb.replied = true
		s.everUp = true // the server holds the session: later subs rejoin it
		if mb := int(binary.BigEndian.Uint16(rp[6:8])) << 20; mb > 0 {
			s.peerBudget = min(s.peerBudget, mb)
		}
		s.mu.Unlock()
	}
	var h [21]byte
	for {
		if _, err := io.ReadFull(r, h[:1]); err != nil {
			s.killSub(sb, "read: "+err.Error(), kLost)
			return
		}
		var err error
		switch h[0] {
		case fData:
			if _, err = io.ReadFull(r, h[1:21]); err != nil {
				break
			}
			seq := binary.BigEndian.Uint64(h[1:9])
			ts := binary.BigEndian.Uint64(h[9:17])
			n := binary.BigEndian.Uint32(h[17:21])
			if n == 0 || n > maxData || sb.ctrl {
				err = errors.New("bad DATA frame")
				break
			}
			data := make([]byte, n)
			if _, err = io.ReadFull(r, data); err != nil {
				break
			}
			s.mu.Lock()
			if !s.closed {
				s.onData(sb, seq, ts, data)
			}
			s.mu.Unlock()
		case fSnap:
			if _, err = io.ReadFull(r, h[1:3]); err != nil {
				break
			}
			n := int(binary.BigEndian.Uint16(h[1:3]))
			if n <= macLen || n > maxSnap+macLen {
				err = errors.New("bad SNAP frame")
				break
			}
			b := make([]byte, n)
			if _, err = io.ReadFull(r, b); err != nil {
				break
			}
			body, tag := b[:n-macLen], b[n-macLen:]
			if s.keyIn != nil && !macOK(s.keyIn, body, tag) {
				s.e.mBadSnap.Add(1)
				err = errors.New("snapshot MAC")
				break
			}
			sn, perr := parseSnapshot(body)
			if perr != nil {
				s.e.mBadSnap.Add(1)
				err = perr
				break
			}
			s.mu.Lock()
			if !s.closed {
				s.onSnapshot(sb, sn)
			}
			s.mu.Unlock()
		case fAbort:
			s.shutdown("peer aborted", true)
			return
		case fDialFail:
			s.shutdown(WhyUnreachable+s.dstAddr, true)
			return
		default:
			err = fmt.Errorf("bad frame type %d", h[0])
		}
		if err != nil {
			s.killSub(sb, err.Error(), kLost)
			return
		}
	}
}

func (s *session) onRefused(sb *sub, st byte) {
	s.mu.Lock()
	txDone := s.sendFin >= 0 && s.peerCum >= uint64(s.sendFin)
	clean := st == stGoneOK && txDone && s.peerFin >= 0 && s.cum >= uint64(s.peerFin)
	s.mu.Unlock()
	switch {
	case clean:
		s.mu.Lock()
		s.peerGone = true
		s.cond.Broadcast()
		s.mu.Unlock()
		s.killSub(sb, "peer finished", kLost)
	case st == stRefused:
		s.killSub(sb, "server refused (session limit)", kLost)
		s.shutdown(WhyRefused, true)
	default:
		s.killSub(sb, fmt.Sprintf("server status %d", st), kLost)
		s.shutdown(WhyNoSession, true)
	}
}

// ---------- receive side ----------

// onData stores one chunk (lock held). Rule 2: a chunk below the highest one received
// fills a hole and is always taken; a new highest one only within the reorder budget.
// Rule 1: whatever is taken is kept until delivered (snapshots report it as received).
func (s *session) onData(sb *sub, seq, ts uint64, data []byte) {
	now := time.Now()
	sb.lastRecv = now
	s.lastPeer = now
	s.everUp = true
	n := len(data)
	sb.inB += int64(n)
	sb.echo = ts
	s.st.rxB += int64(n)
	s.tl.rx += int64(n)
	s.mark()
	if seq < s.cum || s.pending[seq] != nil {
		s.e.mRxDup.Add(1)
		return
	}
	if seq >= s.highest && s.pendingB+n > s.recvCap()+maxData {
		s.e.mRxDrop.Add(1) // the peer overran our budget: it re-sends later
		return
	}
	s.pending[seq] = data
	s.pendAt[seq] = s.vnow()
	s.pendingB += n
	s.e.recvB.Add(int64(n))
	if seq >= s.highest {
		s.highest = seq + 1
	}
	s.cond.Broadcast()
}

// deliverLocal writes in-order data to the LAN side.
func (s *session) deliverLocal() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if s.closed {
			return
		}
		if d := s.pending[s.cum]; d != nil {
			delete(s.pending, s.cum)
			delete(s.pendAt, s.cum)
			s.pendingB -= len(d)
			s.e.recvB.Add(-int64(len(d)))
			s.tl.dlv += int64(len(d))
			s.cum++
			s.mark()
			s.cond.Broadcast()
			s.writingSet = time.Now()
			s.mu.Unlock()
			_, err := s.local.Write(d)
			s.mu.Lock()
			w := time.Since(s.writingSet)
			s.blocked += w
			s.writingSet = time.Time{}
			if w > 50*time.Millisecond {
				s.st.stall += w
				s.e.mStallMs.Add(w.Milliseconds())
			}
			if err != nil {
				s.mu.Unlock()
				s.abort("local write: " + err.Error())
				s.mu.Lock()
				return
			}
			continue
		}
		if s.peerFin >= 0 && s.cum >= uint64(s.peerFin) {
			if tc, ok := s.local.(*net.TCPConn); ok {
				tc.CloseWrite()
			}
			s.rxDone = true
			s.mark()
			s.cond.Broadcast()
			return
		}
		waitTimeout(s.cond, 100*time.Millisecond)
	}
}

// buildSnapshot: our complete state (lock held).
func (s *session) buildSnapshot() *snapshot {
	s.snapSeq++
	sn := &snapshot{seqNo: s.snapSeq, sentNs: nowNs(), cum: s.cum, fin: s.sendFin, rxDone: s.rxDone}
	sn.allDone = s.rxDone && s.sendFin >= 0 && s.peerCum >= uint64(s.sendFin)
	if c := s.recvCap(); s.pendingB < c {
		sn.budget = uint64(c)
	} else {
		sn.budget = uint64(s.pendingB)
	}
	if len(s.pending) > 0 {
		keys := make([]uint64, 0, len(s.pending))
		for q := range s.pending {
			keys = append(keys, q)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		// ranges, each with its earliest arrival; then gaps aged by the earliest arrival
		// of anything above them (how long later chunks have waited for the gap)
		type rr struct {
			r     rng
			first time.Duration
		}
		var rs []rr
		for _, q := range keys {
			a := s.pendAt[q]
			if n := len(rs); n > 0 && rs[n-1].r.hi == q {
				rs[n-1].r.hi = q + 1
				rs[n-1].first = min(rs[n-1].first, a)
				continue
			}
			rs = append(rs, rr{rng{q, q + 1}, a})
		}
		if len(rs) > maxRanges {
			rs = rs[:maxRanges] // truncated: the sender infers nothing beyond the last range
			s.st.truncSnaps++
		}
		vn := s.vnow()
		suffix := make([]time.Duration, len(rs))
		for i := len(rs) - 1; i >= 0; i-- {
			suffix[i] = rs[i].first
			if i+1 < len(rs) && suffix[i+1] < suffix[i] {
				suffix[i] = suffix[i+1]
			}
		}
		prev := s.cum
		for i, x := range rs {
			sn.ranges = append(sn.ranges, x.r)
			if x.r.lo > prev && len(sn.gaps) < maxGaps {
				sn.gaps = append(sn.gaps, gap{prev, x.r.lo, uint64((vn - suffix[i]).Milliseconds())})
			}
			prev = x.r.hi
		}
	}
	for _, x := range s.subs {
		if !x.dead && x.inB > 0 {
			sn.subs = append(sn.subs, subRx{idx: x.idx, bytes: uint64(x.inB), echo: x.echo})
		}
	}
	return sn
}

// recvCap: the reorder budget we offer the peer: -window, but out-of-order data held by all
// sessions together stays within -budget (each session keeps room for two chunks so it
// always progresses). With -budget also bounding unacked data, the process holds at most
// about 2 x -budget of stream data (lock held).
func (s *session) recvCap() int {
	others := int(s.e.recvB.Load()) - s.pendingB
	return max(min(s.recvBudget, s.cfg.Budget-others), 2*s.cfg.Chunk)
}

// sendAbort tells the peer at once, on every channel, that this session is being reset:
// an ABORT frame on each sub and a few UDP snapshots flagged aborted, so the peer frees the
// session now instead of holding it for -grace.
func (s *session) sendAbort() {
	s.mu.Lock()
	var body []byte
	u := s.udp
	if u != nil && !s.closed {
		sn := s.buildSnapshot()
		sn.aborted = true
		body = sn.marshal()
	}
	s.mu.Unlock()
	if body != nil {
		for i := 0; i < 3; i++ {
			s.udpWrite(u, body)
		}
	}
	s.broadcastFrame(fAbort)
}

// ---------- send side: feedback ----------

// onSnapshot applies the peer's newest state (lock held).
// sb is nil for a snapshot that came over UDP.
func (s *session) onSnapshot(sb *sub, sn *snapshot) {
	if sn.aborted {
		go s.shutdown("peer aborted", true)
		return
	}
	now := time.Now()
	s.lastPeer = now
	s.everUp = true
	s.tl.fbIn++
	switch {
	case sb == nil:
		s.tl.fbUDP++
	case sb.ctrl:
		sb.lastRecv = now
		s.tl.fbCtrl++
	default:
		sb.lastRecv = now
	}
	if sn.seqNo <= s.lastPeerSeq {
		return // an older copy from a slower channel: the newest wins
	}
	s.lastPeerSeq = sn.seqNo
	if !s.lastFb.IsZero() && s.unackedB > 0 {
		g := now.Sub(s.lastFb)
		s.st.fbGapMax = max(s.st.fbGapMax, g)
		if g > time.Second {
			s.st.fbGaps++
			s.e.mFbGaps.Add(1)
			s.fbStallTill = now.Add(10 * time.Second)
		}
	}
	s.lastFb = now
	at := s.peerTime(sn.sentNs)
	// delivered in order: free
	for q := s.peerCum; q < sn.cum; q++ {
		s.release(q)
		if n, ok := s.sackSize[q]; ok {
			s.sackB -= n
			delete(s.sackSize, q)
		}
	}
	if sn.cum > s.peerCum {
		s.peerCum = sn.cum
	}
	// received out of order: free too (rule 1 on the peer: it never drops them)
	for _, r := range sn.ranges {
		for q := r.lo; q < r.hi; q++ {
			if c := s.chunks[q]; c != nil {
				n := len(c.data)
				s.release(q)
				s.sackSize[q] = n
				s.sackB += n
			}
		}
	}
	for _, x := range sn.subs {
		for _, y := range s.subs {
			if y.idx == x.idx && !y.dead {
				s.onRcv(y, int64(x.bytes), at)
				if x.echo > 0 {
					if l := time.Duration(nowNs() - x.echo); l > 0 && l < time.Minute {
						s.addLoop(y, l)
					}
				}
				break
			}
		}
	}
	// missing chunks: re-send each one alone, on another sub, once it is clearly late
	for _, g := range sn.gaps {
		age := time.Duration(g.ageMs) * time.Millisecond
		var late []uint64
		for q := g.lo; q < g.hi; q++ {
			c := s.chunks[q]
			if c == nil || c.queued || c.carrier == nil {
				continue
			}
			// the receiver measured how long later chunks have waited for this one (its
			// virtual clock); -hol beyond the queue depth, per attempt, and at least one
			// loop since we last sent it
			// chunks on different subs legitimately arrive up to about one queue depth
			// (targetNow) apart, so the wait counts beyond that
			if age > (s.cfg.Hol+s.targetNow())*time.Duration(c.tries+1) && now.Sub(c.sentAt) > loopOf(c.carrier) {
				late = append(late, q)
			}
		}
		for _, q := range late {
			c := s.chunks[q]
			if c.tries > 0 {
				s.st.repRetry++
			}
			c.gapAt, c.gapAge, c.gapSent = now, age, false
			s.requeue([]uint64{q}, c.carrier, cGap)
		}
	}
	if sn.budget > 0 {
		s.peerBudget = int(min(sn.budget, uint64(1)<<40))
	}
	if sn.fin >= 0 && s.peerFin < 0 {
		s.peerFin = sn.fin
	}
	s.peerAllDone = sn.allDone
	s.cond.Broadcast()
}

// release frees a chunk the peer has (lock held).
func (s *session) release(q uint64) {
	c := s.chunks[q]
	if c == nil {
		return
	}
	n := len(c.data)
	s.unackedB -= n
	s.e.globalB.Add(-int64(n))
	s.tl.ack += int64(n)
	if !c.gapAt.IsZero() {
		addDur(&s.st.repAge, c.gapAge)
		addDur(&s.st.repDone, time.Since(c.gapAt))
	}
	delete(s.chunks, q)
}

// addDur keeps up to 4096 samples for the session diagnostics.
func addDur(v *[]time.Duration, d time.Duration) {
	if len(*v) < 4096 {
		*v = append(*v, d)
	}
}

// margin: one more feedback loop of the carrier for each re-send so far.
func (s *session) margin(c *chunk) time.Duration {
	return loopOf(c.carrier) * time.Duration(c.tries)
}

func loopOf(sb *sub) time.Duration {
	if sb == nil || sb.loopBase <= 0 {
		return 300 * time.Millisecond
	}
	return sb.loopBase
}

// peerTime maps a peer timestamp onto our clock, anchored on the fastest-arriving
// snapshot so far: never in the future, and snapshots that waited in a stalled path
// keep the spacing they were sent with (lock held).
func (s *session) peerTime(ts uint64) time.Time {
	now := time.Now()
	if s.peerT0.IsZero() || ts < s.peerTs0 {
		s.peerT0, s.peerTs0 = now, ts
		return now
	}
	at := s.peerT0.Add(time.Duration(ts - s.peerTs0))
	if at.After(now) {
		s.peerT0 = s.peerT0.Add(-at.Sub(now))
		at = now
	}
	return at
}

// onRcv: the peer received v bytes on sb as of at (its clock on ours) -> rate (lock held).
func (s *session) onRcv(sb *sub, v int64, at time.Time) {
	if v <= sb.rcvdB || v > sb.sentB {
		return
	}
	sb.rcvdB = v
	sb.lastProg = time.Now()
	sb.samples = append(sb.samples, sample{at, v})
	cut := 0
	for cut < len(sb.samples)-2 && at.Sub(sb.samples[cut].t) > 3*time.Second {
		cut++
	}
	sb.samples = sb.samples[cut:]
	k := len(sb.samples) - 1
	i := k
	for i > 0 && at.Sub(sb.samples[i-1].t) <= 2*time.Second {
		i--
	}
	if i == k && k > 0 {
		i = k - 1
	}
	if dt := sb.samples[k].t.Sub(sb.samples[i].t); i < k && dt >= 200*time.Millisecond {
		sb.avg = float64(sb.samples[k].b-sb.samples[i].b) / dt.Seconds()
	}
	best := 0.0
	for j := len(sb.samples) - 1; j > 0; j-- {
		for i := j - 1; i >= 0; i-- {
			dt := sb.samples[j].t.Sub(sb.samples[i].t)
			if dt >= time.Second || (i == 0 && dt >= 200*time.Millisecond) {
				if r := float64(sb.samples[j].b-sb.samples[i].b) / dt.Seconds(); r > best {
					best = r
				}
				break
			}
		}
	}
	if best > 0 {
		sb.rate = best
	}
}

func (s *session) addLoop(sb *sub, d time.Duration) {
	now := time.Now()
	sb.loops = append(sb.loops, loopSample{now, d})
	cut := 0
	for cut < len(sb.loops)-1 && now.Sub(sb.loops[cut].t) > loopWin {
		cut++
	}
	sb.loops = sb.loops[cut:]
	// the base is the lowest loop ever seen on this sub, not a recent minimum: every sample
	// includes the queue our own data built on the sub, so a recent minimum creeps up with
	// it (a deeper cap, a longer queue, a higher "base" -- 1.6-2.9 s on a 0.3 s lab path).
	// A fresh sub starts with an empty queue; subs are replaced now and then, so a path
	// whose round trip grows is picked up by its new subs.
	if sb.loopBase == 0 || d < sb.loopBase {
		sb.loopBase = d
	}
	if len(s.tl.loops) < 4096 {
		s.tl.loops = append(s.tl.loops, d)
	}
}

// capB: bytes one data sub may hold in flight -- its delivery rate times its base
// feedback loop plus -target, at most its rate share of the window (lock held).
func (s *session) capB(sb *sub) int64 {
	chunkN := int64(s.cfg.Chunk)
	if sb.rate <= 0 {
		return 4 * chunkN
	}
	l := sb.loopBase
	if l <= 0 {
		l = 300 * time.Millisecond
	}
	c := int64(sb.rate * (l + s.targetNow()).Seconds())
	if s.cfg.Share && sb.avg > 0 {
		var sum float64
		for _, x := range s.subs {
			if !x.dead && !x.ctrl {
				if x.avg > 0 {
					sum += x.avg
				} else {
					sum += sb.avg
				}
			}
		}
		if sh := int64(float64(s.sendWin) * sb.avg / sum); sh < c {
			c = sh
		}
	}
	return min(max(c, chunkN), int64(s.sendWin))
}

// targetNow: how much time's worth each data sub may hold beyond its feedback loop. While
// feedback arrives promptly, shallow queues win (-target; on a real path 0.3 s beat 1 s); for 10 s
// after feedback stalled (a gap over 1 s with data in flight -- typically feedback that
// rides on data subs whose reverse direction is lossy), deeper queues keep data moving
// across the gaps (-pigtarget). Decided by what feedback does, not by which
// channels exist (lock held).
func (s *session) targetNow() time.Duration {
	if time.Now().Before(s.fbStallTill) {
		return s.cfg.PigTarget
	}
	return s.cfg.Target
}

// inflight: data sb holds that the peer has not reported. While feedback is stale for the
// whole session (no snapshot on any channel for over max(2 loops, 500 ms)), the silence
// says nothing about any one sub -- the feedback direction is what failed -- so each sub is
// credited with delivery at its measured rate since the last snapshot and keeps sending;
// the window still bounds the total. A single sub that stops while feedback keeps coming
// on other channels gets no credit (lock held).
func (s *session) inflight(sb *sub) int64 {
	f := sb.sentB - sb.rcvdB
	if s.lastFb.IsZero() || sb.rate <= 0 {
		return f
	}
	stale := time.Since(s.lastFb)
	if stale > max(2*loopOf(sb), 500*time.Millisecond) {
		credit := int64(sb.rate * min(stale, 10*time.Second).Seconds())
		f = max(f-credit, 0)
	}
	return f
}

// queueTime: how long what sb holds takes to drain at its rate (lock held).
func (s *session) queueTime(sb *sub) time.Duration {
	if sb.rate <= 0 {
		return time.Hour
	}
	return time.Duration(float64(s.inflight(sb)) / sb.rate * float64(time.Second))
}

// takeData picks the next chunk for data sub sb (lock held). A re-send goes first and
// ignores the cap, but only to a sub whose queue drains about as soon as the shortest
// one (it would wait behind that queue), never back to the sub that was late with it.
// Then new data within the cap.
func (s *session) takeData(sb *sub) (uint64, *chunk, int) {
	capN := s.capB(sb)
	infl := s.inflight(sb)
	why := wEmpty
	if len(s.urgQ) > 0 {
		mine := s.queueTime(sb)
		best := time.Duration(-1)
		for _, x := range s.subs {
			if !x.dead && !x.ctrl {
				if t := s.queueTime(x); best < 0 || t < best {
					best = t
				}
			}
		}
		short := mine <= best+best/4+50*time.Millisecond
		for i := 0; i < len(s.urgQ) && i < 32; {
			q := s.urgQ[i]
			c := s.chunks[q]
			if c == nil {
				s.urgQ = dropAt(s.urgQ, i)
				continue
			}
			if c.avoid == sb || !short || (c.avoid != nil && sb.path == c.avoid.path && s.shortElsewhere(c.avoid.path, best)) {
				i++
				continue
			}
			s.urgQ = dropAt(s.urgQ, i)
			if !c.gapAt.IsZero() && !c.gapSent {
				c.gapSent = true
				addDur(&s.st.repWait, time.Since(c.gapAt))
			}
			s.assign(sb, q, c)
			return q, c, 0
		}
	}
	if sb.repair && s.dataUp() {
		return 0, nil, why
	}
	for len(s.newQ) > 0 {
		q := s.newQ[0]
		c := s.chunks[q]
		if c == nil {
			s.newQ = s.newQ[1:]
			continue
		}
		if infl >= capN {
			return 0, nil, wCap
		}
		s.newQ = s.newQ[1:]
		s.assign(sb, q, c)
		return q, c, 0
	}
	return 0, nil, why
}

// shortElsewhere: a data or repair sub on another path than p drains about as soon as the
// shortest queue -- a chunk that went missing on path p is re-sent there instead (a loss on
// one path tends to repeat on it: in one test 13% of the re-sends that stayed on the path of
// the loss needed a second try) (lock held).
func (s *session) shortElsewhere(p uint8, best time.Duration) bool {
	for _, x := range s.subs {
		if !x.dead && !x.ctrl && x.path != p && s.queueTime(x) <= best+best/4+50*time.Millisecond {
			return true
		}
	}
	return false
}

// dataUp: some ordinary (not repair) data sub is up and the server answered it; repair subs
// then carry only re-sends. A dialed sub still waiting for its answer does not count: while a
// data path accepts connections but forwards nothing, its fresh subs kept the repair path to
// re-sends (lab datasilent: half its capacity) (lock held).
func (s *session) dataUp() bool {
	for _, x := range s.subs {
		if !x.dead && !x.ctrl && !x.repair && x.replied {
			return true
		}
	}
	return false
}

func (s *session) assign(sb *sub, q uint64, c *chunk) {
	now := time.Now()
	c.queued = false
	c.carrier = sb
	c.sentAt = now
	ahead := time.Duration(0)
	if sb.rate > 0 {
		ahead = time.Duration(float64(s.inflight(sb)) / sb.rate * float64(time.Second))
	}
	// what the sub holds was sent over the last (ahead) seconds; about one loop's worth of
	// it has already arrived and is only not reported yet, so this chunk is due after the
	// longer of one loop and the queue ahead of it
	c.due = now.Add(min(max(ahead, loopOf(sb)), 10*time.Second))
	if sb.sentB == sb.rcvdB {
		sb.busySince = now
	}
	sb.sentB += int64(len(c.data))
	if c.resend {
		s.tl.re += int64(len(c.data))
	} else {
		c.firstAt = now
		s.tl.tx += int64(len(c.data))
		s.st.chunks++
		s.st.txB += int64(len(c.data))
		s.e.mChunksTx.Add(1)
		s.e.mBytesTx.Add(int64(len(c.data)))
	}
}

func dropAt(q []uint64, i int) []uint64 {
	if i == 0 {
		return q[1:]
	}
	return append(q[:i], q[i+1:]...)
}

// ---------- writer ----------

const (
	wEmpty = iota
	wCap
	nWait
)

var waitNames = [nWait]string{"empty", "cap"}

// snapDue: start a new snapshot generation when something changed (at most every
// -fbevery) or once a second; decide whether sb writes it: every control sub does;
// data subs only as the fallback -- one per second while a control sub is up, else on
// -fbsubs of them (lock held).
func (s *session) snapDue(sb *sub, now time.Time) bool {
	if (s.snapDirty && now.Sub(s.snapAt) >= s.cfg.FbEvery) || now.Sub(s.snapAt) >= time.Second {
		s.snapGen++
		s.snapAt = now
		s.snapDirty = false
		s.genData = 0
	}
	if sb.snapGen >= s.snapGen {
		return false
	}
	if sb.ctrl {
		return true
	}
	if _, nc := s.counts(); nc > 0 || s.udpUp() {
		return now.Sub(s.pigAt) >= time.Second
	}
	return s.genData < max(s.cfg.FbSubs, 1)
}

func (s *session) writer(sb *sub) {
	var wrote time.Duration
	for {
		s.mu.Lock()
		s.st.write += wrote
		wrote = 0
		var frame [][]byte
		isData, isSnap := false, false
		for {
			if s.closed || sb.dead {
				s.mu.Unlock()
				return
			}
			now := time.Now()
			if s.snapDue(sb, now) {
				sn := s.buildSnapshot()
				sb.snapGen = s.snapGen
				if !sb.ctrl {
					s.genData++
					s.pigAt = now
				}
				frame = [][]byte{s.snapFrame(sn)}
				isSnap = true
				break
			}
			why := wEmpty
			if !sb.ctrl {
				q, c, w := s.takeData(sb)
				if c != nil {
					frame = [][]byte{dataHeader(q, nowNs(), len(c.data)), c.data}
					isData = true
					break
				}
				why = w
			}
			wait := 100 * time.Millisecond
			if s.snapDirty {
				wait = s.cfg.FbEvery
			}
			waitTimeout(s.cond, wait)
			if !sb.ctrl {
				s.e.waitUs[why].Add(time.Since(now).Microseconds())
				if why == wEmpty {
					s.tl.starve += time.Since(now)
				}
			}
		}
		s.mu.Unlock()
		t := time.Now()
		err := sb.write(frame...)
		if isData {
			wrote = time.Since(t)
			s.e.writeUs.Add(wrote.Microseconds())
		}
		if isSnap {
			s.mu.Lock()
			s.tl.fbOut++
			s.mu.Unlock()
		}
		if err != nil {
			kind := kLost
			if errors.Is(err, os.ErrDeadlineExceeded) {
				kind = kWrite
			}
			s.killSub(sb, "write: "+err.Error(), kind)
			return
		}
	}
}

func waitTimeout(c *sync.Cond, d time.Duration) {
	t := time.AfterFunc(d, func() { c.L.Lock(); c.Broadcast(); c.L.Unlock() })
	c.Wait()
	t.Stop()
}

// ---------- LAN side ----------

// pumpLocal reads the LAN connection into chunks: within our send window (memory),
// the peer's reorder budget (everything beyond its cumulative point) and -budget.
// A short read is topped up for up to -coalesce.
func (s *session) pumpLocal() {
	for {
		s.mu.Lock()
		w0 := time.Now()
		for !s.closed && (s.unackedB+s.cfg.Chunk > s.sendWin || s.unackedB+s.sackB+s.cfg.Chunk > s.peerBudget ||
			(s.unackedB > 0 && s.e.globalB.Load()+int64(s.cfg.Chunk) > int64(s.cfg.Budget))) {
			waitTimeout(s.cond, 100*time.Millisecond)
		}
		pw := time.Since(w0)
		s.st.pumpWin += pw
		s.tl.pumpWin += pw
		if s.closed {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		buf := make([]byte, s.cfg.Chunk)
		n, err := s.local.Read(buf)
		if err == nil && n < len(buf) && s.cfg.Coalesce > 0 {
			s.local.SetReadDeadline(time.Now().Add(s.cfg.Coalesce))
			for n < len(buf) {
				m, e := s.local.Read(buf[n:])
				n += m
				if e != nil {
					if !errors.Is(e, os.ErrDeadlineExceeded) {
						err = e
					}
					break
				}
			}
			s.local.SetReadDeadline(time.Time{})
		}
		data := buf[:n]
		if n > 0 && n < len(buf)/2 {
			data = append([]byte(nil), buf[:n]...)
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		if n > 0 {
			q := s.nextSeq
			s.nextSeq++
			s.chunks[q] = &chunk{data: data, queued: true}
			s.unackedB += n
			s.e.globalB.Add(int64(n))
			s.newQ = append(s.newQ, q)
		}
		if err != nil && err != io.EOF {
			s.mu.Unlock()
			s.abort("local read: " + err.Error()) // a reset is never a clean end
			return
		}
		if err != nil {
			s.sendFin = int64(s.nextSeq)
			s.mark()
			s.cond.Broadcast()
			s.mu.Unlock()
			return
		}
		s.cond.Broadcast()
		s.mu.Unlock()
	}
}

// ---------- monitor ----------

func (s *session) slowSub(live []*sub) *sub {
	for _, x := range live {
		x.hist[x.histN%len(x.hist)] = x.rcvdB
		x.histN++
	}
	var worst *sub
	byPath := map[uint8][]*sub{}
	for _, x := range live {
		byPath[x.path] = append(byPath[x.path], x)
	}
	for _, group := range byPath { // slow only against subs on the same path
		var el []*sub
		for _, x := range group {
			if x.histN >= len(x.hist) && time.Since(x.born) > 10*time.Second {
				x.rate5 = float64(x.rcvdB-x.hist[x.histN%len(x.hist)]) / (float64(len(x.hist)-1) / 2)
				el = append(el, x)
			} else {
				x.slowTicks = 0
			}
		}
		if len(el) < 3 {
			continue
		}
		rs := make([]float64, len(el))
		for i, x := range el {
			rs[i] = x.rate5
		}
		sort.Float64s(rs)
		med := rs[len(rs)/2]
		for _, x := range el {
			busy := x.sentB-x.rcvdB >= s.capB(x)/2
			if busy && med >= float64(s.cfg.KickFloor) && x.rate5 < med/s.cfg.KickRatio {
				x.slowTicks++
			} else {
				x.slowTicks = 0
			}
			if x.slowTicks >= 20 && (worst == nil || x.rate5 < worst.rate5) {
				worst = x
			}
		}
	}
	if worst == nil || time.Since(s.lastKick) < 10*time.Second {
		return nil
	}
	s.lastKick = time.Now()
	worst.slowTicks = 0
	return worst
}

func lastOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// monitor: dead / slow subs, stalled chunks, session end, grace, redial.
func (s *session) monitor(redial func()) {
	tk := time.NewTicker(500 * time.Millisecond)
	defer tk.Stop()
	lastChaos := time.Now()
	finAt := time.Time{}
	for range tk.C {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		var stale, data []*sub
		live := s.subs[:0]
		for _, x := range s.subs {
			if x.dead {
				continue
			}
			live = append(live, x)
			silent := time.Since(x.lastRecv) > s.cfg.Dead
			if x.ctrl {
				if silent { // snapshots flow both ways at least once a second
					stale = append(stale, x)
				}
				continue
			}
			data = append(data, x)
			// a data sub is judged by the progress the peer reports, never by its own
			// reverse direction (which may be stalled while the forward one delivers)
			if silent && x.sentB > x.rcvdB && time.Since(lastOf(x.lastProg, x.busySince)) > s.cfg.Dead {
				stale = append(stale, x)
			}
		}
		s.subs = live
		slow := s.slowSub(data)
		// the oldest chunks: re-send one that its carrier has held far too long (a tail
		// loss has no later chunk to reveal it; a stuck sub may never report) -- only
		// while feedback flows: stale feedback says nothing about the chunk
		fbFresh := !s.lastFb.IsZero() && time.Since(s.lastFb) < time.Second
		for q, n := s.peerCum, 0; fbFresh && q < s.nextSeq && n < 64; q, n = q+1, n+1 {
			c := s.chunks[q]
			if c == nil || c.queued || c.carrier == nil {
				continue
			}
			if time.Now().After(c.due.Add(3*loopOf(c.carrier) + time.Second + s.margin(c))) {
				s.requeue([]uint64{q}, c.carrier, cStall)
			}
		}
		txDone := s.sendFin >= 0 && s.peerCum >= uint64(s.sendFin)
		allDone := txDone && s.rxDone
		if allDone && finAt.IsZero() {
			finAt = time.Now()
			s.mark()
			s.cond.Broadcast()
		}
		finish := allDone && (s.peerAllDone || len(live) == 0) && time.Since(finAt) > 200*time.Millisecond
		gone := s.peerGone && s.rxDone
		zero := time.Since(s.lastPeer) > s.grace()
		// a new session the server never answered (wrong key, wrong address, server down):
		// tell the application now rather than after -grace
		neverUp := s.isClient && !s.everUp && time.Since(s.started) > 30*time.Second
		zeroClean := zero && s.rxDone && txDone
		var victim *sub
		if s.cfg.Chaos > 0 && s.isClient && len(data) > 1 && time.Since(lastChaos) > s.cfg.Chaos {
			lastChaos = time.Now()
			victim = data[time.Now().UnixNano()%int64(len(data))]
		}
		tl := ""
		if s.cfg.Timeline > 0 && time.Since(s.tl.at) >= s.cfg.Timeline {
			tl = s.timelineLocked(data)
		}
		adopted := s.adopted
		s.mu.Unlock()
		if tl != "" {
			log.Printf("[%x] tl %s", s.id[:3], tl)
		}
		for _, x := range stale {
			s.killSub(x, "silent", kSilent)
		}
		if slow != nil {
			log.Printf("[%x] kick slow sub: %s delivers %.0f KB/s", s.id[:3], slow.name, slow.rate5/1024)
			s.killSub(slow, "slow", kSlow)
		}
		if victim != nil {
			s.killSub(victim, "chaos", kChaos)
		}
		switch {
		case finish:
			time.Sleep(200 * time.Millisecond) // let the last snapshot leave
			s.shutdown("done", false)
			return
		case gone:
			s.shutdown("done (peer already finished)", false)
			return
		case zeroClean:
			s.shutdown("done (peer silent after the end)", false)
			return
		case zero:
			s.shutdown(WhyGrace, true)
			return
		case neverUp:
			s.shutdown(WhyNoAnswer, true)
			return
		}
		if !allDone && !s.peerGone {
			if redial != nil {
				redial()
			}
			if adopted != nil {
				adopted()
			}
		}
	}
}

// adopt gives the session a function that keeps the sub-connections this end dials at
// their numbers, for a session whose monitor was given none: the monitor calls it from
// then on, as it calls the one it was given. It reports whether the session took it: a
// session takes one such function in its life.
func (s *session) adopt(redial func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.redials {
		return false
	}
	s.redials, s.adopted = true, redial
	return true
}

// timelineLocked: one -timeline line (lock held): MB/s new / re-sent / freed / received /
// delivered; MB unacked, reorder backlog, in flight on subs, reported-received above the
// peer's cumulative point; feedback age, frames in / out / in on control subs; subs at
// their cap; oldest waiting chunk (s); share of the interval the LAN reader waited for
// window or budget; feedback loop p50 / p95 (ms) over the interval.
func (s *session) timelineLocked(data []*sub) string {
	now := time.Now()
	from := s.tl.at
	if from.IsZero() {
		from = s.started
	}
	dt := now.Sub(from).Seconds()
	mb := func(b int64) float64 { return float64(b) / (1 << 20) / dt }
	atCap, infl := 0, int64(0)
	for _, x := range data {
		f := x.sentB - x.rcvdB
		infl += f
		if f >= s.capB(x) {
			atCap++
		}
	}
	fbAge := -1.0
	if !s.lastFb.IsZero() {
		fbAge = now.Sub(s.lastFb).Seconds()
	}
	hol := 0.0
	if len(s.pendAt) > 0 {
		v := s.vnow()
		o := v
		for _, t := range s.pendAt {
			o = min(o, t)
		}
		hol = (v - o).Seconds()
	}
	p50, p95 := pctDur(s.tl.loops, 0.5), pctDur(s.tl.loops, 0.95)
	var backlog int64 // read from the LAN, not sent yet
	for _, q := range s.newQ {
		if c := s.chunks[q]; c != nil {
			backlog += int64(len(c.data))
		}
	}
	line := fmt.Sprintf("t=%.1f tx=%.1f re=%.1f ack=%.1f rx=%.1f dlv=%.1f unacked=%d unsent=%d pend=%d infl=%d sacked=%d fbage=%.2f fb=%d/%d fbc=%d fbu=%d cap=%d/%d holage=%.2f pumpwait=%.2f starve=%.2f loop=%d/%d",
		now.Sub(s.started).Seconds(), mb(s.tl.tx), mb(s.tl.re), mb(s.tl.ack), mb(s.tl.rx), mb(s.tl.dlv),
		s.unackedB>>20, backlog>>20, s.pendingB>>20, infl>>20, s.sackB>>20, fbAge, s.tl.fbIn, s.tl.fbOut, s.tl.fbCtrl, s.tl.fbUDP,
		atCap, len(data), hol, s.tl.pumpWin.Seconds()/dt, s.tl.starve.Seconds()/dt/float64(max(len(data), 1)), p50.Milliseconds(), p95.Milliseconds())
	s.tl = timeline{at: now}
	return line
}

func pctDur(v []time.Duration, p float64) time.Duration {
	if len(v) == 0 {
		return 0
	}
	c := append([]time.Duration(nil), v...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[int(p*float64(len(c)-1))]
}

// ---------- end ----------

func (s *session) abort(why string) {
	s.sendAbort()
	s.shutdown(why, true)
}

func (s *session) dialFail(why string) {
	s.e.mDialFail.Add(1)
	log.Printf("[%x] dial %s: %s", s.id[:3], s.dstAddr, why)
	s.broadcastFrame(fDialFail)
	s.shutdown("dial failed", true)
}

func (s *session) broadcastFrame(typ byte) {
	s.mu.Lock()
	var live []*sub
	if !s.closed {
		for _, x := range s.subs {
			if !x.dead {
				live = append(live, x)
			}
		}
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, x := range live {
		wg.Add(1)
		go func(x *sub) {
			defer wg.Done()
			x.wmu.Lock()
			x.c.SetWriteDeadline(time.Now().Add(2 * time.Second))
			x.w.Write([]byte{typ})
			x.w.Flush()
			x.wmu.Unlock()
		}(x)
	}
	wg.Wait()
}

// shutdown ends the session; abort resets the LAN side (RST) instead of a clean close.
func (s *session) shutdown(why string, abort bool) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.cleanEnd = !abort
	s.endWhy = why
	subs := s.subs
	s.e.globalB.Add(-int64(s.unackedB))
	s.unackedB = 0
	s.e.recvB.Add(-int64(s.pendingB))
	s.pendingB = 0
	s.chunks, s.pending, s.pendAt, s.sackSize, s.newQ, s.urgQ = nil, nil, nil, nil, nil, nil
	st := s.st
	local := s.local
	udpDiag := ""
	if u := s.udp; u != nil {
		udpDiag = fmt.Sprintf(" | udp in %d out %d gap max %.2fs", u.nIn, u.nOut, u.gapMax.Seconds())
	}
	s.cond.Broadcast()
	s.mu.Unlock()
	for _, x := range subs {
		if tc, ok := x.c.(*net.TCPConn); ok && abort {
			// the ABORT / DIALFAIL frame was just written: send FIN after it and close a
			// little later, so a reset (unread data at close) cannot overtake the frame
			tc.CloseWrite()
			time.AfterFunc(2*time.Second, func() { x.c.Close() })
			continue
		}
		x.c.Close()
	}
	s.udpClose()
	if local != nil {
		// the connection was armed for a reset when the session got it (ArmReset); only
		// a clean end, with the whole stream already written to it, closes it with a FIN
		if abort {
			resetClose(local)
		} else {
			cleanClose(local)
		}
	}
	s.e.nSessions.Add(-1)
	s.e.registry.Delete(s)
	sum := fmt.Sprintf("%.1fs tx %dMB rx %dMB chunks %d resends %d kicks %d subs %d stall %.1fs",
		time.Since(s.started).Seconds(), st.txB>>20, st.rxB>>20, st.chunks, st.resends, st.kicks, st.subsUp, st.stall.Seconds())
	if abort {
		s.e.mAborts.Add(1)
		log.Printf("[%x] session aborted: %s (%s)", s.id[:3], why, sum)
	} else {
		log.Printf("[%x] session end: %s (%s)", s.id[:3], why, sum)
	}
	var b strings.Builder
	for i, n := range causeNames {
		if st.resentBy[i] > 0 {
			fmt.Fprintf(&b, " %s=%d", n, st.resentBy[i])
		}
	}
	ms := func(v []time.Duration) string {
		return fmt.Sprintf("%d/%d/%d", pctDur(v, 0.5).Milliseconds(), pctDur(v, 0.95).Milliseconds(), pctDur(v, 1).Milliseconds())
	}
	log.Printf("[%x] session diag: pump win %.1fs | resent%s | subs died lost=%d silent=%d write=%d slow=%d | fb gap max %.1fs >1s %d"+
		" | gap repairs %d (retries %d) ms p50/p95/max: peer wait %s, queued %s, to release %s | snapshots truncated %d%s",
		s.id[:3], st.pumpWin.Seconds(), b.String(), st.died[kLost], st.died[kSilent], st.died[kWrite], st.died[kSlow],
		st.fbGapMax.Seconds(), st.fbGaps, len(st.repDone), st.repRetry, ms(st.repAge), ms(st.repWait), ms(st.repDone), st.truncSnaps, udpDiag)
}

// testGrace is the grace time of a session that carries test data (Owner.Test), once
// the other end has answered: nothing is lost with it, so it does not wait long for an
// other end that has gone silent. It is longer than a single connection on a bad line
// can stall and still recover.
const testGrace = 30 * time.Second

// grace is how long the session lasts while nothing arrives from the other end (lock
// held).
func (s *session) grace() time.Duration {
	if s.owner.Test && s.everUp {
		return min(s.cfg.Grace, testGrace)
	}
	return s.cfg.Grace
}

// end says how the session ended. Call it after monitor has returned.
func (s *session) end() End {
	s.mu.Lock()
	defer s.mu.Unlock()
	return End{ID: s.id, Why: s.endWhy, Clean: s.cleanEnd, Up: s.everUp, NoReply: s.noReply}
}

func (s *session) setLocal(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.local = c
	return true
}

func (s *session) logStats() {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for _, x := range s.subs {
		if x.dead {
			continue
		}
		if x.ctrl {
			fmt.Fprintf(&b, " [ctrl p%d]", x.path)
			continue
		}
		fmt.Fprintf(&b, " [p%d %d/%dKB/s loop%dms infl%dKB cap%dKB]", x.path, int(x.rate)>>10, int(x.avg)>>10,
			x.loopBase.Milliseconds(), (x.sentB-x.rcvdB)>>10, s.capB(x)>>10)
	}
	log.Printf("[%x] %s %.0fs tx %dMB rx %dMB unacked %dMB sacked %dMB pending %dMB subs:%s", s.id[:3], s.dstAddr,
		time.Since(s.started).Seconds(), s.st.txB>>20, s.st.rxB>>20, s.unackedB>>20, s.sackB>>20, s.pendingB>>20, b.String())
}
