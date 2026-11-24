package forward

// The path test: how much several sub-connections carry compared with one.
//
// A test runs only when it is asked for (TestPeer). The node that runs it sends data it
// makes up to the peer, in ordinary sessions of the transport whose destination is the
// peer itself (engine.SinkIP, engine.SinkPort): the peer reads the data and throws it
// away. Each path that carries data is measured twice: with one data sub-connection, and
// with as many as the path has. The sessions go the way the node's rules go: over its
// own paths to the peer, or, where it has none, over sub-connections that the peer is
// asked to open ("open" with test_path; the paths are asked for first, with "paths").
//
// Both ends bound a test. The sending end stops each run after the time asked for (at
// most TestMaxSeconds) or after TestMaxBytes, and runs one test at a time. The receiving
// end takes the sessions of a few tests at a time, and ends each one that lasts longer
// or brings more than any test may, whatever the sending end does.
//
// The sending end ends the session of a run by aborting it, and says so on the control
// connection as well ("test_end"): the word on the sub-connections is often lost behind
// the test data that is still on its way, and the receiving end would hold the session
// until it has heard nothing for a while.

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	crand "crypto/rand"

	"github.com/Claw-King/tcpraid/internal/control"
	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/node"
)

const (
	// TestDefaultSeconds and TestMaxSeconds are how long one run of a path test lasts
	// when the request does not say, and at most.
	TestDefaultSeconds = 10
	TestMaxSeconds     = 60
	// TestMaxBytes is the most data one run sends.
	TestMaxBytes = 1 << 30
	// TestLittleGain: when all the sub-connections of a path carry less than this many
	// times what one carries, the test says that they gain little.
	TestLittleGain = 1.5

	// maxSinks is how many sessions of path tests one node takes at the same time. A
	// peer runs one test at a time, and one run may still be ending when the next begins.
	maxSinks = 4
	// sinkSeconds is how long the receiving end lets one run last. It is longer than
	// TestMaxSeconds by the time a run may take to start and to be stopped.
	sinkSeconds = TestMaxSeconds + 30
	// pathsWait is how long the answer to a request for the peer's paths may take.
	pathsWait = 10 * time.Second
	// testSample is how often a run looks at what has arrived.
	testSample = 200 * time.Millisecond
)

// ErrTestRefused is matched by the error of a path test that cannot start: another one
// runs, the peer is not connected, or the peer does not have the test.
var ErrTestRefused = errors.New("the path test cannot start")

// TestOptions says how to run a path test. The zero value tests every path for
// TestDefaultSeconds a run.
type TestOptions struct {
	Seconds int    // how long each run lasts; 0: TestDefaultSeconds
	Path    string // test only the path of this name; empty: every path that carries data
}

// TestResult is the outcome of a path test.
type TestResult struct {
	Peer    string
	Seconds int
	// PeerDials: this node has no path to the peer, and the paths tested are the ones
	// the peer holds to this node.
	PeerDials bool
	Paths     []PathTest
}

// PathTest is the outcome for one path. The rates are bytes a second that the peer
// reported as received, over the second half of each run.
type PathTest struct {
	Name string
	Subs int     // data sub-connections the path has
	One  float64 // with one data sub-connection
	All  float64 // with Subs of them; zero when Subs is 1
	// LittleGain: All is less than TestLittleGain times One.
	LittleGain bool
	// Error says why the path could not be measured; the rates are zero then.
	Error string
}

// pathSpec is a path as a test needs it: its place among the holder's paths, its name
// and the number of data sub-connections it has.
type pathSpec struct {
	index int
	name  string
	subs  int
}

// testGroups returns, of the dial groups of a peer, the ones a run of a path test
// uses: the data group of the path at index, with subs sub-connections (0: as many as
// it has), and every control and repair group as it is. It returns nil when the path at
// index has no data group.
func testGroups(groups []engine.DialGroup, index, subs int) []engine.DialGroup {
	var out []engine.DialGroup
	found := false
	for _, g := range groups {
		switch {
		case g.Ctrl || g.Repair:
			out = append(out, g)
		case int(g.Path) == index:
			if subs > 0 {
				g.Want = min(subs, g.Want)
			}
			out = append(out, g)
			found = true
		}
	}
	if !found {
		return nil
	}
	return out
}

// TestPeer measures every path to the peer called name that carries data (or the one
// that opt names): what one data sub-connection carries, and what all of the path's
// carry. It sends test data for it and returns when it is done or ctx is. An error that
// matches ErrTestRefused means that no test ran; one that matches node.ErrNotFound,
// that there is no such peer or path.
func (rt *Runtime) TestPeer(ctx context.Context, name string, opt TestOptions) (TestResult, error) {
	res := TestResult{Peer: name, Seconds: opt.Seconds, Paths: []PathTest{}}
	if res.Seconds == 0 {
		res.Seconds = TestDefaultSeconds
	}
	if res.Seconds < 1 || res.Seconds > TestMaxSeconds {
		return res, fmt.Errorf("%w: a run lasts 1 to %d seconds", node.ErrInvalid, TestMaxSeconds)
	}
	peer, ok := rt.Node.Peer(name)
	if !ok {
		return res, fmt.Errorf("peer %q: %w", name, node.ErrNotFound)
	}
	rt.mu.Lock()
	switch pc := rt.ctl[name]; {
	case rt.ctx == nil || rt.ctx.Err() != nil:
		rt.mu.Unlock()
		return res, fmt.Errorf("%w: the node is stopping", ErrTestRefused)
	case rt.testing:
		rt.mu.Unlock()
		return res, fmt.Errorf("%w: another one is running. Wait for it to end.", ErrTestRefused)
	case pc == nil || len(pc.conns) == 0:
		rt.mu.Unlock()
		return res, fmt.Errorf("%w: peer %q is not connected to this node. \"tcpraid peers check\" tries the paths.", ErrTestRefused, name)
	case !pc.greeted:
		// connected, and what the peer can do is not known yet
		rt.mu.Unlock()
		return res, fmt.Errorf("%w: the connection with peer %q has only just come up. Try again in a moment.", ErrTestRefused, name)
	case !slices.Contains(pc.features, control.FeaturePathTest):
		version := pc.version
		rt.mu.Unlock()
		return res, fmt.Errorf("%w: peer %q does not have this test (it runs version %q). Both nodes need tcpraid 0.3.0 or later.", ErrTestRefused, name, version)
	}
	rt.testing = true
	rt.mu.Unlock()
	defer func() {
		rt.mu.Lock()
		rt.testing = false
		rt.mu.Unlock()
	}()

	res.PeerDials = len(peer.Paths) == 0
	var paths []pathSpec
	if res.PeerDials {
		var err error
		if paths, err = rt.askPaths(ctx, name); err != nil {
			return res, err
		}
	} else {
		for i, p := range peer.Paths {
			for _, role := range p.Roles {
				if role == node.RoleData {
					paths = append(paths, pathSpec{index: i, name: p.Name, subs: p.Subs})
				}
			}
		}
	}
	if opt.Path != "" {
		var only []pathSpec
		for _, p := range paths {
			if p.name == opt.Path {
				only = append(only, p)
			}
		}
		if only == nil {
			return res, fmt.Errorf("path %q that carries data: %w", opt.Path, node.ErrNotFound)
		}
		paths = only
	}
	if len(paths) == 0 {
		return res, fmt.Errorf("%w: no path between this node and peer %q carries data", ErrTestRefused, name)
	}
	rt.logf("path test with peer %q: %d path(s), %d seconds a run", name, len(paths), res.Seconds)
	length := time.Duration(res.Seconds) * time.Second
	for _, p := range paths {
		pt := PathTest{Name: p.name, Subs: p.subs}
		var err error
		if pt.One, err = rt.testRun(ctx, name, p, 1, length); err == nil && p.subs > 1 {
			pt.All, err = rt.testRun(ctx, name, p, p.subs, length)
		}
		if err != nil {
			pt.One, pt.All, pt.Error = 0, 0, err.Error()
		} else if p.subs > 1 {
			pt.LittleGain = pt.All < TestLittleGain*pt.One
		}
		res.Paths = append(res.Paths, pt)
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
	}
	return res, nil
}

// testRun sends test data to the peer over the path p for length, with subs data
// sub-connections, and returns the bytes a second that the peer reported as received
// over the second half of the time the run took.
func (rt *Runtime) testRun(ctx context.Context, name string, p pathSpec, subs int, length time.Duration) (float64, error) {
	peer, ok := rt.Node.Peer(name)
	if !ok {
		return 0, fmt.Errorf("peer %q is not paired with this node any more", name)
	}
	keys, err := pairKeys(peer)
	if err != nil {
		return 0, err
	}
	owner := engine.Owner{Peer: name, Test: true}
	gen := newGenerator(TestMaxBytes)
	var failMu sync.Mutex
	failed := ""
	fail := func(text string) {
		failMu.Lock()
		if failed == "" {
			failed = text
		}
		failMu.Unlock()
	}
	type outcome struct {
		end engine.End
		ok  bool
	}
	done := make(chan outcome, 1)
	if len(peer.Paths) == 0 {
		cc := rt.liveControl(name)
		if cc == nil {
			return 0, fmt.Errorf("peer %q is not connected to this node any more", name)
		}
		go func() {
			end, ok := rt.runReverse(cc, peer, keys, gen, engine.SinkIP, engine.SinkPort, owner,
				control.Message{TestPath: p.index + 1, TestSubs: subs}, fail, reverseHow{})
			done <- outcome{end, ok}
		}()
	} else {
		ps, err := rt.peerFor(name)
		if err != nil {
			return 0, err
		}
		groups := testGroups(rt.groups(ps, ps.peer), p.index, subs)
		if groups == nil {
			return 0, fmt.Errorf("path %q carries no data any more", p.name)
		}
		route := rt.Engine.NewRoute(groups, keys, ps.peer.UDPDial)
		go func() {
			end, ok := route.Carry(gen, engine.SinkIP, engine.SinkPort, owner, ps.valid)
			done <- outcome{end, ok}
		}()
	}

	// what the peer has, at each look: the bytes read from the generator that are no
	// longer waiting for the peer's word
	type sample struct {
		at  time.Duration
		got int64
	}
	var samples []sample
	began := time.Now()
	tick := time.NewTicker(testSample)
	defer tick.Stop()
	timer := time.NewTimer(length)
	defer timer.Stop()
	var out outcome
	ended := false
loop:
	for {
		select {
		case <-tick.C:
			for _, si := range rt.Engine.SessionList() {
				if si.Client && si.Test && si.Peer == name {
					samples = append(samples, sample{time.Since(began), si.TxBytes + si.UnsentBytes - si.UnackedBytes})
				}
			}
			if done, given := gen.spent(); done && len(samples) > 0 && samples[len(samples)-1].got >= given {
				break loop // everything a run may send has arrived
			}
		case <-timer.C:
			break loop
		case <-ctx.Done():
			break loop
		case out = <-done:
			ended = true
			break loop
		}
	}
	if !ended {
		rt.Engine.AbortOwner(owner, "the path test is over")
		gen.Close()
		select {
		case out = <-done:
		case <-time.After(10 * time.Second):
			return 0, errors.New("the session of the test did not end")
		}
		if cc := rt.liveControl(name); cc != nil && out.ok {
			cc.Send(control.Message{Type: control.TypeTestEnd, Session: hex.EncodeToString(out.end.ID[:])})
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
	} else {
		// the session ended by itself: something went wrong with it
		gen.Close()
		failMu.Lock()
		text := failed
		failMu.Unlock()
		switch {
		case text != "":
			return 0, errors.New(text)
		case !out.ok:
			return 0, errors.New("this node did not start a session for the test: it carries as many as it takes, or it is stopping")
		case strings.HasPrefix(out.end.Why, engine.WhyUnreachable), out.end.Why == engine.WhyNoSession:
			// the peer did not take the session of the test, and says why on the
			// control connection; that word may come a moment after the end
			key := failKey{name, hex.EncodeToString(out.end.ID[:])}
			var d failDetail
			told := false
			for wait := 0; wait < 10 && !told; wait++ {
				rt.mu.Lock()
				d, told = rt.details[key]
				delete(rt.details, key)
				rt.mu.Unlock()
				if !told {
					time.Sleep(100 * time.Millisecond)
				}
			}
			if told && d.reason != "" {
				return 0, fmt.Errorf("peer %q did not take the test: %s", name, d.reason)
			}
			return 0, fmt.Errorf("peer %q did not take the test", name)
		case !out.end.Up:
			return 0, fmt.Errorf("peer %q answered on none of the connections", name)
		default:
			return 0, fmt.Errorf("the session of the test ended early: %s", out.end.Why)
		}
	}
	if len(samples) < 2 {
		return 0, errors.New("the run was too short to measure")
	}
	last := samples[len(samples)-1]
	if last.got <= 0 {
		text := fmt.Sprintf("no test data arrived at peer %q", name)
		rt.mu.Lock()
		d, ok := rt.dialErr[name]
		rt.mu.Unlock()
		if ok && time.Since(d.at) < dialTroubleAge {
			text += ". The last attempt to open a connection failed with: " + d.text
		}
		return 0, errors.New(text)
	}
	half := samples[0]
	for _, s := range samples {
		if s.at > last.at/2 {
			break
		}
		half = s
	}
	if last.at <= half.at {
		return 0, errors.New("the run was too short to measure")
	}
	if last.got <= half.got {
		return 0, errors.New("nothing arrived at the peer in the second half of the run, so there is no rate to give")
	}
	return float64(last.got-half.got) / (last.at - half.at).Seconds(), nil
}

// askPaths asks the peer, over a control connection, for the paths it holds to this
// node, and returns the ones that carry data.
func (rt *Runtime) askPaths(ctx context.Context, name string) ([]pathSpec, error) {
	cc := rt.liveControl(name)
	if cc == nil {
		return nil, fmt.Errorf("%w: peer %q is not connected to this node", ErrTestRefused, name)
	}
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		return nil, err
	}
	key := reverseKey{name, binary.BigEndian.Uint64(b[:])}
	answer := make(chan []pathSpec, 1)
	rt.mu.Lock()
	rt.pathsAsked[key] = answer
	rt.mu.Unlock()
	defer func() {
		rt.mu.Lock()
		delete(rt.pathsAsked, key)
		rt.mu.Unlock()
	}()
	if err := cc.Send(control.Message{Type: control.TypePaths, Request: key.request}); err != nil {
		return nil, fmt.Errorf("%w: peer %q could not be asked for its paths: %v", ErrTestRefused, name, err)
	}
	select {
	case paths := <-answer:
		return paths, nil
	case <-time.After(pathsWait):
		return nil, fmt.Errorf("%w: peer %q did not say which paths it has within %v", ErrTestRefused, name, pathsWait)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// pathsAnswered handles a peer's answer to a request for its paths.
func (rt *Runtime) pathsAnswered(peer string, m control.Message) {
	rt.mu.Lock()
	answer := rt.pathsAsked[reverseKey{peer, m.Request}]
	rt.mu.Unlock()
	if answer == nil {
		return
	}
	var paths []pathSpec
	for i, p := range m.Paths[:min(len(m.Paths), node.MaxPaths)] {
		if p.Data && p.Subs >= 1 && p.Subs <= node.MaxSubs {
			paths = append(paths, pathSpec{index: i, name: printable(p.Name, node.MaxNameLen), subs: p.Subs})
		}
	}
	select {
	case answer <- paths:
	default: // answered already
	}
}

// servePaths answers a peer's request for the paths this node holds to it: their names
// and sizes, in their order, and no address.
func (rt *Runtime) servePaths(cc *ctlConn, m control.Message) {
	reply := control.Message{Type: control.TypePathsReply, Request: m.Request, Paths: []control.PathInfo{}}
	if peer, ok := rt.Node.Peer(cc.peer); ok {
		for _, p := range peer.Paths {
			info := control.PathInfo{Name: p.Name, Subs: p.Subs}
			for _, role := range p.Roles {
				if role == node.RoleData {
					info.Data = true
				}
			}
			reply.Paths = append(reply.Paths, info)
		}
	}
	cc.Send(reply)
}

// sink gives the local end of a session of a path test that ends in this node
// (engine.SubOptions.Sink): what arrives is thrown away. It refuses when this node
// takes too many such sessions already.
func (rt *Runtime) sink() (net.Conn, string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.sinks >= maxSinks {
		return nil, "the node takes as many path tests as it can already"
	}
	rt.sinks++
	s := &sinkConn{left: TestMaxBytes, closed: make(chan struct{})}
	s.release = func() {
		rt.mu.Lock()
		rt.sinks--
		rt.mu.Unlock()
	}
	s.timer = time.AfterFunc(sinkSeconds*time.Second, func() { s.Close() })
	return s, ""
}

// errTestOver is what the two made-up ends of a test session answer once they are
// closed or past their limit; the transport takes it for a broken connection and ends
// the session with a reset.
var errTestOver = errors.New("the path test is over")

// testAddr is the address of an end of a test session that is no socket.
type testAddr struct{}

func (testAddr) Network() string { return "tcpraid" }
func (testAddr) String() string  { return "path test" }

// sinkConn is the receiving end of a test session: it takes up to left bytes and gives
// none. It ends, and the session with it, when it has taken more, when its time is up,
// or when the session closes it.
type sinkConn struct {
	mu      sync.Mutex
	left    int64
	closed  chan struct{}
	once    sync.Once
	timer   *time.Timer
	release func()
}

func (s *sinkConn) Read([]byte) (int, error) {
	<-s.closed
	return 0, errTestOver
}

func (s *sinkConn) Write(p []byte) (int, error) {
	select {
	case <-s.closed:
		return 0, errTestOver
	default:
	}
	s.mu.Lock()
	s.left -= int64(len(p))
	over := s.left < 0
	s.mu.Unlock()
	if over {
		s.Close()
		return 0, errTestOver
	}
	return len(p), nil
}

func (s *sinkConn) Close() error {
	s.once.Do(func() {
		close(s.closed)
		s.timer.Stop()
		s.release()
	})
	return nil
}

func (*sinkConn) LocalAddr() net.Addr              { return testAddr{} }
func (*sinkConn) RemoteAddr() net.Addr             { return testAddr{} }
func (*sinkConn) SetDeadline(time.Time) error      { return nil }
func (*sinkConn) SetReadDeadline(time.Time) error  { return nil }
func (*sinkConn) SetWriteDeadline(time.Time) error { return nil }

// generator is the sending end of a test session: Read gives data that does not
// compress, up to a total, and then waits. It gives an error once it is closed. A Read
// fills the whole buffer or gives nothing, so the transport never waits to top one up.
type generator struct {
	mu     sync.Mutex
	rng    *rand.ChaCha8
	left   int64
	given  int64
	done   bool // nothing more is given
	closed chan struct{}
	once   sync.Once
}

func newGenerator(total int64) *generator {
	var seed [32]byte
	crand.Read(seed[:]) // a seed of zeros would do as well: the data only has to be dense
	return &generator{rng: rand.NewChaCha8(seed), left: total, closed: make(chan struct{})}
}

func (g *generator) Read(p []byte) (int, error) {
	select {
	case <-g.closed:
		return 0, errTestOver
	default:
	}
	g.mu.Lock()
	n := len(p)
	if int64(n) > g.left || n == 0 {
		g.done, n = true, 0
	} else {
		g.rng.Read(p)
		g.left -= int64(n)
		g.given += int64(n)
	}
	g.mu.Unlock()
	if n == 0 {
		// everything was given: the run ends when the caller sees that it has arrived
		<-g.closed
		return 0, errTestOver
	}
	return n, nil
}

// spent reports whether the generator gives nothing more, and how much it gave.
func (g *generator) spent() (bool, int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.done, g.given
}

func (g *generator) Write(p []byte) (int, error) { return len(p), nil }

func (g *generator) Close() error {
	g.once.Do(func() { close(g.closed) })
	return nil
}

func (*generator) LocalAddr() net.Addr              { return testAddr{} }
func (*generator) RemoteAddr() net.Addr             { return testAddr{} }
func (*generator) SetDeadline(time.Time) error      { return nil }
func (*generator) SetReadDeadline(time.Time) error  { return nil }
func (*generator) SetWriteDeadline(time.Time) error { return nil }
