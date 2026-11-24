package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/api"
	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/node"
)

// Each transport flag that came with 0.7.0 reaches the engine's settings, and nothing
// else changes. The line "transport ..." stays as it was, and what is not at its
// default is listed apart.
func TestRunMoreTransportFlags(t *testing.T) {
	o, code, done, stderr := runFlagsOf("-data", "/x", "-target", "0.5s", "-pigtarget", "2s", "-dead", "30s", "-kickratio", "8",
		"-kickfloor", "128KB", "-share=false", "-fbsubs", "4", "-fbevery", "40ms", "-coalesce", "0", "-grace", "20m")
	if done {
		t.Fatalf("ended with code %d: %s", code, stderr)
	}
	want := engine.DefaultConfig()
	want.Version = version
	want.Target, want.PigTarget, want.Dead = 500*time.Millisecond, 2*time.Second, 30*time.Second
	want.KickRatio, want.KickFloor, want.Share = 8, 128<<10, false
	want.FbSubs, want.FbEvery, want.Coalesce = 4, 40*time.Millisecond, 0
	want.Grace = 20 * time.Minute
	got := o.engineConfig()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
	if line := o.transport.String(); line != "window=128MB budget=256MB grace=20m0s hol=1s timeline=off maxrss=off" {
		t.Errorf("the transport line is %q", line)
	}
	if more := tuned(got); more != "target=500ms pigtarget=2s dead=30s kickratio=8 kickfloor=128KB share=false fbsubs=4 fbevery=40ms coalesce=0s" {
		t.Errorf("the settings apart from the transport line: %q", more)
	}
	if more := tuned(engine.DefaultConfig()); more != "" {
		t.Errorf("with every setting at its default the line would read %q", more)
	}
	// what the command line gave is noted by name, the old flags and the new ones
	wantGiven := map[string]bool{"target": true, "pigtarget": true, "dead": true, "kickratio": true, "kickfloor": true, "share": true,
		"fbsubs": true, "fbevery": true, "coalesce": true, "grace": true}
	if !reflect.DeepEqual(o.transport.given, wantGiven) {
		t.Errorf("given: %v, want %v", o.transport.given, wantGiven)
	}
	// -share alone switches it on, as the flag of the manual mode does
	if o, _, done, _ = runFlagsOf("-data", "/x", "-share"); done || !o.engineConfig().Share || !o.transport.given["share"] {
		t.Errorf("-share alone: done=%v share=%v", done, o.engineConfig().Share)
	}
	// every setting of the node is a flag of "tcpraid run"
	for _, s := range node.Settings() {
		v := s.Value(engine.DefaultConfig())
		arg := []string{"-" + s.Name, v}
		if s.Bool {
			arg = []string{"-" + s.Name + "=" + v}
		}
		if _, code, done, stderr := runFlagsOf(append([]string{"-data", "/x"}, arg...)...); done {
			t.Errorf("run %v: ended with code %d: %s", arg, code, stderr)
		}
	}
}

// A value out of its range ends the command before the node starts, with a message
// that names the flag. The chunk size is not a flag of a node.
func TestRunMoreTransportFlagsRefused(t *testing.T) {
	for _, c := range [][2]string{
		{"-target", "1ms"}, {"-target", "1m"}, {"-target", "soon"},
		{"-pigtarget", "0"}, {"-dead", "1s"}, {"-dead", "1h"},
		{"-kickratio", "1"}, {"-kickratio", "big"}, {"-kickfloor", "10"},
		{"-fbsubs", "0"}, {"-fbsubs", "99"}, {"-fbevery", "0"}, {"-fbevery", "5s"},
		{"-coalesce", "-1s"}, {"-coalesce", "1m"}, {"-target", "default"},
		{"-chunk", "128KB"},
	} {
		_, code, done, stderr := runFlagsOf("-data", "/x", c[0], c[1])
		if !done || code != 2 || !strings.Contains(stderr, c[0]) {
			t.Errorf("%v: done=%v code=%d stderr=%q, want exit 2 with a message that names %s", c, done, code, stderr, c[0])
		}
	}
	if _, code, done, stderr := runFlagsOf("-data", "/x", "-share=perhaps"); !done || code != 2 || !strings.Contains(stderr, "-share") {
		t.Errorf("-share=perhaps: done=%v code=%d stderr=%q", done, code, stderr)
	}
}

// runFor runs a node with the given options and returns the function that stops it and
// waits for its end.
func runFor(t *testing.T, o runOptions) (stop func()) {
	t.Helper()
	o.apiAddr = "127.0.0.1:0"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runNodeWith(ctx, o) }()
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		if err := <-done; err != nil {
			t.Errorf("the node ended with %v, want a clean stop", err)
		}
		// the address file of this run goes, so that a node started on the directory
		// afterwards is not asked at the old address before it has written its own
		os.Remove(filepath.Join(o.dataDir, node.AddrFile))
	}
	t.Cleanup(stop)
	waitAddr(t, o.dataDir)
	return stop
}

// optionsOf returns the options of "tcpraid run" with the given command line.
func optionsOf(t *testing.T, args ...string) runOptions {
	t.Helper()
	o, code, done, stderr := runFlagsOf(args...)
	if done {
		t.Fatalf("run %v: ended with code %d: %s", args, code, stderr)
	}
	return o
}

// transportOf asks the node for its transport settings and returns them by name.
func transportOf(t *testing.T, dir string) map[string]api.TransportSetting {
	t.Helper()
	var tr api.Transport
	if err := json.Unmarshal([]byte(must(t, "transport", "-json", "-data", dir)), &tr); err != nil {
		t.Fatal(err)
	}
	out := map[string]api.TransportSetting{}
	for _, s := range tr.Settings {
		out[s.Name] = s
	}
	return out
}

// Two nodes on loopback, driven through the commands only. A session takes each
// setting from its rule, else from its peer, else from the node, at the moment it
// begins, and keeps it. Each node uses its own layers for its end of the same session.
// What the command line of a node gives cannot be set while the node runs. What was set
// while it ran holds after a restart.
func TestTransportSettingsThroughTheCommands(t *testing.T) {
	dirA, dirB := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	addrA := freeAddr(t)
	runFor(t, optionsOf(t, "-data", dirA, "-name", "alpha", "-listen", addrA, "-advertise", addrA, "-window", "64MB"))
	stopB := runFor(t, optionsOf(t, "-data", dirB, "-name", "beta"))
	_, _, stdout, _ := call("pair", "create", "-data", dirA)
	must(t, "pair", "join", strings.TrimSpace(stdout), "-data", dirB)

	dst, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	go func() {
		for {
			c, err := dst.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(io.Discard, c); c.Close() }()
		}
	}()
	in1, in2 := freeAddr(t), freeAddr(t)
	out := must(t, "rules", "add", "r1", "-data", dirB, "-peer", "alpha", "-listen", in1, "-dest", dst.Addr().String(), "-window", "16MB", "-hol", "0.5s")
	if !strings.Contains(out, "with window=16MB hol=500ms") {
		t.Fatalf("rules add with settings: %q", out)
	}
	must(t, "rules", "add", "r2", "-data", dirB, "-peer", "alpha", "-listen", in2, "-dest", dst.Addr().String())
	if list := must(t, "rules", "-data", dirB); !strings.Contains(list, "listening, with window=16MB hol=500ms") {
		t.Fatalf("the list of rules does not show the settings of r1:\n%s", list)
	}

	// open carries a connection through the rule that listens on in and returns the
	// window of the new session at each node, in MB; the connection stays open
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	seen := map[string]bool{}
	open := func(in, rule string) (atB, atA int64) {
		t.Helper()
		c, err := net.Dial("tcp", in)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		c.Write([]byte("hello"))
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			var b, a []api.Session
			json.Unmarshal([]byte(must(t, "sessions", "-json", "-data", dirB)), &b)
			json.Unmarshal([]byte(must(t, "sessions", "-json", "-data", dirA)), &a)
			for _, sb := range b {
				if seen[sb.ID] || sb.Rule != rule || !sb.Up {
					continue
				}
				for _, sa := range a {
					if sa.ID == sb.ID {
						seen[sb.ID] = true
						return sb.WindowBytes >> 20, sa.WindowBytes >> 20
					}
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("no new session of rule %s at both nodes", rule)
			}
		}
	}
	expect := func(what string, in, rule string, wantB, wantA int64) {
		t.Helper()
		if b, a := open(in, rule); b != wantB || a != wantA {
			t.Errorf("%s: a new session of %s has a window of %d MB at beta and %d MB at alpha, want %d and %d", what, rule, b, a, wantB, wantA)
		}
	}
	// the rule's setting at beta, where the rule is; alpha's command line at alpha
	expect("at the start", in1, "r1", 16, 64)
	expect("at the start", in2, "r2", 128, 64)

	// the node layer of beta, while it runs
	tr := transportOf(t, dirB)
	if s := tr["window"]; s.Value != "128MB" || s.Default != "128MB" || s.Source != "default" || s.Shared {
		t.Errorf("beta's window before any change: %+v", s)
	}
	if s := tr["budget"]; !s.Shared {
		t.Errorf("the budget is not marked as shared: %+v", s)
	}
	out = must(t, "transport", "set", "-window", "32MB", "-target", "0.5s", "-data", dirB)
	if !strings.Contains(out, "window is now 32MB (stored), and was 128MB") || !strings.Contains(out, "target is now 500ms (stored), and was 300ms") ||
		!strings.Contains(out, "keep what they began with") {
		t.Errorf("transport set: %q", out)
	}
	if table := must(t, "transport", "-data", dirB); !strings.Contains(table, "SETTING") || !strings.Contains(table, "32MB") {
		t.Errorf("the table of settings:\n%s", table)
	}
	expect("after the node's window was set", in2, "r2", 32, 64)
	expect("after the node's window was set", in1, "r1", 16, 64)

	// the peer layer of beta goes before its node layer, and the rule before both
	out = must(t, "peers", "set", "alpha", "-window", "8MB", "-share=false", "-data", dirB)
	if !strings.Contains(out, "sessions with alpha use, in the place of the node's settings: window=8MB share=false") {
		t.Errorf("peers set with settings: %q", out)
	}
	if show := must(t, "peers", "show", "alpha", "-data", dirB); !strings.Contains(show, "transport    window=8MB share=false") {
		t.Errorf("peers show does not list the settings:\n%s", show)
	}
	expect("after the peer's window was set at beta", in2, "r2", 8, 64)
	expect("after the peer's window was set at beta", in1, "r1", 16, 64)

	// alpha's own peer layer acts on alpha's end of the sessions that beta carries, and
	// goes before what alpha's command line gives
	must(t, "peers", "set", "beta", "-window", "4MB", "-data", dirA)
	expect("after the peer's window was set at alpha", in2, "r2", 8, 4)

	// the rule gives its window up: the peer's holds for it
	out = must(t, "rules", "set", "r1", "-window", "default", "-data", dirB)
	if !strings.Contains(out, "hol=500ms") || strings.Contains(out, "window") {
		t.Errorf("rules set -window default: %q", out)
	}
	expect("after the rule gave its window up", in1, "r1", 8, 4)
	if out = must(t, "rules", "set", "r1", "-hol", "default", "-data", dirB); !strings.Contains(out, "use the transport settings of the peer and the node") {
		t.Errorf("rules set with the last setting taken off: %q", out)
	}
	must(t, "peers", "set", "alpha", "-window", "default", "-share=default", "-data", dirB)
	expect("after the peer gave its settings up at beta", in2, "r2", 32, 4)

	// the sessions that began earlier still have what they began with
	var b []api.Session
	json.Unmarshal([]byte(must(t, "sessions", "-json", "-data", dirB)), &b)
	windows := map[int64]int{}
	for _, s := range b {
		windows[s.WindowBytes>>20]++
	}
	if want := map[int64]int{16: 3, 128: 1, 32: 2, 8: 3}; !reflect.DeepEqual(windows, want) {
		t.Errorf("the windows of beta's sessions, by MB: %v, want %v", windows, want)
	}

	// what cannot be set
	for _, c := range []struct {
		dir  string
		args []string
		want string
	}{
		{dirA, []string{"transport", "set", "-window", "32MB"}, "-window 64MB on its command line"},
		{dirB, []string{"transport", "set"}, "nothing to set"},
		{dirB, []string{"rules", "set", "r1"}, "nothing to set"},
		{dirB, []string{"rules", "set", "nosuch", "-window", "8MB"}, "no rule is called"},
		{dirB, []string{"peers", "set", "nobody", "-window", "8MB"}, "not found"},
	} {
		if code, _, _, stderr := call(append(c.args, "-data", c.dir)...); code != 1 || !strings.Contains(stderr, c.want) {
			t.Errorf("%v: code %d, %q, want code 1 and %q", c.args, code, stderr, c.want)
		}
	}
	for _, args := range [][]string{
		{"transport", "set", "-dead", "1s"},
		{"peers", "set", "alpha", "-budget", "1GB"},
		{"rules", "set", "r1", "-timeline", "1s"},
		{"rules", "add", "r3", "-peer", "alpha", "-listen", "127.0.0.1:1", "-dest", "192.0.2.1:1", "-target", "1h"},
	} {
		if code, _, _, stderr := call(append(args, "-data", dirB)...); code != 2 || stderr == "" {
			t.Errorf("%v: code %d, %q, want code 2 and a message", args, code, stderr)
		}
	}
	// a stored value of a setting that the command line gives can be taken off, and the
	// command line still decides
	if s := transportOf(t, dirA)["window"]; s.Value != "64MB" || s.Source != "command line" {
		t.Errorf("alpha's window: %+v", s)
	}

	// beta again, on the same directory: what was set while it ran is still there
	for _, c := range conns {
		c.Close()
	}
	conns = nil
	stopB()
	stopB = runFor(t, optionsOf(t, "-data", dirB))
	tr = transportOf(t, dirB)
	if s := tr["window"]; s.Value != "32MB" || s.Source != "stored" || s.Stored != "32MB" {
		t.Errorf("beta's window after a restart: %+v", s)
	}
	if s := tr["target"]; s.Value != "500ms" || s.Source != "stored" {
		t.Errorf("beta's target after a restart: %+v", s)
	}
	// and again with the window on the command line, which goes before the stored one
	stopB()
	runFor(t, optionsOf(t, "-data", dirB, "-window", "48MB"))
	tr = transportOf(t, dirB)
	if s := tr["window"]; s.Value != "48MB" || s.Source != "command line" || s.Stored != "32MB" {
		t.Errorf("beta's window with -window 48MB: %+v", s)
	}
	if code, _, _, stderr := call("transport", "set", "-window", "16MB", "-data", dirB); code != 1 || !strings.Contains(stderr, "-window 48MB on its command line") {
		t.Errorf("transport set of a setting the command line gives: code %d, %q", code, stderr)
	}
	out = must(t, "transport", "set", "-window", "default", "-target", "default", "-data", dirB)
	if !strings.Contains(out, "window stays 48MB, which the command line of the node gives") || !strings.Contains(out, "target is now 300ms (default), and was 500ms") {
		t.Errorf("transport set ... default: %q", out)
	}
	if s := transportOf(t, dirB)["window"]; s.Stored != "" {
		t.Errorf("the stored window was not taken off: %+v", s)
	}
}

// A node whose stored budget does not fit the memory limit of its command line does not
// start, and says which flag goes before the stored value.
func TestRunWithAStoredBudgetThatDoesNotFit(t *testing.T) {
	if !engine.RSSSupported {
		t.Skip("-maxrss is refused on this system")
	}
	dir := t.TempDir()
	// while a node runs, a budget that does not fit its memory limit is refused
	stop := runFor(t, optionsOf(t, "-data", dir, "-name", "alpha", "-maxrss", "700"))
	if code, _, _, stderr := call("transport", "set", "-budget", "1GB", "-data", dir); code != 1 || !strings.Contains(stderr, "-maxrss 700 MB is below") {
		t.Errorf("transport set -budget 1GB under -maxrss 700: code %d, %q", code, stderr)
	}
	if out := must(t, "transport", "set", "-budget", "300MB", "-data", dir); !strings.Contains(out, "budget is now 300MB (stored), and was 256MB") {
		t.Errorf("transport set -budget 300MB under -maxrss 700: %q", out)
	}
	stop()
	n, err := node.Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	gb := "1GB"
	if _, err := n.SetTransport(map[string]*string{"budget": &gb}, nil); err != nil {
		t.Fatal(err)
	}
	n.Close()
	o := optionsOf(t, "-data", dir, "-maxrss", "700")
	o.apiAddr = "127.0.0.1:0"
	if err := runNodeWith(context.Background(), o); err == nil || !strings.Contains(err.Error(), "budget of 1GB that is stored") || !strings.Contains(err.Error(), "-budget") {
		t.Fatalf("the node ended with %v, want an error about the stored budget", err)
	}
	// with the budget on the command line it starts, and the limit is checked when
	// the budget is set while it runs
	runFor(t, optionsOf(t, "-data", dir, "-maxrss", "700", "-budget", "256MB"))
	if s := transportOf(t, dir)["budget"]; s.Value != "256MB" || s.Source != "command line" || s.Stored != "1GB" {
		t.Errorf("the budget: %+v", s)
	}
}

// A node from before 0.7.0 answers the new requests in its own ways; the commands say
// what that means.
func TestTransportCommandsWithAnOlderNode(t *testing.T) {
	if err := olderNodeTransport(errors.New("no such path")); !strings.Contains(err.Error(), "older than this command") {
		t.Errorf("an unknown path: %v", err)
	}
	if err := olderNodeTransport(errors.New("something else")); err.Error() != "something else" {
		t.Errorf("another error was changed: %v", err)
	}
	refused := errors.New(`the request body is not the JSON this request takes: json: unknown field "transport"`)
	if err := olderNode(refused, map[string]string{"transport": "transport settings for a peer"}); !strings.Contains(err.Error(), "older than this command") {
		t.Errorf("an unknown field: %v", err)
	}
}
