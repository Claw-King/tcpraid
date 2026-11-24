package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/api"
	"github.com/Claw-King/tcpraid/internal/node"
)

// greeter is a destination that says "ready", sends back what it receives and ends
// when its client does. It returns its port and how many connections it has taken.
func greeter(t *testing.T) (port int, taken func() int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	n := 0
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			n++
			mu.Unlock()
			go func() {
				defer c.Close()
				io.WriteString(c, "ready\n")
				io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, func() int { mu.Lock(); defer mu.Unlock(); return n }
}

// connectWith runs "tcpraid connect" with the given input and returns its exit status
// and what it wrote.
func connectWith(in string, args ...string) (code int, stdout, stderr string) {
	old := connectStdin
	connectStdin = strings.NewReader(in)
	defer func() { connectStdin = old }()
	code, _, stdout, stderr = call(append([]string{"connect"}, args...)...)
	return code, stdout, stderr
}

// The connect command puts one connection to a destination at the peer on its input
// and output, writes nothing else to its output, and says by its exit status how the
// connection went. tls: the node of the command serves its API over TLS.
func testConnectCommand(t *testing.T, tls bool) {
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
	port, taken := greeter(t)
	dest := "127.0.0.1:" + strconv.Itoa(port)

	data := make([]byte, 300<<10)
	rand.Read(data)
	var out bytes.Buffer
	status, err := connect(dirB, api.ConnectRequest{Peer: "alpha", Host: "127.0.0.1", Port: port}, bytes.NewReader(data), &out)
	if status != connectDone || err != nil {
		t.Fatalf("connect: status %d, %v", status, err)
	}
	if got := out.Bytes(); !bytes.HasPrefix(got, []byte("ready\n")) || !bytes.Equal(got[6:], data) {
		t.Fatalf("%d bytes on the output, and they are not the greeting and what was sent", len(got))
	}
	// the two ways to name the destination
	if code, stdout, stderr := connectWith("abc", "-data", dirB, "-peer", "alpha", dest); code != 0 || stdout != "ready\nabc" || stderr != "" {
		t.Errorf("HOST:PORT: status %d, output %q, errors %q", code, stdout, stderr)
	}
	if code, stdout, stderr := connectWith("abc", "-peer", "alpha", "127.0.0.1", strconv.Itoa(port), "-data", dirB); code != 0 || stdout != "ready\nabc" || stderr != "" {
		t.Errorf("HOST PORT: status %d, output %q, errors %q", code, stdout, stderr)
	}

	// a command line that is wrong: status 2, nothing on the output
	for _, bad := range [][]string{
		{"-data", dirB, dest},
		{"-data", dirB, "-peer", "alpha"},
		{"-data", dirB, "-peer", "alpha", "127.0.0.1:0"},
		{"-data", dirB, "-peer", "alpha", "127.0.0.1", "ssh", "x"},
		{"-data", dirB, "-peer", "alpha", "no-port"},
	} {
		if code, stdout, stderr := connectWith("", bad...); code != connectUsage || stdout != "" || stderr == "" {
			t.Errorf("connect %v: status %d, output %q, errors %q", bad, code, stdout, stderr)
		}
	}
	// no node: status 1
	if code, stdout, stderr := connectWith("", "-data", t.TempDir(), "-peer", "alpha", dest); code != connectNoNode || stdout != "" || !strings.Contains(stderr, "the node is not running") {
		t.Errorf("without a node: status %d, output %q, errors %q", code, stdout, stderr)
	}
	// no connection to the destination: status 3, the reason on the error output
	if code, stdout, stderr := connectWith("", "-data", dirB, "-peer", "nobody", dest); code != connectRefused || stdout != "" || !strings.Contains(stderr, "not paired") {
		t.Errorf("an unknown peer: status %d, output %q, errors %q", code, stdout, stderr)
	}
	must(t, "peers", "set", "beta", "-allow", "192.0.2.0/24", "-data", dirA)
	before := taken()
	var said string
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		// The peer says why over the control connection, once that is up, and within
		// the 150 ms that the node waits for it. On a busy machine the word can come
		// later: the connection then began and was cut off, which is status 4.
		code, stdout, stderr := connectWith("abc", "-data", dirB, "-peer", "alpha", dest)
		if (code != connectRefused && code != connectCut) || stdout != "" {
			t.Fatalf("a destination outside the allow list: status %d, output %q, errors %q", code, stdout, stderr)
		}
		if said = stderr; (code == connectRefused && strings.Contains(said, "allow list")) || time.Now().After(deadline) {
			break
		}
	}
	if !strings.Contains(said, "allow list") {
		t.Errorf("a destination outside the allow list: the command said %q", said)
	}
	if taken() != before {
		t.Error("a connection reached a destination outside the allow list")
	}
	must(t, "peers", "set", "beta", "-allow", "", "-data", dirA)

	// A destination that ends its side first and goes on reading. The command goes on
	// sending until its input ends, and the destination reads the whole input and
	// then the end of a complete stream. An input that has already ended is no
	// other case: the command ends with status 0, however the two ends fall.
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
	read := make(chan reading, 1)
	go func() {
		for {
			c, err := hasty.Accept()
			if err != nil {
				return
			}
			c.(*net.TCPConn).CloseWrite()
			h := sha256.New()
			n, err := io.Copy(h, c)
			c.Close()
			var r reading
			r.n, r.err = n, err
			h.Sum(r.sum[:0])
			read <- r
		}
	}()
	toHasty := api.ConnectRequest{Peer: "alpha", Host: "127.0.0.1", Port: hasty.Addr().(*net.TCPAddr).Port}
	wantRead := func(what string, sent []byte) {
		t.Helper()
		select {
		case r := <-read:
			if r.err != nil || r.n != int64(len(sent)) || r.sum != sha256.Sum256(sent) {
				t.Errorf("%s: the destination read %d bytes of %d and ended with %v, or the bytes differ", what, r.n, len(sent), r.err)
			}
		case <-time.After(60 * time.Second):
			t.Fatalf("%s: the destination's reading did not end", what)
		}
	}
	upload := make([]byte, 2<<20)
	rand.Read(upload)
	more, moreW := io.Pipe()
	go func() {
		// in two parts, so that the destination's end has long arrived when the
		// second is sent
		moreW.Write(upload[:1<<20])
		time.Sleep(1500 * time.Millisecond)
		moreW.Write(upload[1<<20:])
		moreW.Close()
	}()
	status, err = connect(dirB, toHasty, more, io.Discard)
	if status != connectDone || err != nil {
		t.Errorf("the destination ended its side first and the input went on: status %d, %v, want %d", status, err, connectDone)
	}
	wantRead("an input that went on after the destination's end", upload)
	for i := 0; i < 10; i++ {
		status, err = connect(dirB, toHasty, strings.NewReader("hi\n"), io.Discard)
		if status != connectDone || err != nil {
			t.Errorf("a short input to a destination that ends its side at once: status %d, %v, want %d", status, err, connectDone)
		}
		wantRead("a short input", []byte("hi\n"))
		status, err = connect(dirB, toHasty, strings.NewReader(""), io.Discard)
		if status != connectDone || err != nil {
			t.Errorf("an empty input to a destination that ends its side at once: status %d, %v, want %d", status, err, connectDone)
		}
		wantRead("an empty input", nil)
	}

	// a connection that is cut off: status 4. The input stays open, as a program's does.
	pr, pw := io.Pipe()
	defer pw.Close()
	or, ow := io.Pipe()
	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		status, err := connect(dirB, api.ConnectRequest{Peer: "alpha", Host: "127.0.0.1", Port: port}, pr, ow)
		ow.Close()
		done <- result{status, err}
	}()
	greeting := make([]byte, 6)
	if _, err := io.ReadFull(or, greeting); err != nil || string(greeting) != "ready\n" {
		t.Fatalf("the greeting of the connection that is to be cut: %q, %v", greeting, err)
	}
	go io.Copy(io.Discard, or)
	must(t, "peers", "remove", "beta", "-data", dirA)
	select {
	case r := <-done:
		if r.status != connectCut || r.err == nil {
			t.Errorf("a connection that was cut off: status %d, %v, want %d and a reason", r.status, r.err, connectCut)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the command did not end after its connection was cut off")
	}
}

func TestConnectCommand(t *testing.T)        { testConnectCommand(t, false) }
func TestConnectCommandOverTLS(t *testing.T) { testConnectCommand(t, true) }

// With TLS on the API the connect command checks the certificate like every command:
// it sends nothing to an API that presents another than the one on record.
func TestConnectSendsNothingToAnotherCertificate(t *testing.T) {
	dir, other := t.TempDir(), t.TempDir()
	runFor(t, optionsOf(t, "-data", dir, "-name", "alpha", "-api-tls"))
	runFor(t, optionsOf(t, "-data", other, "-name", "gamma", "-api-tls"))
	// the record of another node's certificate in the place of this node's
	cert, err := os.ReadFile(filepath.Join(other, node.APITLSFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, node.APITLSFile), cert, 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := connectWith("abc", "-data", dir, "-peer", "beta", "127.0.0.1:22")
	if code != connectNoNode || stdout != "" || !strings.Contains(stderr, "presents another certificate") {
		t.Errorf("status %d, output %q, errors %q", code, stdout, stderr)
	}
	token, _ := node.ReadToken(dir)
	if token == "" || strings.Contains(stderr, token) {
		t.Error("the token is in the message, or could not be read")
	}
}
