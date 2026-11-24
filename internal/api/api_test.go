package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/link"
	"github.com/Claw-King/tcpraid/internal/node"
)

// newServer returns a Server for a fresh node called alpha and the given engine.
func newServer(t *testing.T, e *engine.Engine) *Server {
	t.Helper()
	n, err := node.Open(t.TempDir(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	svc := &link.Service{Node: n, Dialer: &link.Dialer{Identity: n.Identity}}
	return New(Config{Version: "1.2.3-test", Token: n.Token, Node: svc, Engine: e})
}

// nodeOf returns the node behind a test server.
func nodeOf(s *Server) *node.Node { return s.cfg.Node.(*link.Service).Node }

// do sends one request straight to the handler. An empty token sends no Authorization
// header.
func do(s *Server, method, path, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// wantError checks the status code and that the body is {"error": "..."}.
func wantError(t *testing.T, what string, w *httptest.ResponseRecorder, code int) {
	t.Helper()
	if w.Code != code {
		t.Errorf("%s: status %d, want %d", what, w.Code, code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body) != 1 || body["error"] == "" {
		t.Errorf("%s: body %q, want {\"error\": \"...\"}", what, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("%s: content type %q", what, ct)
	}
}

func keys(m map[string]any) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, " ")
}

func TestHealthNeedsNoToken(t *testing.T) {
	s := newServer(t, engine.New(engine.DefaultConfig()))
	w := do(s, "GET", PathHealth, "")
	if w.Code != 200 || w.Body.String() != "ok" {
		t.Fatalf("status %d body %q, want 200 ok", w.Code, w.Body)
	}
}

func TestToken(t *testing.T) {
	s := newServer(t, engine.New(engine.DefaultConfig()))
	token := s.cfg.Token
	for _, path := range []string{PathStatus, PathSessions, PathMetrics, "/no/such/path", "/"} {
		wantError(t, path+" without a token", do(s, "GET", path, ""), 401)
		wantError(t, path+" with a wrong token", do(s, "GET", path, strings.Repeat("0", len(token))), 401)
	}
	for _, path := range []string{PathStatus, PathSessions, PathMetrics} {
		if w := do(s, "GET", path, token); w.Code != 200 {
			t.Errorf("%s with the token: status %d", path, w.Code)
		}
	}
	for what, header := range map[string]string{
		"part of the token":        "Bearer " + token[:len(token)-1],
		"the token and more":       "Bearer " + token + "0",
		"no token after Bearer":    "Bearer ",
		"the token without Bearer": token,
		"another scheme":           "Basic " + token,
	} {
		r := httptest.NewRequest("GET", PathStatus, nil)
		r.Header.Set("Authorization", header)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		wantError(t, what, w, 401)
	}
	if w := do(s, "GET", PathStatus, ""); w.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("401 without WWW-Authenticate: Bearer")
	}

	// a server without a token refuses, it does not accept an empty one
	s.cfg.Token = ""
	r := httptest.NewRequest("GET", PathStatus, nil)
	r.Header.Set("Authorization", "Bearer ")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	wantError(t, "empty token on both sides", w, 401)
}

func TestNotFoundAndMethod(t *testing.T) {
	s := newServer(t, engine.New(engine.DefaultConfig()))
	token := s.cfg.Token
	wantError(t, "unknown path", do(s, "GET", "/api/v1/nothing", token), 404)
	wantError(t, "path with a trailing slash", do(s, "GET", PathStatus+"/", token), 404)
	for _, path := range []string{PathHealth, PathStatus, PathSessions, PathMetrics} {
		for _, method := range []string{"POST", "PUT", "DELETE", "HEAD"} {
			w := do(s, method, path, token)
			if w.Code != 405 || w.Header().Get("Allow") != "GET" {
				t.Errorf("%s %s: status %d, Allow %q", method, path, w.Code, w.Header().Get("Allow"))
			}
		}
		wantError(t, "POST "+path, do(s, "POST", path, token), 405)
	}
}

func TestStatus(t *testing.T) {
	s := newServer(t, engine.New(engine.DefaultConfig()))
	s.started = s.started.Add(-90 * time.Second)
	w := do(s, "GET", PathStatus, s.cfg.Token)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, content type %q", w.Code, w.Header().Get("Content-Type"))
	}
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if got, want := keys(raw), "node peers rules sessions started uptime_seconds version wire_version"; got != want {
		t.Errorf("fields %q, want %q", got, want)
	}
	nodeFields, _ := raw["node"].(map[string]any)
	if got := keys(nodeFields); got != "fingerprint name" {
		t.Errorf("node fields %q, want \"fingerprint name\"", got)
	}
	started, _ := raw["started"].(string)
	if at, err := time.Parse(time.RFC3339, started); err != nil || !at.Equal(s.started) {
		t.Errorf("started %q: %v, want RFC 3339 for %v", started, err, s.started)
	}

	var st Status
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Version != "1.2.3-test" || st.WireVersion != engine.WireVersion {
		t.Errorf("version %q wire %d", st.Version, st.WireVersion)
	}
	if st.UptimeSeconds < 90 || st.UptimeSeconds > 100 {
		t.Errorf("uptime %d s, want about 90", st.UptimeSeconds)
	}
	if st.Node.Name != "alpha" || st.Node.Fingerprint != s.cfg.Node.Fingerprint() || len(st.Node.Fingerprint) != 64 {
		t.Errorf("node %+v", st.Node)
	}
	if st.Sessions != 0 || st.Peers != 0 || st.Rules != 0 {
		t.Errorf("sessions %d peers %d rules %d, want 0 0 0", st.Sessions, st.Peers, st.Rules)
	}

	// the counts follow the state
	for i, name := range []string{"one", "two"} {
		peer := node.Peer{Name: name, Fingerprint: strings.Repeat(string(rune('a'+i)), 64), PairSecret: make([]byte, node.PairSecretLen)}
		if _, err := nodeOf(s).PutPeer(peer); err != nil {
			t.Fatal(err)
		}
	}
	json.Unmarshal(do(s, "GET", PathStatus, s.cfg.Token).Body.Bytes(), &st)
	if st.Peers != 2 || st.Rules != 0 {
		t.Errorf("peers %d rules %d, want 2 0", st.Peers, st.Rules)
	}
}

func TestSessionsEmpty(t *testing.T) {
	s := newServer(t, engine.New(engine.DefaultConfig()))
	w := do(s, "GET", PathSessions, s.cfg.Token)
	if got := strings.TrimSpace(w.Body.String()); w.Code != 200 || got != "[]" {
		t.Fatalf("status %d body %q, want 200 []", w.Code, got)
	}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

// freeAddr returns a loopback address nothing listens on. The engine takes its listen
// address as text, so the test cannot hand it a listener.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln := listen(t)
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// dialSoon connects to an address whose listener is being started by another goroutine.
func dialSoon(t *testing.T, addr string) net.Conn {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); ; {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			return c
		}
		if time.Now().After(end) {
			t.Fatalf("nothing listens on %s: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func getSessions(t *testing.T, e *engine.Engine) []Session {
	t.Helper()
	s := newServer(t, e)
	w := do(s, "GET", PathSessions, s.cfg.Token)
	var list []Session
	if err := json.Unmarshal(w.Body.Bytes(), &list); w.Code != 200 || err != nil {
		t.Fatalf("status %d, %v: %s", w.Code, err, w.Body)
	}
	return list
}

// The API lists a session the engine holds. The session is a real one: a client engine
// and a server engine on loopback carry one application connection, and each end's API
// shows its side of it.
func TestSessionsListsEngineSessions(t *testing.T) {
	// the destination reports the first bytes it is sent
	dst := listen(t)
	arrived := make(chan string, 1)
	go func() {
		c, err := dst.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		b := make([]byte, 5)
		io.ReadFull(c, b)
		arrived <- string(b)
		io.Copy(io.Discard, c)
	}()

	scfg := engine.DefaultConfig()
	scfg.Listen = freeAddr(t)
	server := engine.New(scfg)
	go server.RunServer()
	dialSoon(t, scfg.Listen).Close() // the server listens before the client dials its sub-connections

	ccfg := engine.DefaultConfig()
	ccfg.Listen = freeAddr(t)
	ccfg.FixedDst = dst.Addr().String()
	client := engine.New(ccfg)
	dial, err := engine.NewDial(scfg.Listen, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	go client.RunClient([]engine.DialGroup{{Name: "data", Want: 2, Dial: dial}})

	if got := getSessions(t, client); len(got) != 0 {
		t.Fatalf("%d sessions before any connection", len(got))
	}
	app := dialSoon(t, ccfg.Listen)
	defer app.Close()
	if _, err := app.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	// once the data is through, the session exists at both ends with sub-connections up
	select {
	case got := <-arrived:
		if got != "hello" {
			t.Fatalf("the destination received %q", got)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the data did not reach the destination")
	}

	cs, ss := getSessions(t, client), getSessions(t, server)
	if len(cs) != 1 || len(ss) != 1 {
		t.Fatalf("client lists %d sessions, server %d, want 1 and 1", len(cs), len(ss))
	}
	c, s := cs[0], ss[0]
	if c.Role != "client" || s.Role != "server" {
		t.Errorf("roles %q and %q", c.Role, s.Role)
	}
	if len(c.ID) != 32 || strings.Trim(c.ID, "0123456789abcdef") != "" || c.ID != s.ID {
		t.Errorf("ids %q and %q, want the same 32 hex digits", c.ID, s.ID)
	}
	if c.Destination != ccfg.FixedDst || s.Destination != ccfg.FixedDst {
		t.Errorf("destinations %q and %q, want %q", c.Destination, s.Destination, ccfg.FixedDst)
	}
	if c.TxBytes != 5 || s.RxBytes != 5 {
		t.Errorf("client sent %d bytes, server received %d, want 5 and 5", c.TxBytes, s.RxBytes)
	}
	if c.Subs < 1 || c.Subs > 2 || s.Subs < 1 || s.Subs > 2 {
		t.Errorf("sub-connections %d and %d, want 1 or 2", c.Subs, s.Subs)
	}
	for _, x := range []Session{c, s} {
		if age := time.Since(x.Started); age < 0 || age > time.Minute {
			t.Errorf("%s session started %v", x.Role, x.Started)
		}
	}

	// the status counts it, and the raw element has exactly the documented fields
	api := newServer(t, client)
	var st Status
	json.Unmarshal(do(api, "GET", PathStatus, api.cfg.Token).Body.Bytes(), &st)
	if st.Sessions != 1 {
		t.Errorf("status counts %d sessions, want 1", st.Sessions)
	}
	var raw []map[string]any
	json.Unmarshal(do(api, "GET", PathSessions, api.cfg.Token).Body.Bytes(), &raw)
	// the first nine are the fields of 0.1.1, which stay as they are; "test" is there
	// only for the session of a path test
	if got, want := keys(raw[0]), "destination grace_left_ms id inflight_bytes resent_bytes role rx_bytes silent_ms started subs tx_bytes unacked_bytes unsent_bytes up"; got != want {
		t.Errorf("session fields %q, want %q", got, want)
	}
	if !c.Up || !s.Up || c.SilentMs < 0 || c.SilentMs > 5000 || c.GraceLeftMs <= 0 {
		t.Errorf("state of a session that carries data: client %+v, server %+v", c, s)
	}
	if c.UnsentBytes+c.InflightBytes != c.UnackedBytes {
		t.Errorf("unsent %d and in flight %d do not add up to unacked %d", c.UnsentBytes, c.InflightBytes, c.UnackedBytes)
	}
}

func TestMetrics(t *testing.T) {
	s := newServer(t, engine.New(engine.DefaultConfig()))
	w := do(s, "GET", PathMetrics, s.cfg.Token)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("status %d, content type %q", w.Code, w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	samples := 0
	for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		if strings.HasPrefix(line, "# TYPE tcpraid_") {
			continue
		}
		// a sample: name, optional {label="key"}, a space, an integer
		name, value, ok := strings.Cut(line, " ")
		if !ok || !strings.HasPrefix(name, "tcpraid_") || strings.Trim(value, "0123456789") != "" || value == "" {
			t.Errorf("unexpected line %q", line)
		}
		samples++
	}
	if want := len(engine.New(engine.DefaultConfig()).Counters()); samples != want {
		t.Errorf("%d samples, the engine has %d counters", samples, want)
	}
	for _, want := range []string{
		"# TYPE tcpraid_sessions_total counter\ntcpraid_sessions_total 0\n",
		"# TYPE tcpraid_sessions_active gauge\ntcpraid_sessions_active 0\n",
		"tcpraid_bytes_tx 0\n",
		"# TYPE tcpraid_resend_by counter\ntcpraid_resend_by{cause=\"gap\"} 0\n",
		"tcpraid_send_wait_ms{reason=\"cap\"} 0\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}

// Serve answers on the listener, and stops and closes it when the context ends.
func TestServeUntilContextDone(t *testing.T) {
	s := newServer(t, engine.New(engine.DefaultConfig()))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()

	resp, err := http.Get("http://" + ln.Addr().String() + PathHealth)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "ok" {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v after the context ended, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after the context ended")
	}
	if c, err := net.Dial("tcp", ln.Addr().String()); err == nil {
		c.Close()
		t.Fatal("the listener still accepts connections")
	}
}

// rawRequest sends one request as written and returns the status line of the answer.
func rawRequest(t *testing.T, addr, request string) string {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(line)
}

// "OPTIONS *" is no way around the token: the server does not answer it on its own.
func TestOptionsStarNeedsToken(t *testing.T) {
	s := newServer(t, engine.New(engine.DefaultConfig()))
	ln := listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	defer func() { cancel(); <-done }()

	addr := ln.Addr().String()
	if got := rawRequest(t, addr, "OPTIONS * HTTP/1.1\r\nHost: node\r\nConnection: close\r\n\r\n"); !strings.Contains(got, " 401 ") {
		t.Errorf("OPTIONS * without a token: %q, want 401", got)
	}
	withToken := "OPTIONS * HTTP/1.1\r\nHost: node\r\nAuthorization: Bearer " + s.cfg.Token + "\r\nConnection: close\r\n\r\n"
	if got := rawRequest(t, addr, withToken); !strings.Contains(got, " 404 ") {
		t.Errorf("OPTIONS * with the token: %q, want 404 (no such path)", got)
	}
}
