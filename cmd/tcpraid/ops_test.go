package main

// Tests of the config commands, the metrics listener, the hook and package client, each
// against nodes that run in this process.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Claw-King/tcpraid/client"
	"github.com/Claw-King/tcpraid/internal/api"
	"github.com/Claw-King/tcpraid/internal/node"
)

// The test binary is the hook of these tests: started with HOOK_HELPER_OUT in its
// environment it is not a test run but a hook. It appends its TCPRAID_ variables and
// the line on its standard input to that file and sleeps for HOOK_HELPER_SLEEP if that
// is set. No shell and no other program is needed, on any system.
func TestMain(m *testing.M) {
	if out := os.Getenv("HOOK_HELPER_OUT"); out != "" {
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		var env []string
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, "TCPRAID_") {
				env = append(env, kv)
			}
		}
		rec, _ := json.Marshal(map[string]any{"env": env, "stdin": strings.TrimSuffix(line, "\n")})
		if f, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			f.Write(append(rec, '\n'))
			f.Close()
		}
		if d, err := time.ParseDuration(os.Getenv("HOOK_HELPER_SLEEP")); err == nil {
			time.Sleep(d)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// hookEvents reads the events the helper has written to file so far, each with the
// variables it got.
func hookEvents(file string) (events []map[string]any, envs [][]string) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		var rec struct {
			Env   []string `json:"env"`
			Stdin string   `json:"stdin"`
		}
		var e map[string]any
		if line == "" || json.Unmarshal([]byte(line), &rec) != nil || json.Unmarshal([]byte(rec.Stdin), &e) != nil {
			continue // a line that is still being written
		}
		events, envs = append(events, e), append(envs, rec.Env)
	}
	return events, envs
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("it did not happen: %s", what)
		}
	}
}

// secretsOf returns what a data directory holds that must leave it nowhere: the token,
// the lines of the private keys, the pair secrets and the hashes of the pairing codes
// as the state file has them.
func secretsOf(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	token, err := node.ReadToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, token)
	for _, name := range []string{node.KeyFile, node.APIKeyFile} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue // a node without TLS on its API has no key for it
		}
		if blk, _ := pem.Decode(data); blk != nil {
			for _, line := range strings.Split(string(pem.EncodeToMemory(blk)), "\n") {
				if len(line) >= 24 && !strings.HasPrefix(line, "-----") {
					out = append(out, line)
				}
			}
		}
	}
	// the hash of the password of the web page, where the node has one
	if hash, err := os.ReadFile(filepath.Join(dir, node.UIPasswordFile)); err == nil && len(strings.TrimSpace(string(hash))) >= 16 {
		out = append(out, strings.TrimSpace(string(hash)))
	}
	var st struct {
		Peers []struct {
			PairSecret string `json:"pair_secret"`
		} `json:"peers"`
		PairingCodes []struct {
			SecretHash string `json:"secret_hash"`
		} `json:"pairing_codes"`
	}
	data, err := os.ReadFile(filepath.Join(dir, node.StateFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	for _, p := range st.Peers {
		if p.PairSecret == "" {
			t.Fatal("the state file has a peer without a pair secret: the test reads the wrong field")
		}
		out = append(out, p.PairSecret)
	}
	for _, c := range st.PairingCodes {
		out = append(out, c.SecretHash)
	}
	if len(out) < 3 {
		t.Fatalf("only %d secrets found in %s: the test does not see what it should", len(out), dir)
	}
	return out
}

func holdsNone(t *testing.T, what, text string, secrets []string) {
	t.Helper()
	for _, s := range secrets {
		if s != "" && strings.Contains(text, s) {
			t.Errorf("%s holds a secret of the data directory (%.12s...)", what, s)
		}
	}
}

// The configuration of a node goes out as a file without a secret, and a file makes a
// running node match it: what differs is changed, nothing else, no peer is paired or
// removed, and rules that the file lacks go only with -prune.
func TestConfigExportAndApply(t *testing.T) {
	dirA, dirB, dirC := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b"), filepath.Join(t.TempDir(), "c")
	addrA, addrC := freeAddr(t), freeAddr(t)
	runFor(t, optionsOf(t, "-data", dirA, "-name", "alpha", "-listen", addrA, "-advertise", addrA))
	runFor(t, optionsOf(t, "-data", dirC, "-name", "gamma", "-listen", addrC, "-advertise", addrC))
	// beta gives its window on the command line: a file cannot set that one
	runFor(t, optionsOf(t, "-data", dirB, "-name", "beta", "-window", "64MB"))
	for _, dir := range []string{dirA, dirC} {
		_, _, code, _ := call("pair", "create", "-data", dir)
		must(t, "pair", "join", strings.TrimSpace(code), "-data", dirB)
	}
	// a code that is still open at alpha, for the search for secrets there
	must(t, "pair", "create", "-data", dirA)
	in1, in2 := freeAddr(t), freeAddr(t)
	must(t, "peers", "set", "alpha", "-allow", "192.0.2.0/24:873", "-window", "8MB", "-data", dirB)
	must(t, "peers", "path", "set", "alpha", "default", "-addr", addrA+",192.0.2.50:9851", "-subs", "4", "-data", dirB)
	must(t, "peers", "path", "set", "gamma", "byproxy", "-type", "socks5", "-proxy", "socks5://someone:pass-of-the-proxy@127.0.0.1:1", "-addr", "192.0.2.9:9851", "-roles", "repair,control", "-subs", "2", "-data", dirB)
	must(t, "rules", "add", "zfs", "-peer", "alpha", "-listen", in1, "-dest", "192.0.2.1:873", "-paths", "default", "-subs", "3", "-dial", "self", "-window", "16MB", "-data", dirB)
	must(t, "rules", "add", "web", "-peer", "gamma", "-socks", in2, "-data", dirB)
	must(t, "rules", "disable", "web", "-data", dirB)
	must(t, "transport", "set", "-hol", "0.5s", "-data", dirB)

	exported := must(t, "config", "export", "-data", dirB)
	for _, want := range []string{"version: 1", "name: beta", "name: alpha", "name: gamma", "allow: 192.0.2.0/24:873", "window: 8MB", "192.0.2.50:9851",
		"proxy: socks5://127.0.0.1:1", "proxy_login: " + proxyLoginKept, "name: zfs", "dial: self", "window: 16MB", "socks: " + in2, "enabled: false", "hol: 500ms"} {
		if !strings.Contains(exported, want) {
			t.Errorf("the exported file lacks %q:\n%s", want, exported)
		}
	}
	holdsNone(t, "the exported file of beta", exported, append(secretsOf(t, dirB), "pass-of-the-proxy", "someone"))
	holdsNone(t, "the exported file of alpha", must(t, "config", "export", "-data", dirA), secretsOf(t, dirA))

	file := filepath.Join(t.TempDir(), "node.yaml")
	write := func(f configFile) {
		t.Helper()
		data, err := yaml.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	nodeNow := func() configFile {
		t.Helper()
		st, err := readNodeState(client.New(dirB))
		if err != nil {
			t.Fatal(err)
		}
		return st.file
	}
	// the file as exported, applied to the node it came from: nothing to do
	if err := os.WriteFile(file, []byte(exported), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-dry-run"}, {}, {"-prune"}} {
		out := must(t, append([]string{"config", "apply", file, "-data", dirB}, args...)...)
		if !strings.Contains(out, "nothing to change") || strings.Contains(out, "changed:") {
			t.Fatalf("apply %v of the node's own export:\n%s", args, out)
		}
	}
	before := nodeNow()

	// a file that differs: a rule changed, one added, a peer's settings, a setting of the node
	want, err := parseConfig([]byte(exported))
	if err != nil {
		t.Fatal(err)
	}
	for i := range want.Rules {
		if want.Rules[i].Name == "zfs" {
			want.Rules[i].Destination = "192.0.2.1:874"
			want.Rules[i].Transport = map[string]string{"target": "500ms"}
			want.Rules[i].Dial = "auto"
		}
	}
	want.Rules = append(want.Rules, configRule{Name: "added", Peer: "alpha", Listen: freeAddr(t), Destination: "db.example:5432"})
	for i := range want.Peers {
		if want.Peers[i].Name == "alpha" {
			want.Peers[i].Allow = "192.0.2.0/24:873-874"
			want.Peers[i].Transport = nil
			want.Peers[i].Paths[0].Subs = 6
		}
	}
	want.Node.Transport = map[string]string{"hol": "250ms", "dead": "30s"}
	write(want)
	out := must(t, "config", "apply", file, "-dry-run", "-data", dirB)
	for _, line := range []string{`would change: node: transport settings dead=30s hol=250ms`, `would change: peer "alpha": paths, allow list, transport settings`, `would change: rule "added": added`, `would change: rule "zfs": changed`} {
		if !strings.Contains(out, line) {
			t.Errorf("the dry run lacks %q:\n%s", line, out)
		}
	}
	if got := nodeNow(); !sameConfig(got, before) {
		t.Fatal("a dry run changed the node")
	}
	out = must(t, "config", "apply", file, "-data", dirB)
	if strings.Count(out, "changed: ") != 4 {
		t.Errorf("apply made other changes than the four the file asks for:\n%s", out)
	}
	if got := nodeNow(); !sameConfig(got, normalConfig(want)) {
		g, _ := yaml.Marshal(got)
		w, _ := yaml.Marshal(normalConfig(want))
		t.Fatalf("after apply the node is not as the file describes it.\nnode:\n%s\nfile:\n%s", g, w)
	}
	// the proxy kept its user and password, which the file never had
	state, _ := os.ReadFile(filepath.Join(dirB, node.StateFile))
	if !strings.Contains(string(state), "pass-of-the-proxy") {
		t.Error("apply took the password of a proxy away")
	}
	if out := must(t, "config", "apply", file, "-data", dirB); !strings.Contains(out, "nothing to change") {
		t.Errorf("the same file applied again:\n%s", out)
	}

	// a rule that the file does not have stays, and goes with -prune; so does a stored setting
	var kept []configRule
	for _, r := range want.Rules {
		if r.Name != "added" {
			kept = append(kept, r)
		}
	}
	want.Rules = kept
	want.Node.Transport = map[string]string{"hol": "250ms"}
	write(want)
	out = must(t, "config", "apply", file, "-data", dirB)
	if !strings.Contains(out, "The node keeps 1 rule that the file does not have") || len(nodeNow().Rules) != 3 || nodeNow().Node.Transport["dead"] != "30s" {
		t.Fatalf("apply without -prune:\n%s", out)
	}
	out = must(t, "config", "apply", file, "-prune", "-data", dirB)
	if !strings.Contains(out, `changed: rule "added": removed`) || !strings.Contains(out, "dead=default") || !sameConfig(nodeNow(), normalConfig(want)) {
		t.Fatalf("apply with -prune:\n%s", out)
	}

	// with -prune a rule of the file can take the address of a rule that goes
	moved := want
	moved.Rules = append([]configRule{}, want.Rules...)
	for i := range moved.Rules {
		if moved.Rules[i].Name == "zfs" {
			moved.Rules[i].Name = "zfs-moved"
		}
	}
	write(moved)
	out = must(t, "config", "apply", file, "-prune", "-data", dirB)
	gone, came := strings.Index(out, `changed: rule "zfs": removed`), strings.Index(out, `changed: rule "zfs-moved": added`)
	if gone < 0 || came < gone || !sameConfig(nodeNow(), normalConfig(moved)) {
		t.Fatalf("a rule of the file that takes the address of one that goes:\n%s", out)
	}
	write(want)
	must(t, "config", "apply", file, "-prune", "-data", dirB)
	if !sameConfig(nodeNow(), normalConfig(want)) {
		t.Fatal("the node did not come back to the file after the address was moved back")
	}

	// a peer that is not paired, and one with another fingerprint: listed, and no peer is touched
	stranger := want
	stranger.Peers = append(append([]configPeer{}, want.Peers...), configPeer{Name: "delta", Fingerprint: strings.Repeat("ab", 32)})
	for i := range stranger.Peers {
		if stranger.Peers[i].Name == "gamma" {
			stranger.Peers[i].Fingerprint = strings.Repeat("cd", 32)
			stranger.Peers[i].Allow = "198.51.100.0/24"
		}
	}
	// a rule of the file for the peer that is another node there: it is left out, and
	// is not given to the node's own peer of that name
	stranger.Rules = append(append([]configRule{}, want.Rules...), configRule{Name: "astray", Peer: "gamma", Listen: freeAddr(t), Destination: "192.0.2.9:22"})
	write(stranger)
	code, _, stdout, stderr := call("config", "apply", file, "-data", dirB)
	if !strings.Contains(stdout, `rule "astray" is left out`) || strings.Contains(stdout, `rule "astray": added`) {
		t.Errorf("a rule for a peer that is another node in the file: code %d\n%s\n%s", code, stdout, stderr)
	}
	stranger.Rules = want.Rules
	write(stranger)
	code, _, stdout, stderr = call("config", "apply", file, "-prune", "-data", dirB)
	if code != 1 || !strings.Contains(stdout, `peer "delta"`) || !strings.Contains(stdout, `peer "gamma"`) || !strings.Contains(stdout, "Apply does not pair") || !strings.Contains(stderr, "because it has 2 peers that this node is not paired with") {
		t.Errorf("a file with peers that are not paired: code %d\n%s\n%s", code, stdout, stderr)
	}
	if got := nodeNow(); len(got.Peers) != 2 || !sameConfig(got, normalConfig(want)) {
		t.Error("a file with peers that are not paired changed the peers of the node")
	}

	// The name. A file of another node, and one that does not say whose it is, leave
	// the name of this node alone and say so. The node's own file gives it the name.
	for _, fp := range []string{"", strings.Repeat("ef", 32)} {
		other := want
		other.Node.Name, other.Node.Fingerprint = "elsewhere", fp
		write(other)
		out := must(t, "config", "apply", file, "-data", dirB)
		if !strings.Contains(out, "The name is left as it is") || strings.Contains(out, "becomes") || nodeNow().Node.Name != want.Node.Name {
			t.Errorf("a file of another node (fingerprint %q) and the name:\n%s", fp, out)
		}
	}
	own := want
	own.Node.Name = "renamed"
	write(own)
	if out := must(t, "config", "apply", file, "-data", dirB); !strings.Contains(out, `becomes "renamed"`) || nodeNow().Node.Name != "renamed" {
		t.Errorf("the node's own file with another name:\n%s", out)
	}
	write(want)
	must(t, "config", "apply", file, "-data", dirB)
	// a rule that does not say whether it is enabled is enabled
	text, _ := yaml.Marshal(want)
	plain := strings.ReplaceAll(string(text), "      enabled: true\n", "")
	if plain == string(text) || strings.Contains(plain, "enabled: true") {
		t.Fatalf("the test did not take the enabled lines out:\n%s", text)
	}
	if err := os.WriteFile(file, []byte(plain), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := must(t, "config", "apply", file, "-data", dirB); !strings.Contains(out, "nothing to change") {
		t.Errorf("the same file without its enabled lines:\n%s", out)
	}
	write(want)

	// a setting that the command line of the node gives: refused before anything is changed
	pinned := want
	pinned.Node.Transport = map[string]string{"hol": "250ms", "window": "32MB"}
	pinned.Rules = nil
	write(pinned)
	code, _, stdout, stderr = call("config", "apply", file, "-prune", "-data", dirB)
	if code != 1 || !strings.Contains(stderr, "started with -window on its command line") || !strings.Contains(stderr, "Nothing was changed") || strings.Contains(stdout, "changed:") || len(nodeNow().Rules) != 2 {
		t.Errorf("a file that sets what the command line gives: code %d\n%s\n%s", code, stdout, stderr)
	}

	// files that are not read: a field nobody knows, another version, two documents
	good, _ := yaml.Marshal(want)
	for name, text := range map[string]string{
		"an unknown field":           string(good) + "surprise: 1\n",
		"an unknown field in a rule": strings.Replace(string(good), "enabled:", "enabeld:", 1),
		"another version":            strings.Replace(string(good), "version: 1", "version: 2", 1),
		"two documents":              string(good) + "---\n" + string(good),
		"an empty file":              "",
		"a note of export changed":   strings.Replace(exported, proxyLoginKept, "user:password", 1),
	} {
		if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if code, _, stdout, stderr := call("config", "apply", file, "-data", dirB); code != 1 || strings.Contains(stdout, "changed:") || stderr == "" {
			t.Errorf("%s: code %d\n%s\n%s", name, code, stdout, stderr)
		}
	}
	if !sameConfig(nodeNow(), normalConfig(want)) {
		t.Error("a file that is not read changed the node")
	}
}

func sameConfig(a, b configFile) bool {
	x, _ := yaml.Marshal(a)
	y, _ := yaml.Marshal(b)
	return bytes.Equal(x, y)
}

// checkMetricsText holds text against the Prometheus text format as its documentation
// describes it: every line ends with a line feed; a line is a comment, a TYPE line with
// a name and one of the five types, or a sample "name value" or "name{label="value"}
// value" whose value Go's ParseFloat reads; the TYPE line of a name comes before its
// first sample and only once; the samples of a name are one group; a name with its
// labels appears once; and a label value has the backslash, the double quote and the
// line feed only in their escaped forms. It returns the samples by "name{labels}".
func checkMetricsText(t *testing.T, text string) map[string]float64 {
	t.Helper()
	if text == "" || !strings.HasSuffix(text, "\n") {
		t.Fatalf("the text does not end with a line feed: %q", text)
	}
	okName := func(s string) bool {
		for i, c := range s {
			if !(c >= 'a' && c <= 'z' || c == '_' || i > 0 && c >= '0' && c <= '9') {
				return false
			}
		}
		return s != ""
	}
	samples := map[string]float64{}
	typed, closed := map[string]bool{}, map[string]bool{}
	last := ""
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			f := strings.Fields(line)
			if len(f) >= 2 && f[1] == "TYPE" {
				if len(f) != 4 || !okName(f[2]) || !strings.Contains(" counter gauge histogram summary untyped ", " "+f[3]+" ") {
					t.Errorf("a TYPE line that is not a name and a type: %q", line)
					continue
				}
				if typed[f[2]] || closed[f[2]] {
					t.Errorf("a second TYPE line for %s, or one after its samples", f[2])
				}
				typed[f[2]] = true
			}
			continue
		}
		head, value, ok := strings.Cut(line, " ")
		name, labels := head, ""
		if i := strings.IndexByte(line, '{'); i >= 0 {
			j := strings.LastIndex(line, "} ")
			if j < i {
				t.Errorf("a sample with labels that do not close: %q", line)
				continue
			}
			name, labels, value, ok = line[:i], line[i+1:j], line[j+2:], true
			label, v, found := strings.Cut(labels, "=")
			if !found || !okName(label) || len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
				t.Errorf("a label that is not name=\"value\": %q", line)
				continue
			}
			inner := v[1 : len(v)-1]
			for k := 0; k < len(inner); k++ {
				switch inner[k] {
				case '\\':
					if k+1 >= len(inner) || !strings.ContainsRune(`\"n`, rune(inner[k+1])) {
						t.Errorf("a backslash that escapes nothing in %q", line)
					}
					k++
				case '"', '\n':
					t.Errorf("a label value with a character that is not escaped: %q", line)
				}
			}
		}
		if !ok || !okName(name) {
			t.Errorf("a line that is no sample: %q", line)
			continue
		}
		v, err := strconv.ParseFloat(strings.Fields(value)[0], 64)
		if err != nil {
			t.Errorf("a value that ParseFloat does not read: %q", line)
		}
		if !typed[name] {
			t.Errorf("a sample of %s before its TYPE line", name)
		}
		if name != last {
			if closed[name] {
				t.Errorf("the samples of %s are not one group", name)
			}
			if last != "" {
				closed[last] = true
			}
			last = name
		}
		key := name
		if labels != "" {
			key += "{" + labels + "}"
		}
		if _, dup := samples[key]; dup {
			t.Errorf("%s appears twice", key)
		}
		samples[key] = v
	}
	return samples
}

func httpGet(t *testing.T, method, url, token string) (int, string, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header.Get("Content-Type")
}

// sinkAt listens on loopback, reads every connection to its end and reports the bytes
// of each.
func sinkAt(t *testing.T) (addr string, got chan int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan int64, 64)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				n, _ := io.Copy(io.Discard, c)
				c.Close()
				got <- n
			}()
		}
	}()
	return ln.Addr().String(), got
}

// sendThrough sends n bytes to addr and closes; a connection that is reset before the
// rule's peer has answered is tried again.
func sendThrough(t *testing.T, addr string, n int, got chan int64) {
	t.Helper()
	data := make([]byte, n)
	rand.Read(data)
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			_, err = c.Write(data)
			if err == nil {
				c.(*net.TCPConn).CloseWrite()
				_, err = io.Copy(io.Discard, c)
			}
			c.Close()
		}
		if err == nil {
			select {
			case arrived := <-got:
				if arrived == int64(n) {
					return
				}
			case <-time.After(20 * time.Second):
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d bytes did not get through %s: %v", n, addr, err)
		}
	}
}

// A node with -metrics serves its figures on that address without a token, in the
// Prometheus text format and as JSON, and nothing else. The figures move with the
// traffic, name peers and rules, and hold no address and no secret. The API itself
// still asks for the token. A node with -hook runs the command for each event, with
// the event in the environment and on the standard input.
func TestMetricsListenerAndHook(t *testing.T) {
	hookOut := filepath.Join(t.TempDir(), "hook.out")
	t.Setenv("HOOK_HELPER_OUT", hookOut)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dirA, dirB := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	addrA := freeAddr(t)
	stopA := runFor(t, optionsOf(t, "-data", dirA, "-name", "alpha", "-listen", addrA, "-advertise", addrA))
	// the node takes a free port for its figures and says in its status which
	runFor(t, optionsOf(t, "-data", dirB, "-name", "beta", "-metrics", "127.0.0.1:0", "-hook", exe, "-hook-timeout", "20s"))
	var metricsAddr string
	waitFor(t, "the node says where its figures are served", func() bool {
		st, err := client.New(dirB).Status(context.Background())
		if err != nil {
			return false
		}
		metricsAddr = st.Metrics
		return metricsAddr != ""
	})
	_, _, code, _ := call("pair", "create", "-data", dirA)
	must(t, "pair", "join", strings.TrimSpace(code), "-data", dirB)
	dst, got := sinkAt(t)
	in, inBad := freeAddr(t), freeAddr(t)
	must(t, "rules", "add", "carry", "-peer", "alpha", "-listen", in, "-dest", dst, "-data", dirB)

	metrics := func() map[string]float64 {
		t.Helper()
		status, body, ctype := httpGet(t, "GET", "http://"+metricsAddr+"/metrics", "")
		if status != 200 || !strings.HasPrefix(ctype, "text/plain") || !strings.Contains(ctype, "version=0.0.4") {
			t.Fatalf("GET /metrics without a token: %d %q", status, ctype)
		}
		return checkMetricsText(t, body)
	}
	waitFor(t, "the peer shows as online in the figures", func() bool { return metrics()[`tcpraid_peer_online{peer="alpha"}`] == 1 })
	first := metrics()
	sendThrough(t, in, 1<<20, got)
	waitFor(t, "the session has ended", func() bool {
		return metrics()[`tcpraid_rule_sessions{rule="carry"}`] == 0 && metrics()["tcpraid_sessions_active"] == 0
	})
	second := metrics()
	if second["tcpraid_bytes_tx"]-first["tcpraid_bytes_tx"] < 1<<20 || second["tcpraid_sessions_total"] <= first["tcpraid_sessions_total"] {
		t.Errorf("the figures did not move with a transfer of 1 MiB: bytes_tx %v -> %v, sessions_total %v -> %v",
			first["tcpraid_bytes_tx"], second["tcpraid_bytes_tx"], first["tcpraid_sessions_total"], second["tcpraid_sessions_total"])
	}
	for _, name := range []string{`tcpraid_rule_listening{rule="carry"}`, "tcpraid_runtime_memory_mb", "tcpraid_hook_runs", "tcpraid_hook_events_dropped", "tcpraid_subs_up"} {
		if _, ok := second[name]; !ok {
			t.Errorf("the figures lack %s", name)
		}
	}
	if second[`tcpraid_rule_listening{rule="carry"}`] != 1 {
		t.Error("the rule does not show as listening")
	}
	// no address, no destination, no secret: in neither form
	_, text, _ := httpGet(t, "GET", "http://"+metricsAddr+"/metrics", "")
	status, asJSON, ctype := httpGet(t, "GET", "http://"+metricsAddr+"/metrics.json", "")
	var decoded struct {
		Metrics []api.MetricsSample `json:"metrics"`
	}
	if status != 200 || ctype != "application/json" || json.Unmarshal([]byte(asJSON), &decoded) != nil || len(decoded.Metrics) != len(second) {
		t.Errorf("GET /metrics.json: %d %q, %d samples for %d of the text", status, ctype, len(decoded.Metrics), len(second))
	}
	apiAddr, _ := node.ReadAPIAddr(dirB)
	for _, both := range []string{text, asJSON} {
		holdsNone(t, "the figures", both, secretsOf(t, dirB))
		for _, addr := range []string{addrA, dst, in, apiAddr, "127.0.0.1"} {
			if strings.Contains(both, addr) {
				t.Errorf("the figures hold the address %s", addr)
			}
		}
	}
	// read-only, two paths, and the API is not served here
	for _, c := range []struct {
		method, path string
		want         int
	}{{"POST", "/metrics", 405}, {"DELETE", "/metrics.json", 405}, {"GET", "/", 404}, {"GET", "/api/v1/status", 404}, {"GET", "/metrics/x", 404}} {
		if status, _, _ := httpGet(t, c.method, "http://"+metricsAddr+c.path, ""); status != c.want {
			t.Errorf("%s %s on the metrics address: %d, want %d", c.method, c.path, status, c.want)
		}
	}
	// the API still wants the token for everything, its own /metrics included
	for _, path := range []string{api.PathStatus, api.PathMetrics, api.PathRules} {
		if status, _, _ := httpGet(t, "GET", "http://"+apiAddr+path, ""); status != 401 {
			t.Errorf("GET %s of the API without a token: %d, want 401", path, status)
		}
	}
	token, _ := node.ReadToken(dirB)
	if status, body, _ := httpGet(t, "GET", "http://"+apiAddr+api.PathMetrics, token); status != 200 || checkMetricsText(t, body)[`tcpraid_peer_online{peer="alpha"}`] != 1 {
		t.Errorf("GET /metrics of the API with the token: %d", status)
	}

	// the hook: one run for each event, with its fields
	find := func(kind string, match func(e map[string]any) bool) (map[string]any, []string) {
		t.Helper()
		var found map[string]any
		var env []string
		waitFor(t, "the hook was run for "+kind, func() bool {
			events, envs := hookEvents(hookOut)
			for i, e := range events {
				if e["event"] == kind && (match == nil || match(e)) {
					found, env = e, envs[i]
					return true
				}
			}
			return false
		})
		return found, env
	}
	if e, env := find("peer_online", nil); e["peer"] != "alpha" || !strings.Contains(strings.Join(env, "\n"), "TCPRAID_PEER=alpha") {
		t.Errorf("peer_online: %v %v", e, env)
	}
	start, _ := find("session_start", func(e map[string]any) bool { return e["rule"] == "carry" })
	if start["peer"] != "alpha" || start["role"] != "client" || start["destination"] != dst || start["session"] == "" {
		t.Errorf("session_start: %v", start)
	}
	end, env := find("session_end", func(e map[string]any) bool { return e["rule"] == "carry" && e["clean"] == true })
	if end["session"] != start["session"] || end["tx_bytes"] != float64(1<<20) || end["rx_bytes"] != float64(0) || end["reason"] == "" || end["destination"] != dst {
		t.Errorf("session_end: %v", end)
	}
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for _, want := range []string{"TCPRAID_EVENT=session_end", "TCPRAID_RULE=carry", "TCPRAID_PEER=alpha", "TCPRAID_ROLE=client", "TCPRAID_DESTINATION=" + dst, "TCPRAID_TX_BYTES=1048576", "TCPRAID_RX_BYTES=0", "TCPRAID_CLEAN=true"} {
		if !strings.Contains(joined, "\n"+want+"\n") {
			t.Errorf("the environment of session_end lacks %q:%s", want, joined)
		}
	}
	// a connection of a rule that fails: nothing listens at the destination
	closed := freeAddr(t)
	must(t, "rules", "add", "nowhere", "-peer", "alpha", "-listen", inBad, "-dest", closed, "-data", dirB)
	waitFor(t, "a connection of the rule failed", func() bool {
		if c, err := net.Dial("tcp", inBad); err == nil {
			c.Write([]byte("x"))
			io.Copy(io.Discard, c)
			c.Close()
		}
		events, _ := hookEvents(hookOut)
		for _, e := range events {
			if e["event"] == "rule_failure" && e["rule"] == "nowhere" {
				return true
			}
		}
		return false
	})
	if e, _ := find("rule_failure", func(e map[string]any) bool { return e["rule"] == "nowhere" }); e["reason"] == "" || e["reason"] == nil {
		t.Errorf("rule_failure without a reason: %v", e)
	}
	// An event that comes while as many runs go on as may go on is dropped, and the
	// failed connections above were many events: wait until no run goes on.
	waitFor(t, "no run of the hook goes on", func() bool { return metrics()["tcpraid_hook_running"] == 0 })
	stopA()
	if e, _ := find("peer_offline", nil); e["peer"] != "alpha" {
		t.Errorf("peer_offline: %v", e)
	}
	// what the hook was given holds no secret either
	all, _ := os.ReadFile(hookOut)
	holdsNone(t, "what the hook was given", string(all), secretsOf(t, dirB))
}

// A hook that does not return does not hold the node up: data goes through while runs
// of it go on, runs are ended when their time is over, and events beyond the runs that
// may go on at one time are dropped and counted.
func TestHookThatHangs(t *testing.T) {
	t.Setenv("HOOK_HELPER_OUT", filepath.Join(t.TempDir(), "hook.out"))
	t.Setenv("HOOK_HELPER_SLEEP", "10m")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dirA, dirB := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	addrA := freeAddr(t)
	runFor(t, optionsOf(t, "-data", dirA, "-name", "alpha", "-listen", addrA, "-advertise", addrA))
	runFor(t, optionsOf(t, "-data", dirB, "-name", "beta", "-hook", exe, "-hook-timeout", "3s"))
	_, _, code, _ := call("pair", "create", "-data", dirA)
	must(t, "pair", "join", strings.TrimSpace(code), "-data", dirB)
	dst, got := sinkAt(t)
	in := freeAddr(t)
	must(t, "rules", "add", "carry", "-peer", "alpha", "-listen", in, "-dest", dst, "-data", dirB)
	figures := func() map[string]float64 {
		t.Helper()
		token, _ := node.ReadToken(dirB)
		apiAddr, _ := node.ReadAPIAddr(dirB)
		_, body, _ := httpGet(t, "GET", "http://"+apiAddr+api.PathMetrics, token)
		return checkMetricsText(t, body)
	}
	// every transfer is two events; ten of them while the runs hang
	for i := 0; i < 10; i++ {
		sendThrough(t, in, 256<<10, got)
	}
	waitFor(t, "events were dropped and runs were ended", func() bool {
		f := figures()
		return f["tcpraid_hook_events_dropped"] > 0 && f["tcpraid_hook_runs_timed_out"] > 0 && f["tcpraid_hook_runs"] >= 4
	})
	// and data still goes through
	sendThrough(t, in, 1<<20, got)
}

// A hook that cannot be run stops the node at its start, before anything is bound.
func TestHookThatIsNotThere(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a")
	o := optionsOf(t, "-data", dir, "-hook", filepath.Join(t.TempDir(), "no-such-program"))
	o.apiAddr = "127.0.0.1:0"
	err := runNodeWith(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "-hook") {
		t.Fatalf("a node with a hook that is not there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, node.AddrFile)); err == nil {
		t.Error("the node recorded an API address before it gave up")
	}
	if _, code, done, stderr := runFlagsOf("-data", dir, "-hook", "x", "-hook-timeout", "0s"); !done || code != 2 || !strings.Contains(stderr, "-hook-timeout") {
		t.Errorf("-hook-timeout 0s: done=%v code=%d %q", done, code, stderr)
	}
}

// A connection of package client, over the plain API and over the API with TLS: each
// direction ends apart, a stream that was cut is an error, and a close before the end
// of what is written was passed on is a reset at the destination.
func testClientDial(t *testing.T, tls bool) {
	dirA, dirB := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	addrA := freeAddr(t)
	runFor(t, optionsOf(t, "-data", dirA, "-name", "alpha", "-listen", addrA, "-advertise", addrA))
	argsB := []string{"-data", dirB, "-name", "beta"}
	if tls {
		argsB = append(argsB, "-api-tls")
	}
	runFor(t, optionsOf(t, argsB...))
	_, _, code, _ := call("pair", "create", "-data", dirA)
	must(t, "pair", "join", strings.TrimSpace(code), "-data", dirB)
	c := client.New(dirB)
	c.Timeout = shortCall
	ctx := context.Background()

	// what the methods read is what the commands show
	st, err := c.Status(ctx)
	if err != nil || st.Node.Name != "beta" {
		t.Fatalf("Status: %+v, %v", st, err)
	}
	peers, err := c.Peers(ctx)
	if err != nil || len(peers) != 1 || peers[0].Name != "alpha" {
		t.Fatalf("Peers: %+v, %v", peers, err)
	}
	waitFor(t, "the peer is online", func() bool { p, err := c.Peer(ctx, "alpha"); return err == nil && p.Online })
	var refusal *client.Error
	if _, err := c.Peer(ctx, "nobody"); !errors.As(err, &refusal) || refusal.Status != 404 {
		t.Errorf("Peer of an unknown name: %v", err)
	}
	// nothing the package returns holds a secret of the data directory
	rules, _ := c.Rules(ctx)
	sessions, _ := c.Sessions(ctx)
	tr, _ := c.Transport(ctx)
	everything, _ := json.Marshal([]any{st, peers, rules, sessions, tr})
	holdsNone(t, "what package client returns", string(everything), secretsOf(t, dirB))

	// a destination that echoes: this end finishes first
	port, _ := greeter(t)
	echo := "127.0.0.1:" + strconv.Itoa(port)
	dial := func(addr string) *client.Conn {
		t.Helper()
		var conn *client.Conn
		waitFor(t, "a connection to "+addr+" began", func() bool {
			var err error
			conn, err = c.Dial(ctx, "alpha", addr)
			return err == nil
		})
		return conn
	}
	data := make([]byte, 600<<10)
	rand.Read(data)
	conn := dial(echo)
	var back bytes.Buffer
	done := make(chan error, 1)
	go func() { _, err := io.Copy(&back, conn); done <- err }()
	if _, err := conn.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := conn.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("reading to the end of a complete stream: %v", err)
	}
	if got := back.Bytes(); !bytes.HasPrefix(got, []byte("ready\n")) || !bytes.Equal(got[6:], data) {
		t.Fatalf("%d bytes came back, and they are not the greeting and what was sent", len(got))
	}
	if err := conn.Close(); err != nil {
		t.Errorf("Close after both directions ended: %v", err)
	}

	// a destination that ends its side at once and goes on reading
	hasty, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer hasty.Close()
	type reading struct {
		n   int64
		sum [sha256.Size]byte
		err error
	}
	read := make(chan reading, 64)
	go func() {
		for {
			dc, err := hasty.Accept()
			if err != nil {
				return
			}
			go func() {
				dc.(*net.TCPConn).CloseWrite()
				h := sha256.New()
				n, err := io.Copy(h, dc)
				dc.Close()
				var r reading
				r.n, r.err = n, err
				h.Sum(r.sum[:0])
				read <- r
			}()
		}
	}()
	next := func(what string) reading {
		t.Helper()
		select {
		case r := <-read:
			return r
		case <-time.After(60 * time.Second):
			t.Fatalf("%s: the destination's reading did not end", what)
		}
		return reading{}
	}
	conn = dial(hasty.Addr().String())
	if n, err := io.Copy(io.Discard, conn); n != 0 || err != nil {
		t.Fatalf("reading from a destination that ended its side at once: %d bytes, %v", n, err)
	}
	// the destination's end has been read; the other direction is still whole
	if _, err := conn.Write(data[:300<<10]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := conn.Write(data[300<<10:]); err != nil {
		t.Fatal(err)
	}
	if err := conn.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if r := next("the destination ended first"); r.err != nil || r.n != int64(len(data)) || r.sum != sha256.Sum256(data) {
		t.Errorf("the destination ended first and went on reading: it read %d bytes of %d and ended with %v, or the bytes differ", r.n, len(data), r.err)
	}
	// both ends at nearly the same moment, a short stream each time
	for i := 0; i < 10; i++ {
		conn = dial(hasty.Addr().String())
		conn.Write([]byte("hi\n"))
		if err := conn.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, conn); err != nil {
			t.Errorf("a short stream: reading ended with %v", err)
		}
		conn.Close()
		if r := next("a short stream"); r.err != nil || r.n != 3 {
			t.Errorf("a short stream: the destination read %d bytes of 3 and ended with %v", r.n, r.err)
		}
	}
	// Close before CloseWrite: the destination must not read the end of a stream
	conn = dial(hasty.Addr().String())
	if _, err := conn.Write(data[:200<<10]); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if r := next("a close before the end was passed on"); r.err == nil {
		t.Errorf("the connection was closed before CloseWrite, and the destination read %d bytes and then the end of a complete stream", r.n)
	}

	// A Dial that is given up while the node answers. The destination ends its side a
	// moment after it took the connection, so the node writes its answer at about the
	// time the client goes away, and the write of the answer can be the call that the
	// reset is reported to. Whatever the order: Dial returns the error of its context,
	// or a connection that the test then closes before CloseWrite, and the destination
	// never reads the end of a stream. The order that the entry has to cope with comes
	// up only now and then, so one run of this part does not always catch its absence.
	late, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer late.Close()
	lateRead := make(chan reading, 64)
	go func() {
		for {
			dc, err := late.Accept()
			if err != nil {
				return
			}
			go func() {
				time.Sleep(20 * time.Millisecond)
				dc.(*net.TCPConn).CloseWrite()
				n, err := io.Copy(io.Discard, dc)
				dc.Close()
				lateRead <- reading{n: n, err: err}
			}()
		}
	}()
	for i := 0; i < 20; i++ {
		short, cancel := context.WithTimeout(ctx, 60*time.Millisecond)
		conn, err := c.Dial(short, "alpha", late.Addr().String())
		cancel()
		switch {
		case err == nil:
			conn.Close()
		case !errors.Is(err, context.DeadlineExceeded):
			t.Errorf("a Dial that was given up: %v, want the error of its context", err)
		}
	}
	for quiet := false; !quiet; {
		select {
		case r := <-lateRead:
			if r.err == nil {
				t.Errorf("a Dial was given up or closed before CloseWrite, and the destination read %d bytes and then the end of a complete stream", r.n)
			}
		case <-time.After(3 * time.Second):
			quiet = true
		}
	}
	// an answer of the node that is no refusal of the connection: the node's own words
	// and its status, not "the node is not running"
	var said *client.Error
	if _, err := c.Dial(ctx, "alpha", "bad host!:22"); !errors.As(err, &said) || said.Status != 400 || errors.Is(err, client.ErrNoNode) {
		t.Errorf("Dial to a host name that is none: %v, want the node's answer with status 400", err)
	}

	// no connection begins: nothing listens at the destination, and a peer nobody knows
	var refused *client.RefusedError
	waitFor(t, "a destination where nothing listens is refused with the reason", func() bool {
		conn, err := c.Dial(ctx, "alpha", freeAddr(t))
		if err == nil {
			conn.Close() // the peer's word came after the node had answered: once more
		}
		return errors.As(err, &refused) && refused.Message != ""
	})
	if _, err := c.Dial(ctx, "nobody", echo); !errors.As(err, &refused) {
		t.Errorf("Dial through a peer that is not there: %v", err)
	}
	if _, err := client.New(t.TempDir()).Dial(ctx, "alpha", echo); !errors.Is(err, client.ErrNoNode) {
		t.Errorf("Dial without a node: %v", err)
	}
	if _, err := c.Dial(ctx, "alpha", "no-port"); err == nil {
		t.Error("Dial took an address without a port")
	}
}

func TestClientDial(t *testing.T)        { testClientDial(t, false) }
func TestClientDialOverTLS(t *testing.T) { testClientDial(t, true) }
