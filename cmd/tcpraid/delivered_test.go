package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/client"
	"github.com/Claw-King/tcpraid/internal/api"
)

// deliveryNodes starts alpha, which listens, and beta, which holds the path to it and
// runs the commands, pairs them and returns beta's data directory.
func deliveryNodes(t *testing.T) (dirA, dirB string) {
	t.Helper()
	dirA, dirB = filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	addrA := freeAddr(t)
	runFor(t, optionsOf(t, "-data", dirA, "-name", "alpha", "-listen", addrA, "-advertise", addrA))
	runFor(t, optionsOf(t, "-data", dirB, "-name", "beta"))
	_, _, code, _ := call("pair", "create", "-data", dirA)
	must(t, "pair", "join", strings.TrimSpace(code), "-data", dirB)
	// a connection that works: the control connection is up and the peers have said
	// what they can do
	port, _ := greeter(t)
	deadline := time.Now().Add(30 * time.Second)
	for {
		status, _ := connect(dirB, api.ConnectRequest{Peer: "alpha", Host: "127.0.0.1", Port: port}, strings.NewReader("x"), io.Discard)
		peers, _ := client.New(dirB).Peers(context.Background())
		if status == connectDone && len(peers) == 1 && len(peers[0].Features) > 0 {
			return dirA, dirB
		}
		if time.Now().After(deadline) {
			t.Fatalf("the two nodes did not come to carry a connection: status %d, peers %+v", status, peers)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// An exchange that goes well ends with status 0, whatever the order of the two
// ends: with a destination that answers and ends when its client does, and with one
// that ends its side first and reads on, for a short, an empty and a large input.
func TestConnectStatusZeroForWhatWasDelivered(t *testing.T) {
	_, dirB := deliveryNodes(t)
	echo, _ := greeter(t)
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
	read := make(chan reading, 4)
	go func() {
		for {
			c, err := hasty.Accept()
			if err != nil {
				return
			}
			go func() {
				c.(*net.TCPConn).CloseWrite()
				h := sha256.New()
				n, err := io.Copy(h, c)
				c.Close()
				r := reading{n: n, err: err}
				h.Sum(r.sum[:0])
				read <- r
			}()
		}
	}()
	large := make([]byte, 2<<20)
	rand.Read(large)
	inputs := map[string][]byte{"a short input": []byte("hi\n"), "an empty input": nil, "a large input": large}
	want := map[[sha256.Size]byte]bool{}
	for _, input := range inputs {
		want[sha256.Sum256(input)] = true
	}
	// the six exchanges of a round run side by side: the node says how a session ended
	// about a second after the two directions have
	for round := 1; round <= 20; round++ {
		failed := make(chan string, 2*len(inputs))
		var wg sync.WaitGroup
		for what, input := range inputs {
			wg.Add(2)
			go func() {
				defer wg.Done()
				var out bytes.Buffer
				status, err := connect(dirB, api.ConnectRequest{Peer: "alpha", Host: "127.0.0.1", Port: echo}, bytes.NewReader(input), &out)
				if status != connectDone || err != nil {
					failed <- fmt.Sprintf("%s to a destination that answers: status %d, %v", what, status, err)
				} else if got := out.Bytes(); !bytes.HasPrefix(got, []byte("ready\n")) || !bytes.Equal(got[6:], input) {
					failed <- fmt.Sprintf("%s: %d bytes on the output, and they are not the greeting and what was sent", what, len(got))
				}
			}()
			go func() {
				defer wg.Done()
				status, err := connect(dirB, api.ConnectRequest{Peer: "alpha", Host: "127.0.0.1", Port: hasty.Addr().(*net.TCPAddr).Port}, bytes.NewReader(input), io.Discard)
				if status != connectDone || err != nil {
					failed <- fmt.Sprintf("%s to a destination that ends its side first: status %d, %v", what, status, err)
				}
			}()
		}
		wg.Wait()
		close(failed)
		for f := range failed {
			t.Errorf("round %d, %s", round, f)
		}
		if t.Failed() {
			return
		}
		// status 0 was given three times: the destination has each whole input by now
		got := map[[sha256.Size]byte]bool{}
		for range inputs {
			select {
			case r := <-read:
				if r.err != nil {
					t.Fatalf("round %d: the destination read %d bytes and ended with %v", round, r.n, r.err)
				}
				got[r.sum] = true
			case <-time.After(30 * time.Second):
				t.Fatalf("round %d: the commands ended with status 0, and the destination's reading has not ended", round)
			}
		}
		for sum := range want {
			if !got[sum] {
				t.Fatalf("round %d: the destination did not read one of the three inputs whole", round)
			}
		}
	}
}
