package api

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
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
	w := call(s, "POST", PathLogin, fmt.Sprintf(`{"password":%q}`, password), nil, true, from)
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

// A web interface that is switched off: its pages and the login answer 404, a cookie of
// an earlier login gives no access, and the token works as before.
func TestUIOff(t *testing.T) {
	s, _ := newUIServer(t)
	_, cookie := login(t, s, "secret word", "")
	if cookie == nil {
		t.Fatal("no login before the interface is switched off")
	}
	s.cfg.UI.Off = true
	s = New(s.cfg)
	for _, c := range [][2]string{{"GET", "/"}, {"HEAD", "/"}, {"GET", "/index.html"}, {"GET", "/app.js"}, {"POST", PathLogin}, {"GET", PathLogin}} {
		w := call(s, c[0], c[1], `{"password":"secret word"}`, nil, true, "")
		if w.Code != 404 {
			t.Errorf("%s %s with the interface off: %d, want 404", c[0], c[1], w.Code)
		}
		if len(w.Result().Cookies()) != 0 {
			t.Errorf("%s %s with the interface off set a cookie", c[0], c[1])
		}
	}
	// what is not a page of the interface is as closed as ever, and tells nothing
	for _, path := range []string{"/nothing.js", PathStatus, PathLogout} {
		if w := call(s, "GET", path, "", nil, false, ""); w.Code != 401 {
			t.Errorf("GET %s without the token: %d, want 401", path, w.Code)
		}
	}
	// the cookie of the login made before does not authorize
	if w := call(s, "GET", PathStatus, "", cookie, true, ""); w.Code != 401 {
		t.Errorf("a cookie with the interface off: %d, want 401", w.Code)
	}
	// the token does, and the logout route is gone with the interface
	r := httptest.NewRequest("GET", PathStatus, nil)
	r.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("the status with the token: %d", w.Code)
	}
	r = httptest.NewRequest("POST", PathLogout, nil)
	r.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Errorf("logout with the token and the interface off: %d, want 404", w.Code)
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

// A login needs the request header that a page of another site cannot make a browser
// send. Without it the login is refused before the password is looked at, so such
// requests do not count as wrong passwords and close the login to nobody.
func TestLoginNeedsTheHeader(t *testing.T) {
	s, _ := newUIServer(t)
	const a = "192.0.2.1:1000"
	for i := 0; i < 3*MaxLoginFailures; i++ {
		w := call(s, "POST", PathLogin, `{"password":"wrong"}`, nil, false, a)
		if w.Code != 403 || len(w.Result().Cookies()) != 0 {
			t.Fatalf("a login without the header: %d", w.Code)
		}
	}
	if w := call(s, "POST", PathLogin, `{"password":"secret word"}`, nil, false, a); w.Code != 403 {
		t.Fatalf("the right password without the header: %d, want 403", w.Code)
	}
	s.cfg.UI.mu.Lock()
	n := len(s.cfg.UI.failed)
	s.cfg.UI.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d addresses have failures on record after logins without the header", n)
	}
	if w, c := login(t, s, "secret word", a); w.Code != 204 || c == nil {
		t.Fatalf("a login with the header afterwards: %d", w.Code)
	}
}

// When the record of failures is full, the address whose last wrong password is the
// oldest gives way. The others stay closed out.
func TestFullLoginRecordDropsTheOldest(t *testing.T) {
	s, now := newUIServer(t)
	addr := func(i int) string { return fmt.Sprintf("[2001:db8::%x]:1000", i+1) }
	for i := 0; i < maxLoginFailed; i++ {
		for k := 0; k < MaxLoginFailures; k++ {
			if w, _ := login(t, s, "wrong", addr(i)); w.Code != 401 {
				t.Fatalf("address %d, wrong password %d: %d", i, k, w.Code)
			}
		}
		*now = now.Add(time.Millisecond)
	}
	for _, i := range []int{0, 1, maxLoginFailed - 1} {
		if w, _ := login(t, s, "secret word", addr(i)); w.Code != 429 {
			t.Fatalf("address %d is not closed out with the record full: %d", i, w.Code)
		}
	}
	// one more address with a wrong password: the record has no room for it
	if w, _ := login(t, s, "wrong", "192.0.2.77:1000"); w.Code != 401 {
		t.Fatalf("a wrong password from a new address: %d", w.Code)
	}
	if w, c := login(t, s, "secret word", addr(0)); w.Code != 204 || c == nil {
		t.Errorf("the address with the oldest failures was not let go: %d", w.Code)
	}
	for _, i := range []int{1, 2, maxLoginFailed / 2, maxLoginFailed - 1} {
		if w, _ := login(t, s, "secret word", addr(i)); w.Code != 429 {
			t.Errorf("address %d was let go with the oldest: %d, want 429", i, w.Code)
		}
	}
	s.cfg.UI.mu.Lock()
	n := len(s.cfg.UI.failed)
	s.cfg.UI.mu.Unlock()
	if n > maxLoginFailed {
		t.Errorf("%d addresses on record, more than %d", n, maxLoginFailed)
	}
}

// slowChecks makes the check of the password "slow" wait until the returned function is
// called, and reports each check that has begun on the returned channel.
func slowChecks(t *testing.T, u *UI) (release func(), began chan struct{}) {
	t.Helper()
	gate := make(chan struct{})
	began = make(chan struct{}, 64)
	u.check = func(hash, password string) bool {
		if password == "slow" {
			began <- struct{}{}
			<-gate
		}
		return node.CheckPassword(hash, password)
	}
	var once bool
	release = func() {
		if !once {
			once = true
			close(gate)
		}
	}
	t.Cleanup(release)
	return release, began
}

// While the password of one address is being checked, another address logs in without
// waiting for it, and a second login of the first address is sent away.
func TestLoginDoesNotWaitForAnotherAddress(t *testing.T) {
	s, _ := newUIServer(t)
	release, began := slowChecks(t, s.cfg.UI)
	const a, b = "192.0.2.1:1000", "192.0.2.2:1000"
	done := make(chan int, 1)
	go func() {
		w, _ := login(t, s, "slow", a)
		done <- w.Code
	}()
	<-began
	got := make(chan int, 1)
	go func() {
		w, _ := login(t, s, "secret word", b)
		got <- w.Code
	}()
	select {
	case code := <-got:
		if code != 204 {
			t.Fatalf("the login of another address while one is being checked: %d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("the login of another address waited for the check that was running")
	}
	w, _ := login(t, s, "secret word", "192.0.2.1:2000")
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("a second login of an address whose password is being checked: %d", w.Code)
	}
	release()
	if code := <-done; code != 401 {
		t.Fatalf("the slow login with a wrong password: %d", code)
	}
	if w, c := login(t, s, "secret word", a); w.Code != 204 || c == nil {
		t.Fatalf("a login of the first address afterwards: %d", w.Code)
	}
}

// No more than MaxLoginChecks passwords are checked at one time. One more login is told
// to come back, and counts as no failure.
func TestLoginChecksAreBounded(t *testing.T) {
	s, _ := newUIServer(t)
	release, began := slowChecks(t, s.cfg.UI)
	done := make(chan int, MaxLoginChecks)
	for i := 0; i < MaxLoginChecks; i++ {
		go func() {
			w, _ := login(t, s, "slow", fmt.Sprintf("192.0.2.%d:1000", i+1))
			done <- w.Code
		}()
	}
	for i := 0; i < MaxLoginChecks; i++ {
		<-began
	}
	const late = "198.51.100.9:1000"
	w, c := login(t, s, "secret word", late)
	if w.Code != 503 || c != nil || w.Header().Get("Retry-After") == "" {
		t.Fatalf("one login more than %d at one time: %d", MaxLoginChecks, w.Code)
	}
	release()
	for i := 0; i < MaxLoginChecks; i++ {
		if code := <-done; code != 401 {
			t.Fatalf("a slow login with a wrong password: %d", code)
		}
	}
	if w, c := login(t, s, "secret word", late); w.Code != 204 || c == nil {
		t.Fatalf("the login that was sent away, tried again: %d", w.Code)
	}
}

// Two nodes at two ports of one host: a browser keeps one set of cookies for the host,
// whatever the port. The cookie of each login is named after the port, so that the
// second login does not replace the first.
func TestTwoNodesOnOneHostKeepTheirLogins(t *testing.T) {
	a, _ := newUIServer(t)
	b, _ := newUIServer(t)
	sa, sb := httptest.NewServer(a), httptest.NewServer(b)
	defer sa.Close()
	defer sb.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	request := func(method, base, path, body string) int {
		t.Helper()
		req, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(RequestHeader, "1")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, base := range []string{sa.URL, sb.URL} {
		if code := request("POST", base, PathLogin, `{"password":"secret word"}`); code != 204 {
			t.Fatalf("login at %s: %d", base, code)
		}
	}
	for _, base := range []string{sa.URL, sb.URL} {
		if code := request("GET", base, PathStatus, ""); code != 200 {
			t.Errorf("the status at %s after a login at each node: %d", base, code)
		}
	}
	// the jar holds both, under two names, and sends both to each node
	u, _ := url.Parse(sa.URL)
	names := map[string]bool{}
	for _, c := range jar.Cookies(u) {
		names[c.Name] = true
	}
	ua, _ := url.Parse(sa.URL)
	ub, _ := url.Parse(sb.URL)
	wantA, wantB := SessionCookie+"_"+ua.Port(), SessionCookie+"_"+ub.Port()
	if len(names) != 2 || !names[wantA] || !names[wantB] {
		t.Errorf("cookies that go to the first node: %v, want %s and %s", names, wantA, wantB)
	}
	// logging out of one leaves the other
	if code := request("POST", sa.URL, PathLogout, ""); code != 204 {
		t.Fatalf("logout: %d", code)
	}
	if code := request("GET", sa.URL, PathStatus, ""); code != 401 {
		t.Errorf("the status at the node that was logged out of: %d, want 401", code)
	}
	if code := request("GET", sb.URL, PathStatus, ""); code != 200 {
		t.Errorf("the status at the other node: %d", code)
	}
}

func TestSessionCookieName(t *testing.T) {
	for host, want := range map[string]string{
		"nas.example":        SessionCookie,
		"nas.example:9850":   SessionCookie + "_9850",
		"127.0.0.1:19850":    SessionCookie + "_19850",
		"[2001:db8::1]:443":  SessionCookie + "_443",
		"[2001:db8::1]":      SessionCookie,
		"nas.example:":       SessionCookie,
		"nas.example:9x":     SessionCookie,
		"nas.example:123456": SessionCookie,
		"":                   SessionCookie,
	} {
		if got := SessionCookieName(host); got != want {
			t.Errorf("host %q: cookie %q, want %q", host, got, want)
		}
	}
}

// Over TLS the cookie of a login is marked for TLS only; without TLS it is not.
func TestCookieIsSecureOverTLS(t *testing.T) {
	s, _ := newUIServer(t)
	if _, c := login(t, s, "secret word", ""); c == nil || c.Secure {
		t.Fatalf("the cookie of a login without TLS: %+v", c)
	}
	s.cfg.UI.Secure = true
	_, c := login(t, s, "secret word", "")
	if c == nil || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("the cookie of a login over TLS: %+v", c)
	}
	w := call(s, "POST", PathLogout, "", c, true, "")
	gone := w.Result().Cookies()
	if w.Code != 204 || len(gone) != 1 || !gone[0].Secure || gone[0].MaxAge >= 0 {
		t.Fatalf("logout over TLS: %d, cookies %+v", w.Code, gone)
	}
}
