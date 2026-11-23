package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/api"
	"github.com/Claw-King/tcpraid/internal/node"
)

// call runs subcommand and returns what it wrote.
func call(args ...string) (code int, ok bool, stdout, stderr string) {
	var out, errb bytes.Buffer
	code, ok = subcommand(args, &out, &errb)
	return code, ok, out.String(), errb.String()
}

// No arguments, or a first argument that starts with "-", is the manual mode: subcommand
// leaves the command line alone.
func TestManualModeIsNotASubcommand(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{},
		{"-mode", "server", "-listen", "127.0.0.1:9901"},
		{"-version"},
		{"--check", "-config", "c.yaml"},
		{"-h"},
		{"-"},
		{"-mode", "client", "run"}, // a subcommand name later in the line is an ordinary argument
	} {
		code, ok, stdout, stderr := call(args...)
		if ok || code != 0 || stdout != "" || stderr != "" {
			t.Errorf("%q: handled=%v code=%d stdout=%q stderr=%q, want it left to the manual mode", args, ok, code, stdout, stderr)
		}
	}
}

func TestUnknownSubcommand(t *testing.T) {
	for _, args := range [][]string{{"bogus"}, {"help"}, {"Run"}, {""}, {"server", "-listen", "127.0.0.1:9901"}} {
		code, ok, stdout, stderr := call(args...)
		if !ok || code == 0 {
			t.Errorf("%q: handled=%v code=%d, want an error exit", args, ok, code)
		}
		if stdout != "" {
			t.Errorf("%q: wrote %q to stdout", args, stdout)
		}
		for _, want := range []string{"unknown command", "run", "status", "version"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("%q: message %q does not mention %q", args, stderr, want)
			}
		}
	}
}

func TestVersionCommand(t *testing.T) {
	code, ok, stdout, stderr := call("version")
	if !ok || code != 0 || stderr != "" {
		t.Fatalf("handled=%v code=%d stderr=%q", ok, code, stderr)
	}
	// the same line -version prints (main prints versionLine too)
	if stdout != versionLine()+"\n" || !strings.Contains(stdout, version) || !strings.Contains(stdout, "wire protocol 1") {
		t.Fatalf("printed %q", stdout)
	}
	if code, _, _, stderr := call("version", "extra"); code == 0 || stderr == "" {
		t.Fatalf("extra argument: code %d, message %q", code, stderr)
	}
}

func TestSubcommandFlagErrors(t *testing.T) {
	for _, args := range [][]string{
		{"status", "-nosuchflag"},
		{"status", "extra"},
		{"run", "-nosuchflag"},
		{"run", "extra"},
	} {
		code, ok, stdout, stderr := call(args...)
		if !ok || code == 0 || stdout != "" || stderr == "" {
			t.Errorf("%q: handled=%v code=%d stdout=%q stderr=%q, want an error exit with a message", args, ok, code, stdout, stderr)
		}
	}
	// asking for help is not an error; the flags and their defaults are listed
	code, _, _, stderr := call("run", "-h")
	if code != 0 || !strings.Contains(stderr, defaultDataDir) || !strings.Contains(stderr, defaultAPIAddr) || !strings.Contains(stderr, "-name") {
		t.Errorf("run -h: code %d, text %q", code, stderr)
	}
}

// status without a node: one line that says so, and an error exit.
func TestStatusWithoutNode(t *testing.T) {
	dir := t.TempDir()
	check := func(what string) {
		t.Helper()
		code, _, stdout, stderr := call("status", "-data", dir)
		if code == 0 || stdout != "" {
			t.Errorf("%s: code %d, stdout %q", what, code, stdout)
		}
		if !strings.Contains(stderr, "not running") || !strings.Contains(stderr, dir) || strings.Count(stderr, "\n") != 1 {
			t.Errorf("%s: message %q, want one line saying the node is not running or the directory is wrong", what, stderr)
		}
	}
	check("empty directory")

	// a data directory whose node has stopped: the files are there, nothing answers
	n, err := node.Open(dir, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	n.Close()
	if err := node.WriteAPIAddr(dir, freeAddr(t)); err != nil {
		t.Fatal(err)
	}
	check("stopped node")
}

// freeAddr returns a loopback address nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// waitAddr returns the API address a node that is starting on dir writes there.
func waitAddr(t *testing.T, dir string) string {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		addr, err := node.ReadAPIAddr(dir)
		if err == nil {
			return addr
		}
		if !errors.Is(err, fs.ErrNotExist) || time.Now().After(end) {
			t.Fatalf("the node did not write its API address: %v", err)
		}
	}
}

// A node is started, queried with the status command, stopped and started again: it
// keeps its fingerprint, and it stops cleanly when its context ends.
func TestRunAndStatus(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	start := func(name string) (stop func()) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- runNode(ctx, dir, "127.0.0.1:0", name) }()
		return func() {
			t.Helper()
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("the node ended with %v, want a clean stop", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the node did not stop")
			}
		}
	}

	stop := start("alpha")
	waitAddr(t, dir)
	code, _, stdout, stderr := call("status", "-data", dir, "-json")
	if code != 0 {
		t.Fatalf("status -json: code %d: %s", code, stderr)
	}
	var st api.Status
	if err := json.Unmarshal([]byte(stdout), &st); err != nil {
		t.Fatalf("status -json printed %q: %v", stdout, err)
	}
	if st.Node.Name != "alpha" || len(st.Node.Fingerprint) != 64 || st.Version != version {
		t.Fatalf("status %+v", st)
	}
	code, _, stdout, stderr = call("status", "-data", dir)
	if code != 0 || stderr != "" {
		t.Fatalf("status: code %d: %s", code, stderr)
	}
	for _, want := range []string{"alpha", st.Node.Fingerprint, version, "uptime", "sessions     0", "peers        0", "rules        0"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status output lacks %q:\n%s", want, stdout)
		}
	}
	stop()
	if code, _, _, _ := call("status", "-data", dir); code == 0 {
		t.Fatal("status succeeded after the node stopped")
	}

	// restarted without a name: the same identity and the name it had
	// (the address file of the previous run is removed so that the wait sees the new one)
	restart := func(name string) api.Status {
		t.Helper()
		if err := os.Remove(filepath.Join(dir, node.AddrFile)); err != nil {
			t.Fatal(err)
		}
		stop = start(name)
		waitAddr(t, dir)
		code, _, stdout, stderr := call("status", "-data", dir, "-json")
		if code != 0 {
			t.Fatalf("status after a restart: code %d: %s", code, stderr)
		}
		var st api.Status
		if err := json.Unmarshal([]byte(stdout), &st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	if again := restart(""); again.Node.Fingerprint != st.Node.Fingerprint || again.Node.Name != "alpha" {
		t.Fatalf("restarted without a name: %+v, want fingerprint %s and name alpha", again.Node, st.Node.Fingerprint)
	}
	stop()
	// restarted with another name: the same identity, renamed
	if again := restart("beta"); again.Node.Fingerprint != st.Node.Fingerprint || again.Node.Name != "beta" {
		t.Fatalf("restarted as beta: %+v, want fingerprint %s and name beta", again.Node, st.Node.Fingerprint)
	}
	stop()
}

// startNode runs a node until the test ends and returns its recorded API address.
func startNode(t *testing.T, dir, apiAddr, name string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runNode(ctx, dir, apiAddr, name) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("the node ended with %v, want a clean stop", err)
		}
	})
	return waitAddr(t, dir)
}

// files returns the content of every file in dir.
func files(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		m[e.Name()] = string(b)
	}
	return m
}

// A second node on a data directory that is in use does not start, whatever API address
// and name it is given, and changes no file: not the recorded address, not the name.
func TestRunDataDirInUse(t *testing.T) {
	dir := t.TempDir()
	startNode(t, dir, "127.0.0.1:0", "alpha")
	before := files(t, dir)
	err := runNode(context.Background(), dir, "127.0.0.1:0", "beta")
	if err == nil || !strings.Contains(err.Error(), "in use by another tcpraid") {
		t.Fatalf("second node on the directory: %v, want it refused as in use", err)
	}
	if after := files(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatalf("the refused node changed the directory:\nbefore %q\nafter  %q", before, after)
	}
	// the command: one line, exit code 1
	code, _, _, stderr := call("run", "-data", dir, "-api", "127.0.0.1:0", "-name", "beta")
	if code != 1 || strings.Count(stderr, "\n") != 1 || !strings.Contains(stderr, "in use by another tcpraid") {
		t.Fatalf("run on a directory in use: code %d, message %q", code, stderr)
	}
	if code, _, stdout, _ := call("status", "-data", dir); code != 0 || !strings.Contains(stdout, "alpha") {
		t.Fatalf("the first node no longer answers as alpha: code %d, %q", code, stdout)
	}
}

// A node whose API address is taken does not start and records no address.
func TestRunAPIAddressInUse(t *testing.T) {
	addr := startNode(t, t.TempDir(), "127.0.0.1:0", "alpha")
	dir := t.TempDir()
	if err := runNode(context.Background(), dir, addr, "beta"); err == nil || !strings.Contains(err.Error(), "-api") {
		t.Fatalf("node on a taken API address: %v, want a listen error", err)
	}
	if _, err := node.ReadAPIAddr(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the node that did not start recorded an API address (%v)", err)
	}
}

// The recorded API address connects from this host also when the API listens on every
// address.
func TestReachableAddr(t *testing.T) {
	for addr, want := range map[string]string{
		"0.0.0.0:9850":      "127.0.0.1:9850",
		"[::]:9850":         "127.0.0.1:9850",
		"127.0.0.1:9850":    "127.0.0.1:9850",
		"[::1]:9850":        "[::1]:9850",
		"192.0.2.7:9850":    "192.0.2.7:9850",
		"[2001:db8::7]:985": "[2001:db8::7]:985",
	} {
		tcp, err := net.ResolveTCPAddr("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		if got := reachableAddr(tcp); got != want {
			t.Errorf("listener on %s: recorded %s, want %s", addr, got, want)
		}
	}
	if got := reachableAddr(&net.TCPAddr{Port: 9850}); got != "127.0.0.1:9850" {
		t.Errorf("listener without an address: recorded %s", got)
	}

	dir := t.TempDir()
	addr := startNode(t, dir, "0.0.0.0:0", "alpha")
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("node on 0.0.0.0: recorded %s, want a loopback address", addr)
	}
	if code, _, stdout, stderr := call("status", "-data", dir); code != 0 || !strings.Contains(stdout, "alpha") {
		t.Fatalf("status of a node on 0.0.0.0: code %d, %q %q", code, stdout, stderr)
	}
}

// Something else answers on the recorded address: whatever it says, status reports no
// node and fails.
func TestStatusAnswerIsNotAStatus(t *testing.T) {
	dir := t.TempDir()
	n, err := node.Open(dir, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	n.Close()
	for what, answer := range map[string]string{
		"other JSON":             `{"hello":1}`,
		"empty object":           `{}`,
		"status without version": `{"node":{"name":"x","fingerprint":"ab"},"wire_version":1}`,
		"status without node":    `{"version":"1.0","wire_version":1}`,
		"not JSON":               `ok`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(answer))
		}))
		if err := node.WriteAPIAddr(dir, srv.Listener.Addr().String()); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"status", "-data", dir}, {"status", "-data", dir, "-json"}} {
			code, _, stdout, stderr := call(args...)
			if code != 1 || stdout != "" {
				t.Errorf("%s, %q: code %d, stdout %q, want code 1 and nothing printed", what, args[1:], code, stdout)
			}
			if !strings.Contains(stderr, "not running") || strings.Count(stderr, "\n") != 1 {
				t.Errorf("%s: message %q, want the one line about no node", what, stderr)
			}
		}
		srv.Close()
	}
}

func TestEmptyDataDir(t *testing.T) {
	for _, cmd := range []string{"run", "status"} {
		code, _, stdout, stderr := call(cmd, "-data", "")
		if code == 0 || stdout != "" || !strings.Contains(stderr, "-data is empty") || strings.Count(stderr, "\n") != 1 {
			t.Errorf("%s -data \"\": code %d, stdout %q, message %q", cmd, code, stdout, stderr)
		}
	}
}

// lockedBuffer collects log output written by another goroutine.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// A data directory others can write to, or a key others can read, is logged at the
// start; the node runs.
func TestRunWarnsAboutPermissions(t *testing.T) {
	var logged lockedBuffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	// a directory the node makes itself has nothing to warn about
	startNode(t, filepath.Join(t.TempDir(), "data"), "127.0.0.1:0", "tight")
	if strings.Contains(logged.String(), "warning") {
		t.Fatalf("warning for a directory the node made itself:\n%s", logged.String())
	}
	dir := filepath.Join(t.TempDir(), "data")
	n, err := node.Open(dir, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	n.Close()
	if err := os.Chmod(dir, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, node.KeyFile), 0o640); err != nil {
		t.Fatal(err)
	}
	startNode(t, dir, "127.0.0.1:0", "")
	out := logged.String()
	for _, want := range []string{
		"warning: " + dir + " is writable by group or others (mode 0775)",
		"warning: " + filepath.Join(dir, node.KeyFile) + " is accessible by group or others (mode 0640)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if code, _, stdout, _ := call("status", "-data", dir); code != 0 || !strings.Contains(stdout, "alpha") {
		t.Fatalf("the node with loose permissions does not answer: code %d, %q", code, stdout)
	}
}
