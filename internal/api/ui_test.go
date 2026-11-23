package api

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/node"
)

// newUIServer returns a test server with a web interface whose password is "secret
// word", and a clock the test moves.
func newUIServer(t *testing.T) (*Server, *time.Time) {
	t.Helper()
	old := node.PasswordIterations
	node.PasswordIterations = 10
	t.Cleanup(func() { node.PasswordIterations = old })
	hash, err := node.HashPassword("secret word")
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(t, engine.New(engine.DefaultConfig()))
	now := time.Now()
	s.cfg.UI = &UI{
		PasswordHash: hash,
		Files: fstest.MapFS{
			"index.html": {Data: []byte("<!doctype html><title>t</title>")},
			"app.js":     {Data: []byte("// script")},
		},
		Now: func() time.Time { return now },
	}
	s = New(s.cfg)
	return s, &now
}

// call sends one request. cookie may be nil; header adds X-Tcpraid-Request: 1; from is
// the source address ("" for the default).
func call(s *Server, method, path, body string, cookie *http.Cookie, header bool, from string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if header {
		r.Header.Set(RequestHeader, "1")
	}
	if from != "" {
		r.RemoteAddr = from
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func login(t *testing.T, s *Server, password, from string) (*httptest.ResponseRecorder, *http.Cookie) {
	t.Helper()
	w := call(s, "POST", PathLogin, fmt.Sprintf(`{"password":%q}`, password), nil, false, from)
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookie {
			return w, c
		}
	}
	return w, nil
}

func TestLoginAndCookie(t *testing.T) {
	s, now := newUIServer(t)
	if w := call(s, "GET", PathStatus, "", nil, false, ""); w.Code != 401 {
		t.Fatalf("status without login: %d", w.Code)
	}
	w, c := login(t, s, "wrong", "")
	if w.Code != 401 || c != nil {
		t.Fatalf("a wrong password: status %d, cookie %v", w.Code, c)
	}
	w, c = login(t, s, "secret word", "")
	if w.Code != 204 || c == nil {
		t.Fatalf("the right password: status %d, cookie %v", w.Code, c)
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || len(c.Value) != 64 {
		t.Errorf("cookie %+v", c)
	}
	if w := call(s, "GET", PathStatus, "", c, false, ""); w.Code != 200 {
		t.Fatalf("status with the cookie: %d %s", w.Code, w.Body)
	}
	// a cookie nobody was given
	forged := &http.Cookie{Name: SessionCookie, Value: strings.Repeat("0", 64)}
	if w := call(s, "GET", PathStatus, "", forged, false, ""); w.Code != 401 {
		t.Fatalf("status with a forged cookie: %d", w.Code)
	}

	// changing something with the cookie needs the header
	body := `{"name":"renamed"}`
	if w := call(s, "PUT", PathNode, body, c, false, ""); w.Code != 403 {
		t.Fatalf("a change with the cookie and no header: %d", w.Code)
	}
	if nodeOf(s).Name() != "alpha" {
		t.Fatal("the refused request changed the name")
	}
	if w := call(s, "PUT", PathNode, body, c, true, ""); w.Code != 200 || nodeOf(s).Name() != "renamed" {
		t.Fatalf("a change with cookie and header: %d %s", w.Code, w.Body)
	}
	if w := call(s, "PUT", PathNode, `{"name":""}`, c, true, ""); w.Code != 400 {
		t.Fatalf("an empty name: %d", w.Code)
	}
	// the header alone is nothing
	if w := call(s, "PUT", PathNode, body, nil, true, ""); w.Code != 401 {
		t.Fatalf("a change with the header and no cookie: %d", w.Code)
	}

	// the token works as before, without the header
	r := httptest.NewRequest("PUT", PathNode, strings.NewReader(`{"name":"by-token"}`))
	r.Header.Set("Authorization", "Bearer "+nodeOf(s).Token)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != 200 || nodeOf(s).Name() != "by-token" {
		t.Fatalf("a change with the token: %d %s", rec.Code, rec.Body)
	}
	if w := do(s, "GET", PathStatus, nodeOf(s).Token); w.Code != 200 {
		t.Fatalf("status with the token: %d", w.Code)
	}

	// logout needs the header like any change, and ends the login
	if w := call(s, "POST", PathLogout, "", c, false, ""); w.Code != 403 {
		t.Fatalf("logout without the header: %d", w.Code)
	}
	if w := call(s, "POST", PathLogout, "", c, true, ""); w.Code != 204 {
		t.Fatalf("logout: %d", w.Code)
	}
	if w := call(s, "GET", PathStatus, "", c, false, ""); w.Code != 401 {
		t.Fatalf("status after logout: %d", w.Code)
	}

	// a login ends after its lifetime
	_, c = login(t, s, "secret word", "")
	*now = now.Add(SessionLifetime - time.Minute)
	if w := call(s, "GET", PathStatus, "", c, false, ""); w.Code != 200 {
		t.Fatalf("status just before the login ends: %d", w.Code)
	}
	*now = now.Add(2 * time.Minute)
	if w := call(s, "GET", PathStatus, "", c, false, ""); w.Code != 401 {
		t.Fatalf("status after the login ended: %d", w.Code)
	}
}

func TestLoginLimit(t *testing.T) {
	s, now := newUIServer(t)
	const a, b = "192.0.2.1:1000", "192.0.2.2:1000"
	for i := 1; i <= MaxLoginFailures; i++ {
		if w, _ := login(t, s, "wrong", a); w.Code != 401 {
			t.Fatalf("wrong password %d: %d", i, w.Code)
		}
	}
	// closed to that address, even for the right password, from any port
	w, c := login(t, s, "secret word", "192.0.2.1:2222")
	if w.Code != 429 || c != nil || w.Header().Get("Retry-After") == "" {
		t.Fatalf("after %d failures: %d, cookie %v", MaxLoginFailures, w.Code, c)
	}
	// another address is not affected
	if w, c := login(t, s, "secret word", b); w.Code != 204 || c == nil {
		t.Fatalf("another address: %d", w.Code)
	}
	*now = now.Add(LoginLockout + time.Second)
	if w, c := login(t, s, "secret word", a); w.Code != 204 || c == nil {
		t.Fatalf("after the lockout: %d", w.Code)
	}
	// a success in between starts the count again
	for i := 1; i < MaxLoginFailures; i++ {
		login(t, s, "wrong", a)
	}
	login(t, s, "secret word", a)
	for i := 1; i < MaxLoginFailures; i++ {
		if w, _ := login(t, s, "wrong", a); w.Code != 401 {
			t.Fatalf("failure %d after a success: %d", i, w.Code)
		}
	}
	if w, _ := login(t, s, "secret word", a); w.Code != 204 {
		t.Fatalf("fewer than %d failures in a row closed the login: %d", MaxLoginFailures, w.Code)
	}
}

// The records the login keeps are bounded, whatever arrives.
func TestLoginRecordsBounded(t *testing.T) {
	s, _ := newUIServer(t)
	u := s.cfg.UI
	for i := 0; i < maxLoginFailed+50; i++ {
		login(t, s, "wrong", fmt.Sprintf("198.51.100.%d:%d", i%250, 1000+i))
		// the source is the address without the port: make distinct ones
		login(t, s, "wrong", fmt.Sprintf("[2001:db8::%x]:1000", i))
	}
	u.mu.Lock()
	n := len(u.failed)
	u.mu.Unlock()
	if n > maxLoginFailed {
		t.Fatalf("%d addresses on record, more than %d", n, maxLoginFailed)
	}
	var last *http.Cookie
	for i := 0; i < maxUISessions+20; i++ {
		_, c := login(t, s, "secret word", "")
		if c == nil {
			t.Fatal("no cookie")
		}
		last = c
	}
	u.mu.Lock()
	n = len(u.sessions)
	u.mu.Unlock()
	if n > maxUISessions {
		t.Fatalf("%d logins on record, more than %d", n, maxUISessions)
	}
	if w := call(s, "GET", PathStatus, "", last, false, ""); w.Code != 200 {
		t.Fatalf("the newest login was dropped: %d", w.Code)
	}
}

func TestStaticFiles(t *testing.T) {
	s, _ := newUIServer(t)
	for path, want := range map[string]string{"/": "<!doctype html>", "/index.html": "<!doctype html>", "/app.js": "// script"} {
		w := call(s, "GET", path, "", nil, false, "")
		if w.Code != 200 || !strings.HasPrefix(w.Body.String(), want) {
			t.Errorf("GET %s without login: %d %q", path, w.Code, w.Body)
		}
		if w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s: headers %v", path, w.Header())
		}
	}
	if ct := call(s, "GET", "/app.js", "", nil, false, "").Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("content type of the script: %q", ct)
	}
	if w := call(s, "HEAD", "/", "", nil, false, ""); w.Code != 200 || w.Body.Len() != 0 {
		t.Errorf("HEAD /: %d, %d bytes", w.Code, w.Body.Len())
	}
	// what is not a file is as closed as before
	for _, path := range []string{"/nothing.js", "/../api.go", "/api/v1/status", "/metrics", "/web/index.html"} {
		if w := call(s, "GET", path, "", nil, false, ""); w.Code != 401 {
			t.Errorf("GET %s without login: %d, want 401", path, w.Code)
		}
	}
	if w := call(s, "POST", "/", "", nil, false, ""); w.Code != 401 {
		t.Errorf("POST /: %d, want 401", w.Code)
	}
}

// Without a web interface the server is as it was: no files, no login.
func TestNoUI(t *testing.T) {
	s := newServer(t, engine.New(engine.DefaultConfig()))
	for _, c := range [][2]string{{"GET", "/"}, {"POST", PathLogin}, {"POST", PathLogout}} {
		if w := call(s, c[0], c[1], `{"password":"x"}`, nil, true, ""); w.Code != 401 {
			t.Errorf("%s %s: %d, want 401", c[0], c[1], w.Code)
		}
	}
}

// Nothing the login answers or logs holds the password or the token.
func TestLoginAnswersHoldNoSecret(t *testing.T) {
	s, _ := newUIServer(t)
	var all bytes.Buffer
	w, c := login(t, s, "secret word", "")
	all.Write(w.Body.Bytes())
	w, _ = login(t, s, "secret wor", "")
	all.Write(w.Body.Bytes())
	all.Write(call(s, "GET", PathStatus, "", c, false, "").Body.Bytes())
	for _, secret := range []string{"secret wor", nodeOf(s).Token, s.cfg.UI.PasswordHash} {
		if strings.Contains(all.String(), secret) {
			t.Errorf("an answer holds %q", secret)
		}
	}
}
