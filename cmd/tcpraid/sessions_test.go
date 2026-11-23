package main

import (
	"bytes"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/api"
)

// One line per session, with the speed worked out from two looks at it.
func TestPrintSessions(t *testing.T) {
	now := time.Date(2026, 1, 15, 10, 0, 40, 0, time.UTC)
	before := []api.Session{
		{ID: "ddc465aabbccddeeff00112233445566", TxBytes: 300 << 20, RxBytes: 1 << 20},
	}
	after := []api.Session{
		{ID: "ddc465aabbccddeeff00112233445566", Destination: "192.0.2.10:873", Role: "client", Started: now.Add(-40 * time.Second),
			TxBytes: 400 << 20, RxBytes: 1 << 20, ResentBytes: 6 << 20, Subs: 8, Peer: "nas-a", Rule: "rsync"},
		{ID: "0102030405060708090a0b0c0d0e0f10", Destination: "192.0.2.20:5432", Role: "server", Started: now.Add(-500 * time.Millisecond),
			RxBytes: 2 << 30, Subs: 12, Peer: "nas-b"},
	}
	var out bytes.Buffer
	if err := printSessions(&out, before, after, 2*time.Second, now); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want a heading and two lines:\n%s", out.String())
	}
	fields := func(l string) []string {
		return strings.Split(strings.Join(strings.Fields(strings.ReplaceAll(l, " MB", "MB")), " "), " ")
	}
	// 400 MB + 1 MB in 40 s; 100 MB more than two seconds earlier; 6 MB of 400 MB sent again
	if got, want := strings.Join(fields(lines[1]), " "), "ddc465 client rsync nas-a 192.0.2.10:873 40s 400.0MB 1.0MB 10.0MB/s 50.0MB/s 1.5% 8"; got != want {
		t.Errorf("first session:\n got  %s\n want %s", got, want)
	}
	// younger than a second, not in the first look, nothing sent: no speeds, no share
	if got, want := strings.Join(fields(lines[2]), " "), "010203 server - nas-b 192.0.2.20:5432 0s 0 B 2.00 GB - - - 12"; got != want {
		t.Errorf("second session:\n got  %s\n want %s", got, want)
	}
	out.Reset()
	printSessions(&out, before, nil, time.Second, now)
	if strings.TrimSpace(out.String()) != "no sessions" {
		t.Errorf("the sessions ended between the two looks: %q", out.String())
	}
}

// Two nodes on loopback, driven through the commands only: the allow list and the UDP
// feedback address of a peer, and the sessions while a connection runs.
func TestSessionsAndPeersSetCommands(t *testing.T) {
	defer func(d time.Duration) { sessionsInterval = d }(sessionsInterval)
	sessionsInterval = 300 * time.Millisecond
	dirA, dirB := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	addrA := freeAddr(t)
	startNodeWith(t, runOptions{dataDir: dirA, name: "alpha", listen: addrA, advertise: stringList{addrA}})
	startNodeWith(t, runOptions{dataDir: dirB, name: "beta"})
	_, _, stdout, _ := call("pair", "create", "-data", dirA)
	must(t, "pair", "join", strings.TrimSpace(stdout), "-data", dirB)

	// the destination counts what it is given; the sender below writes slowly
	dst, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	var accepted, got atomic.Int64
	go func() {
		for {
			c, err := dst.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go func() {
				defer c.Close()
				buf := make([]byte, 64<<10)
				for {
					n, err := c.Read(buf)
					got.Add(int64(n))
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	in := freeAddr(t)
	must(t, "rules", "add", "zfs", "-data", dirB, "-peer", "alpha", "-listen", in, "-dest", dst.Addr().String())

	if out := must(t, "sessions", "-data", dirB); strings.TrimSpace(out) != "no sessions" {
		t.Fatalf("sessions on an idle node: %q", out)
	}
	if out := must(t, "sessions", "-json", "-data", dirB); strings.TrimSpace(out) != "[]" {
		t.Fatalf("sessions -json on an idle node: %q", out)
	}

	// nothing to set, an unknown peer, a list that is none
	if code, _, _, stderr := call("peers", "set", "beta", "-data", dirA); code != 1 || !strings.Contains(stderr, "-allow") {
		t.Fatalf("peers set without a setting: code %d, %q", code, stderr)
	}
	if code, _, _, stderr := call("peers", "set", "nobody", "-allow", "192.0.2.0/24", "-data", dirA); code != 1 || !strings.Contains(stderr, "not found") {
		t.Fatalf("peers set of an unknown peer: code %d, %q", code, stderr)
	}
	if code, _, _, stderr := call("peers", "set", "beta", "-allow", "everything", "-data", dirA); code != 1 || stderr == "" {
		t.Fatalf("peers set with a list that is none: code %d, %q", code, stderr)
	}

	// tried opens a connection through the rule, sends a little and says whether the
	// destination was reached
	tried := func() bool {
		t.Helper()
		was := accepted.Load()
		c, err := net.Dial("tcp", in)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.Write([]byte("hello"))
		c.(*net.TCPConn).CloseWrite()
		c.SetReadDeadline(time.Now().Add(20 * time.Second))
		io.Copy(io.Discard, c) // until the far end closes or resets
		return accepted.Load() > was
	}
	if !tried() {
		t.Fatal("without an allow list the destination was not reached")
	}
	// a list that does not hold the destination: on the node the connections come out of
	out := must(t, "peers", "set", "beta", "-allow", "192.0.2.0/24:873", "-data", dirA)
	if !strings.Contains(out, "beta may reach through this node: 192.0.2.0/24:873") {
		t.Fatalf("peers set -allow: %q", out)
	}
	if show := must(t, "peers", "show", "beta", "-data", dirA); !strings.Contains(show, "allow        192.0.2.0/24:873") {
		t.Fatalf("peers show after -allow:\n%s", show)
	}
	if tried() {
		t.Fatal("a destination outside the allow list was reached")
	}
	// the destination's own address and port
	must(t, "peers", "set", "beta", "-allow", "127.0.0.1/32:"+portOf(dst.Addr()), "-data", dirA)
	if !tried() {
		t.Fatal("a destination in the allow list was not reached")
	}
	// cleared again, together with a feedback address
	out = must(t, "peers", "set", "beta", "-allow", "", "-udp-dial", "192.0.2.7:8446", "-data", dirA)
	if !strings.Contains(out, "beta may reach any destination") || !strings.Contains(out, "goes to 192.0.2.7:8446") {
		t.Fatalf("peers set -allow \"\" -udp-dial: %q", out)
	}
	show := must(t, "peers", "show", "beta", "-data", dirA)
	if strings.Contains(show, "allow   ") || !strings.Contains(show, "udp feedback 192.0.2.7:8446") {
		t.Fatalf("peers show after clearing the list:\n%s", show)
	}
	if out := must(t, "peers", "set", "beta", "-udp-dial", "", "-data", dirA); !strings.Contains(out, "no UDP feedback is sent to beta") {
		t.Fatalf("peers set -udp-dial \"\": %q", out)
	}
	if show := must(t, "peers", "show", "beta", "-data", dirA); strings.Contains(show, "udp feedback") {
		t.Fatalf("peers show after clearing the feedback address:\n%s", show)
	}

	// the short connections above are over on both nodes a moment after they closed
	for _, dir := range []string{dirA, dirB} {
		for deadline := time.Now().Add(20 * time.Second); strings.TrimSpace(must(t, "sessions", "-data", dir)) != "no sessions"; time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("sessions that ended are still listed:\n%s", must(t, "sessions", "-data", dir))
			}
		}
	}
	// a connection that keeps sending: both nodes list the session, with a speed
	c, err := net.Dial("tcp", in)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	stop := make(chan bool)
	defer close(stop)
	go func() {
		buf := make([]byte, 64<<10)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := c.Write(buf); err != nil {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	for deadline := time.Now().Add(20 * time.Second); got.Load() < 1<<20; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the connection did not carry data")
		}
	}
	for _, c := range []struct{ dir, role, rule, peer string }{{dirB, "client", "zfs", "alpha"}, {dirA, "server", "-", "beta"}} {
		out := must(t, "sessions", "-data", c.dir)
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "ID ") {
			t.Fatalf("sessions on %s:\n%s", c.peer, out)
		}
		f := strings.Fields(lines[1])
		// ID ROLE RULE PEER DESTINATION AGE ...
		if len(f) < 6 || f[1] != c.role || f[2] != c.rule || f[3] != c.peer || f[4] != dst.Addr().String() {
			t.Fatalf("the session's line:\n%s", out)
		}
		if strings.Contains(lines[1], " 0.0 MB/s") || !strings.Contains(lines[1], "MB/s") {
			t.Fatalf("a session that carries data shows no speed:\n%s", out)
		}
	}
	if raw := must(t, "sessions", "-json", "-data", dirB); !strings.Contains(raw, `"rule":"zfs"`) || !strings.Contains(raw, `"tx_bytes"`) {
		t.Fatalf("sessions -json: %q", raw)
	}
}

// portOf returns the port of a TCP address.
func portOf(a net.Addr) string {
	_, port, _ := net.SplitHostPort(a.String())
	return port
}
