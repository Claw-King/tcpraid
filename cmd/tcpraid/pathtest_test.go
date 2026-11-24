package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/api"
)

// The result of a path test as a person reads it: the rates, and one sentence for each
// path that says what they mean and no more than that.
func TestPrintPeerTest(t *testing.T) {
	res := api.PeerTest{Peer: "nas-b", Seconds: 10, Paths: []api.PathTest{
		{Name: "default", Subs: 8, One: 2.5 * (1 << 20), All: 19 * (1 << 20)},
		{Name: "tunnel", Subs: 8, One: 20 * (1 << 20), All: 21 * (1 << 20), LittleGain: true},
		{Name: "single", Subs: 1, One: 3 * (1 << 20)},
	}}
	var out bytes.Buffer
	if err := printPeerTest(&out, res); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"PATH SUB-CONNECTIONS ONE ALONE ALL TOGETHER TIMES",
		"default 8 2.5 MB/s 19.0 MB/s 7.6",
		"tunnel 8 20.0 MB/s 21.0 MB/s 1.1",
		"single 1 3.0 MB/s - -",
		"default: 8 sub-connections together carried 7.6 times what one carried alone.",
		`tunnel: 8 sub-connections together carried little more than one alone. Either one connection already fills this line, or something on the path puts the connections into one. "Connecting two sites" in the documentation (docs/en/connecting.md) says how to tell them apart.`,
		"single: the path has one sub-connection, so there is nothing to compare.",
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != len(want) {
		t.Fatalf("%d lines, want %d:\n%s", len(lines), len(want), out.String())
	}
	for i, l := range lines {
		if got := strings.Join(strings.Fields(l), " "); got != want[i] {
			t.Errorf("line %d:\n got  %s\n want %s", i, got, want[i])
		}
	}

	// a path that could not be measured, on a node that has no path of its own
	out.Reset()
	res = api.PeerTest{Peer: "nas-b", PeerDials: true, Paths: []api.PathTest{
		{Name: "default", Subs: 8, Error: `no test data arrived at peer "nas-b"`},
	}}
	err := printPeerTest(&out, res)
	if err == nil || err.Error() != "1 of 1 path(s) could not be measured" {
		t.Errorf("the error: %v", err)
	}
	if got := out.String(); !strings.HasPrefix(got, "This node has no path to nas-b. The paths below are the ones nas-b holds to this node.\n") ||
		!strings.Contains(got, "default: not measured. no test data arrived at peer \"nas-b\"\n") {
		t.Errorf("printed:\n%s", got)
	}
}

func TestAgo(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second:                  "0s ago",
		12 * time.Second:              "12s ago",
		59*time.Second + 900e6:        "59s ago",
		3*time.Minute + 5*time.Second: "3m ago",
		2*time.Hour + 5*time.Minute:   "2h5m ago",
		72 * time.Hour:                "3d ago",
	} {
		if got := ago(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}

// The state of a rule says when its last connection failed; a state that only holds
// now has no time.
func TestRuleStateSaysWhen(t *testing.T) {
	at := time.Now().Add(-3*time.Minute - 10*time.Second)
	r := api.Rule{Enabled: true, Listening: true, Error: `peer "nas-b" could not connect to 192.0.2.10:873: connect: connection refused`, ErrorAt: &at}
	if got, want := ruleState(r), `listening, last connection: peer "nas-b" could not connect to 192.0.2.10:873: connect: connection refused (3m ago)`; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	r.ErrorAt = nil
	if got := ruleState(r); strings.Contains(got, "ago") {
		t.Errorf("a state without a time: %s", got)
	}
	r.Listening = false
	if got := ruleState(r); !strings.HasPrefix(got, "FAILED: ") || strings.Contains(got, "ago") {
		t.Errorf("a rule that does not listen: %s", got)
	}
}

// Two nodes on loopback, driven through the commands only: the path test from either
// node, and the sessions table printed again and again.
func TestPeersTestAndSessionsWatchCommands(t *testing.T) {
	dirA, dirB := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	addrA := freeAddr(t)
	startNodeWith(t, runOptions{dataDir: dirA, name: "alpha", listen: addrA, advertise: stringList{addrA}})
	startNodeWith(t, runOptions{dataDir: dirB, name: "beta"})
	_, _, stdout, _ := call("pair", "create", "-data", dirA)
	must(t, "pair", "join", strings.TrimSpace(stdout), "-data", dirB)

	if code, _, _, stderr := call("peers", "test", "alpha", "-seconds", "61", "-data", dirB); code != 1 || !strings.Contains(stderr, "-seconds is out of range") {
		t.Errorf("-seconds 61: code %d, %q", code, stderr)
	}
	if code, _, _, stderr := call("peers", "test", "nobody", "-seconds", "1", "-data", dirB); code != 1 || !strings.Contains(stderr, "not found") {
		t.Errorf("an unknown peer: code %d, %q", code, stderr)
	}
	if code, _, _, _ := call("peers", "test", "-data", dirB); code != 2 {
		t.Errorf("no peer named: code %d", code)
	}
	// the two nodes connect within a few seconds of the pairing; until then the test
	// is refused, and the command says so and can be run again
	var out, stderr string
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		var code int
		code, _, out, stderr = call("peers", "test", "alpha", "-seconds", "1", "-data", dirB)
		if code == 0 {
			break
		}
		refused := strings.Contains(stderr, "is not connected to this node") || strings.Contains(stderr, "has only just come up")
		if !refused || time.Now().After(deadline) {
			t.Fatalf("peers test: code %d\nstdout: %s\nstderr: %s", code, out, stderr)
		}
	}
	if !strings.Contains(stderr, "path test with alpha: two runs of up to 1 seconds for each path") {
		t.Errorf("what the command says before it starts: %q", stderr)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "PATH") || !strings.HasPrefix(lines[1], "default  8 ") || !strings.HasPrefix(lines[2], "default: 8 sub-connections together carried ") {
		t.Errorf("peers test printed:\n%s", out)
	}
	// from the node without a path, as JSON
	var raw string
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		code, _, stdout, stderr := call("peers", "test", "beta", "-seconds", "1", "-json", "-data", dirA)
		if raw = stdout; code == 0 {
			break
		}
		if !strings.Contains(stderr, "has only just come up") || time.Now().After(deadline) {
			t.Fatalf("peers test from the node without a path: code %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
	}
	var res api.PeerTest
	if err := json.Unmarshal([]byte(raw), &res); err != nil || !res.PeerDials || len(res.Paths) != 1 || res.Paths[0].One <= 0 || res.Paths[0].Error != "" {
		t.Errorf("peers test -json from the node without a path: %v\n%s", err, raw)
	}
	// no session is left: the test ended them (the receiving end may take a few seconds)
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		a, b := must(t, "sessions", "-json", "-data", dirA), must(t, "sessions", "-json", "-data", dirB)
		if strings.TrimSpace(a) == "[]" && strings.TrimSpace(b) == "[]" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sessions after the tests:\nalpha %s\nbeta %s", a, b)
		}
	}

	// sessions -watch: a table every interval, each under the time it was taken
	defer func(n int) { watchRounds = n }(watchRounds)
	watchRounds = 2
	watched := must(t, "sessions", "-watch", "-interval", "1s", "-data", dirB)
	if strings.Count(watched, "no sessions") != 2 || len(strings.Split(strings.TrimSpace(watched), "\n")) != 5 {
		t.Errorf("sessions -watch for two rounds printed:\n%s", watched)
	}
	if code, _, _, stderr := call("sessions", "-watch", "-json", "-data", dirB); code != 1 || !strings.Contains(stderr, "do not go together") {
		t.Errorf("-watch with -json: code %d, %q", code, stderr)
	}
	if code, _, _, stderr := call("sessions", "-watch", "-interval", "10ms", "-data", dirB); code != 1 || !strings.Contains(stderr, "-interval is out of range") {
		t.Errorf("-interval 10ms: code %d, %q", code, stderr)
	}
}
