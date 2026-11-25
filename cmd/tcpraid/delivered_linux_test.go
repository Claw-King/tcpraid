package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/client"
	"github.com/Claw-King/tcpraid/internal/api"
)

// blackhole returns the port of a listener on the loopback address that answers no
// connect: its queue of connections that wait to be accepted is full, and nothing
// accepts. A connect to it gets no answer until whoever connects gives up.
func blackhole(t *testing.T) int {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Listen(fd, 0); err != nil {
		t.Fatal(err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	port := sa.(*syscall.SockaddrInet4).Port
	addr := "127.0.0.1:" + strconv.Itoa(port)
	for i := 0; i < 8; i++ {
		c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err != nil {
			return port
		}
		t.Cleanup(func() { c.Close() })
	}
	t.Skip("this system answers the connects to a listener whose queue is full")
	return 0
}

// A destination that does not answer the peer's connect at all: no connection began,
// which is status 3 with the reason. Before version 0.11.0 the node said that the
// connection began a moment after the peer took it, and the command ended with status
// 4 when the peer gave up.
func TestConnectToADestinationThatNeverAnswers(t *testing.T) {
	_, dirB := deliveryNodes(t)
	port := blackhole(t)
	code, stdout, stderr := connectWith("abc", "-data", dirB, "-peer", "alpha", "127.0.0.1:"+strconv.Itoa(port))
	if code != connectRefused || stdout != "" || !strings.Contains(stderr, "could not connect to") {
		t.Errorf("status %d, output %q, errors %q, want status %d and the reason", code, stdout, stderr, connectRefused)
	}
}

// stubborn is a destination that ends its side at once and reads nothing until it is
// released; it then resets the connection. Of what a program sends to it, only what
// the buffers of the system hold on the way can have been written to its connection.
func stubborn(t *testing.T) (port int, release func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	gate := make(chan struct{})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				c.(*net.TCPConn).SetReadBuffer(64 << 10)
				c.(*net.TCPConn).CloseWrite()
				<-gate
				c.(*net.TCPConn).SetLinger(0)
				c.Close()
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, func() { close(gate) }
}

// beyondBuffers returns an amount of data that the buffers of two TCP sockets of this
// system cannot hold: the largest send buffer and the largest receive buffer the
// system grows a socket to, and 8 MiB more. A destination that reads nothing cannot
// have been written that much.
func beyondBuffers(t *testing.T) int {
	t.Helper()
	largest := func(file string) int {
		text, err := os.ReadFile(file)
		if err != nil {
			t.Skipf("the sizes of the system's socket buffers cannot be read: %v", err)
		}
		f := strings.Fields(string(text))
		n, err := strconv.Atoi(f[len(f)-1])
		if err != nil || len(f) != 3 {
			t.Skipf("%s reads %q", file, text)
		}
		return n
	}
	return largest("/proc/sys/net/ipv4/tcp_wmem") + largest("/proc/sys/net/ipv4/tcp_rmem") + 8<<20
}

// Status 0 of the connect command says that the other node wrote the whole input to
// the destination's connection. A destination that ended its side first and then went
// away without reading: the node had taken all of the input, and before version 0.11.0
// the command ended with status 0 at that point.
func TestConnectStatusZeroMeansDelivered(t *testing.T) {
	_, dirB := deliveryNodes(t)
	port, release := stubborn(t)
	input := make([]byte, beyondBuffers(t))
	rand.Read(input)
	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		status, err := connect(dirB, api.ConnectRequest{Peer: "alpha", Host: "127.0.0.1", Port: port}, bytes.NewReader(input), io.Discard)
		done <- result{status, err}
	}()
	// the node takes the whole input, and the command has nothing more to do but to
	// wait for the word of the node
	select {
	case r := <-done:
		t.Fatalf("the command ended with status %d (%v) while the destination had read nothing", r.status, r.err)
	case <-time.After(3 * time.Second):
	}
	release()
	select {
	case r := <-done:
		if r.status != connectCut || r.err == nil {
			t.Errorf("the destination went away without the input: status %d, %v, want %d and a reason", r.status, r.err, connectCut)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the command did not end after its connection was cut off")
	}

	// the same for a program that uses the package: Done says that the stream was cut
	port, release = stubborn(t)
	conn, err := client.New(dirB).Dial(context.Background(), "alpha", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write(input); err != nil {
		t.Fatal(err)
	}
	if err := conn.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, conn); err != nil {
		t.Fatalf("the destination ended its side in order, and reading ended with %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	if err := conn.Done(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("while the destination reads nothing, Done returned %v, want to wait", err)
	}
	cancel()
	release()
	var cut *client.CutError
	if err := conn.Done(context.Background()); !errors.As(err, &cut) || cut.Reason == "" {
		t.Errorf("after the destination went away, Done returned %v, want a *CutError with a reason", err)
	}
}
