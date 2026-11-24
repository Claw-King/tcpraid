package main

import (
	"bytes"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Claw-King/tcpraid/internal/api"
	"github.com/Claw-King/tcpraid/internal/node"
)

// The tests of this package start many nodes; each start hashes a password. The real
// iteration count would take seconds per start under the race detector.
func init() { node.PasswordIterations = 1000 }

// captureLog returns what the function logs.
func captureLog(t *testing.T, f func()) string {
	t.Helper()
	var b bytes.Buffer
	old := log.Writer()
	log.SetOutput(&b)
	defer log.SetOutput(old)
	f()
	return b.String()
}

func TestUIPassword(t *testing.T) {
	t.Setenv(uiPasswordEnv, "")
	dir := t.TempDir()

	// nothing set: a password is made, logged once, and only its hash is stored
	var hash string
	out := captureLog(t, func() {
		var err error
		if hash, err = uiPassword(dir, ""); err != nil {
			t.Fatal(err)
		}
	})
	m := regexp.MustCompile(`shown this once: ([a-z2-7]{16})\n`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("the made password is not in the log:\n%s", out)
	}
	made := m[1]
	if !node.CheckPassword(hash, made) {
		t.Fatal("the logged password does not match the hash")
	}
	stored, _ := os.ReadFile(filepath.Join(dir, node.UIPasswordFile))
	if strings.Contains(string(stored), made) || strings.TrimSpace(string(stored)) != hash {
		t.Fatalf("stored %q", stored)
	}
	// the next start uses it and logs nothing
	out = captureLog(t, func() {
		again, err := uiPassword(dir, "")
		if err != nil || again != hash {
			t.Fatalf("second start: %q, %v", again, err)
		}
	})
	if out != "" {
		t.Fatalf("the second start logged:\n%s", out)
	}

	// the environment variable replaces it; the password itself is not logged
	t.Setenv(uiPasswordEnv, "pw-of-the-env-9")
	out = captureLog(t, func() {
		if hash, err := uiPassword(dir, ""); err != nil || !node.CheckPassword(hash, "pw-of-the-env-9") || node.CheckPassword(hash, made) {
			t.Fatalf("password of the variable: %v", err)
		}
	})
	if strings.Contains(out, "pw-of-the-env-9") {
		t.Fatalf("the password is in the log:\n%s", out)
	}
	// the same password again: the stored hash stays
	before, _ := os.ReadFile(filepath.Join(dir, node.UIPasswordFile))
	if _, err := uiPassword(dir, ""); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, node.UIPasswordFile)); !bytes.Equal(before, after) {
		t.Error("the stored hash was rewritten for the same password")
	}

	// a file comes before the variable; a line end is not part of the password
	file := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(file, []byte("from a file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if hash, err := uiPassword(dir, file); err != nil || !node.CheckPassword(hash, "from a file") {
		t.Fatalf("password from a file: %v", err)
	}
	// without file and variable, the last one stays
	t.Setenv(uiPasswordEnv, "")
	if hash, err := uiPassword(dir, ""); err != nil || !node.CheckPassword(hash, "from a file") {
		t.Fatalf("the stored password after file and variable are gone: %v", err)
	}

	if err := os.WriteFile(file, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := uiPassword(dir, file); err == nil {
		t.Error("an empty password file was accepted")
	}
	if _, err := uiPassword(dir, filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing password file was accepted")
	}
}

// A node started with -no-ui has no web interface: its pages and the login answer 404,
// no password is made, stored or logged, the log does not point at a page, and the API
// with its token works as before.
func TestRunWithoutWebInterface(t *testing.T) {
	t.Setenv(uiPasswordEnv, "a password that must not be used")
	var logged lockedBuffer
	old := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(old)

	dir := t.TempDir()
	startNodeWith(t, runOptions{dataDir: dir, name: "alpha", noUI: true})
	addr := waitAddr(t, dir)
	get := func(method, path string, token bool) int {
		t.Helper()
		req, err := http.NewRequest(method, "http://"+addr+path, strings.NewReader(`{"password":"a password that must not be used"}`))
		if err != nil {
			t.Fatal(err)
		}
		if token {
			tok, err := node.ReadToken(dir)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, c := range [][2]string{{"GET", "/"}, {"GET", "/index.html"}, {"GET", "/app.js"}, {"POST", api.PathLogin}} {
		if code := get(c[0], c[1], false); code != 404 {
			t.Errorf("%s %s on a node without web interface: %d, want 404", c[0], c[1], code)
		}
	}
	if code := get("GET", api.PathStatus, false); code != 401 {
		t.Errorf("the status without the token: %d, want 401", code)
	}
	if code := get("GET", api.PathStatus, true); code != 200 {
		t.Errorf("the status with the token: %d, want 200", code)
	}
	if out := must(t, "status", "-data", dir); !strings.Contains(out, "alpha") {
		t.Errorf("tcpraid status: %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, node.UIPasswordFile)); !os.IsNotExist(err) {
		t.Errorf("a password file in the data directory of a node without web interface: %v", err)
	}
	if out := logged.String(); strings.Contains(out, "web interface") || strings.Contains(out, "password") {
		t.Errorf("the log of a node without web interface speaks of one:\n%s", out)
	}

	// the switch and a password file together are refused before anything starts
	_, code, done, stderr := runFlagsOf("-data", "/x", "-no-ui", "-ui-password-file", "/y")
	if !done || code != 2 || !strings.Contains(stderr, "-no-ui") {
		t.Errorf("-no-ui with -ui-password-file: done=%v code=%d stderr=%q", done, code, stderr)
	}
	if o, _, done, _ := runFlagsOf("-data", "/x", "-no-ui"); done || !o.noUI {
		t.Errorf("-no-ui alone: done=%v noUI=%v", done, o.noUI)
	}
}
