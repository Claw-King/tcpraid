package link

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/Claw-King/tcpraid/internal/node"
	"github.com/Claw-King/tcpraid/internal/pair"
)

const (
	// DefaultMaxUnauthenticated is how many connections that have not yet proved who they
	// are each listener of a Server holds at a time when nothing else is set, on a
	// process that may open enough files (see defaultLimit).
	DefaultMaxUnauthenticated = 2048
	// DefaultFirstByteTimeout is how long a new connection on the TLS listener may stay
	// silent when nothing else is set.
	DefaultFirstByteTimeout = 3 * time.Second
)

// Server takes the connections that arrive at a node's listeners. Use it by pointer.
type Server struct {
	Identity *node.Identity
	// OwnCert is the operator's certificate, shown to dialers of the owncert tier. nil:
	// the node has none and refuses them.
	OwnCert *tls.Certificate
	// PeerByFingerprint finds the peer whose identity key has the fingerprint.
	PeerByFingerprint func(fingerprint string) (node.Peer, bool)
	// HandlePair serves a pairing connection whose handshake is done; it closes it. The
	// connection's deadline is set. nil: pairing connections are closed.
	HandlePair func(conn *tls.Conn)
	// HandlePeer gets a connection from a peer that TLS has authenticated; it closes
	// it.
	HandlePeer func(peer node.Peer, conn net.Conn)
	// HandlePlain gets a connection that arrived at the plain listener, from an end
	// nothing is known about; it closes it. It calls authenticated once the other end
	// has proved who it is: from then on the connection does not count against
	// MaxUnauthenticated. authenticated returns false when the connection was closed
	// to make room in the meantime: the handler then drops it. A connection whose
	// handler returns stops counting too.
	HandlePlain func(conn net.Conn, authenticated func() bool)
	// MaxUnauthenticated bounds, for each listener on its own, the connections that have
	// not yet proved who they are: in a TLS handshake, in a pairing, or on the plain
	// listener and not yet authenticated by the handler. Anyone can open such
	// connections, so without a bound they could use up the process's file descriptors.
	// A new connection is always taken: when the bound is reached, another one is
	// closed to make room (see waiting). 0 means DefaultMaxUnauthenticated, or less on
	// a process with a low limit of open files (see defaultLimit).
	MaxUnauthenticated int
	// FirstByteTimeout is how long a new connection on the TLS listener may stay silent.
	// 0 means DefaultFirstByteTimeout.
	FirstByteTimeout time.Duration
	// Logf logs one line about a refused connection. nil: nothing is logged.
	Logf func(format string, args ...any)

	handlers sync.WaitGroup // connections being handled
	refused  struct {       // refused connections: see LogRefused
		sync.Mutex
		last    time.Time // when a line was last written
		skipped int       // lines left out since
		pending bool      // flushRefused is scheduled
	}
}

// waiting is the set of connections of one listener that have not yet proved who they
// are. It never refuses a newcomer. When the set is full, a connection gives way
// instead:
//
//   - normally the one that has waited longest, whatever its source;
//   - but if a source holds more than its share of the set (an eighth), the oldest
//     connection of the source that holds the most.
//
// A source is an IPv4 address, or the /64 network of an IPv6 address: one host has a
// whole /64 to connect from. Nothing a connection has sent counts: before it has proved
// who it is, whatever it sends can be imitated.
//
// What this gives. A connection whose source stays within its share is closed for want
// of room only after as many newer connections have arrived as the set has places;
// someone who wants to push a node's handshake out has to bring that many in the time
// the handshake takes. A single source cannot do that by itself: beyond its share it
// only replaces its own connections. A node that opens no more connections at once than
// the share is never treated as such a source. The exception is a source that a node
// shares with someone else (one address for several hosts): once the set is full, a
// share's worth of newer connections from that address is enough, because they put the
// source over its share and its oldest connection is then the first to go.
type waiting struct {
	mu      sync.Mutex
	limit   int
	share   int       // a source holding more than this gives way first
	n       int       // connections in the set
	seq     uint64    // arrival order of the last connection
	queue   []*waiter // all of them in order of arrival; some may have left already
	sources map[string]*source
	over    map[string]*source // the sources that hold more than their share
}

// source holds the waiting connections of one source in order of arrival.
type source struct {
	queue []*waiter // some may have left already
	n     int       // how many of them are in the set
}

type waiter struct {
	c    net.Conn
	id   uint64 // order of arrival over the whole set
	src  string // see sourceOf
	gone bool   // has left the set (lock held)
}

func newWaiting(limit int) *waiting {
	if limit <= 0 {
		limit = defaultLimit()
	}
	return &waiting{limit: limit, share: max(limit/8, 1), sources: map[string]*source{}, over: map[string]*source{}}
}

// defaultLimit returns DefaultMaxUnauthenticated, or a quarter of the files the process
// may open if that is less (and at least 4): two listeners at their bound must leave
// descriptors for the connections that carry traffic and for the node's own files.
func defaultLimit() int {
	limit := DefaultMaxUnauthenticated
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err == nil && rl.Cur/4 < uint64(limit) {
		limit = int(rl.Cur / 4)
	}
	return max(limit, 4)
}

// sourceOf names the source a remote address counts for: the address itself for IPv4,
// the /64 network for IPv6.
func sourceOf(addr net.Addr) string {
	host := addr.String()
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	ip = ip.Unmap().WithZone("")
	if ip.Is4() {
		return ip.String()
	}
	prefix, err := ip.Prefix(64)
	if err != nil {
		return ip.String()
	}
	return prefix.String()
}

// admit adds c to the set. evicted is the connection that had to give way, if any, for
// the caller to close. leave takes c out of the set; it reports whether c was still in
// it, which is false after c was evicted and on every call but the first.
func (w *waiting) admit(c net.Conn) (leave func() bool, evicted net.Conn) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.n >= w.limit {
		if v := w.victim(); v != nil {
			evicted = v.c
			w.remove(v)
		}
	}
	w.seq++
	e := &waiter{c: c, id: w.seq, src: sourceOf(c.RemoteAddr())}
	s := w.sources[e.src]
	if s == nil {
		s = &source{}
		w.sources[e.src] = s
	}
	s.queue = append(s.queue, e)
	s.n++
	if s.n > w.share {
		w.over[e.src] = s
	}
	w.queue = append(w.queue, e)
	w.n++
	return func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		if e.gone {
			return false
		}
		w.remove(e)
		return true
	}, evicted
}

// victim returns the connection that gives way (lock held): see waiting. There are at
// most eight sources over their share, so this takes a few steps whatever the size of
// the set.
func (w *waiting) victim() *waiter {
	var worst *source
	for _, s := range w.over {
		if worst == nil || s.n > worst.n || (s.n == worst.n && head(&s.queue).id < head(&worst.queue).id) {
			worst = s
		}
	}
	if worst != nil {
		return head(&worst.queue)
	}
	if w.n == 0 {
		return nil
	}
	return head(&w.queue)
}

// head returns the oldest connection of a queue that is still in the set, dropping the
// ones before it. The queue must hold one.
func head(q *[]*waiter) *waiter {
	for (*q)[0].gone {
		(*q)[0] = nil
		*q = (*q)[1:]
	}
	return (*q)[0]
}

// remove takes e out of the set (lock held). Queues keep entries that have left until
// they reach the front (see head); a queue that holds mostly such entries is rebuilt.
func (w *waiting) remove(e *waiter) {
	e.gone = true
	w.n--
	s := w.sources[e.src]
	s.n--
	if s.n <= w.share {
		delete(w.over, e.src)
	}
	if s.n == 0 {
		delete(w.sources, e.src)
	} else {
		compact(&s.queue, s.n)
	}
	compact(&w.queue, w.n)
}

// compact rebuilds a queue that has grown to more than twice its live entries.
func compact(q *[]*waiter, live int) {
	if len(*q) <= 2*live+16 {
		return
	}
	kept := make([]*waiter, 0, live)
	for _, x := range *q {
		if x != nil && !x.gone {
			kept = append(kept, x)
		}
	}
	*q = kept
}

// quickStart is a connection that has to send its first bytes soon. Until it does, the
// deadline its creator set holds; from its first bytes on it has rest for what follows.
type quickStart struct {
	net.Conn
	rest    time.Duration
	started bool // only the reading goroutine uses it
}

func (q *quickStart) Read(p []byte) (int, error) {
	n, err := q.Conn.Read(p)
	if n > 0 && !q.started {
		q.started = true
		q.Conn.SetDeadline(time.Now().Add(q.rest))
	}
	return n, err
}

// Wait waits until every connection the listeners handed out has been handled, for at
// most d. It reports whether that happened. Call it after the Serve methods returned.
func (s *Server) Wait(d time.Duration) bool {
	done := make(chan struct{})
	go func() { s.handlers.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// config returns the TLS configuration of the listener. A client that offers ALPNOwn is
// shown the operator's certificate; every other client the identity certificate. Both
// must show a certificate: which one is accepted depends on the protocol (verify).
func (s *Server) config() *tls.Config {
	base := &tls.Config{
		MinVersion:       tls.VersionTLS13,
		Certificates:     []tls.Certificate{s.Identity.TLSCertificate()},
		NextProtos:       []string{ALPN, pair.ALPN},
		ClientAuth:       tls.RequireAnyClientCert,
		VerifyConnection: s.verify,
		// Every connection does the full handshake: who the client is, and under which
		// protocol, is decided anew each time and never taken from an earlier session.
		SessionTicketsDisabled: true,
	}
	if s.OwnCert == nil {
		return base
	}
	own := base.Clone()
	own.Certificates = []tls.Certificate{*s.OwnCert}
	own.NextProtos = []string{ALPNOwn}
	base.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		if slices.Contains(hello.SupportedProtos, ALPNOwn) {
			return own, nil
		}
		return nil, nil
	}
	return base
}

// verify decides whether the client's certificate is accepted. A pairing connection may
// come from any node: it proves itself with the pairing code afterwards. Any other
// connection must come from a peer.
func (s *Server) verify(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("no certificate")
	}
	switch cs.NegotiatedProtocol {
	case pair.ALPN:
		return nil
	case ALPN, ALPNOwn:
		fp := node.Fingerprint(cs.PeerCertificates[0])
		if _, ok := s.PeerByFingerprint(fp); !ok {
			return fmt.Errorf("not a peer of this node (fingerprint %s)", fp)
		}
		return nil
	}
	return errors.New("no protocol this node speaks")
}

// ServeTLS takes connections from ln until ctx is done, then closes ln and returns nil.
// An error of Accept does not end it (see serve); it returns an error only when ln was
// closed by something else.
func (s *Server) ServeTLS(ctx context.Context, ln net.Listener) error {
	cfg := s.config()
	first := s.FirstByteTimeout
	if first <= 0 {
		first = DefaultFirstByteTimeout
	}
	return s.serve(ctx, ln, func(c net.Conn, authenticated func() bool) {
		// a connection that says nothing goes soon; one that has started its handshake
		// gets HandshakeTimeout to finish it
		c.SetDeadline(time.Now().Add(first))
		tc := tls.Server(&quickStart{Conn: c, rest: HandshakeTimeout}, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			// a connection that was closed to make room has its line already
			if authenticated() {
				s.LogRefused("refused a connection from %s: %v", c.RemoteAddr(), err)
			}
			c.Close()
			return
		}
		cs := tc.ConnectionState()
		if cs.NegotiatedProtocol == pair.ALPN {
			if s.HandlePair == nil {
				tc.Close()
				return
			}
			// the deadline of the handshake stays: it bounds the whole pairing
			s.HandlePair(tc)
			return
		}
		peer, ok := s.PeerByFingerprint(node.Fingerprint(cs.PeerCertificates[0]))
		if !ok || s.HandlePeer == nil {
			// unpaired between the handshake and here
			tc.Close()
			return
		}
		if !authenticated() {
			// closed to make room while its handshake finished
			tc.Close()
			return
		}
		c.SetDeadline(time.Time{})
		s.HandlePeer(peer, tc)
	})
}

// ServePlain takes connections from ln, without TLS, until ctx is done; see ServeTLS.
func (s *Server) ServePlain(ctx context.Context, ln net.Listener) error {
	return s.serve(ctx, ln, func(c net.Conn, authenticated func() bool) {
		if s.HandlePlain == nil {
			c.Close()
			return
		}
		s.HandlePlain(c, authenticated)
	})
}

// Accept errors are retried after a pause that starts at acceptRetryMin and doubles up
// to acceptRetryMax.
const (
	acceptRetryMin = 5 * time.Millisecond
	acceptRetryMax = time.Second
)

// serve runs the accept loop of one listener. Each connection joins the listener's set of
// connections that have not proved who they are (see waiting), and is handled in its own
// goroutine; handle calls authenticated when the connection has proved it, and the
// connection leaves the set then or when handle returns.
//
// An error of Accept is logged and retried after a pause: running out of file
// descriptors, which anyone who can connect may cause, must not end the node. The loop
// ends when ctx is done (nil) or when the listener was closed by something else (the
// error).
func (s *Server) serve(ctx context.Context, ln net.Listener, handle func(c net.Conn, authenticated func() bool)) error {
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	unproven := newWaiting(s.MaxUnauthenticated)
	var pause time.Duration
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			pause = min(max(2*pause, acceptRetryMin), acceptRetryMax)
			s.logf("accepting on %s failed, trying again in %v: %v", ln.Addr(), pause, err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(pause):
			}
			continue
		}
		pause = 0
		leave, evicted := unproven.admit(c)
		if evicted != nil {
			// its handler sees the connection fail and returns
			evicted.Close()
			s.LogRefused("closed a waiting connection from %s to make room: too many connections that have not proved who they are", evicted.RemoteAddr())
		}
		s.handlers.Add(1)
		go func() {
			defer s.handlers.Done()
			defer leave()
			handle(c, leave)
		}()
	}
}

// LogRefused logs a line about a connection that was refused or closed to make room.
// Anyone who can connect can cause such lines, as many as it likes, so one is written
// and the rest of that second are only counted; the count is written when the second
// ends. Whatever logs on behalf of a connection that has not proved who it is, such as
// the pairing service, logs through this.
func (s *Server) LogRefused(format string, args ...any) {
	s.refused.Lock()
	if wait := time.Until(s.refused.last.Add(time.Second)); wait > 0 {
		s.refused.skipped++
		if !s.refused.pending {
			s.refused.pending = true
			time.AfterFunc(wait, s.flushRefused)
		}
		s.refused.Unlock()
		return
	}
	s.refused.last = time.Now()
	s.refused.Unlock()
	s.logf(format, args...)
}

// flushRefused writes how many lines LogRefused left out in the second that has ended.
// The next refusal is written in full again, so a steady stream shows one line with its
// address and one count, every second.
func (s *Server) flushRefused() {
	s.refused.Lock()
	skipped := s.refused.skipped
	s.refused.skipped, s.refused.pending = 0, false
	s.refused.Unlock()
	if skipped > 0 {
		s.logf("%d more connections were refused or closed to make room in the last second", skipped)
	}
}
