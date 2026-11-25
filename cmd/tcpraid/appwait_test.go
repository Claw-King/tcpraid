package main

import (
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/api"
)

// appWaitNodes starts alpha, which listens and has a window of 4 MB, and beta with a
// rule to dest at alpha, and returns their data directories and the rule's address.
func appWaitNodes(t *testing.T, dest string) (dirA, dirB, in string) {
	t.Helper()
	dirA, dirB = filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	addrA := freeAddr(t)
	runFor(t, optionsOf(t, "-data", dirA, "-name", "alpha", "-listen", addrA, "-advertise", addrA, "-window", "4MB"))
	runFor(t, optionsOf(t, "-data", dirB, "-name", "beta"))
	_, _, code, _ := call("pair", "create", "-data", dirA)
	must(t, "pair", "join", strings.TrimSpace(code), "-data", dirB)
	in = freeAddr(t)
	must(t, "rules", "add", "file", "-data", dirB, "-peer", "alpha", "-listen", in, "-dest", dest)
	return dirA, dirB, in
}

// sessionsOf returns the node's sessions as the API gives them (the web page reads the
// same answer) and the table of "tcpraid sessions".
func sessionsOf(t *testing.T, dir string) ([]api.Session, string) {
	t.Helper()
	var list []api.Session
	if err := json.Unmarshal([]byte(must(t, "sessions", "-json", "-data", dir)), &list); err != nil {
		t.Fatal(err)
	}
	return list, must(t, "sessions", "-data", dir)
}

// A client that reads a file through two nodes piece by piece, asking for the next
// piece when it has the one before: the node that sends the file says in the API and in
// "tcpraid sessions" that the session waits for the application. The node that
// receives the file does not say it.
func TestSessionsSayThatTheApplicationIsWaitedFor(t *testing.T) {
	const piece = 32 << 10
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
			go func() {
				defer c.Close()
				ask, answer := make([]byte, 1), make([]byte, piece)
				for {
					if _, err := io.ReadFull(c, ask); err != nil {
						return
					}
					if _, err := c.Write(answer); err != nil {
						return
					}
				}
			}()
		}
	}()
	dirA, dirB, in := appWaitNodes(t, ln.Addr().String())
	var c net.Conn
	deadline := time.Now().Add(30 * time.Second)
	for {
		// the first connections can come before the nodes have each other
		if c, err = net.Dial("tcp", in); err == nil {
			c.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err = c.Write([]byte{1}); err == nil {
				if _, err = io.ReadFull(c, make([]byte, piece)); err == nil {
					break
				}
			}
			c.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("the rule never carried: %v", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	defer c.Close()
	c.SetDeadline(time.Time{})
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		buf := make([]byte, piece)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := c.Write([]byte{1}); err != nil {
				return
			}
			if _, err := io.ReadFull(c, buf); err != nil {
				return
			}
		}
	}()
	deadline = time.Now().Add(30 * time.Second)
	for {
		time.Sleep(500 * time.Millisecond)
		list, table := sessionsOf(t, dirA)
		if len(list) == 1 && list[0].WaitsForApplication && list[0].IdleShare != nil && *list[0].IdleShare >= 0.9 &&
			strings.Contains(table, "ok, waiting for the application") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the node that sends the file never said that the session waits for the application: %+v\n%s", list, table)
		}
	}
	if list, table := sessionsOf(t, dirB); len(list) != 1 || list[0].WaitsForApplication || strings.Contains(table, "waiting for the application") {
		t.Errorf("the node that receives the file says that the session waits for the application: %+v\n%s", list, table)
	}
}

// The same nodes, and a destination that sends without pause to a client that reads
// slowly: the window of the sending node is full, and it does not say that the session
// waits for the application.
func TestSessionsDoNotSayItForAFullWindow(t *testing.T) {
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
			go func() {
				defer c.Close()
				buf := make([]byte, 64<<10)
				for {
					if _, err := c.Write(buf); err != nil {
						return
					}
				}
			}()
		}
	}()
	dirA, _, in := appWaitNodes(t, ln.Addr().String())
	var c net.Conn
	deadline := time.Now().Add(30 * time.Second)
	for {
		if c, err = net.Dial("tcp", in); err == nil {
			c.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err = io.ReadFull(c, make([]byte, 1000)); err == nil {
				break
			}
			c.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("the rule never carried: %v", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	defer c.Close()
	c.SetDeadline(time.Time{})
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		buf := make([]byte, 16<<10)
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			if _, err := io.ReadFull(c, buf); err != nil {
				return
			}
		}
	}()
	// the figure looks back over five seconds
	time.Sleep(8 * time.Second)
	for i := 0; i < 6; i++ {
		list, table := sessionsOf(t, dirA)
		for _, s := range list {
			if s.WaitsForApplication {
				t.Fatalf("the node whose window is full says that a session waits for the application: %+v\n%s", s, table)
			}
		}
		if len(list) == 0 {
			t.Fatal("the node has no session")
		}
		time.Sleep(500 * time.Millisecond)
	}
}
