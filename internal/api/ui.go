package api

// The web interface: its static files, the login with a password, and the API routes
// that exist for it. The interface is a client of the API like any other; it has no
// routes of its own for what it shows.
//
//	GET  /                 the page; /app.js, /style.css, ... its files. No login needed.
//	POST /api/v1/login     log in (LoginRequest); the answer sets the session cookie.
//	                       It needs the header "X-Tcpraid-Request: 1" (since 0.8.0)
//	POST /api/v1/logout    log out: the session ends
//	PUT  /api/v1/node      rename the node (NodeUpdate -> StatusNode)
//
// A request is authorized by the access token, as before, or by the cookie of a login.
// A request authorized by the cookie that can change something (any method but GET and
// HEAD) must also carry the header "X-Tcpraid-Request: 1", which a page of another
// site cannot make a browser send; without it the answer is 403. The login needs the
// header as well, and a login without it is refused before the password is looked at,
// so a page of another site cannot use up the wrong passwords of the address it is
// opened at.
//
// The cookie of a login is named after the port the browser used to reach the node
// (SessionCookieName), because a browser keeps one set of cookies for a host name
// whatever the port: two nodes at two ports of one host would otherwise overwrite each
// other's login.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Claw-King/tcpraid/internal/node"
)

const (
	PathLogin  = "/api/v1/login"
	PathLogout = "/api/v1/logout"
	PathNode   = "/api/v1/node"

	// SessionCookie is the name of the cookie of a login where the request names no
	// port, and what the name begins with where it names one (SessionCookieName).
	SessionCookie = "tcpraid_session"
	// RequestHeader must be "1" on a changing request that the cookie authorizes.
	RequestHeader = "X-Tcpraid-Request"
	// SessionLifetime is how long a login lasts.
	SessionLifetime = 12 * time.Hour
	// MaxLoginFailures wrong passwords in a row from one source address close the login
	// to that address for LoginLockout.
	MaxLoginFailures = 5
	LoginLockout     = time.Minute

	// MaxLoginChecks is how many logins may have their password checked at one time.
	// One more is told to try again. A source address has one check at a time.
	MaxLoginChecks = 4

	maxUISessions  = 64   // logins at the same time; the one that ends soonest gives way
	maxLoginFailed = 1024 // source addresses with failures on record; when full, the one whose last failure is oldest gives way
)

// SessionCookieName returns the name of the cookie of a login made with a request for
// host, the Host header of the request: SessionCookie, with the port after it where
// the host names one ("tcpraid_session_9850").
func SessionCookieName(host string) string {
	_, port, err := net.SplitHostPort(host)
	if err != nil || port == "" || len(port) > 5 {
		return SessionCookie
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return SessionCookie
		}
	}
	return SessionCookie + "_" + port
}

// LoginRequest is the body of POST /api/v1/login.
type LoginRequest struct {
	Password string `json:"password"`
}

// NodeUpdate is the body of PUT /api/v1/node.
type NodeUpdate struct {
	Name string `json:"name"`
}

// UI is the web interface of a Server. Set the exported fields before the Server is
// used.
type UI struct {
	// PasswordHash is the hash of the password (node.HashPassword). With an empty one
	// nobody can log in.
	PasswordHash string
	// Files are the static files, served at the root; index.html is the page. nil: none.
	Files fs.FS
	// Off switches the web interface off: the paths of its files and the login answer
	// 404, and no cookie authorizes anything. Files is still needed, to know which
	// paths those are. The token and everything it gives access to stay as they are.
	Off bool
	// Secure marks the cookie of a login for connections with TLS only. Set it when
	// the API is served over TLS.
	Secure bool
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu       sync.Mutex
	sessions map[[sha256.Size]byte]time.Time // digest of a cookie's value -> when the login ends
	failed   map[string]*loginFailures       // by source address
	checking map[string]bool                 // source addresses whose password is being checked now
	// check is node.CheckPassword; a test puts its own in.
	check func(hash, password string) bool
}

type loginFailures struct {
	n     int       // wrong passwords in a row
	until time.Time // the login is closed to the address until then
	last  time.Time // when the last wrong password came
}

func (u *UI) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

// serve answers the requests that need no authorization: the static files and the
// login. It reports whether it answered.
func (u *UI) serve(w http.ResponseWriter, r *http.Request) bool {
	if u.Off {
		if r.URL.Path == PathLogin || ((r.Method == http.MethodGet || r.Method == http.MethodHead) && u.isFile(r.URL.Path)) {
			writeError(w, http.StatusNotFound, "the web interface of this node is switched off")
			return true
		}
		return false
	}
	switch {
	case r.URL.Path == PathLogin && r.Method == http.MethodPost:
		u.login(w, r)
		return true
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		return u.file(w, r)
	}
	return false
}

// fileName returns the name of the static file that the request path stands for, or ""
// when it cannot stand for one.
func (u *UI) fileName(urlPath string) string {
	if u.Files == nil {
		return ""
	}
	name := strings.TrimPrefix(urlPath, "/")
	if name == "" {
		name = "index.html"
	}
	if strings.Contains(name, "/") || !fs.ValidPath(name) {
		return ""
	}
	return name
}

// isFile reports whether the request path names one of the static files.
func (u *UI) isFile(urlPath string) bool {
	name := u.fileName(urlPath)
	if name == "" {
		return false
	}
	fi, err := fs.Stat(u.Files, name)
	return err == nil && !fi.IsDir()
}

// file serves a static file, if the path names one.
func (u *UI) file(w http.ResponseWriter, r *http.Request) bool {
	name := u.fileName(r.URL.Path)
	if name == "" {
		return false
	}
	b, err := fs.ReadFile(u.Files, name)
	if err != nil {
		return false
	}
	ct := mime.TypeByExtension(path.Ext(name))
	if ct == "" {
		ct = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Content-Length", strconv.Itoa(len(b)))
	// the files change with the program, not between two requests: let the browser ask
	h.Set("Cache-Control", "no-cache")
	// the page loads nothing from anywhere else and runs no inline script
	h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	if r.Method == http.MethodGet {
		w.Write(b)
	}
	return true
}

// source returns what counts as the origin of a login attempt: the address the
// connection comes from, without the port.
func source(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (u *UI) login(w http.ResponseWriter, r *http.Request) {
	// Before anything is counted: a page of another site can make a browser send a
	// login, but not with this header, so it cannot close the login to the address
	// its visitor comes from.
	if r.Header.Get(RequestHeader) != "1" {
		writeError(w, http.StatusForbidden, "a login needs the header "+RequestHeader+": 1")
		return
	}
	var req LoginRequest
	if !readBody(w, r, &req) {
		return
	}
	// A password check costs a lot of CPU. One address has one check at a time, so an
	// address that is being closed out has no second check running beside the first,
	// and its failures hold up nobody else. All addresses together have
	// MaxLoginChecks, and one more is sent away at once.
	from, now := source(r), u.now()
	u.mu.Lock()
	switch f := u.failed[from]; {
	case f != nil && now.Before(f.until):
		wait := int(f.until.Sub(now)/time.Second) + 1
		u.mu.Unlock()
		w.Header().Set("Retry-After", strconv.Itoa(wait))
		writeError(w, http.StatusTooManyRequests, "too many wrong passwords from this address. Try again in a minute.")
		return
	case u.checking[from]:
		u.mu.Unlock()
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "another login from this address is being checked. Try again in a moment.")
		return
	case len(u.checking) >= MaxLoginChecks:
		u.mu.Unlock()
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "too many logins are being checked at this moment. Try again in a moment.")
		return
	}
	if u.checking == nil {
		u.checking = map[string]bool{}
	}
	u.checking[from] = true
	u.mu.Unlock()

	check := u.check
	if check == nil {
		check = node.CheckPassword
	}
	ok := u.PasswordHash != "" && check(u.PasswordHash, req.Password)

	now = u.now()
	u.mu.Lock()
	delete(u.checking, from)
	if !ok {
		if u.failed == nil {
			u.failed = map[string]*loginFailures{}
		}
		f := u.failed[from]
		if f == nil {
			if len(u.failed) >= maxLoginFailed {
				// bounded: the address whose last wrong password is the oldest
				// gives way, and the others stay closed out
				var oldest string
				var at time.Time
				for addr, x := range u.failed {
					if oldest == "" || x.last.Before(at) {
						oldest, at = addr, x.last
					}
				}
				delete(u.failed, oldest)
			}
			f = &loginFailures{}
			u.failed[from] = f
		}
		f.last = now
		if f.n++; f.n >= MaxLoginFailures {
			f.n, f.until = 0, now.Add(LoginLockout)
		}
		u.mu.Unlock()
		writeError(w, http.StatusUnauthorized, "wrong password")
		return
	}
	delete(u.failed, from)
	u.mu.Unlock()

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		writeError(w, http.StatusInternalServerError, "no random numbers")
		return
	}
	value := hex.EncodeToString(raw)
	u.mu.Lock()
	if u.sessions == nil {
		u.sessions = map[[sha256.Size]byte]time.Time{}
	}
	for k, end := range u.sessions {
		if !now.Before(end) {
			delete(u.sessions, k)
		}
	}
	for len(u.sessions) >= maxUISessions {
		var first [sha256.Size]byte
		var at time.Time
		for k, end := range u.sessions {
			if at.IsZero() || end.Before(at) {
				first, at = k, end
			}
		}
		delete(u.sessions, first)
	}
	u.sessions[sha256.Sum256([]byte(value))] = now.Add(SessionLifetime)
	u.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName(r.Host),
		Value:    value,
		Path:     "/",
		MaxAge:   int(SessionLifetime / time.Second),
		HttpOnly: true,
		Secure:   u.Secure,
		SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// loggedIn reports whether the request carries the cookie of a login that has not ended.
func (u *UI) loggedIn(r *http.Request) bool {
	c, err := r.Cookie(SessionCookieName(r.Host))
	if err != nil || c.Value == "" || u.Off {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	end, ok := u.sessions[sha256.Sum256([]byte(c.Value))]
	return ok && u.now().Before(end)
}

// logout ends the login of the request's cookie, if it has one.
func (s *Server) logout(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	name, secure := SessionCookieName(r.Host), false
	if u := s.cfg.UI; u != nil {
		secure = u.Secure
		if c, err := r.Cookie(name); err == nil {
			u.mu.Lock()
			delete(u.sessions, sha256.Sum256([]byte(c.Value)))
			u.mu.Unlock()
		}
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) putNode(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	var req NodeUpdate
	if !readBody(w, r, &req) {
		return
	}
	if err := s.cfg.Node.SetName(req.Name); err != nil {
		fail(w, err, false)
		return
	}
	writeJSON(w, http.StatusOK, StatusNode{Name: s.cfg.Node.Name(), Fingerprint: s.cfg.Node.Fingerprint()})
}

// changes reports whether a request of the method can change something.
func changes(method string) bool {
	return method != http.MethodGet && method != http.MethodHead
}
