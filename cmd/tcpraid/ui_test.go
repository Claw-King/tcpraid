package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

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
