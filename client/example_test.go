package client_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Claw-King/tcpraid/client"
)

// standIn serves just enough of a node's API for the examples to run without a node:
// the status, the rules, and a connect route whose destination sends back what it
// gets. It returns a data directory that records where it listens and its token, as a
// node's does. A program that uses the package gives the data directory of its node
// instead, such as /var/lib/tcpraid.
func standIn() (dataDir string, done func()) {
	const token = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	mux := http.NewServeMux()
	authorized := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc(client.PathStatus, func(w http.ResponseWriter, r *http.Request) {
		if authorized(w, r) {
			json.NewEncoder(w).Encode(client.Status{Version: "0.10.0", Node: client.StatusNode{Name: "nas-a"}, Peers: 1, Rules: 1})
		}
	})
	mux.HandleFunc(client.PathRules, func(w http.ResponseWriter, r *http.Request) {
		if authorized(w, r) {
			json.NewEncoder(w).Encode([]client.Rule{{Name: "zfs", Listen: "127.0.0.1:5000", Destination: "192.0.2.10:5000", Peer: "nas-b", Enabled: true, Listening: true}})
		}
	})
	mux.HandleFunc(client.PathConnect, func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		io.Copy(io.Discard, r.Body) // the request: which peer, which destination
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + client.ConnectUpgrade + "\r\n\r\n")
		rw.Flush()
		io.Copy(conn, rw) // until the caller's CloseWrite
	})
	srv := httptest.NewServer(mux)
	dir, _ := os.MkdirTemp("", "tcpraid-example")
	os.WriteFile(filepath.Join(dir, "api.token"), []byte(token), 0o600)
	os.WriteFile(filepath.Join(dir, "api.addr"), []byte(strings.TrimPrefix(srv.URL, "http://")), 0o600)
	return dir, func() { srv.Close(); os.RemoveAll(dir) }
}

// Read the status and the rules of the node that runs on a data directory.
func Example() {
	dataDir, done := standIn()
	defer done()

	c := client.New(dataDir)
	c.Timeout = 10 * time.Second
	ctx := context.Background()
	st, err := c.Status(ctx)
	if err != nil {
		fmt.Println("the node could not be asked:", err)
		return
	}
	fmt.Printf("node %s, version %s, %d peer(s)\n", st.Node.Name, st.Version, st.Peers)
	rules, err := c.Rules(ctx)
	if err != nil {
		fmt.Println(err)
		return
	}
	for _, r := range rules {
		fmt.Printf("rule %s: %s -> %s at %s, listening=%v\n", r.Name, r.Listen, r.Destination, r.Peer, r.Listening)
	}
	// Output:
	// node nas-a, version 0.10.0, 1 peer(s)
	// rule zfs: 127.0.0.1:5000 -> 192.0.2.10:5000 at nas-b, listening=true
}

// Open a connection to a destination at a peer, send a request, pass on its end, and
// read the answer to the end. An error from the reading means that the connection was
// cut off: the end of a complete answer is io.EOF and nothing else.
func ExampleClient_Dial() {
	dataDir, done := standIn()
	defer done()

	conn, err := client.New(dataDir).Dial(context.Background(), "nas-b", "192.0.2.10:7")
	if err != nil {
		fmt.Println("no connection:", err)
		return
	}
	// Close before CloseWrite would reset the connection; here it follows the end of
	// both directions and is an ordinary close.
	defer conn.Close()
	var _ net.Conn = conn
	if _, err := conn.Write([]byte("hello\n")); err != nil {
		fmt.Println(err)
		return
	}
	if err := conn.CloseWrite(); err != nil {
		fmt.Println(err)
		return
	}
	answer, err := io.ReadAll(conn)
	if err != nil {
		fmt.Println("the connection was cut off:", err)
		return
	}
	fmt.Printf("%s", answer)
	// Output:
	// hello
}
