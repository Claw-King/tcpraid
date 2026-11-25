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
//	GET    /api/v1/transport              the transport settings of the node (Transport)
//	PUT    /api/v1/transport              set some of them while the node runs (TransportUpdate -> Transport)
//	POST   /api/v1/connect                carry this very connection to a destination at a peer (ConnectRequest); see connect.go
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
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Claw-King/tcpraid/client"
	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/forward"
	"github.com/Claw-King/tcpraid/internal/link"
	"github.com/Claw-King/tcpraid/internal/node"
	"github.com/Claw-King/tcpraid/internal/pair"
)

// Paths of the API.
const (
	PathHealth       = client.PathHealth
	PathStatus       = client.PathStatus
	PathSessions     = client.PathSessions
	PathMetrics      = client.PathMetrics
	PathPairingCodes = client.PathPairingCodes
	PathPeers        = client.PathPeers
	PathRules        = client.PathRules
	PathTransport    = client.PathTransport
	PathConnect      = client.PathConnect
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
	UpdatePeer(name string, paths *[]node.Path, allow, udpDial *string, transport map[string]*string) (node.Peer, error)
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
	Transport() []forward.TransportValue
	SetTransport(change map[string]*string) ([]forward.TransportValue, error)
	Connect(peer, host string, port int, c net.Conn, rd io.Reader, begin func(session string) error) error
	SessionEnd(ctx context.Context, id [16]byte) (end forward.SessionEnd, known bool)
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
	// Listen are the addresses at which the node takes connections of other nodes
	// with TLS, as they are bound; none when it has no such listener. Advertise are
	// the addresses the node was started with for other nodes to reach it. The status
	// reports both.
	Listen    []string
	Advertise []string
	// Metrics is the address at which the figures are served without a token, as
	// bound; empty when they are not. The status reports it.
	Metrics string
	// MetricsExtra, if not nil, adds figures to the answer of GET /metrics.
	MetricsExtra func() []MetricsSample
}

// The requests and answers of the API are the types of package client, where programs
// that use the API find them. The names here stand for the same types.
type (
	Status             = client.Status
	StatusNode         = client.StatusNode
	Session            = client.Session
	PairingCodeRequest = client.PairingCodeRequest
	PairingCode        = client.PairingCode
	JoinRequest        = client.JoinRequest
	Peer               = client.Peer
	Path               = client.Path
	PeerUpdate         = client.PeerUpdate
	PathUpdate         = client.PathUpdate
	PathSet            = client.PathSet
	RuleSpec           = client.RuleSpec
	Rule               = client.Rule
	Transport          = client.Transport
	TransportSetting   = client.TransportSetting
	TransportUpdate    = client.TransportUpdate
	PeerRemoval        = client.PeerRemoval
	PeerCheck          = client.PeerCheck
	PathCheck          = client.PathCheck
	PeerTestRequest    = client.PeerTestRequest
	PeerTest           = client.PeerTest
	PathTest           = client.PathTest
)

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
		add(http.MethodGet, PathTransport, s.getTransport)
		add(http.MethodPut, PathTransport, s.putTransport)
		add(http.MethodPost, PathConnect, s.connect)
		add(http.MethodGet, PathSessions+"/{id}/end", s.sessionEnd)
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
		ErrorLog:          log.New(serverLog{}, "", 0),
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

// serverLog takes what the HTTP server logs by itself. A TLS handshake that fails is
// not logged: anyone who reaches the listener can cause as many as they like. Anything
// else goes to the log of the program.
type serverLog struct{}

func (serverLog) Write(p []byte) (int, error) {
	if !bytes.Contains(p, []byte("TLS handshake error")) {
		log.Print(strings.TrimRight(string(p), "\n"))
	}
	return len(p), nil
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
		// no addresses is [], not null
		Listen:    append([]string{}, s.cfg.Listen...),
		Advertise: append([]string{}, s.cfg.Advertise...),
		Metrics:   s.cfg.Metrics,
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
			SubsDialed:   si.Dialed,
			Peer:         si.Peer,
			Rule:         si.Rule,

			UnsentBytes:   si.UnsentBytes,
			InflightBytes: si.UnackedBytes - si.UnsentBytes,
			Up:            si.Up,
			SilentMs:      si.Silent.Milliseconds(),
			GraceLeftMs:   si.GraceLeft.Milliseconds(),
			Test:          si.Test,
			WindowBytes:   int64(si.Window),
		})
	}
	writeJSON(w, http.StatusOK, list)
}

// metrics writes the engine's counters in the Prometheus text exposition format, under
// their expvar names with the prefix "tcpraid_".
func (s *Server) metrics(w http.ResponseWriter, _ *http.Request, _ map[string]string) {
	src := MetricsSource{Engine: s.cfg.Engine, Node: s.cfg.Node, Rules: s.cfg.Rules, Extra: s.cfg.MetricsExtra}
	w.Header().Set("Content-Type", metricsContentType)
	w.Write(MetricsText(src.Samples()))
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
	if req.Paths == nil && req.Allow == nil && req.UDPDial == nil && len(req.Transport) == 0 {
		writeError(w, http.StatusBadRequest, "the request changes nothing, because it has none of the fields \"paths\", \"allow\", \"udp_dial\", \"transport\". An empty list for \"paths\" removes every path.")
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
	p, err := s.cfg.Node.UpdatePeer(arg["name"], paths, req.Allow, req.UDPDial, req.Transport)
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
	out := Rule{Name: r.Name, Listen: r.Listen, Destination: r.Destination, Match: r.Match, Socks: r.Socks, Peer: r.Peer, Enabled: r.Enabled,
		Listening: st.Listening, Error: st.Error, Sessions: st.Sessions, Subs: r.Subs, PeerDials: st.PeerDials, Dial: r.Dial}
	if len(r.Paths) > 0 {
		out.Paths = append([]string{}, r.Paths...)
	}
	out.Transport = r.Transport.Clone()
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
// the request replaces: what the request does not say about paths, sub-connections, who
// dials and the transport settings stays as was has it, and so does what the rule takes
// (listen and destination, match, or socks) when the request names none of the four:
// a caller that does not know a kind of rule can still switch it on and off. An error
// is a transport setting that cannot be stored.
func nodeRule(spec RuleSpec, was *node.Rule) (node.Rule, error) {
	r := node.Rule{Name: spec.Name, Listen: spec.Listen, Destination: spec.Destination, Match: spec.Match, Socks: spec.Socks, Peer: spec.Peer,
		Enabled: spec.Enabled == nil || *spec.Enabled}
	if r.Socks != "" {
		// ":1080" is the loopback address. What cannot be read stays as it came, for
		// the rule's check to refuse it.
		if stored, err := node.SocksAddr(r.Socks); err == nil {
			r.Socks = stored
		}
	}
	if was != nil {
		r.Paths, r.Subs, r.Dial, r.Transport = was.Clone().Paths, was.Subs, was.Dial, was.Transport.Clone()
		if r.Listen == "" && r.Destination == "" && r.Match == "" && r.Socks == "" {
			r.Listen, r.Destination, r.Match, r.Socks = was.Listen, was.Destination, was.Match, was.Socks
		}
	}
	if spec.Transport != nil {
		next, err := r.Transport.Merge(spec.Transport, false)
		if err != nil {
			return r, fmt.Errorf("%w: rule %q: %w", node.ErrInvalid, spec.Name, err)
		}
		r.Transport = next
	}
	if spec.Dial != nil {
		// a value that is none of the four stays as it came, for the rule's check to
		// refuse it
		if stored, err := node.CheckDial(*spec.Dial); err == nil {
			r.Dial = stored
		} else {
			r.Dial = *spec.Dial
		}
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
	return r, nil
}

func (s *Server) addRule(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	var spec RuleSpec
	if !readBody(w, r, &spec) {
		return
	}
	rule, err := nodeRule(spec, nil)
	if err == nil {
		err = s.cfg.Rules.AddRule(rule)
	}
	if err != nil {
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
	rule, err := nodeRule(spec, was)
	if err == nil {
		err = s.cfg.Rules.ReplaceRule(arg["name"], rule)
	}
	if err != nil {
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

func apiTransport(values []forward.TransportValue) Transport {
	out := Transport{Settings: []TransportSetting{}}
	for _, v := range values {
		out.Settings = append(out.Settings, TransportSetting{Name: v.Name, Value: v.Value, Default: v.Default, Source: v.Source, Stored: v.Stored, Shared: v.Shared})
	}
	return out
}

func (s *Server) getTransport(w http.ResponseWriter, _ *http.Request, _ map[string]string) {
	writeJSON(w, http.StatusOK, apiTransport(s.cfg.Rules.Transport()))
}

func (s *Server) putTransport(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	var req TransportUpdate
	if !readBody(w, r, &req) {
		return
	}
	if len(req.Set) == 0 {
		writeError(w, http.StatusBadRequest, "the request changes nothing, because \"set\" names no setting. Give a name with a value, or with null for its default.")
		return
	}
	values, err := s.cfg.Rules.SetTransport(req.Set)
	if err != nil {
		fail(w, err, false)
		return
	}
	writeJSON(w, http.StatusOK, apiTransport(values))
}

// peer returns a peer in the form the API shows, with what the control connections
// say about it.
func (s *Server) peer(p node.Peer) Peer {
	out := apiPeer(p)
	if s.cfg.Rules != nil {
		st := s.cfg.Rules.PeerStatus(p.Name)
		out.Online, out.Version, out.Spares = st.Online, st.Version, st.Spares
		out.Features, out.Problem = st.Features, st.Problem
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
		Allow: p.Allow, UDPDial: p.UDPDial, Transport: p.Transport.Clone()}
	for _, path := range p.Paths {
		shown := Path{
			Name: path.Name, Type: path.Type, Addr: path.Addr, Proxy: withoutUser(path.Proxy), Security: path.Security,
			Verify: path.Verify, Pin: path.Pin, Roles: append([]string{}, path.Roles...), Subs: path.Subs, Mark: path.Mark,
		}
		if len(path.Addrs) > 0 {
			shown.Addrs = append([]string{}, path.Addrs...)
		}
		if u, err := url.Parse(path.Proxy); err == nil && path.Proxy != "" && u.User != nil {
			shown.ProxyLogin = true
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
