package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Claw-King/tcpraid/internal/api"
	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/node"
)

// fakeNode writes into a fresh directory the address and a token of an API that answers
// with handler, and returns the directory: what a command needs to talk to it.
func fakeNode(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	dir := t.TempDir()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	if err := os.WriteFile(filepath.Join(dir, node.AddrFile), []byte(strings.TrimPrefix(srv.URL, "http://")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, node.TokenFile), []byte(strings.Repeat("ab", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Through the commands: a path gets a second address, the lists show both in the order
// they are tried, the check answers for each, and a rule keeps to a path and a number.
func TestPathAddressesAndRulePathsCommands(t *testing.T) {
	dirA, dirB := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	lnA, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addrA := lnA.Addr().String()
	lnA.Close()
	startNodeWith(t, runOptions{dataDir: dirA, name: "alpha", listen: addrA, advertise: stringList{addrA}})
	startNodeWith(t, runOptions{dataDir: dirB, name: "beta"})
	withStdin(t, strings.TrimSpace(must(t, "pair", "create", "-data", dirA)))
	must(t, "pair", "join", "-data", dirB)

	// an address that refuses: a port that was just closed
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()

	out := must(t, "peers", "path", "set", "alpha", "default", "-addr", dead+","+addrA, "-data", dirB)
	if !strings.Contains(out, "changed") || !strings.Contains(out, dead+","+addrA) || !strings.Contains(out, "MARK") {
		t.Fatalf("peers path set with two addresses:\n%s", out)
	}
	if out := must(t, "peers", "-data", dirB); !strings.Contains(out, dead+","+addrA+" (tls, data)") {
		t.Errorf("peers does not list both addresses:\n%s", out)
	}
	if out := must(t, "peers", "show", "alpha", "-data", dirB); !strings.Contains(out, dead+","+addrA) {
		t.Errorf("peers show does not list both addresses:\n%s", out)
	}
	code, _, stdout, stderr := call("peers", "check", "alpha", "-data", dirB)
	if code != 1 || !strings.Contains(stdout, "default  "+dead+" (tls): FAILED: ") || !strings.Contains(stdout, "default  "+addrA+" (tls): ok") ||
		!strings.Contains(stderr, "1 of 2 address(es) to alpha failed. Every path still has an address that works.") {
		t.Errorf("peers check of a path with one address down: code %d\n%s\n%s", code, stdout, stderr)
	}
	// one address alone takes the other away, as the command always set an address
	out = must(t, "peers", "path", "set", "alpha", "default", "-addr", addrA, "-data", dirB)
	if strings.Contains(out, dead) {
		t.Fatalf("after one address alone:\n%s", out)
	}
	if out := must(t, "peers", "check", "alpha", "-data", dirB); !strings.Contains(out, "default  "+addrA+" (tls): ok") {
		t.Errorf("peers check of the path with one address: %q", out)
	}
	// the mark
	code, _, stdout, stderr = call("peers", "path", "set", "alpha", "default", "-mark", "7", "-data", dirB)
	if engine.MarkSupported {
		if code != 0 || !strings.Contains(stdout, "  7\n") {
			t.Errorf("peers path set -mark 7: code %d\n%s\n%s", code, stdout, stderr)
		}
		must(t, "peers", "path", "set", "alpha", "default", "-mark", "0", "-data", dirB)
	} else if code != 1 || !strings.Contains(stderr, "only be set on Linux") {
		t.Errorf("peers path set -mark 7 on a system without marks: code %d, %q", code, stderr)
	}

	// a rule that keeps to a path and to a number of sub-connections
	lnR, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	in := lnR.Addr().String()
	lnR.Close()
	out = must(t, "rules", "add", "zfs", "-peer", "alpha", "-listen", in, "-dest", "192.0.2.1:873", "-paths", "default", "-subs", "3", "-data", dirB)
	want := "listening, over the paths default, with at most 3 sub-connections on a path"
	if !strings.Contains(out, "rule zfs added: "+want) {
		t.Fatalf("rules add with -paths and -subs: %q", out)
	}
	if out := must(t, "rules", "-data", dirB); !strings.Contains(out, want) {
		t.Errorf("rules does not say what the rule keeps to:\n%s", out)
	}
	// switched off and on again by the commands that know nothing of paths: they stay
	must(t, "rules", "disable", "zfs", "-data", dirB)
	if out := must(t, "rules", "enable", "zfs", "-data", dirB); !strings.Contains(out, want) {
		t.Errorf("after disable and enable: %q", out)
	}
	if code, _, _, stderr := call("rules", "add", "x", "-peer", "alpha", "-listen", "127.0.0.1:1", "-dest", "192.0.2.1:1", "-paths", "nope", "-data", dirB); code != 1 ||
		!strings.Contains(stderr, `the rule names the path "nope"`) {
		t.Errorf("rules add with a path that is not there: code %d, %q", code, stderr)
	}
	// a rule without the two reads as it always did
	lnS, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	plain := lnS.Addr().String()
	lnS.Close()
	if out := must(t, "rules", "add", "plain", "-peer", "alpha", "-listen", plain, "-dest", "192.0.2.1:873", "-data", dirB); strings.TrimSpace(out) != "rule plain added: listening" {
		t.Errorf("rules add without -paths and -subs: %q", out)
	}
}

// What the state of a rule says after the word "listening".
func TestRuleStateSaysWhatIsParticular(t *testing.T) {
	for want, r := range map[string]api.Rule{
		"listening":                     {Enabled: true, Listening: true},
		"listening, over the paths a,b": {Enabled: true, Listening: true, Paths: []string{"a", "b"}},
		"listening, with at most 2 sub-connections on a path":                                          {Enabled: true, Listening: true, Subs: 2},
		"listening, the peer opens the connections while this node's path is down":                     {Enabled: true, Listening: true, PeerDials: true},
		"listening, over the paths a, with at most 2 sub-connections on a path":                        {Enabled: true, Listening: true, Paths: []string{"a"}, Subs: 2},
		"listening, over the paths a, last connection: it failed":                                      {Enabled: true, Listening: true, Paths: []string{"a"}, Error: "it failed"},
		"listening, last connection: it failed":                                                        {Enabled: true, Listening: true, Error: "it failed"},
		"disabled":                                                                                     {Paths: []string{"a"}, Subs: 2},
		"FAILED: the port is taken":                                                                    {Enabled: true, Error: "the port is taken", Paths: []string{"a"}},
		"listening, the peer opens the connections while this node's path is down, last connection: x": {Enabled: true, Listening: true, PeerDials: true, Error: "x"},
	} {
		if got := ruleState(r); got != want {
			t.Errorf("ruleState(%+v) = %q, want %q", r, got, want)
		}
	}
}

// A node of an older version refuses a request with a field it does not know. The
// command says that the node is older, and what to do.
func TestCommandsSayWhenTheNodeIsOlder(t *testing.T) {
	refuse := func(field string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":"the request body is not the JSON this request takes: json: unknown field \"%s\""}`+"\n", field)
		}
	}
	for field, args := range map[string][]string{
		"addrs": {"peers", "path", "set", "alpha", "default", "-addr", "192.0.2.1:9851,192.0.2.2:9851"},
		"mark":  {"peers", "path", "set", "alpha", "default", "-mark", "7"},
		"paths": {"rules", "add", "zfs", "-peer", "alpha", "-listen", "127.0.0.1:5000", "-dest", "192.0.2.1:873", "-paths", "default"},
		"subs":  {"rules", "add", "zfs", "-peer", "alpha", "-listen", "127.0.0.1:5000", "-dest", "192.0.2.1:873", "-subs", "2"},
	} {
		dir := fakeNode(t, refuse(field))
		code, _, _, stderr := call(append(args, "-data", dir)...)
		if code != 1 || !strings.Contains(stderr, "the running node is older than this command and does not know") || !strings.Contains(stderr, "Restart the node to finish an update.") {
			t.Errorf("%s: code %d, %q", field, code, stderr)
		}
	}
	// any other refusal is passed on as it is
	other := errors.New("rule \"x\": no peer is called \"y\"")
	if got := olderNode(other, map[string]string{"paths": "paths for a rule"}); got != other {
		t.Errorf("an error that is about something else was changed: %v", got)
	}
	// what is sent without the new flags has none of the new fields
	var bodies []string
	dir := fakeNode(t, func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		bodies = append(bodies, string(b[:n]))
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"refused for the test"}`)
	})
	call("peers", "path", "set", "alpha", "default", "-addr", "192.0.2.1:9851", "-data", dir)
	call("rules", "add", "zfs", "-peer", "alpha", "-listen", "127.0.0.1:5000", "-dest", "192.0.2.1:873", "-data", dir)
	if len(bodies) != 2 {
		t.Fatalf("%d requests arrived", len(bodies))
	}
	for i, fields := range [][]string{{`"addrs"`, `"mark"`}, {`"paths"`, `"subs"`}} {
		for _, field := range fields {
			if strings.Contains(bodies[i], field) {
				t.Errorf("a request without the new flags has %s: %s", field, bodies[i])
			}
		}
	}
}

// The summary of a check: by paths when each has one address, as it always was, and by
// addresses when a path has several.
func TestPeersCheckSummary(t *testing.T) {
	answer := func(paths string) string {
		return fakeNode(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"peer":"alpha","paths":[%s]}`, paths)
		})
	}
	ok := func(name, addr string) string {
		return fmt.Sprintf(`{"name":%q,"addr":%q,"security":"tls","ok":true}`, name, addr)
	}
	bad := func(name, addr string) string {
		return fmt.Sprintf(`{"name":%q,"addr":%q,"security":"tls","ok":false,"error":"refused"}`, name, addr)
	}
	for name, c := range map[string]struct {
		paths string
		code  int
		want  string
	}{
		"one address each, all well":      {ok("a", "192.0.2.1:1") + "," + ok("b", "192.0.2.2:1"), 0, ""},
		"one address each, one down":      {ok("a", "192.0.2.1:1") + "," + bad("b", "192.0.2.2:1"), 1, "1 of 2 path(s) to alpha failed"},
		"two addresses, all well":         {ok("a", "192.0.2.1:1") + "," + ok("a", "192.0.2.2:1"), 0, ""},
		"two addresses, one down":         {bad("a", "192.0.2.1:1") + "," + ok("a", "192.0.2.2:1") + "," + ok("b", "192.0.2.3:1"), 1, "1 of 3 address(es) to alpha failed. Every path still has an address that works."},
		"two addresses, both down":        {bad("a", "192.0.2.1:1") + "," + bad("a", "192.0.2.2:1") + "," + ok("b", "192.0.2.3:1"), 1, "2 of 3 address(es) to alpha failed, and 1 of 2 path(s) have no address that works"},
		"a path down and an address down": {bad("a", "192.0.2.1:1") + "," + ok("a", "192.0.2.2:1") + "," + bad("b", "192.0.2.3:1"), 1, "2 of 3 address(es) to alpha failed, and 1 of 2 path(s) have no address that works"},
	} {
		code, _, stdout, stderr := call("peers", "check", "alpha", "-data", answer(c.paths))
		if code != c.code || (c.want != "" && !strings.Contains(stderr, c.want)) || (c.want == "" && strings.TrimSpace(stderr) != "") {
			t.Errorf("%s: code %d\n%s\n%s", name, code, stdout, stderr)
		}
	}
}
