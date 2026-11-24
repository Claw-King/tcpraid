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
//	DELETE /api/v1/peers/{name}           end the pairing with a peer and reset its sessions
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
	// UI is the web interface. nil: there is none, and only the token gives access.
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
type Path struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Addr     string   `json:"addr"`
	Proxy    string   `json:"proxy,omitempty"`
	Security string   `json:"security"`
	Verify   string   `json:"verify,omitempty"`
	Pin      string   `json:"pin,omitempty"`
	Roles    []string `json:"roles"`
	Subs     int      `json:"subs"`
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

// RuleSpec is the body of POST /api/v1/rules and PUT /api/v1/rules/{name}: a fixed rule
// (listen and destination, each IP:port or IP:first-last, with ranges of the same
// length) or a transparent rule (match, a list of networks with optional ports). Enabled
// is true when absent. In a PUT, an absent name means the name in the path.
type RuleSpec struct {
	Name        string `json:"name"`
	Listen      string `json:"listen,omitempty"`
	Destination string `json:"destination,omitempty"`
	Match       string `json:"match,omitempty"`
	Peer        string `json:"peer"`
	Enabled     *bool  `json:"enabled,omitempty"`
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
}

// PeerCheck is the answer of POST /api/v1/peers/{name}/check.
type PeerCheck struct {
	Peer  string      `json:"peer"`
	Paths []PathCheck `json:"paths"` // empty: this node has no path to the peer
}

// PathCheck is the outcome of the check of one path.
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
	add(http.MethodPut, PathNode, s.putNode)
	if cfg.UI != nil {
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
			list = append(list, node.Path{
				Name: p.Name, Type: p.Type, Addr: p.Addr, Proxy: p.Proxy, Security: p.Security,
				Verify: p.Verify, Pin: p.Pin, Roles: append([]string{}, p.Roles...), Subs: p.Subs,
			})
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

func (s *Server) removePeer(w http.ResponseWriter, _ *http.Request, arg map[string]string) {
	if err := s.cfg.Node.RemovePeer(arg["name"]); err != nil {
		fail(w, err, false)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
		Listening: st.Listening, Error: st.Error, Sessions: st.Sessions}
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

func nodeRule(spec RuleSpec) node.Rule {
	return node.Rule{Name: spec.Name, Listen: spec.Listen, Destination: spec.Destination, Match: spec.Match, Peer: spec.Peer,
		Enabled: spec.Enabled == nil || *spec.Enabled}
}

func (s *Server) addRule(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	var spec RuleSpec
	if !readBody(w, r, &spec) {
		return
	}
	if err := s.cfg.Rules.AddRule(nodeRule(spec)); err != nil {
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
	if err := s.cfg.Rules.ReplaceRule(arg["name"], nodeRule(spec)); err != nil {
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
		out.Paths = append(out.Paths, Path{
			Name: path.Name, Type: path.Type, Addr: path.Addr, Proxy: withoutUser(path.Proxy), Security: path.Security,
			Verify: path.Verify, Pin: path.Pin, Roles: append([]string{}, path.Roles...), Subs: path.Subs,
		})
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
