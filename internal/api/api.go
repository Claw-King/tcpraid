// Package api is the local HTTP API of a tcpraid node. The command line and other
// programs on the host read the node's status and manage its pairing codes, peers and
// forwarding rules through it.
//
//	GET    /healthz                       200 "ok"; the only path that needs no token
//	GET    /api/v1/status                 the node's status (Status)
//	GET    /api/v1/sessions               the live sessions (a list of Session)
//	GET    /metrics                       the engine's counters in the Prometheus text format
//	POST   /api/v1/pairing-codes          make a pairing code (PairingCodeRequest -> PairingCode, with the code)
//	GET    /api/v1/pairing-codes          the codes that have not expired (a list of PairingCode, without the codes)
//	DELETE /api/v1/pairing-codes/{id}     revoke a code
//	POST   /api/v1/peers                  pair with the node that made a code (JoinRequest -> Peer)
//	GET    /api/v1/peers                  the peers (a list of Peer)
//	GET    /api/v1/peers/{name}           one peer (Peer)
//	PUT    /api/v1/peers/{name}           change a peer's paths, allow list, UDP address (PeerUpdate -> Peer)
//	DELETE /api/v1/peers/{name}           end the pairing with a peer and reset its sessions; with the query
//	                                      rules=keep or rules=remove the answer is a PeerRemoval
//	PUT    /api/v1/peers/{name}/paths/{path}     change or add one path of a peer (PathUpdate -> PathSet)
//	DELETE /api/v1/peers/{name}/paths/{path}     remove one path of a peer (Peer)
//	POST   /api/v1/peers/{name}/check     try every path to a peer (PeerCheck)
//	POST   /api/v1/peers/{name}/test      send test data to a peer and measure the paths (PeerTestRequest -> PeerTest)
//	GET    /api/v1/rules                  the forwarding rules with their status (a list of Rule)
//	POST   /api/v1/rules                  make a rule (RuleSpec -> Rule)
//	PUT    /api/v1/rules/{name}           replace a rule (RuleSpec -> Rule)
//	DELETE /api/v1/rules/{name}           remove a rule; its sessions run to their end
//
// A change of the rules takes effect at once and touches only the listeners of the
// rules that changed.
//
// With a web interface (Config.UI) there are also its files, the login and the routes
// in ui.go.
//
// Every path but /healthz needs "Authorization: Bearer TOKEN" (or, with a web
// interface, the cookie of a login) and answers 401 without it, whether the path exists
// or not. An unknown path is 404; a path that exists, asked
// with a method it does not have, is 405. Errors are JSON: {"error": "..."}.
//
// A request body is JSON of at most 64 KB; a field the request does not have is an
// error (400). No answer carries a pair secret; a pairing code is in the answer to the
// request that made it and nowhere else. A proxy is shown without its user and password;
// a path sent back with the proxy as shown keeps the user and password it has.
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/forward"
	"github.com/Claw-King/tcpraid/internal/link"
	"github.com/Claw-King/tcpraid/internal/node"
	"github.com/Claw-King/tcpraid/internal/pair"
)

// Paths of the API.
const (
	PathHealth       = "/healthz"
	PathStatus       = "/api/v1/status"
	PathSessions     = "/api/v1/sessions"
	PathMetrics      = "/metrics"
	PathPairingCodes = "/api/v1/pairing-codes"
	PathPeers        = "/api/v1/peers"
	PathRules        = "/api/v1/rules"
)

// MaxBody is the largest request body the API reads.
const MaxBody = 64 << 10

// Node is what the API needs of the node it serves: the queries and the operations, and
// nothing that holds a key. *link.Service is one.
type Node interface {
	Name() string
	SetName(name string) error
	Fingerprint() string
	PeerCount() int
	RuleCount() int

	CreatePairingCode(addresses []string, ttl time.Duration) (code string, rec node.PairingCode, err error)
	PairingCodes() []node.PairingCode
	RevokePairingCode(id string) error

	Join(ctx context.Context, code, address string) (node.Peer, error)
	Peers() []node.Peer
	Peer(name string) (node.Peer, error)
	UpdatePeer(name string, paths *[]node.Path, allow, udpDial *string) (node.Peer, error)
	SetPath(name, path string, c node.PathChange) (stored node.Peer, created bool, err error)
	RemovePath(name, path string) (node.Peer, error)
	RemovePeer(name string) error
	CheckPeer(ctx context.Context, name string) ([]link.PathResult, error)
}

// Rules is what the API needs of the rules runtime. *forward.Runtime is one.
type Rules interface {
	Rules() []forward.RuleStatus
	AddRule(r node.Rule) error
	ReplaceRule(name string, r node.Rule) error
	RemoveRule(name string) error
	PeerStatus(name string) forward.PeerStatus
	TestPeer(ctx context.Context, name string, opt forward.TestOptions) (forward.TestResult, error)
}

// Config is what a Server serves.
type Config struct {
	Version string // version of the program
	Token   string // access token; with an empty one every request that needs a token is refused
	Node    Node
	Engine  *engine.Engine
	// Rules runs the forwarding rules. nil: the node has none and the rule paths
	// answer 404.
	Rules Rules
	// UI is the web interface. nil: there is none, and only the token gives access (a
	// request for one of its pages is then answered like any request without a token,
	// with 401). A UI that is switched off (UI.Off) answers 404 for its pages instead.
	UI *UI
}

// Status is the answer of GET /api/v1/status.
type Status struct {
	Version       string     `json:"version"`
	WireVersion   int        `json:"wire_version"`
	Started       time.Time  `json:"started"`
	UptimeSeconds int64      `json:"uptime_seconds"`
	Node          StatusNode `json:"node"`
	Sessions      int        `json:"sessions"`
	Peers         int        `json:"peers"`
	Rules         int        `json:"rules"`
}

// StatusNode names the node in a Status.
type StatusNode struct {
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
}

// Session is one element of the answer of GET /api/v1/sessions.
type Session struct {
	ID           string    `json:"id"` // hex
	Destination  string    `json:"destination"`
	Role         string    `json:"role"` // "client" or "server"
	Started      time.Time `json:"started"`
	TxBytes      int64     `json:"tx_bytes"`
	RxBytes      int64     `json:"rx_bytes"`
	UnackedBytes int64     `json:"unacked_bytes"`
	ResentBytes  int64     `json:"resent_bytes"`   // stream bytes queued for sending again
	Subs         int       `json:"subs"`           // live sub-connections
	Peer         string    `json:"peer,omitempty"` // the peer at the other end
	Rule         string    `json:"rule,omitempty"` // role client: the rule that carries the connection
	// Since 0.3.0. unacked_bytes is the sum of the next two: read from the application
	// and never sent yet, and sent but not yet reported received by the peer.
	UnsentBytes   int64 `json:"unsent_bytes"`
	InflightBytes int64 `json:"inflight_bytes"`
	// Up: the peer has answered on a sub-connection at least once.
	Up bool `json:"up"`
	// SilentMs is how long ago anything arrived from the peer. GraceLeftMs is how much
	// longer the session lasts if nothing arrives; at zero it is reset.
	SilentMs    int64 `json:"silent_ms"`
	GraceLeftMs int64 `json:"grace_left_ms"`
	// Test: the session carries the data of a path test, not an application connection.
	Test bool `json:"test,omitempty"`
}

// PairingCodeRequest is the body of POST /api/v1/pairing-codes. Both fields are
// optional: without addresses the node uses the ones it was started with, without a
// time to live the default.
type PairingCodeRequest struct {
	Addresses  []string `json:"addresses"`
	TTLSeconds int64    `json:"ttl_seconds"`
}

// PairingCode describes a pairing code. Code is the code itself and is set only in the
// answer to the request that made it.
type PairingCode struct {
	ID      string    `json:"id"`
	Expires time.Time `json:"expires"`
	Used    bool      `json:"used"`
	Code    string    `json:"code,omitempty"`
}

// JoinRequest is the body of POST /api/v1/peers. Address is optional: where to connect
// instead of the addresses in the code.
type JoinRequest struct {
	Code    string `json:"code"`
	Address string `json:"address"`
}

// Peer describes a peer. It has no pair secret.
type Peer struct {
	Name        string    `json:"name"`
	Fingerprint string    `json:"fingerprint"`
	Addresses   []string  `json:"addresses"`
	PairedAt    time.Time `json:"paired_at"`
	Paths       []Path    `json:"paths"`
	Allow       string    `json:"allow"`    // destinations the peer may reach through this node; empty: any
	UDPDial     string    `json:"udp_dial"` // where UDP feedback for the peer is sent; empty: nowhere
	// What the control connections with the peer say.
	Online      bool       `json:"online"`                 // one is up, in either direction
	Version     string     `json:"version,omitempty"`      // the program version the peer reported
	LastMessage *time.Time `json:"last_message,omitempty"` // when the last message from the peer arrived
}

// Path describes one way to a peer. A proxy is shown without its user and password.
// Addrs (since 0.5.0) are further addresses of the same listener, tried in their order
// after Addr; absent when the path has one address. Mark (since 0.5.0) is the socket
// mark of the path's connections; absent when it has none.
type Path struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Addr     string   `json:"addr"`
	Addrs    []string `json:"addrs,omitempty"`
	Proxy    string   `json:"proxy,omitempty"`
	Security string   `json:"security"`
	Verify   string   `json:"verify,omitempty"`
	Pin      string   `json:"pin,omitempty"`
	Roles    []string `json:"roles"`
	Subs     int      `json:"subs"`
	Mark     int      `json:"mark,omitempty"`
}

// PeerUpdate is the body of PUT /api/v1/peers/{name}. A field that is absent leaves
// that setting as it is; at least one must be there. Paths replaces the peer's paths as
// a whole (an empty list leaves the peer without a path). Allow is a list of networks
// with optional ports ("192.0.2.0/24:873, [2001:db8::/32]:5000-5010"); empty means any
// destination. UDPDial is a host:port; empty means none.
type PeerUpdate struct {
	Paths   *[]Path `json:"paths"`
	Allow   *string `json:"allow"`
	UDPDial *string `json:"udp_dial"`
}

// PathUpdate is the body of PUT /api/v1/peers/{name}/paths/{path} (since 0.4.0): what
// to set on that one path. A field that is absent stays as it is; at least one must be
// there. A path that the peer does not have yet is added: it needs addr, and what the
// request does not name is type tcp, security tls, the data role and 8 sub-connections.
// The node changes the one path in one step, so requests for two paths of a peer that
// arrive at the same time both take effect, which a PUT of the whole list cannot
// promise.
//
// Since 0.5.0: addrs sets the further addresses of the path (an empty list: none), and
// a request that sets addr without addrs takes them away. mark sets the socket mark (0:
// none).
type PathUpdate struct {
	Addr *string `json:"addr"`
	// left out of a request that does not set them, so that a node from before 0.5.0
	// takes a request that only names what it knows
	Addrs    *[]string `json:"addrs,omitempty"`
	Mark     *int      `json:"mark,omitempty"`
	Type     *string   `json:"type"`
	Proxy    *string   `json:"proxy"`
	Security *string   `json:"security"`
	Verify   *string   `json:"verify"`
	Pin      *string   `json:"pin"`
	Roles    *[]string `json:"roles"`
	Subs     *int      `json:"subs"`
}

// PathSet is the answer of PUT /api/v1/peers/{name}/paths/{path}: whether the path is
// new, and the peer with all its paths as stored.
type PathSet struct {
	Created bool `json:"created"`
	Peer    Peer `json:"peer"`
}

// RuleSpec is the body of POST /api/v1/rules and PUT /api/v1/rules/{name}: a fixed rule
// (listen and destination, each IP:port or IP:first-last, with ranges of the same
// length) or a transparent rule (match, a list of networks with optional ports). Since
// 0.4.0 the destination may have a host name in the place of the IP, which the peer
// looks up for every connection. Enabled is true when absent. In a PUT, an absent name
// means the name in the path.
//
// Since 0.5.0: paths names the paths the rule's connections use (an empty list: every
// path), and subs the most data sub-connections on each (0: as many as the path has).
// In a PUT, either one that is absent stays as the stored rule has it, so that a
// caller that does not know the two fields does not take them away.
type RuleSpec struct {
	Name        string    `json:"name"`
	Listen      string    `json:"listen,omitempty"`
	Destination string    `json:"destination,omitempty"`
	Match       string    `json:"match,omitempty"`
	Peer        string    `json:"peer"`
	Enabled     *bool     `json:"enabled,omitempty"`
	Paths       *[]string `json:"paths,omitempty"`
	Subs        *int      `json:"subs,omitempty"`
}

// Rule describes a forwarding rule and how it is doing.
type Rule struct {
	Name        string `json:"name"`
	Listen      string `json:"listen,omitempty"`
	Destination string `json:"destination,omitempty"`
	Match       string `json:"match,omitempty"`
	Peer        string `json:"peer"`
	Enabled     bool   `json:"enabled"`
	Listening   bool   `json:"listening"`       // connections for the rule are being taken
	Error       string `json:"error,omitempty"` // why not, or what went wrong with the last connection
	Sessions    int    `json:"sessions"`        // live sessions the rule carries
	// ErrorAt is when what Error says happened (since 0.3.0); absent where Error is a
	// state that holds now and not an event.
	ErrorAt *time.Time `json:"error_at,omitempty"`
	// Paths and Subs (since 0.5.0): the paths the rule's connections use and the most
	// data sub-connections on each; absent when the rule uses everything.
	Paths []string `json:"paths,omitempty"`
	Subs  int      `json:"subs,omitempty"`
	// PeerDials (since 0.5.0): new connections of the rule are opened by the peer for
	// now, because this node's own path to the peer is down. Absent otherwise.
	PeerDials bool `json:"peer_dials,omitempty"`
}

// PeerRemoval is the answer of DELETE /api/v1/peers/{name} when the request has the
// query "rules" (since 0.4.0): "rules=keep" removes the peer and lists the rules that
// still name it, "rules=remove" removes those rules with it. Without the query the
// answer is 204 with no body, as before, and the rules stay.
type PeerRemoval struct {
	Peer string `json:"peer"`
	// Rules are the names of the rules that named the peer, in the order of the list.
	Rules []string `json:"rules"`
	// RulesRemoved: they were removed with the peer. Otherwise they are still there,
	// and a node that is paired under the same name later takes them over.
	RulesRemoved bool `json:"rules_removed"`
}

// PeerCheck is the answer of POST /api/v1/peers/{name}/check.
type PeerCheck struct {
	Peer  string      `json:"peer"`
	Paths []PathCheck `json:"paths"` // empty: this node has no path to the peer
}

// PathCheck is the outcome of the check of one path at one address. Since 0.5.0 a path
// with several addresses has one entry for each, in the order of the path.
type PathCheck struct {
	Name     string `json:"name"`
	Addr     string `json:"addr"`
	Security string `json:"security"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

// PeerTestRequest is the body of POST /api/v1/peers/{name}/test. Both fields are
// optional: Seconds is how long each run lasts (1 to 60; absent or 0: 10), Path names
// the one path to test (absent or empty: every path that carries data).
type PeerTestRequest struct {
	Seconds int    `json:"seconds"`
	Path    string `json:"path"`
}

// PeerTest is the answer of POST /api/v1/peers/{name}/test.
type PeerTest struct {
	Peer    string `json:"peer"`
	Seconds int    `json:"seconds"` // how long each run lasted at most
	// PeerDials: this node has no path to the peer, and the paths are the ones the peer
	// holds to this node.
	PeerDials bool       `json:"peer_dials"`
	Paths     []PathTest `json:"paths"`
}

// PathTest is the outcome of a path test for one path: the bytes a second that the
// peer reported as received with one data sub-connection, and with all the path has
// (0 when it has one). LittleGain says that all of them carried less than 1.5 times
// what one carried. With Error, the path could not be measured.
type PathTest struct {
	Name       string  `json:"name"`
	Subs       int     `json:"subs"`
	One        float64 `json:"one_bytes_per_second"`
	All        float64 `json:"all_bytes_per_second"`
	LittleGain bool    `json:"little_gain"`
	Error      string  `json:"error,omitempty"`
}

// route is one method on one path. A segment of the form {name} matches any one segment
// of a request path and gives its value to the handler.
type route struct {
	method   string
	segments []string
	handle   func(w http.ResponseWriter, r *http.Request, arg map[string]string)
}

// Server answers API requests. It is an http.Handler; Serve runs it on a listener.
type Server struct {
	cfg     Config
	started time.Time
	routes  []route
}

// New returns a Server. Its start time, which the status reports, is the time of the call.
func New(cfg Config) *Server {
	s := &Server{cfg: cfg, started: time.Now()}
	add := func(method, path string, h func(http.ResponseWriter, *http.Request, map[string]string)) {
		s.routes = append(s.routes, route{method, strings.Split(path, "/"), h})
	}
	add(http.MethodGet, PathHealth, health)
	add(http.MethodGet, PathStatus, s.status)
	add(http.MethodGet, PathSessions, s.sessions)
	add(http.MethodGet, PathMetrics, s.metrics)
	add(http.MethodPost, PathPairingCodes, s.createPairingCode)
	add(http.MethodGet, PathPairingCodes, s.listPairingCodes)
	add(http.MethodDelete, PathPairingCodes+"/{id}", s.revokePairingCode)
	add(http.MethodPost, PathPeers, s.join)
	add(http.MethodGet, PathPeers, s.listPeers)
	add(http.MethodGet, PathPeers+"/{name}", s.getPeer)
	add(http.MethodPut, PathPeers+"/{name}", s.putPeer)
	add(http.MethodDelete, PathPeers+"/{name}", s.removePeer)
	add(http.MethodPost, PathPeers+"/{name}/check", s.checkPeer)
	add(http.MethodPut, PathPeers+"/{name}/paths/{path}", s.putPath)
	add(http.MethodDelete, PathPeers+"/{name}/paths/{path}", s.removePath)
	add(http.MethodPut, PathNode, s.putNode)
	if cfg.UI != nil && !cfg.UI.Off {
		add(http.MethodPost, PathLogout, s.logout)
	}
	if cfg.Rules != nil {
		add(http.MethodGet, PathRules, s.listRules)
		add(http.MethodPost, PathRules, s.addRule)
		add(http.MethodPut, PathRules+"/{name}", s.putRule)
		add(http.MethodDelete, PathRules+"/{name}", s.removeRule)
		add(http.MethodPost, PathPeers+"/{name}/test", s.testPeer)
	}
	return s
}

// Serve answers requests on ln until ctx is done. It then closes the listener, gives
// requests in progress up to 5 s to finish and returns nil. It returns earlier, with the
// error, only when the listener fails.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	hs := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// "OPTIONS *" goes through ServeHTTP like every request, and so needs the token;
		// the server would otherwise answer it 200 itself
		DisableGeneralOptionsHandler: true,
	}
	shut := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(shut)
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if hs.Shutdown(c) != nil {
			hs.Close()
		}
	})
	err := hs.Serve(ln)
	if stop() {
		return err // ctx is not done: the listener failed
	}
	<-shut
	return nil
}

// match compares the segments of a request path with a route's. ok says whether they
// match; arg holds the values of the {name} segments.
func match(pattern, segments []string) (arg map[string]string, ok bool) {
	if len(pattern) != len(segments) {
		return nil, false
	}
	arg = map[string]string{}
	for i, p := range pattern {
		if strings.HasPrefix(p, "{") && strings.HasSuffix(p, "}") {
			if segments[i] == "" {
				return nil, false
			}
			arg[p[1:len(p)-1]] = segments[i]
		} else if p != segments[i] {
			return nil, false
		}
	}
	return arg, true
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ui := s.cfg.UI
	if ui != nil && ui.serve(w, r) {
		return // a file of the web interface, or the login
	}
	if r.URL.Path != PathHealth && !s.authorized(r) {
		switch {
		case ui == nil || !ui.loggedIn(r):
			// before the path is looked at: without the token a caller does not learn
			// which paths exist
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "missing or wrong access token")
			return
		case changes(r.Method) && r.Header.Get(RequestHeader) != "1":
			writeError(w, http.StatusForbidden, "a request that the login cookie authorizes and that changes something needs the header "+RequestHeader+": 1")
			return
		}
	}
	// The path is split while still escaped, so that a name with a "/" in it, sent as
	// %2F, stays one segment.
	segments := strings.Split(r.URL.EscapedPath(), "/")
	for i, seg := range segments {
		v, err := url.PathUnescape(seg)
		if err != nil {
			writeError(w, http.StatusBadRequest, "the path is not properly escaped")
			return
		}
		segments[i] = v
	}
	var allowed []string
	for _, rt := range s.routes {
		arg, ok := match(rt.segments, segments)
		if !ok {
			continue
		}
		if rt.method == r.Method {
			rt.handle(w, r, arg)
			return
		}
		allowed = append(allowed, rt.method)
	}
	if allowed == nil {
		writeError(w, http.StatusNotFound, "no such path")
		return
	}
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

// authorized reports whether the request carries the access token. Digests are compared,
// in constant time, so the time taken says nothing about the token or its length.
func (s *Server) authorized(r *http.Request) bool {
	scheme, got, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || s.cfg.Token == "" {
		return false
	}
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(s.cfg.Token))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func health(w http.ResponseWriter, _ *http.Request, _ map[string]string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok"))
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request, _ map[string]string) {
	n := s.cfg.Node
	writeJSON(w, http.StatusOK, Status{
		Version:       s.cfg.Version,
		WireVersion:   engine.WireVersion,
		Started:       s.started.UTC(),
		UptimeSeconds: int64(time.Since(s.started) / time.Second),
		Node:          StatusNode{Name: n.Name(), Fingerprint: n.Fingerprint()},
		Sessions:      int(s.cfg.Engine.Sessions()),
		Peers:         n.PeerCount(),
		Rules:         n.RuleCount(),
	})
}

func (s *Server) sessions(w http.ResponseWriter, _ *http.Request, _ map[string]string) {
	list := []Session{} // no sessions is [], not null
	for _, si := range s.cfg.Engine.SessionList() {
		role := "server"
		if si.Client {
			role = "client"
		}
		list = append(list, Session{
			ID:           hex.EncodeToString(si.ID[:]),
			Destination:  si.Destination,
			Role:         role,
			Started:      si.Started.UTC(),
			TxBytes:      si.TxBytes,
			RxBytes:      si.RxBytes,
			UnackedBytes: si.UnackedBytes,
			ResentBytes:  si.ResentBytes,
			Subs:         si.Subs,
			Peer:         si.Peer,
			Rule:         si.Rule,

			UnsentBytes:   si.UnsentBytes,
			InflightBytes: si.UnackedBytes - si.UnsentBytes,
			Up:            si.Up,
			SilentMs:      si.Silent.Milliseconds(),
			GraceLeftMs:   si.GraceLeft.Milliseconds(),
			Test:          si.Test,
		})
	}
	writeJSON(w, http.StatusOK, list)
}

// metrics writes the engine's counters in the Prometheus text exposition format, under
// their expvar names with the prefix "tcpraid_".
func (s *Server) metrics(w http.ResponseWriter, _ *http.Request, _ map[string]string) {
	var b bytes.Buffer
	typed := map[string]bool{}
	for _, c := range s.cfg.Engine.Counters() {
		name := "tcpraid_" + c.Name
		if !typed[name] {
			typed[name] = true
			kind := "counter"
			if c.Gauge {
				kind = "gauge"
			}
			fmt.Fprintf(&b, "# TYPE %s %s\n", name, kind)
		}
		if c.Key != "" {
			fmt.Fprintf(&b, "%s{%s=%q} %d\n", name, c.Label, c.Key, c.Value)
		} else {
			fmt.Fprintf(&b, "%s %d\n", name, c.Value)
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Write(b.Bytes())
}

// readBody decodes the JSON body of a request into v. An empty body leaves v as it is.
// It answers the request itself and returns false when the body cannot be used.
func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the request body is larger than %d bytes", MaxBody))
		} else {
			writeError(w, http.StatusBadRequest, "the request body could not be read")
		}
		return false
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return true
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "the request body is not the JSON this request takes: "+err.Error())
		return false
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, "the request body holds more than one JSON value")
		return false
	}
	return true
}

// fail answers a request with the error of an operation: what the caller got wrong is
// 400, what does not exist 404, a pairing that ran out of time 504, one that the other
// node did not complete 502, a path test that cannot start now 409, anything else 500.
func fail(w http.ResponseWriter, err error, remote bool) {
	switch {
	case errors.Is(err, forward.ErrTestRefused):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, link.ErrInvalid), errors.Is(err, node.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, node.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, link.ErrTimeout):
		writeError(w, http.StatusGatewayTimeout, err.Error())
	case remote:
		writeError(w, http.StatusBadGateway, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func (s *Server) createPairingCode(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	var req PairingCodeRequest
	if !readBody(w, r, &req) {
		return
	}
	// the range is checked on the number of seconds, before it becomes a duration: a
	// large number would overflow one and come out small
	if max := int64(pair.MaxTTL / time.Second); req.TTLSeconds < 0 || req.TTLSeconds > max {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("ttl_seconds is out of range: 1 to %d (30 days), or 0 or absent for the default", max))
		return
	}
	code, rec, err := s.cfg.Node.CreatePairingCode(req.Addresses, time.Duration(req.TTLSeconds)*time.Second)
	if err != nil {
		fail(w, err, false)
		return
	}
	// the one answer that carries a code: it is not to be kept by anything in between
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, PairingCode{ID: rec.ID, Expires: rec.Expires.UTC(), Used: rec.Used, Code: code})
}

func (s *Server) listPairingCodes(w http.ResponseWriter, _ *http.Request, _ map[string]string) {
	list := []PairingCode{}
	for _, rec := range s.cfg.Node.PairingCodes() {
		list = append(list, PairingCode{ID: rec.ID, Expires: rec.Expires.UTC(), Used: rec.Used})
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) revokePairingCode(w http.ResponseWriter, _ *http.Request, arg map[string]string) {
	if err := s.cfg.Node.RevokePairingCode(arg["id"]); err != nil {
		fail(w, err, false)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) join(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	var req JoinRequest
	if !readBody(w, r, &req) {
		return
	}
	if req.Code == "" {
		writeError(w, http.StatusBadRequest, "the request has no pairing code (field \"code\")")
		return
	}
	p, err := s.cfg.Node.Join(r.Context(), req.Code, req.Address)
	if err != nil {
		fail(w, err, true)
		return
	}
	writeJSON(w, http.StatusCreated, s.peer(p))
}

func (s *Server) listPeers(w http.ResponseWriter, _ *http.Request, _ map[string]string) {
	list := []Peer{}
	for _, p := range s.cfg.Node.Peers() {
		list = append(list, s.peer(p))
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) getPeer(w http.ResponseWriter, _ *http.Request, arg map[string]string) {
	p, err := s.cfg.Node.Peer(arg["name"])
	if err != nil {
		fail(w, err, false)
		return
	}
	writeJSON(w, http.StatusOK, s.peer(p))
}

func (s *Server) putPeer(w http.ResponseWriter, r *http.Request, arg map[string]string) {
	var req PeerUpdate
	if !readBody(w, r, &req) {
		return
	}
	if req.Paths == nil && req.Allow == nil && req.UDPDial == nil {
		writeError(w, http.StatusBadRequest, "the request changes nothing, because it has none of the fields \"paths\", \"allow\", \"udp_dial\". An empty list for \"paths\" removes every path.")
		return
	}
	var paths *[]node.Path
	if req.Paths != nil {
		list := make([]node.Path, 0, len(*req.Paths))
		for _, p := range *req.Paths {
			path := node.Path{
				Name: p.Name, Type: p.Type, Addr: p.Addr, Proxy: p.Proxy, Security: p.Security,
				Verify: p.Verify, Pin: p.Pin, Roles: append([]string{}, p.Roles...), Subs: p.Subs, Mark: p.Mark,
			}
			if len(p.Addrs) > 0 {
				path.Addrs = append([]string{}, p.Addrs...)
			}
			list = append(list, path)
		}
		paths = &list
	}
	p, err := s.cfg.Node.UpdatePeer(arg["name"], paths, req.Allow, req.UDPDial)
	if err != nil {
		fail(w, err, false)
		return
	}
	writeJSON(w, http.StatusOK, s.peer(p))
}

func (s *Server) putPath(w http.ResponseWriter, r *http.Request, arg map[string]string) {
	var req PathUpdate
	if !readBody(w, r, &req) {
		return
	}
	c := node.PathChange{Addr: req.Addr, Addrs: req.Addrs, Mark: req.Mark, Type: req.Type, Proxy: req.Proxy, Security: req.Security, Verify: req.Verify, Pin: req.Pin, Roles: req.Roles, Subs: req.Subs}
	if c.Empty() {
		writeError(w, http.StatusBadRequest, "the request changes nothing, because it has none of the fields \"addr\", \"addrs\", \"mark\", \"type\", \"proxy\", \"security\", \"verify\", \"pin\", \"roles\", \"subs\"")
		return
	}
	p, created, err := s.cfg.Node.SetPath(arg["name"], arg["path"], c)
	if err != nil {
		fail(w, err, false)
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, PathSet{Created: created, Peer: s.peer(p)})
}

func (s *Server) removePath(w http.ResponseWriter, _ *http.Request, arg map[string]string) {
	p, err := s.cfg.Node.RemovePath(arg["name"], arg["path"])
	if err != nil {
		fail(w, err, false)
		return
	}
	writeJSON(w, http.StatusOK, s.peer(p))
}

func (s *Server) removePeer(w http.ResponseWriter, r *http.Request, arg map[string]string) {
	name := arg["name"]
	mode := ""
	if q := r.URL.Query(); q.Has("rules") {
		if mode = q.Get("rules"); mode != "keep" && mode != "remove" {
			writeError(w, http.StatusBadRequest, "the query \"rules\" is \"keep\" or \"remove\"")
			return
		}
	}
	out := PeerRemoval{Peer: name, Rules: []string{}, RulesRemoved: mode == "remove"}
	if mode != "" {
		// the peer has to be there before any of its rules goes
		if _, err := s.cfg.Node.Peer(name); err != nil {
			fail(w, err, false)
			return
		}
		if s.cfg.Rules != nil {
			for _, st := range s.cfg.Rules.Rules() {
				if st.Rule.Peer == name {
					out.Rules = append(out.Rules, st.Rule.Name)
				}
			}
		}
	}
	// the peer first: if its removal fails nothing has changed, and if the removal of a
	// rule fails afterwards the rules are merely still there, as without "remove"
	if err := s.cfg.Node.RemovePeer(name); err != nil {
		fail(w, err, false)
		return
	}
	if mode == "remove" {
		for _, rule := range out.Rules {
			// one that somebody else removed in between is gone all the same
			if err := s.cfg.Rules.RemoveRule(rule); err != nil && !errors.Is(err, node.ErrNotFound) {
				fail(w, err, false)
				return
			}
		}
	}
	if mode == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) checkPeer(w http.ResponseWriter, r *http.Request, arg map[string]string) {
	if !readBody(w, r, &struct{}{}) {
		return
	}
	results, err := s.cfg.Node.CheckPeer(r.Context(), arg["name"])
	if err != nil {
		fail(w, err, false)
		return
	}
	out := PeerCheck{Peer: arg["name"], Paths: []PathCheck{}}
	for _, res := range results {
		out.Paths = append(out.Paths, PathCheck{Name: res.Name, Addr: res.Addr, Security: res.Security, OK: res.OK, Error: res.Error})
	}
	writeJSON(w, http.StatusOK, out)
}

// testPeer runs a path test. The answer comes when the test is over: two runs for each
// path, each as long as the request says. A caller that goes away ends the test.
func (s *Server) testPeer(w http.ResponseWriter, r *http.Request, arg map[string]string) {
	var req PeerTestRequest
	if !readBody(w, r, &req) {
		return
	}
	res, err := s.cfg.Rules.TestPeer(r.Context(), arg["name"], forward.TestOptions{Seconds: req.Seconds, Path: req.Path})
	if err != nil {
		if r.Context().Err() != nil {
			return // the caller is gone
		}
		fail(w, err, false)
		return
	}
	out := PeerTest{Peer: res.Peer, Seconds: res.Seconds, PeerDials: res.PeerDials, Paths: []PathTest{}}
	for _, p := range res.Paths {
		out.Paths = append(out.Paths, PathTest{Name: p.Name, Subs: p.Subs, One: p.One, All: p.All, LittleGain: p.LittleGain, Error: p.Error})
	}
	writeJSON(w, http.StatusOK, out)
}

func apiRule(st forward.RuleStatus) Rule {
	r := st.Rule
	out := Rule{Name: r.Name, Listen: r.Listen, Destination: r.Destination, Match: r.Match, Peer: r.Peer, Enabled: r.Enabled,
		Listening: st.Listening, Error: st.Error, Sessions: st.Sessions, Subs: r.Subs, PeerDials: st.PeerDials}
	if len(r.Paths) > 0 {
		out.Paths = append([]string{}, r.Paths...)
	}
	if st.Error != "" && !st.ErrorAt.IsZero() {
		at := st.ErrorAt.UTC()
		out.ErrorAt = &at
	}
	return out
}

func (s *Server) listRules(w http.ResponseWriter, _ *http.Request, _ map[string]string) {
	list := []Rule{}
	for _, st := range s.cfg.Rules.Rules() {
		list = append(list, apiRule(st))
	}
	writeJSON(w, http.StatusOK, list)
}

// ruleStatus answers a request that stored the rule called name with the rule as it is
// now.
func (s *Server) ruleStatus(w http.ResponseWriter, code int, name string) {
	for _, st := range s.cfg.Rules.Rules() {
		if st.Rule.Name == name {
			writeJSON(w, code, apiRule(st))
			return
		}
	}
	// removed again by another request in between
	writeError(w, http.StatusNotFound, fmt.Sprintf("rule %q: not found", name))
}

// nodeRule makes the rule a request describes. was, if not nil, is the stored rule that
// the request replaces: what the request does not say about paths and sub-connections
// stays as was has it.
func nodeRule(spec RuleSpec, was *node.Rule) node.Rule {
	r := node.Rule{Name: spec.Name, Listen: spec.Listen, Destination: spec.Destination, Match: spec.Match, Peer: spec.Peer,
		Enabled: spec.Enabled == nil || *spec.Enabled}
	if was != nil {
		r.Paths, r.Subs = was.Clone().Paths, was.Subs
	}
	if spec.Paths != nil {
		r.Paths = nil
		if len(*spec.Paths) > 0 {
			r.Paths = append([]string{}, *spec.Paths...)
		}
	}
	if spec.Subs != nil {
		r.Subs = *spec.Subs
	}
	return r
}

func (s *Server) addRule(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	var spec RuleSpec
	if !readBody(w, r, &spec) {
		return
	}
	if err := s.cfg.Rules.AddRule(nodeRule(spec, nil)); err != nil {
		fail(w, err, false)
		return
	}
	s.ruleStatus(w, http.StatusCreated, spec.Name)
}

func (s *Server) putRule(w http.ResponseWriter, r *http.Request, arg map[string]string) {
	var spec RuleSpec
	if !readBody(w, r, &spec) {
		return
	}
	if spec.Name == "" {
		spec.Name = arg["name"]
	}
	var was *node.Rule
	for _, st := range s.cfg.Rules.Rules() {
		if st.Rule.Name == arg["name"] {
			was = &st.Rule
			break
		}
	}
	if err := s.cfg.Rules.ReplaceRule(arg["name"], nodeRule(spec, was)); err != nil {
		fail(w, err, false)
		return
	}
	s.ruleStatus(w, http.StatusOK, spec.Name)
}

func (s *Server) removeRule(w http.ResponseWriter, _ *http.Request, arg map[string]string) {
	if err := s.cfg.Rules.RemoveRule(arg["name"]); err != nil {
		fail(w, err, false)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// peer returns a peer in the form the API shows, with what the control connections
// say about it.
func (s *Server) peer(p node.Peer) Peer {
	out := apiPeer(p)
	if s.cfg.Rules != nil {
		st := s.cfg.Rules.PeerStatus(p.Name)
		out.Online, out.Version = st.Online, st.Version
		if !st.LastMessage.IsZero() {
			at := st.LastMessage.UTC()
			out.LastMessage = &at
		}
	}
	return out
}

// apiPeer copies a peer into the form the API shows. The pair secret has no field there.
func apiPeer(p node.Peer) Peer {
	out := Peer{Name: p.Name, Fingerprint: p.Fingerprint, Addresses: append([]string{}, p.Addresses...), PairedAt: p.PairedAt.UTC(), Paths: []Path{},
		Allow: p.Allow, UDPDial: p.UDPDial}
	for _, path := range p.Paths {
		shown := Path{
			Name: path.Name, Type: path.Type, Addr: path.Addr, Proxy: withoutUser(path.Proxy), Security: path.Security,
			Verify: path.Verify, Pin: path.Pin, Roles: append([]string{}, path.Roles...), Subs: path.Subs, Mark: path.Mark,
		}
		if len(path.Addrs) > 0 {
			shown.Addrs = append([]string{}, path.Addrs...)
		}
		out.Paths = append(out.Paths, shown)
	}
	return out
}

// withoutUser returns a proxy URL without its user and password.
func withoutUser(proxy string) string {
	if proxy == "" {
		return ""
	}
	u, err := url.Parse(proxy)
	if err != nil {
		return "(not shown: not a URL)"
	}
	if u.User == nil {
		return proxy
	}
	u.User = nil
	return u.String()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
