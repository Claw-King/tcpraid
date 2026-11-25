package hook

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The test binary is the hook of these tests: started with HOOK_HELPER_OUT in its
// environment it is not a test run but a hook. It appends its TCPRAID_ variables and
// the line on its standard input to that file, sleeps for HOOK_HELPER_SLEEP if that is
// set, and ends with the status HOOK_HELPER_EXIT asks for. No shell and no other
// program is needed, on any system.
func TestMain(m *testing.M) {
	if out := os.Getenv("HOOK_HELPER_OUT"); out != "" {
		helper(out)
	}
	os.Exit(m.Run())
}

func helper(out string) {
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "TCPRAID_") {
			env = append(env, kv)
		}
	}
	rec, _ := json.Marshal(map[string]any{"env": env, "stdin": strings.TrimSuffix(line, "\n"), "args": os.Args[1:]})
	if f, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.Write(append(rec, '\n'))
		f.Close()
	}
	if d, err := time.ParseDuration(os.Getenv("HOOK_HELPER_SLEEP")); err == nil {
		time.Sleep(d)
	}
	if os.Getenv("HOOK_HELPER_EXIT") == "3" {
		os.Exit(3)
	}
	os.Exit(0)
}

type record struct {
	Env   []string `json:"env"`
	Stdin string   `json:"stdin"`
	Args  []string `json:"args"`
}

// records reads what the helper wrote so far. A line that is still being written is
// left for the next call.
func records(t *testing.T, file string) []record {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var out []record
	for _, line := range strings.Split(string(data), "\n") {
		var r record
		if line != "" && json.Unmarshal([]byte(line), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("it did not happen: %s", what)
		}
	}
}

func self(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// A run gets the event in its environment and as one line of JSON on its standard
// input, no arguments, and none of the node's own TCPRAID_ variables.
func TestRunGetsTheEvent(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("HOOK_HELPER_OUT", out)
	t.Setenv("TCPRAID_UI_PASSWORD", "the password of the page")
	var logged []string
	r, err := New(self(t), 0, func(f string, a ...any) { logged = append(logged, f) })
	if err != nil {
		t.Fatal(err)
	}
	tx, rx, clean := int64(1234), int64(0), true
	at := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	r.Emit(Event{Event: SessionEnd, Time: at, Peer: "nas-b", Rule: "backup", Session: "00ff", Role: "client", Destination: "192.0.2.7:873",
		TxBytes: &tx, RxBytes: &rx, Clean: &clean, Reason: "done; rm -rf $(x)\nsecond line"})
	eventually(t, "the hook ran", func() bool { return len(records(t, out)) == 1 })
	got := records(t, out)[0]
	if len(got.Args) != 0 {
		t.Errorf("the hook got arguments: %q", got.Args)
	}
	want := []string{
		"TCPRAID_EVENT=session_end", "TCPRAID_TIME=2026-01-15T12:00:00Z", "TCPRAID_PEER=nas-b", "TCPRAID_RULE=backup", "TCPRAID_SESSION=00ff",
		"TCPRAID_ROLE=client", "TCPRAID_DESTINATION=192.0.2.7:873", "TCPRAID_TX_BYTES=1234", "TCPRAID_RX_BYTES=0", "TCPRAID_CLEAN=true",
		"TCPRAID_REASON=done; rm -rf $(x) second line",
	}
	env := strings.Join(got.Env, "\n")
	for _, w := range want {
		if !strings.Contains("\n"+env+"\n", "\n"+w+"\n") {
			t.Errorf("the environment lacks %q:\n%s", w, env)
		}
	}
	if len(got.Env) != len(want) {
		t.Errorf("the environment has %d TCPRAID_ variables, want %d:\n%s", len(got.Env), len(want), env)
	}
	if strings.Contains(env, "TCPRAID_UI_PASSWORD") || strings.Contains(env+got.Stdin, "the password of the page") {
		t.Error("the hook got a variable of the node's own environment that begins with TCPRAID_")
	}
	var e Event
	if err := json.Unmarshal([]byte(got.Stdin), &e); err != nil {
		t.Fatalf("the line on the standard input is not JSON: %q: %v", got.Stdin, err)
	}
	if e.Event != SessionEnd || e.Peer != "nas-b" || e.Reason != "done; rm -rf $(x)\nsecond line" || e.TxBytes == nil || *e.TxBytes != 1234 || e.Clean == nil || !*e.Clean {
		t.Errorf("the event on the standard input: %+v", e)
	}
	// an event that has fewer fields sets fewer variables
	r.Emit(Event{Event: PeerOnline, Peer: "nas-b"})
	eventually(t, "the hook ran again", func() bool { return len(records(t, out)) == 2 })
	if got := records(t, out)[1]; len(got.Env) != 3 {
		t.Errorf("a peer_online event sets %q, want the event, the time and the peer", got.Env)
	}
	if runs, dropped, failed, timeouts := r.Counts(); runs != 2 || dropped != 0 || failed != 0 || timeouts != 0 {
		t.Errorf("counts %d %d %d %d, want 2 0 0 0", runs, dropped, failed, timeouts)
	}
}

// The command is one program: a line with a space in it is not split and not given to
// a shell. A command that is not there is an error when the Runner is made.
func TestCommandIsNotAShellLine(t *testing.T) {
	for _, bad := range []string{"", filepath.Join(t.TempDir(), "nothing-here"), self(t) + " -an-argument", "echo hi; true"} {
		if r, err := New(bad, 0, nil); err == nil {
			t.Errorf("New(%q) made a Runner for %q", bad, r.Path())
		}
	}
}

// At most MaxRunning runs go on at one time. An event that comes while that many run is
// dropped and counted, and Emit returns at once all the same. A run that lasts longer
// than the timeout is ended, and one that fails is counted.
func TestRunsAreBoundedAndEnded(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("HOOK_HELPER_OUT", out)
	t.Setenv("HOOK_HELPER_SLEEP", "1m")
	r, err := New(self(t), 2*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	for i := 0; i < MaxRunning+6; i++ {
		r.Emit(Event{Event: PeerOnline, Peer: "p"})
	}
	if d := time.Since(began); d > time.Second {
		t.Errorf("Emit took %v with %d runs going on", d, MaxRunning)
	}
	if runs, dropped, _, _ := r.Counts(); runs != MaxRunning || dropped != 6 {
		t.Errorf("%d runs and %d dropped, want %d and 6", runs, dropped, MaxRunning)
	}
	r.Wait(30 * time.Second)
	if _, _, failed, timeouts := r.Counts(); timeouts != MaxRunning || failed != MaxRunning {
		t.Errorf("%d runs were ended for lasting too long and %d failed, want %d of each", timeouts, failed, MaxRunning)
	}
	if d := time.Since(began); d > 20*time.Second {
		t.Errorf("the runs were ended after %v, with a timeout of 2s", d)
	}
	// the places are free again, and a run that returns another status than 0 is counted
	t.Setenv("HOOK_HELPER_SLEEP", "")
	t.Setenv("HOOK_HELPER_EXIT", "3")
	r.Emit(Event{Event: PeerOffline, Peer: "p"})
	r.Wait(30 * time.Second)
	if runs, _, failed, _ := r.Counts(); runs != MaxRunning+1 || failed != MaxRunning+1 {
		t.Errorf("after one more run that returns 3: %d runs, %d failed", runs, failed)
	}
}
