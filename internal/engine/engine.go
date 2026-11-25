// Package engine is the tcpraid transport: it stripes ONE TCP connection over several TCP
// sub-connections and puts it back together in order at the far end, for long, lossy
// paths where a single TCP connection is slow or erratic.
//
// Design (wire format: PROTOCOL.md). In short:
//   - data plane: data sub-connections carry chunks (sequence number, send time); each
//     sub holds only its delivery rate x (feedback loop + Config.Target) in flight, so one
//     sub's loss recovery strands little; new chunks are read only within our send
//     window and the peer's reorder budget;
//   - control plane: each end sends complete state snapshots (cumulative point,
//     received ranges, missing ranges with their age, per-sub counters, echoed send
//     times, budget, end-of-stream) on every control sub and, as a fallback, on data
//     subs (once a second while a control sub is up). The newest snapshot wins, so a
//     lost or late one costs nothing. Chunks reported received -- in order or not --
//     are released at once; a missing chunk is re-sent alone, on another sub, once it
//     is clearly late;
//   - session robustness: RST on abnormal ends, no session from
//     a late sub, dial outside the lock and DIALFAIL at once, Config.Grace across outages,
//     confirmed end of stream, destination allow list, memory limits.
//
// No encryption of data: run the sub-connections inside an encrypted path (an SSH
// tunnel, a VPN, an encrypted proxy chain). With a PSK, headers and snapshots carry a MAC.
//
// An Engine holds the settings, the session table and the counters of one transport. The
// command in cmd/tcpraid builds a Config from its flags and runs one Engine. A process
// runs one Engine: Fatal and Guard end the process, and PublishExpvar registers
// process-wide names.
//
// Order of calls: New, PublishExpvar (optional), StartUDPListener, Guard (in its own
// goroutine), SetPSK, then RunClient or RunServer.
//
// RunClient and RunServer listen on Config.Listen and serve one other end with one key.
// A caller that takes the connections itself, and has several other ends with a key
// each, uses what they are made of instead: NewRoute and Route.Carry for a connection
// to carry, ServeSub for a sub-connection that arrived, AbortPeer to end what belongs
// to one of the other ends, Shutdown before it exits.
package engine

import (
	"expvar"
	"log"
	"net"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// Config holds the settings of one Engine. The command-line flag of each field is named
// in its comment; cmd/tcpraid documents them.
type Config struct {
	Version string // printed in the start-up line

	Listen   string // -listen
	FixedDst string // -dst: client, fixed destination instead of the redirected one
	Allow    string // -allow: server, allowed destinations (empty = any)

	UDPListen string  // -udplisten
	UDPDial   string  // -udpdial
	UDPLoss   float64 // -udploss (testing)

	Chunk     int           // -chunk
	Window    int           // -window
	Budget    int           // -budget
	Target    time.Duration // -target
	PigTarget time.Duration // -pigtarget
	Coalesce  time.Duration // -coalesce
	Dead      time.Duration // -dead
	Grace     time.Duration // -grace
	KickRatio float64       // -kickratio
	KickFloor int           // -kickfloor
	Share     bool          // -share
	FbSubs    int           // -fbsubs
	FbEvery   time.Duration // -fbevery
	Hol       time.Duration // -hol

	Timeline time.Duration // -timeline
	Stats    time.Duration // -stats
	Verbose  bool          // -v
	Chaos    time.Duration // -chaos (testing)

	MaxRSS  int           // -maxrss, MB
	MaxSess int           // -maxsess
	Life    time.Duration // -life
}

// DefaultConfig returns the same values as the flag defaults of cmd/tcpraid; a test there
// keeps the two equal.
func DefaultConfig() Config {
	return Config{
		Chunk:     256 * 1024,
		Window:    128 << 20,
		Budget:    256 << 20,
		Target:    300 * time.Millisecond,
		PigTarget: time.Second,
		Coalesce:  20 * time.Millisecond,
		Dead:      20 * time.Second,
		Grace:     900 * time.Second,
		KickRatio: 5,
		KickFloor: 256 * 1024,
		Share:     true,
		FbSubs:    3,
		FbEvery:   20 * time.Millisecond,
		Hol:       time.Second,
		MaxSess:   64,
	}
}

// WireVersion is the wire protocol version (PROTOCOL.md); MaxChunk is the largest chunk
// size the protocol carries.
const (
	WireVersion = wireVer
	MaxChunk    = maxData
)

// t0 is the origin of the monotonic clock sent in DATA frames and echoed in snapshots,
// and the start from which Config.Life is measured.
var t0 = time.Now()

// Engine is one running transport: settings, keys, sessions and counters.
type Engine struct {
	cfg Config
	ks  keys

	// Watch, if not nil, is told of every session that this engine creates and of
	// its end. Set it before the engine carries anything. It is called while the
	// engine holds its own locks and must return at once, without calling the engine.
	Watch func(SessionEvent)

	nSessions atomic.Int64
	globalB   atomic.Int64 // unacked bytes we sent, all sessions
	recvB     atomic.Int64 // out-of-order bytes we hold for delivery, all sessions
	registry  sync.Map     // *session -> true

	// smu guards sess, ended and stopping, and the creation of a session against
	// AbortPeer and Shutdown.
	smu      sync.Mutex
	stopping bool                  // Shutdown was called: no session is created any more
	sess     map[[16]byte]*session // sessions the other end created (ServeSub), by id
	ended    map[[16]byte]endInfo  // ... and those that have ended, for late subs

	mSessions   atomic.Int64
	mAborts     atomic.Int64
	mDialFail   atomic.Int64
	mDialCapped atomic.Int64
	mSubsUp     atomic.Int64
	mSubsLost   atomic.Int64
	mKickSlow   atomic.Int64
	mKickSilent atomic.Int64
	mKickWrite  atomic.Int64
	mChunksTx   atomic.Int64
	mBytesTx    atomic.Int64
	mResend     atomic.Int64
	mRxDrop     atomic.Int64
	mRxDup      atomic.Int64
	mStallMs    atomic.Int64
	mFbGaps     atomic.Int64
	mBadSnap    atomic.Int64
	writeUs     atomic.Int64
	waitUs      [nWait]atomic.Int64
	resendBy    [nCause]atomic.Int64

	udpLn     *net.UDPConn // Config.UDPListen socket (shared by all sessions)
	udpByTag  sync.Map     // [8]byte -> *session
	mUDPIn    atomic.Int64
	mUDPOut   atomic.Int64
	mUDPBad   atomic.Int64
	mUDPBig   atomic.Int64 // snapshots too large for a datagram (not sent over UDP)
	mUDPOld   atomic.Int64 // authentic datagrams with a counter not above the highest seen (replayed or reordered)
	udpBadTok struct {
		sync.Mutex
		tokens float64
		at     time.Time
	}
}

// New returns an Engine with the given settings. It does no I/O and does not check the
// settings: the caller validates them (cmd/tcpraid checks Chunk, Window and MaxRSS).
func New(cfg Config) *Engine {
	return &Engine{cfg: cfg, sess: map[[16]byte]*session{}, ended: map[[16]byte]endInfo{}}
}

// Config returns the settings the Engine was made with. A session runs with a copy of
// them unless its creator gave it others (Route.CarryWith, NewReverseWith,
// SubOptions.Config).
func (e *Engine) Config() Config { return e.cfg }

// Keys are the keys two ends share: they authenticate the headers and the snapshots of
// the sessions between them, and seal their UDP feedback. The zero value means none.
type Keys struct {
	ks keys
}

// NewKeys derives Keys from key material both ends hold (16 bytes or more). Empty
// material gives the zero Keys: no authentication.
func NewKeys(material []byte) Keys {
	return Keys{deriveKeys(material)}
}

// Owner says whom a session belongs to. Peer names the other end of the pair, Rule
// what made this end carry the connection; both are empty where there is one pair only.
// Test marks a session that carries test data from one end into the other and no
// application connection (SubOptions.Sink).
type Owner struct {
	Peer string
	Rule string
	Test bool
}

// SessionEvent tells a watcher of the engine that a session was created or has ended.
type SessionEvent struct {
	Ended       bool // false: the session was created
	ID          [16]byte
	Destination string
	Client      bool // this end took the application connection
	Owner       Owner
	// Of a session that ended: the stream bytes sent and received, whether the stream
	// ended completely in both directions, and the reason as the log has it.
	TxBytes, RxBytes int64
	Clean            bool
	Why              string
	// Confirmed, of a session that ended Clean: the other end said that it was done
	// too, so it wrote the whole of this end's stream to its application and ended
	// that direction in order. A session ends Clean without it when the other end took
	// every chunk and was gone, with all its sub-connections, before it said so: it may
	// not have written the last of them.
	Confirmed bool
}

// End says how a session ended.
type End struct {
	ID [16]byte
	// Why is the reason as the log has it: "done" and its variants for a clean end,
	// otherwise what cut the session short (see the constants below for the ones that
	// callers tell apart).
	Why string
	// Clean: the stream ended completely in both directions. Otherwise the application
	// connection was reset.
	Clean bool
	// Up: the other end answered on a sub-connection at least once.
	Up bool
	// NoReply is how the last sub-connection that the other end never answered ended:
	// the error of reading its answer. Empty when every one was answered.
	NoReply string
}

// Reasons in End.Why that a caller can tell apart.
const (
	// WhyNoAnswer: no sub-connection was answered by the other end within 30 seconds.
	WhyNoAnswer = "no answer from the server in 30 s"
	// WhyGrace: nothing arrived from the other end for the grace time.
	WhyGrace = "grace expired"
	// WhyUnreachable starts the reason of a session whose other end could not connect
	// it to its destination; the destination follows.
	WhyUnreachable = "server could not reach "
	// WhyRefused: the other end takes no more sessions.
	WhyRefused = "server refused the session"
	// WhyNoSession: the other end answered a sub-connection that it does not hold the
	// session. It ended there, for a reason this end was not told on that connection.
	WhyNoSession = "peer has no such session"
)

// SetPSK derives the MAC and UDP keys from a pre-shared key (16 bytes or more). An empty
// key means no authentication. Call it before RunClient or RunServer.
func (e *Engine) SetPSK(psk []byte) {
	e.ks = deriveKeys(psk)
}

// Sessions returns the number of live sessions.
func (e *Engine) Sessions() int64 { return e.nSessions.Load() }

// PublishExpvar registers the engine's counters with package expvar under their
// established names. Call it at most once per process.
func (e *Engine) PublishExpvar() {
	for name, v := range map[string]*atomic.Int64{
		"sessions_total":       &e.mSessions,
		"sessions_aborted":     &e.mAborts,
		"dialfail":             &e.mDialFail,
		"dials_capped":         &e.mDialCapped,
		"subs_up":              &e.mSubsUp,
		"subs_lost":            &e.mSubsLost,
		"kicks_slow":           &e.mKickSlow,
		"kicks_silent":         &e.mKickSilent,
		"kicks_write":          &e.mKickWrite,
		"chunks_tx":            &e.mChunksTx,
		"bytes_tx":             &e.mBytesTx,
		"resend_chunks":        &e.mResend,
		"rx_drop":              &e.mRxDrop,
		"rx_dup":               &e.mRxDup,
		"local_write_stall_ms": &e.mStallMs,
		"fb_gaps_1s":           &e.mFbGaps,
		"bad_snapshots":        &e.mBadSnap,
	} {
		expvar.Publish(name, expvar.Func(func() any { return v.Load() }))
	}
	expvar.Publish("sessions_active", expvar.Func(func() any { return e.nSessions.Load() }))
	expvar.Publish("unacked_mb", expvar.Func(func() any { return e.globalB.Load() >> 20 }))
	if RSSSupported {
		expvar.Publish("rss_mb", expvar.Func(func() any { return rssMB() }))
	}
	expvar.Publish("sojourn_ms_max", expvar.Func(func() any { return e.maxSojourn() }))
	expvar.Publish("udp_in", expvar.Func(func() any { return e.mUDPIn.Load() }))
	expvar.Publish("udp_out", expvar.Func(func() any { return e.mUDPOut.Load() }))
	expvar.Publish("udp_bad", expvar.Func(func() any { return e.mUDPBad.Load() }))
	expvar.Publish("udp_too_big", expvar.Func(func() any { return e.mUDPBig.Load() }))
	expvar.Publish("udp_old", expvar.Func(func() any { return e.mUDPOld.Load() }))
	expvar.Publish("send_write_ms", expvar.Func(func() any { return e.writeUs.Load() / 1000 }))
	expvar.Publish("send_wait_ms", expvar.Func(func() any {
		m := map[string]int64{}
		for i, n := range waitNames {
			m[n] = e.waitUs[i].Load() / 1000
		}
		return m
	}))
	expvar.Publish("resend_by", expvar.Func(func() any {
		m := map[string]int64{}
		for i, n := range causeNames {
			m[n] = e.resendBy[i].Load()
		}
		return m
	}))
}

// maxSojourn: the longest any data sub's in-flight data takes to drain at its rate (ms),
// over all sessions -- how deep the per-sub queues run.
func (e *Engine) maxSojourn() int64 {
	var worst int64
	e.registry.Range(func(k, _ any) bool {
		s := k.(*session)
		s.mu.Lock()
		for _, x := range s.subs {
			if !x.dead && !x.ctrl && x.rate > 0 {
				worst = max(worst, int64(float64(x.sentB-x.rcvdB)/x.rate*1000))
			}
		}
		s.mu.Unlock()
		return true
	})
	return worst
}

// abortAll tells every session's peer that it is being reset (ABORT on each sub, flagged
// UDP snapshots), in parallel and within about 2 s, so the peer does not hold the session
// for Config.Grace.
func (e *Engine) abortAll() {
	var wg sync.WaitGroup
	e.registry.Range(func(k, _ any) bool {
		wg.Add(1)
		go func(s *session) { defer wg.Done(); s.sendAbort() }(k.(*session))
		return true
	})
	done := make(chan bool)
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}

// Fatal tells the peers, then resets every LAN-side connection before exiting (a FIN
// would make a cut-off stream look complete).
func (e *Engine) Fatal(f string, a ...interface{}) {
	e.Shutdown()
	log.Fatalf(f, a...)
}

// Shutdown tells the peers that every session is being reset, then resets every
// LAN-side connection that exists; one that a session is still dialing is reset by the
// dialing side when it comes up. Call it before the process exits. From here on Carry
// resets the connection it is given and ServeSub refuses a new session: a session that
// began after this point would be cut off by the exit without a reset.
func (e *Engine) Shutdown() {
	e.smu.Lock()
	e.stopping = true
	e.smu.Unlock()
	e.abortAll()
	e.registry.Range(func(k, _ any) bool {
		s := k.(*session)
		s.mu.Lock()
		l := s.local
		s.mu.Unlock()
		if l != nil {
			resetClose(l)
		}
		return true
	})
}

// AbortPeer resets every session that belongs to the peer (Owner.Peer), as if the other
// end had aborted it: the other end is told, the application connection is reset. It
// returns how many sessions that were.
//
// The validity check of Carry and ServeSub runs under the same lock as the search
// here: make the check fail first, then call AbortPeer, and no session of the peer is
// left or can still appear.
func (e *Engine) AbortPeer(peer string) int {
	return len(e.abortPeer(peer, time.Time{}, "the peer was removed"))
}

// AbortPeerBefore is AbortPeer for those sessions of the peer that began before t, and
// for no others: for a peer whose process has been replaced by a new one, which holds
// none of the sessions the old one had. why is the reason the log gives. It returns the
// owners of the sessions it reset. A zero t resets nothing.
func (e *Engine) AbortPeerBefore(peer string, t time.Time, why string) []Owner {
	if t.IsZero() {
		return nil
	}
	return e.abortPeer(peer, t, why)
}

// AbortOwner ends the sessions that this end carries (Route.Carry, NewReverse) and that
// belong to exactly owner, the way AbortPeer ends a peer's: for a caller that carries a
// connection of its own making and is done with it. why is the reason the log gives. It
// returns how many sessions that were.
func (e *Engine) AbortOwner(owner Owner, why string) int {
	return len(e.abortWhere(func(s *session) bool { return s.isClient && s.owner == owner }, why))
}

// AbortSession resets the session with the given id that this end holds with the peer,
// whichever end created it, as AbortPeer resets all of a peer's: for a caller that the
// peer told that it does not hold the session. why is the reason the log gives. It
// returns the owner of the session and whether there was one.
func (e *Engine) AbortSession(peer string, id [16]byte, why string) (Owner, bool) {
	owners := e.abortWhere(func(s *session) bool { return s.id == id && s.owner.Peer == peer }, why)
	if len(owners) == 0 {
		return Owner{}, false
	}
	return owners[0], true
}

// Unreachable ends the session with the given id that this end carries for the peer, as
// a session ends whose other end reports on a sub-connection that it could not connect
// it to its destination: for a caller that the other end told so by another way. The
// report on the sub-connection can be lost when the other end closes the connection
// right after it. It returns whether there was such a session.
func (e *Engine) Unreachable(peer string, id [16]byte) bool {
	var found *session
	e.smu.Lock()
	e.registry.Range(func(k, _ any) bool {
		if s := k.(*session); s.isClient && s.id == id && s.owner.Peer == peer {
			found = s
			return false
		}
		return true
	})
	e.smu.Unlock()
	if found == nil {
		return false
	}
	go found.shutdown(WhyUnreachable+found.dstAddr, true)
	return true
}

// EndTest ends the session with the given id that this end takes for the peer, if it
// carries test data into this end (SubOptions.Sink): for a caller that the other end,
// which carries the session, told that it has ended it. The word on a sub-connection
// can be lost when the other end closes the connections right after it. It returns
// whether there was such a session.
func (e *Engine) EndTest(peer string, id [16]byte) bool {
	return len(e.abortWhere(func(s *session) bool {
		return !s.isClient && s.id == id && s.owner.Peer == peer && s.owner.Test
	}, "the other end ended the test")) > 0
}

// abortPeer resets the sessions of the peer; with a t that is not zero, only those that
// began before t.
func (e *Engine) abortPeer(peer string, t time.Time, why string) []Owner {
	return e.abortWhere(func(s *session) bool {
		return s.owner.Peer == peer && (t.IsZero() || s.started.Before(t))
	}, why)
}

// abortWhere resets the sessions that match and returns their owners.
func (e *Engine) abortWhere(match func(*session) bool, why string) []Owner {
	var mine []*session
	e.smu.Lock()
	e.registry.Range(func(k, _ any) bool {
		if s := k.(*session); match(s) {
			mine = append(mine, s)
		}
		return true
	})
	e.smu.Unlock()
	owners := make([]Owner, len(mine))
	for i, s := range mine {
		owners[i] = s.owner
		go s.abort(why)
	}
	return owners
}

// Guard enforces Config.MaxRSS / Config.Life and logs Config.Stats. It does not return.
func (e *Engine) Guard() { e.GuardWith(e.Fatal) }

// GuardWith is Guard for a caller that ends the process in its own way: once a limit is
// passed it calls exit with the reason and returns.
func (e *Engine) GuardWith(exit func(format string, a ...interface{})) {
	if e.cfg.MaxRSS > 0 {
		debug.SetMemoryLimit(int64(e.cfg.MaxRSS) << 20 * 7 / 10)
	}
	last := time.Now()
	for range time.Tick(time.Second) {
		if e.cfg.Life > 0 && time.Since(t0) > e.cfg.Life {
			exit("-life %v reached, exiting", e.cfg.Life)
			return
		}
		rss := rssMB()
		if e.cfg.MaxRSS > 0 && rss > e.cfg.MaxRSS {
			exit("rss %d MB > -maxrss %d MB, exiting", rss, e.cfg.MaxRSS)
			return
		}
		if e.cfg.Stats > 0 && time.Since(last) >= e.cfg.Stats {
			last = time.Now()
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			log.Printf("stats sessions=%d goroutines=%d rss=%s heap=%dMB unacked=%dMB resends=%d kicks=%d/%d/%d rxdrop=%d udp in/out/bad/toobig/old=%d/%d/%d/%d/%d",
				e.nSessions.Load(), runtime.NumGoroutine(), rssText(rss), m.HeapInuse>>20, e.globalB.Load()>>20, e.mResend.Load(),
				e.mKickSlow.Load(), e.mKickSilent.Load(), e.mKickWrite.Load(), e.mRxDrop.Load(),
				e.mUDPIn.Load(), e.mUDPOut.Load(), e.mUDPBad.Load(), e.mUDPBig.Load(), e.mUDPOld.Load())
			e.registry.Range(func(k, _ any) bool {
				k.(*session).logStats()
				return true
			})
		}
	}
}
