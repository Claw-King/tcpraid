package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/api"
	"github.com/Claw-King/tcpraid/internal/node"
	"github.com/Claw-King/tcpraid/internal/pair"
)

// startNodeWith runs a node with the given options until the test ends. The API listens
// on a free loopback port.
func startNodeWith(t *testing.T, o runOptions) {
	t.Helper()
	needsName(t, o.dataDir, o.name)
	o.apiAddr = "127.0.0.1:0"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runNodeWith(ctx, o) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("the node ended with %v, want a clean stop", err)
		}
	})
	waitAddr(t, o.dataDir)
}

// must runs a subcommand that has to succeed and returns its standard output.
func must(t *testing.T, args ...string) string {
	t.Helper()
	code, ok, stdout, stderr := call(args...)
	if !ok || code != 0 {
		t.Fatalf("tcpraid %s: code %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, stdout, stderr)
	}
	return stdout
}

// Two nodes on loopback, driven through the commands only: pair, list, check, unpair.
func TestPairAndPeersCommands(t *testing.T) {
	dirA, dirB := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	addrA := freeAddr(t)
	startNodeWith(t, runOptions{dataDir: dirA, name: "alpha", listen: addrA, advertise: stringList{addrA}})
	startNodeWith(t, runOptions{dataDir: dirB, name: "beta"})

	if out := must(t, "peers", "-data", dirA); strings.TrimSpace(out) != "no peers" {
		t.Fatalf("peers on a fresh node: %q", out)
	}
	if out := must(t, "pair", "list", "-data", dirA); strings.TrimSpace(out) != "no pairing codes" {
		t.Fatalf("pair list on a fresh node: %q", out)
	}

	// create: the code alone on standard output, the explanation on standard error
	code, _, stdout, stderr := call("pair", "create", "-data", dirA)
	pairingCode := strings.TrimSpace(stdout)
	if code != 0 || !strings.HasPrefix(pairingCode, "tcpraid-pair-1-") || strings.Contains(pairingCode, "\n") {
		t.Fatalf("pair create: code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if strings.Contains(stderr, pairingCode) || !strings.Contains(stderr, "pair join") {
		t.Fatalf("pair create wrote to stderr: %q", stderr)
	}
	list := must(t, "pair", "list", "-data", dirA)
	if strings.Contains(list, pairingCode) || !strings.Contains(list, "no") || strings.Count(list, "\n") != 2 {
		t.Fatalf("pair list: %q", list)
	}

	// join: the flag stands behind the code here, before it elsewhere
	out := must(t, "pair", "join", pairingCode, "-data", dirB)
	if !strings.Contains(out, "paired with alpha") {
		t.Fatalf("pair join: %q", out)
	}
	if code, _, _, stderr := call("pair", "join", "-data", dirB, pairingCode); code != 1 || !strings.Contains(stderr, "did not accept") {
		t.Fatalf("joining with a used code: code %d, %q", code, stderr)
	}
	if out := must(t, "pair", "list", "-data", dirA); !strings.Contains(out, "yes") {
		t.Fatalf("pair list after the pairing: %q", out)
	}

	peersA, peersB := must(t, "peers", "-data", dirA), must(t, "peers", "list", "-data", dirB)
	if !strings.Contains(peersA, "beta") || !strings.Contains(peersA, "cannot reach") {
		t.Fatalf("peers on alpha: %q", peersA)
	}
	if !strings.Contains(peersB, "alpha") || !strings.Contains(peersB, addrA) || !strings.Contains(peersB, "tls") {
		t.Fatalf("peers on beta: %q", peersB)
	}
	if st := must(t, "status", "-data", dirB); !strings.Contains(st, "peers        1") {
		t.Fatalf("status on beta: %q", st)
	}

	// check: beta reaches alpha; alpha has no path to beta
	if out := must(t, "peers", "check", "alpha", "-data", dirB); !strings.Contains(out, addrA) || !strings.Contains(out, ": ok") {
		t.Fatalf("peers check on beta: %q", out)
	}
	if code, _, _, stderr := call("peers", "check", "beta", "-data", dirA); code != 1 || !strings.Contains(stderr, "no path") {
		t.Fatalf("peers check on alpha: code %d, %q", code, stderr)
	}
	if code, _, _, stderr := call("peers", "check", "nobody", "-data", dirA); code != 1 || !strings.Contains(stderr, "not found") {
		t.Fatalf("peers check of an unknown peer: code %d, %q", code, stderr)
	}

	// alpha ends the pairing: it no longer lists beta, and beta's check fails
	if out := must(t, "peers", "remove", "-data", dirA, "beta"); !strings.Contains(out, "removed") {
		t.Fatalf("peers remove: %q", out)
	}
	if code, _, _, _ := call("peers", "remove", "-data", dirA, "beta"); code != 1 {
		t.Fatalf("removing twice: code %d", code)
	}
	code, _, stdout, stderr = call("peers", "check", "-data", dirB, "alpha")
	if code != 1 || !strings.Contains(stdout, "FAILED") || !strings.Contains(stderr, "1 of 1") {
		t.Fatalf("peers check after the other node ended the pairing: code %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	// revoke
	code2, _, stdout, _ := call("pair", "create", "-data", dirA, "-ttl", "5m", "-addr", "nas.example.org:9851", "-addr", "192.0.2.4:9851")
	if code2 != 0 {
		t.Fatal("pair create with -ttl and two -addr failed")
	}
	_ = stdout
	list = must(t, "pair", "list", "-data", dirA)
	fields := strings.Fields(strings.Split(strings.TrimSpace(list), "\n")[2])
	if out := must(t, "pair", "revoke", fields[0], "-data", dirA); !strings.Contains(out, "revoked") {
		t.Fatalf("pair revoke: %q", out)
	}
	if code, _, _, _ := call("pair", "revoke", fields[0], "-data", dirA); code != 1 {
		t.Fatalf("revoking twice: code %d", code)
	}
}

// A node without -advertise cannot make a code unless the request names an address.
func TestPairCreateNeedsAnAddress(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a")
	startNodeWith(t, runOptions{dataDir: dir, name: "alpha"})
	code, _, stdout, stderr := call("pair", "create", "-data", dir)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "-advertise") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if out := must(t, "pair", "create", "-data", dir, "-addr", "nas.example.org:9851"); !strings.HasPrefix(out, "tcpraid-pair-1-") {
		t.Fatalf("with -addr: %q", out)
	}
}

// withStdin makes text the standard input of the commands until the test ends.
func withStdin(t *testing.T, text string) {
	t.Helper()
	old := stdin
	stdin = strings.NewReader(text)
	t.Cleanup(func() { stdin = old })
}

func TestPairAndPeersCommandLines(t *testing.T) {
	dir := t.TempDir() // no node runs here
	withStdin(t, "")
	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"pair"}, 2, "which one"},
		{[]string{"pair", "bogus"}, 2, "unknown command"},
		{[]string{"peers", "bogus"}, 2, "unknown command"},
		{[]string{"pair", "join"}, 1, "no pairing code"}, // nothing on standard input either
		{[]string{"pair", "join", "-"}, 1, "no pairing code"},
		{[]string{"pair", "join", "a", "b"}, 2, "takes 0 to 1 argument"},
		{[]string{"peers", "show"}, 2, "takes 1 argument"},
		{[]string{"peers", "path"}, 2, "which one"},
		{[]string{"peers", "path", "bogus"}, 2, "unknown command"},
		{[]string{"peers", "path", "set", "alpha"}, 2, "takes 2 argument"},
		{[]string{"peers", "path", "remove", "alpha"}, 2, "takes 2 argument"},
		{[]string{"peers", "path", "set", "alpha", "p", "-subs", "many"}, 2, "invalid value"},
		{[]string{"pair", "revoke"}, 2, "takes 1 argument"},
		{[]string{"pair", "create", "extra"}, 2, "takes 0 argument"},
		{[]string{"peers", "remove"}, 2, "takes 1 argument"},
		{[]string{"peers", "check"}, 2, "takes 1 argument"},
		{[]string{"pair", "list", "-nosuchflag"}, 2, "nosuchflag"},
		{[]string{"pair", "list", "-data", ""}, 2, "-data is empty"},
		{[]string{"pair", "create", "-data", dir, "-ttl", "-5m"}, 1, "whole seconds"},
		{[]string{"pair", "create", "-data", dir, "-ttl", "500ms"}, 1, "whole seconds"},
		{[]string{"pair", "create", "-data", dir, "-ttl", "1500ms"}, 1, "whole seconds"},
		{[]string{"pair", "create", "-data", dir, "-ttl", "721h"}, 1, "whole seconds"},
		// no node: one line that says so
		{[]string{"pair", "list", "-data", dir}, 1, "not running"},
		{[]string{"peers", "-data", dir}, 1, "not running"},
		{[]string{"pair", "join", "tcpraid-pair-1-aaaa", "-data", dir}, 1, "not running"},
		{[]string{"peers", "show", "alpha", "-data", dir}, 1, "not running"},
		{[]string{"peers", "path", "set", "alpha", "p", "-addr", "192.0.2.1:1", "-data", dir}, 1, "not running"},
		{[]string{"peers", "path", "remove", "alpha", "p", "-data", dir}, 1, "not running"},
	} {
		code, ok, stdout, stderr := call(c.args...)
		if !ok || code != c.code || stdout != "" || !strings.Contains(stderr, c.want) {
			t.Errorf("tcpraid %s: handled=%v code=%d stdout=%q stderr=%q, want code %d and %q", strings.Join(c.args, " "), ok, code, stdout, stderr, c.code, c.want)
		}
	}
	if code, _, _, stderr := call("pair", "join", "-h"); code != 0 || !strings.Contains(stderr, "CODE") || !strings.Contains(stderr, "-addr") {
		t.Errorf("pair join -h: code %d, %q", code, stderr)
	}
}

func TestRunOptionsRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-cert", "c.pem"}, "-cert and -key go together"},
		{[]string{"-key", "k.pem"}, "-cert and -key go together"},
		{[]string{"-cert", "c.pem", "-key", "k.pem"}, "Set -listen"},
		{[]string{"-advertise", "no port"}, "-advertise"},
		{[]string{"-spares", "9"}, "-spares is 9. Give 0 to 8."},
		{[]string{"-spares", "-1"}, "-spares is -1. Give 0 to 8."},
	} {
		code, _, _, stderr := call(append([]string{"run", "-data", dir}, c.args...)...)
		if code != 2 || !strings.Contains(stderr, c.want) {
			t.Errorf("run %s: code %d, %q, want code 2 and %q", strings.Join(c.args, " "), code, stderr, c.want)
		}
	}
	// a certificate that cannot be read ends the start
	code, _, _, stderr := call("run", "-data", dir, "-name", "gamma", "-api", "127.0.0.1:0", "-listen", "127.0.0.1:0", "-cert", filepath.Join(dir, "none.pem"), "-key", filepath.Join(dir, "none.key"))
	if code != 1 || !strings.Contains(stderr, "-cert / -key") {
		t.Errorf("run with a missing certificate: code %d, %q", code, stderr)
	}
	// a taken -listen address ends the start, and the API address is not recorded
	taken := freeAddr(t)
	dirB := filepath.Join(t.TempDir(), "b")
	startNodeWith(t, runOptions{dataDir: dirB, name: "beta", listen: taken})
	dirC := filepath.Join(t.TempDir(), "c")
	code, _, _, stderr = call("run", "-data", dirC, "-name", "gamma", "-api", "127.0.0.1:0", "-listen", taken)
	if code != 1 || !strings.Contains(stderr, "-listen:") {
		t.Errorf("run on a taken -listen address: code %d, %q", code, stderr)
	}
	if got := files(t, dirC); got["api.addr"] != "" {
		t.Errorf("a start that failed at -listen recorded an API address")
	}
}

// The pairing code can come from standard input instead of the command line; the tier of
// a path is switched, a path is added and removed, and a peer is shown, all through the
// commands.
func TestJoinFromStdinAndPathCommands(t *testing.T) {
	dirA, dirB := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	addrA, plainA := freeAddr(t), freeAddr(t)
	startNodeWith(t, runOptions{dataDir: dirA, name: "alpha", listen: addrA, listenPlain: plainA, advertise: stringList{addrA}})
	startNodeWith(t, runOptions{dataDir: dirB, name: "beta"})

	pairingCode := strings.TrimSpace(must(t, "pair", "create", "-data", dirA))
	// as pasted: with a line break in it and one behind it
	withStdin(t, pairingCode[:40]+"\n"+pairingCode[40:]+"\n")
	if out := must(t, "pair", "join", "-data", dirB); !strings.Contains(out, "paired with alpha") {
		t.Fatalf("pair join from standard input: %q", out)
	}
	// "-" is the same
	pairingCode = strings.TrimSpace(must(t, "pair", "create", "-data", dirA))
	withStdin(t, pairingCode)
	if out := must(t, "pair", "join", "-", "-data", dirB); !strings.Contains(out, "paired with alpha") {
		t.Fatalf("pair join - : %q", out)
	}
	// more than a code can be
	withStdin(t, strings.Repeat("x", maxCodeInput+1))
	if code, _, _, stderr := call("pair", "join", "-data", dirB); code != 1 || !strings.Contains(stderr, "not a pairing code") {
		t.Fatalf("pair join with too much on standard input: code %d, %q", code, stderr)
	}

	show := must(t, "peers", "show", "alpha", "-data", dirB)
	for _, want := range []string{"name         alpha", "fingerprint  ", addrA, "default", "tls", "data"} {
		if !strings.Contains(show, want) {
			t.Fatalf("peers show lacks %q:\n%s", want, show)
		}
	}
	if code, _, _, stderr := call("peers", "show", "nobody", "-data", dirB); code != 1 || !strings.Contains(stderr, "not found") {
		t.Fatalf("peers show of an unknown peer: code %d, %q", code, stderr)
	}
	if out := must(t, "peers", "show", "beta", "-data", dirA); !strings.Contains(out, "cannot reach") {
		t.Fatalf("peers show of a peer without a path: %q", out)
	}

	// switch the default path to the off tier at the plain listener: only what is given changes
	out := must(t, "peers", "path", "set", "alpha", "default", "-security", "off", "-addr", plainA, "-data", dirB)
	if !strings.Contains(out, "changed") || !strings.Contains(out, plainA) || !strings.Contains(out, "off") {
		t.Fatalf("peers path set: %q", out)
	}
	if out := must(t, "peers", "check", "alpha", "-data", dirB); !strings.Contains(out, plainA+" (off): ok") {
		t.Fatalf("check on the off tier: %q", out)
	}
	// a second path, through a proxy; its password is stored and not shown
	out = must(t, "peers", "path", "set", "alpha", "side", "-data", dirB, "-addr", "192.0.2.9:9851", "-type", "socks5",
		"-proxy", "socks5://user:hunter2@192.0.2.1:1080", "-roles", "repair,control", "-subs", "2")
	if !strings.Contains(out, "added") || !strings.Contains(out, "socks5://192.0.2.1:1080") || strings.Contains(out, "hunter2") ||
		!strings.Contains(out, "repair,control") {
		t.Fatalf("adding a path: %q", out)
	}
	// changing something else on it keeps the proxy's password
	must(t, "peers", "path", "set", "alpha", "side", "-subs", "3", "-data", dirB)
	state, err := os.ReadFile(filepath.Join(dirB, node.StateFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(state), "socks5://user:hunter2@192.0.2.1:1080") || !strings.Contains(string(state), `"subs": 3`) {
		t.Fatalf("the state after changing the number of sub-connections:\n%s", state)
	}
	// owncert wants a pin; given one it is stored; going back to tls drops it
	pin := strings.Repeat("ab", 32)
	if code, _, _, stderr := call("peers", "path", "set", "alpha", "side", "-security", "owncert", "-data", dirB); code != 1 || !strings.Contains(stderr, "pin") {
		t.Fatalf("owncert without a pin: code %d, %q", code, stderr)
	}
	if out := must(t, "peers", "path", "set", "alpha", "side", "-security", "owncert", "-pin", pin, "-data", dirB); !strings.Contains(out, pin) {
		t.Fatalf("owncert with a pin: %q", out)
	}
	if out := must(t, "peers", "path", "set", "alpha", "side", "-security", "tls", "-data", dirB); strings.Contains(out, pin) {
		t.Fatalf("back to tls, the pin is still there: %q", out)
	}
	// what cannot be a path is refused and nothing changes
	before := must(t, "peers", "show", "alpha", "-data", dirB)
	for _, bad := range [][]string{{"-addr", "no-port"}, {"-subs", "0"}, {"-roles", "bulk"}, {"-type", "udp"}, {"-security", "none"}} {
		args := append([]string{"peers", "path", "set", "alpha", "side", "-data", dirB}, bad...)
		if code, _, _, stderr := call(args...); code != 1 || stderr == "" || strings.Contains(stderr, "hunter2") {
			t.Errorf("%v: code %d, %q", bad, code, stderr)
		}
	}
	if code, _, _, stderr := call("peers", "path", "set", "alpha", "new", "-subs", "2", "-data", dirB); code != 1 || !strings.Contains(stderr, "-addr") {
		t.Errorf("a new path without -addr: code %d, %q", code, stderr)
	}
	if after := must(t, "peers", "show", "alpha", "-data", dirB); after != before {
		t.Fatalf("refused changes changed the peer:\n%s\n%s", before, after)
	}

	if out := must(t, "peers", "path", "remove", "alpha", "side", "-data", dirB); !strings.Contains(out, "removed") {
		t.Fatalf("peers path remove: %q", out)
	}
	if code, _, _, stderr := call("peers", "path", "remove", "alpha", "side", "-data", dirB); code != 1 || !strings.Contains(stderr, "no path") {
		t.Fatalf("removing a path twice: code %d, %q", code, stderr)
	}
	if out := must(t, "peers", "path", "remove", "alpha", "default", "-data", dirB); !strings.Contains(out, "no path left") {
		t.Fatalf("removing the last path: %q", out)
	}
}

// A node that takes too long to answer is reported as that, not as a node that is not
// running.
func TestCommandTimeoutIsNotNoNode(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // held open, never answered
		}
	}()
	if err := os.WriteFile(filepath.Join(dir, node.TokenFile), []byte(strings.Repeat("ab", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := node.WriteAPIAddr(dir, ln.Addr().String()); err != nil {
		t.Fatal(err)
	}
	err = apiCall(dir, http.MethodGet, api.PathPeers, nil, nil, 200*time.Millisecond)
	if !errors.Is(err, errTimeout) || errors.Is(err, errNoNode) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v, want an error that says timed out and does not match errNoNode", err)
	}
}

// A stopping node waits for a pairing in progress before it gives up its data directory.
func TestRunWaitsForConnectionsAtShutdown(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a")
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runNodeWith(ctx, runOptions{dataDir: dir, apiAddr: "127.0.0.1:0", name: "alpha", listen: addr})
	}()
	waitAddr(t, dir)

	// a node that has begun a pairing and has not sent its request yet
	other, err := node.Open(filepath.Join(t.TempDir(), "b"), "beta")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	c, err := tls.Dial("tcp", addr, &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{other.Identity.TLSCertificate()},
		NextProtos:         []string{pair.ALPN},
		InsecureSkipVerify: true, // whom this test talks to is not what it is about
	})
	if err != nil {
		t.Fatal(err)
	}
	// the listener takes the connection in when it has read the client's certificate
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		t.Fatalf("the node stopped with a pairing in progress: %v", err)
	case <-time.After(400 * time.Millisecond):
	}
	c.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the node ended with %v, want a clean stop", err)
		}
	case <-time.After(shutdownWait - time.Second):
		t.Fatal("the node did not stop when the pairing connection was gone")
	}
}

// "peers remove" names the rules that still name the peer and says what becomes of
// them; with -with-rules they go with it.
func TestPeersRemoveAndTheRules(t *testing.T) {
	dirA, dirB := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	addrA := freeAddr(t)
	startNodeWith(t, runOptions{dataDir: dirA, name: "alpha", listen: addrA, advertise: stringList{addrA}})
	startNodeWith(t, runOptions{dataDir: dirB, name: "beta"})
	pairAgain := func() {
		t.Helper()
		_, _, stdout, _ := call("pair", "create", "-data", dirA)
		must(t, "pair", "join", strings.TrimSpace(stdout), "-data", dirB)
	}
	pairAgain()
	must(t, "rules", "add", "zfs", "-data", dirB, "-peer", "alpha", "-listen", freeAddr(t), "-dest", "192.0.2.1:873")
	must(t, "rules", "add", "ssh", "-data", dirB, "-peer", "alpha", "-listen", freeAddr(t), "-dest", "192.0.2.1:22")

	out := must(t, "peers", "remove", "alpha", "-data", dirB)
	if !strings.Contains(out, "peer alpha removed") || !strings.Contains(out, "These rules still name alpha: zfs, ssh") || !strings.Contains(out, "takes them over") {
		t.Errorf("peers remove with two rules that name the peer printed:\n%s", out)
	}
	if rules := must(t, "rules", "-data", dirB); !strings.Contains(rules, "zfs") || !strings.Contains(rules, "ssh") {
		t.Errorf("the rules after peers remove without -with-rules:\n%s", rules)
	}

	pairAgain()
	out = must(t, "peers", "remove", "alpha", "-with-rules", "-data", dirB)
	if !strings.Contains(out, "peer alpha removed") || !strings.Contains(out, "The rules that named it were removed with it: zfs, ssh") {
		t.Errorf("peers remove -with-rules printed:\n%s", out)
	}
	if rules := must(t, "rules", "-data", dirB); strings.TrimSpace(rules) != "no rules" {
		t.Errorf("the rules after peers remove -with-rules:\n%s", rules)
	}

	// a peer that no rule names: one line, as before
	pairAgain()
	if out := must(t, "peers", "remove", "alpha", "-data", dirB); strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Errorf("peers remove of a peer without rules printed:\n%s", out)
	}
}

// Two "peers path set" for two paths of one peer, and a "peers set", that run at the
// same time all take effect: none overwrites what another has just stored.
func TestPathCommandsAtTheSameTime(t *testing.T) {
	dirA, dirB := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	addrA := freeAddr(t)
	startNodeWith(t, runOptions{dataDir: dirA, name: "alpha", listen: addrA, advertise: stringList{addrA}})
	startNodeWith(t, runOptions{dataDir: dirB, name: "beta"})
	_, _, stdout, _ := call("pair", "create", "-data", dirA)
	must(t, "pair", "join", strings.TrimSpace(stdout), "-data", dirB)
	must(t, "peers", "path", "set", "alpha", "side", "-addr", "192.0.2.9:9851", "-data", dirB)

	for round := 1; round <= 30; round++ {
		one, two := strconv.Itoa(1+round%60), strconv.Itoa(1+(round*7)%60)
		allow := fmt.Sprintf("192.0.2.0/24:%d", 1000+round)
		var wg sync.WaitGroup
		for _, args := range [][]string{
			{"peers", "path", "set", "alpha", "default", "-subs", one, "-data", dirB},
			{"peers", "path", "set", "alpha", "side", "-subs", two, "-data", dirB},
			{"peers", "set", "alpha", "-allow", allow, "-data", dirB},
		} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if code, _, _, stderr := call(args...); code != 0 {
					t.Errorf("round %d: tcpraid %s: code %d, %s", round, strings.Join(args, " "), code, stderr)
				}
			}()
		}
		wg.Wait()
		var p api.Peer
		if err := apiCall(dirB, "GET", peerPath("alpha"), nil, &p, shortCall); err != nil {
			t.Fatal(err)
		}
		if len(p.Paths) != 2 || strconv.Itoa(p.Paths[0].Subs) != one || strconv.Itoa(p.Paths[1].Subs) != two || p.Allow != allow {
			t.Fatalf("round %d: the peer is %+v, want subs %s and %s and allow %s", round, p, one, two, allow)
		}
	}

	// a command that names nothing to change shows the path as it is
	if out := must(t, "peers", "path", "set", "alpha", "side", "-data", dirB); !strings.Contains(out, "path side of alpha changed") {
		t.Errorf("peers path set without a setting: %q", out)
	}
	if code, _, _, stderr := call("peers", "path", "set", "alpha", "third", "-data", dirB); code != 1 || !strings.Contains(stderr, "needs -addr") {
		t.Errorf("a new path without anything: code %d, %q", code, stderr)
	}
}
