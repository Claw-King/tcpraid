package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/node"
)

// Two nodes on loopback, driven through the commands only: pair, make a rule, carry a
// connection through it, switch the rule off and on, remove it.
func TestRulesCommands(t *testing.T) {
	dirA, dirB := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	addrA := freeAddr(t)
	startNodeWith(t, runOptions{dataDir: dirA, name: "alpha", listen: addrA, advertise: stringList{addrA}})
	startNodeWith(t, runOptions{dataDir: dirB, name: "beta", transparent: freeAddr(t), udpListen: "127.0.0.1:0"})
	_, _, stdout, _ := call("pair", "create", "-data", dirA)
	must(t, "pair", "join", strings.TrimSpace(stdout), "-data", dirB)

	if out := must(t, "rules", "-data", dirB); strings.TrimSpace(out) != "no rules" {
		t.Fatalf("rules on a fresh node: %q", out)
	}
	dst, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	arrived := make(chan [sha256.Size]byte, 4)
	go func() {
		for {
			c, err := dst.Accept()
			if err != nil {
				return
			}
			go func() {
				h := sha256.New()
				io.Copy(h, c)
				c.Close()
				var sum [sha256.Size]byte
				copy(sum[:], h.Sum(nil))
				arrived <- sum
			}()
		}
	}()
	in := freeAddr(t)
	out := must(t, "rules", "add", "zfs", "-data", dirB, "-peer", "alpha", "-listen", in, "-dest", dst.Addr().String())
	if !strings.Contains(out, "rule zfs added: listening") {
		t.Fatalf("rules add: %q", out)
	}
	list := must(t, "rules", "list", "-data", dirB)
	for _, want := range []string{"zfs", in, "alpha", dst.Addr().String(), "listening"} {
		if !strings.Contains(list, want) {
			t.Fatalf("rules list lacks %q: %q", want, list)
		}
	}
	if st := must(t, "status", "-data", dirB); !strings.Contains(st, "rules        1") {
		t.Fatalf("status: %q", st)
	}

	transfer := func() error {
		c, err := net.Dial("tcp", in)
		if err != nil {
			return err
		}
		defer c.Close()
		data := make([]byte, 3<<20)
		rand.Read(data)
		if _, err := c.Write(data); err != nil {
			return err
		}
		c.(*net.TCPConn).CloseWrite()
		c.SetReadDeadline(time.Now().Add(20 * time.Second))
		if _, err := io.Copy(io.Discard, c); err != nil {
			return err
		}
		select {
		case sum := <-arrived:
			if sum != sha256.Sum256(data) {
				return fmt.Errorf("what arrived differs from what was sent")
			}
		case <-time.After(20 * time.Second):
			return fmt.Errorf("nothing arrived")
		}
		return nil
	}
	if err := transfer(); err != nil {
		t.Fatalf("a connection through the rule: %v", err)
	}

	if out := must(t, "rules", "disable", "zfs", "-data", dirB); !strings.Contains(out, "disabled") {
		t.Fatalf("rules disable: %q", out)
	}
	if _, err := net.DialTimeout("tcp", in, time.Second); err == nil {
		t.Fatal("a disabled rule takes connections")
	}
	if out := must(t, "rules", "enable", "zfs", "-data", dirB); !strings.Contains(out, "listening") {
		t.Fatalf("rules enable: %q", out)
	}
	if err := transfer(); err != nil {
		t.Fatalf("a connection after switching the rule on again: %v", err)
	}

	// a transparent rule, and what the command line refuses
	if out := must(t, "rules", "add", "lan", "-data", dirB, "-peer", "alpha", "-match", "198.51.100.0/24:22", "-disabled"); !strings.Contains(out, "disabled") {
		t.Fatalf("rules add -match -disabled: %q", out)
	}
	if out := must(t, "rules", "enable", "lan", "-data", dirB); !strings.Contains(out, "listening") {
		t.Fatalf("a transparent rule on a node with -transparent: %q", out)
	}
	for what, args := range map[string][]string{
		"-peer is missing":     {"rules", "add", "x", "-data", dirB, "-listen", "127.0.0.1:1", "-dest", "192.0.2.1:1"},
		"no peer is called":    {"rules", "add", "x", "-data", dirB, "-peer", "nobody", "-listen", "127.0.0.1:1", "-dest", "192.0.2.1:1"},
		"cannot be used":       {"rules", "add", "x", "-data", dirB, "-peer", "alpha", "-listen", "127.0.0.1:1", "-dest", "nas..example.net:1"},
		"same length":          {"rules", "add", "x", "-data", dirB, "-peer", "alpha", "-listen", "127.0.0.1:1-2", "-dest", "192.0.2.1:1"},
		"not both":             {"rules", "add", "x", "-data", dirB, "-peer", "alpha", "-listen", "127.0.0.1:1", "-dest", "192.0.2.1:1", "-match", "192.0.2.0/24"},
		"already takes":        {"rules", "add", "x", "-data", dirB, "-peer", "alpha", "-listen", in, "-dest", "192.0.2.1:1"},
		"no rule is called":    {"rules", "enable", "nope", "-data", dirB},
		"not found":            {"rules", "remove", "nope", "-data", dirB},
		"takes 1 argument(s)":  {"rules", "add", "-data", dirB, "-peer", "alpha"},
		"unknown command":      {"rules", "frobnicate", "-data", dirB},
		"flag provided but no": {"rules", "add", "x", "-data", dirB, "-nonsense"},
	} {
		code, _, _, stderr := call(args...)
		if code == 0 || !strings.Contains(stderr, what) {
			t.Errorf("%v: code %d, stderr %q, want an error with %q", args, code, stderr, what)
		}
	}
	if out := must(t, "rules", "remove", "zfs", "-data", dirB); !strings.Contains(out, "removed") {
		t.Fatalf("rules remove: %q", out)
	}
	if out := must(t, "rules", "-data", dirB); strings.Contains(out, "zfs") || !strings.Contains(out, "lan") || !strings.Contains(out, "198.51.100.0/24:22") {
		t.Fatalf("rules after the removal: %q", out)
	}
}

// The rules survive a restart of the node: they are in its state file.
func TestRulesSurviveRestart(t *testing.T) {
	dirA, dirB := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	addrA := freeAddr(t)
	startNodeWith(t, runOptions{dataDir: dirA, name: "alpha", listen: addrA, advertise: stringList{addrA}})
	in := freeAddr(t)
	func() {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- runNodeWith(ctx, runOptions{dataDir: dirB, name: "beta", apiAddr: "127.0.0.1:0"}) }()
		waitAddr(t, dirB)
		_, _, stdout, _ := call("pair", "create", "-data", dirA)
		must(t, "pair", "join", strings.TrimSpace(stdout), "-data", dirB)
		must(t, "rules", "add", "keep", "-data", dirB, "-peer", "alpha", "-listen", in, "-dest", "192.0.2.1:873")
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("the node ended with %v", err)
		}
	}()
	if _, err := net.DialTimeout("tcp", in, time.Second); err == nil {
		t.Fatal("the rule's listener outlived the node")
	}
	// the address file of the first run goes, so that nothing reads it while the
	// node of the second run writes its own (a file that is open cannot always be
	// replaced on Windows)
	if err := os.Remove(filepath.Join(dirB, node.AddrFile)); err != nil {
		t.Fatal(err)
	}
	startNodeWith(t, runOptions{dataDir: dirB})
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		code, _, out, stderr := call("rules", "-data", dirB)
		if code == 0 && strings.Contains(out, "keep") && strings.Contains(out, "listening") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rules after the restart: code %d, %q, %q", code, out, stderr)
		}
	}
	c, err := net.DialTimeout("tcp", in, time.Second)
	if err != nil {
		t.Fatalf("the rule does not listen after the restart: %v", err)
	}
	c.Close()
}
