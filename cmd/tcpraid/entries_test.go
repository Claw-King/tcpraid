package main

import (
	"bytes"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/engine"
)

// A SOCKS5 rule through the commands: added with -socks, listed, switched off and on,
// given a setting, and the warning for an address that other hosts reach.
func TestSocksRuleThroughTheCommands(t *testing.T) {
	dirA, dirB := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	addrA := freeAddr(t)
	runFor(t, optionsOf(t, "-data", dirA, "-name", "alpha", "-listen", addrA, "-advertise", addrA))
	runFor(t, optionsOf(t, "-data", dirB, "-name", "beta"))
	_, _, code, _ := call("pair", "create", "-data", dirA)
	must(t, "pair", "join", strings.TrimSpace(code), "-data", dirB)

	_, port, _ := strings.Cut(freeAddr(t), ":")
	out := must(t, "rules", "add", "web", "-peer", "alpha", "-socks", ":"+port, "-data", dirB)
	if out != "rule web added: listening\n" {
		t.Errorf("rules add -socks on loopback: %q", out)
	}
	list := must(t, "rules", "-data", dirB)
	if !strings.Contains(list, "SOCKS5 on 127.0.0.1:"+port) || !strings.Contains(list, "(as the client asks)") {
		t.Errorf("the list of rules:\n%s", list)
	}
	if out := must(t, "rules", "disable", "web", "-data", dirB); !strings.Contains(out, "disabled") {
		t.Errorf("rules disable: %q", out)
	}
	if out := must(t, "rules", "enable", "web", "-data", dirB); !strings.Contains(out, "listening") {
		t.Errorf("rules enable: %q", out)
	}
	if out := must(t, "rules", "set", "web", "-window", "16MB", "-data", dirB); !strings.Contains(out, "window=16MB") {
		t.Errorf("rules set: %q", out)
	}
	if list := must(t, "rules", "-data", dirB); !strings.Contains(list, "SOCKS5 on 127.0.0.1:"+port) || !strings.Contains(list, "with window=16MB") {
		t.Errorf("the rule after it was switched and set:\n%s", list)
	}
	// on an address that others reach: the command says what that means
	_, port2, _ := strings.Cut(freeAddr(t), ":")
	out = must(t, "rules", "add", "open", "-peer", "alpha", "-socks", "0.0.0.0:"+port2, "-data", dirB)
	if !strings.Contains(out, "Note: 0.0.0.0:"+port2+" is not a loopback address") {
		t.Errorf("rules add -socks on every address: %q", out)
	}
	// a rule is one kind only
	if code, _, _, stderr := call("rules", "add", "both", "-peer", "alpha", "-socks", ":1", "-listen", ":2", "-dest", "192.0.2.1:3", "-data", dirB); code != 1 || !strings.Contains(stderr, "one of socks") {
		t.Errorf("a rule of two kinds: code %d, %q", code, stderr)
	}
}

// The options of -redirect are checked before anything is opened.
func TestRunRedirectFlags(t *testing.T) {
	if !engine.TransparentSupported {
		_, code, done, stderr := runFlagsOf("-data", "/d", "-redirect", "forwarded")
		if !done || code != 2 || !strings.Contains(stderr, "only available on Linux") {
			t.Errorf("-redirect where it does not exist: code %d, %q", code, stderr)
		}
		return
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-redirect", "forwarded"}, "-redirect needs -transparent"},
		{[]string{"-transparent", ":9800", "-redirect", "all"}, `"all" is none of forwarded, local and both`},
		{[]string{"-transparent", ":9800", "-redirect-print"}, "-redirect-print goes with -redirect"},
	} {
		_, code, done, stderr := runFlagsOf(append([]string{"-data", "/d"}, c.args...)...)
		if !done || code != 2 || !strings.Contains(stderr, c.want) {
			t.Errorf("run %v: code %d, %q, want %q", c.args, code, stderr, c.want)
		}
	}
	if o := optionsOf(t, "-data", "/d", "-transparent", ":9800", "-redirect", "both", "-redirect-print"); o.redirect != "both" || !o.redirectPrint {
		t.Errorf("the options: %+v", o)
	}
}

// With -redirect-print the node logs the rules it would install, at the start and
// when its transparent rules change, and runs no program.
func TestRedirectPrintFollowsTheRules(t *testing.T) {
	if !engine.TransparentSupported {
		t.Skip("transparent rules are for Linux")
	}
	var logged lockedBuffer
	old := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(old)
	dirA, dirB := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	addrA := freeAddr(t)
	runFor(t, optionsOf(t, "-data", dirA, "-name", "alpha", "-listen", addrA, "-advertise", addrA))
	runFor(t, optionsOf(t, "-data", dirB, "-name", "beta", "-transparent", "127.0.0.1:0", "-redirect", "both", "-redirect-print"))
	_, _, code, _ := call("pair", "create", "-data", dirA)
	must(t, "pair", "join", strings.TrimSpace(code), "-data", dirB)
	has := func(text string) bool { return strings.Contains(logged.String(), text) }
	if !has("-redirect-print: the node would give nft these rules") || !has("table inet tcpraid {") || !has("hook prerouting priority -110") || !has("hook output priority -110") {
		t.Fatalf("the log at the start:\n%s", logged.String())
	}
	if has("redirect to :") {
		t.Fatal("a redirect line without a transparent rule")
	}
	must(t, "rules", "add", "lan", "-peer", "alpha", "-match", "198.51.100.0/24:873", "-data", dirB)
	for deadline := time.Now().Add(10 * time.Second); !has("ip daddr 198.51.100.0/24 tcp dport 873 redirect to :"); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the rule did not reach the log:\n%s", logged.String())
		}
	}
	before := bytes.Count([]byte(logged.String()), []byte("-redirect-print:"))
	must(t, "rules", "disable", "lan", "-data", dirB)
	for deadline := time.Now().Add(10 * time.Second); bytes.Count([]byte(logged.String()), []byte("-redirect-print:")) == before; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("switching the rule off did not print the rules again")
		}
	}
	last := logged.String()[strings.LastIndex(logged.String(), "-redirect-print:"):]
	if strings.Contains(last, "198.51.100.0/24") {
		t.Errorf("the rules after the transparent rule was switched off still hold it:\n%s", last)
	}
}
