package api

// The web interface: its static files, the login with a password, and the API routes
// that exist for it. The interface is a client of the API like any other; it has no
// routes of its own for what it shows.
//
//	GET  /                 the page; /app.js, /style.css, ... its files. No login needed.
//	POST /api/v1/login     log in (LoginRequest); the answer sets the session cookie
//	POST /api/v1/logout    log out: the session ends
//	PUT  /api/v1/node      rename the node (NodeUpdate -> StatusNode)
//
// A request is authorized by the access token, as before, or by the cookie of a login.
// A request authorized by the cookie that can change something (any method but GET and
// HEAD) must also carry the header "X-Tcpraid-Request: 1", which a page of another
// site cannot make a browser send; without it the answer is 403.

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

	// SessionCookie is the name of the cookie of a login.
	SessionCookie = "tcpraid_session"
	// RequestHeader must be "1" on a changing request that the cookie authorizes.
	RequestHeader = "X-Tcpraid-Request"
	// SessionLifetime is how long a login lasts.
	SessionLifetime = 12 * time.Hour
	// MaxLoginFailures wrong passwords in a row from one source address close the login
	// to that address for LoginLockout.
	MaxLoginFailures = 5
	LoginLockout     = time.Minute

	maxUISessions  = 64   // logins at the same time; the one that ends soonest gives way
	maxLoginFailed = 1024 // source addresses with failures on record; the record is dropped when full
)

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
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu       sync.Mutex
	sessions map[[sha256.Size]byte]time.Time // digest of a cookie's value -> when the login ends
	failed   map[string]*loginFailures       // by source address
	hashing  sync.Mutex                      // held for a whole login
}

type loginFailures struct {
	n     int       // wrong passwords in a row
	until time.Time // the login is closed to the address until then
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
	switch {
	case r.URL.Path == PathLogin && r.Method == http.MethodPost:
		u.login(w, r)
		return true
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		return u.file(w, r)
	}
	return false
}

// file serves a static file, if the path names one.
func (u *UI) file(w http.ResponseWriter, r *http.Request) bool {
	if u.Files == nil {
		return false
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	if strings.Contains(name, "/") || !fs.ValidPath(name) {
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
	var req LoginRequest
	if !readBody(w, r, &req) {
		return
	}
	// One login at a time: a password check costs a lot of CPU, and an address that is
	// being closed out must not have a second check running beside the first.
	u.hashing.Lock()
	defer u.hashing.Unlock()
	from, now := source(r), u.now()
	u.mu.Lock()
	if f := u.failed[from]; f != nil && now.Before(f.until) {
		wait := int(f.until.Sub(now)/time.Second) + 1
		u.mu.Unlock()
		w.Header().Set("Retry-After", strconv.Itoa(wait))
		writeError(w, http.StatusTooManyRequests, "too many wrong passwords from this address. Try again in a minute.")
		return
	}
	u.mu.Unlock()

	ok := u.PasswordHash != "" && node.CheckPassword(u.PasswordHash, req.Password)

	now = u.now()
	u.mu.Lock()
	if !ok {
		if u.failed == nil {
			u.failed = map[string]*loginFailures{}
		}
		f := u.failed[from]
		if f == nil {
			if len(u.failed) >= maxLoginFailed {
				// bounded: start the record again rather than let it grow
				clear(u.failed)
			}
			f = &loginFailures{}
			u.failed[from] = f
		}
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
		Name:     SessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   int(SessionLifetime / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// loggedIn reports whether the request carries the cookie of a login that has not ended.
func (u *UI) loggedIn(r *http.Request) bool {
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	end, ok := u.sessions[sha256.Sum256([]byte(c.Value))]
	return ok && u.now().Before(end)
}

// logout ends the login of the request's cookie, if it has one.
func (s *Server) logout(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if u := s.cfg.UI; u != nil {
		if c, err := r.Cookie(SessionCookie); err == nil {
			u.mu.Lock()
			delete(u.sessions, sha256.Sum256([]byte(c.Value)))
			u.mu.Unlock()
		}
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
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
