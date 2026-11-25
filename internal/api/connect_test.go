package api

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// echo is a destination that sends back what it receives and ends when its client does.
// greet, if not empty, is sent first.
func echo(t *testing.T, greet string) (port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.WriteString(c, greet)
				io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// askConnect sends a connect request to the node's API at addr, as the connect command
// does, and returns the connection, the reader that holds what came after the answer,
// and the answer.
func askConnect(t *testing.T, addr, token, body string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(60 * time.Second))
	req := "POST " + PathConnect + " HTTP/1.1\r\nHost: " + addr + "\r\nContent-Type: application/json\r\nConnection: Upgrade\r\nUpgrade: " + ConnectUpgrade + "\r\n"
	if token != "" {
		req += "Authorization: Bearer " + token + "\r\n"
	}
	req += "Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("no answer to the connect request: %v", err)
	}
	return c, br, resp
}

// refusal reads the reason of an answer that is not 101.
func refusal(t *testing.T, resp *http.Response, code int) string {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	var e struct {
		Error string `json:"error"`
	}
	if resp.StatusCode != code || json.Unmarshal(b, &e) != nil || e.Error == "" {
		t.Fatalf("status %d with %q, want %d with an error", resp.StatusCode, b, code)
	}
	return e.Error
}

// A program that holds the token gets a connection to a destination at the peer on the
// very connection of its request. The answer comes first, then the stream and nothing
// else. Without the token it gets nothing.
func TestConnectRoute(t *testing.T) {
	a, b := newPeerNode(t, "alpha"), newPeerNode(t, "beta")
	pairOver(t, a, b)
	online(t, b, "alpha")
	online(t, a, "beta")
	srv := httptest.NewServer(b.Server)
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	dst := echo(t, "ready\n")
	body := func(host string, port int) string {
		return jsonBody(ConnectRequest{Peer: "alpha", Host: host, Port: port})
	}

	// the route needs the token like every other, before anything is looked at
	for _, token := range []string{"", "wrong"} {
		_, _, resp := askConnect(t, addr, token, body("127.0.0.1", dst))
		if why := refusal(t, resp, 401); !strings.Contains(why, "access token") {
			t.Errorf("token %q: %q", token, why)
		}
	}
	// a request the node does not take
	for what, bad := range map[string]string{
		"no peer":       `{"host":"127.0.0.1","port":80}`,
		"port 0":        `{"peer":"alpha","host":"127.0.0.1","port":0}`,
		"no host":       `{"peer":"alpha","host":"","port":80}`,
		"a bad host":    `{"peer":"alpha","host":"not a name","port":80}`,
		"unknown field": `{"peer":"alpha","host":"127.0.0.1","port":80,"x":1}`,
	} {
		if _, _, resp := askConnect(t, addr, b.cfg.Token, bad); resp.StatusCode != 400 {
			t.Errorf("%s: status %d, want 400", what, resp.StatusCode)
		}
	}
	_, _, resp := askConnect(t, addr, b.cfg.Token, `{"peer":"nobody","host":"127.0.0.1","port":80}`)
	if why := refusal(t, resp, 502); !strings.Contains(why, `peer "nobody" is not paired`) {
		t.Errorf("an unknown peer: %q", why)
	}

	// the stream
	c, br, resp := askConnect(t, addr, b.cfg.Token, body("127.0.0.1", dst))
	if resp.StatusCode != 101 || resp.Header.Get("Upgrade") != ConnectUpgrade {
		t.Fatalf("status %d, Upgrade %q", resp.StatusCode, resp.Header.Get("Upgrade"))
	}
	data := make([]byte, 1<<20)
	rand.Read(data)
	go func() {
		c.Write(data)
		c.(*net.TCPConn).CloseWrite()
	}()
	back, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("the stream ended with %v, want a plain end", err)
	}
	if !strings.HasPrefix(string(back), "ready\n") || sha256.Sum256(back[6:]) != sha256.Sum256(data) {
		t.Fatalf("%d bytes came back, and they are not the greeting and what was sent", len(back))
	}

	// the answer named the session, and the node says how it ended to who holds the token
	session := resp.Header.Get("Tcpraid-Session")
	if len(session) != 32 {
		t.Fatalf("the answer names the session %q, want 32 hexadecimal digits", session)
	}
	ask := func(id, token, query string) (int, string) {
		req, err := http.NewRequest("GET", srv.URL+PathSessions+"/"+id+"/end"+query, nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		answer, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(answer)
	}
	var end struct {
		Ended, Complete bool
		Reason          string
	}
	if code, answer := ask(session, b.cfg.Token, ""); code != 200 || json.Unmarshal([]byte(answer), &end) != nil || !end.Ended || !end.Complete {
		t.Errorf("the end of the session: %d %s", code, answer)
	}
	for what, c := range map[string]struct {
		id, token, query string
		want             int
	}{
		"without the token":         {session, "", "", 401},
		"with a wrong token":        {session, "wrong", "", 401},
		"an id that is none":        {"xyz", b.cfg.Token, "", 400},
		"a wait that is no number":  {session, b.cfg.Token, "?wait=soon", 400},
		"a session that never was":  {strings.Repeat("0e", 16), b.cfg.Token, "?wait=1", 404},
		"the session, a short wait": {session, b.cfg.Token, "?wait=0", 200},
	} {
		if code, answer := ask(c.id, c.token, c.query); code != c.want {
			t.Errorf("%s: %d %s, want %d", what, code, answer, c.want)
		}
	}

	// the destination that the peer's allow list does not hold
	if w := a.send("PUT", PathPeers+"/beta", `{"allow":"192.0.2.0/24"}`); w.Code != 200 {
		t.Fatalf("setting the allow list: %d %s", w.Code, w.Body)
	}
	_, _, resp = askConnect(t, addr, b.cfg.Token, body("127.0.0.1", dst))
	if why := refusal(t, resp, 403); !strings.Contains(why, "not in the allow list") {
		t.Errorf("a destination outside the allow list: %q", why)
	}
	noSecret(t, "the answers of connect", fmt.Sprint(resp.Header), a, b)
}

// A stream that is cut off ends with an error at the program, never with the plain end
// that a complete stream has.
func TestConnectCutIsNotAnEnd(t *testing.T) {
	a, b := newPeerNode(t, "alpha"), newPeerNode(t, "beta")
	pairOver(t, a, b)
	online(t, b, "alpha")
	online(t, a, "beta")
	srv := httptest.NewServer(b.Server)
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	dst := echo(t, "ready\n")
	c, br, resp := askConnect(t, addr, b.cfg.Token, jsonBody(ConnectRequest{Peer: "alpha", Host: "127.0.0.1", Port: dst}))
	if resp.StatusCode != 101 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	line, err := br.ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("the greeting: %q, %v", line, err)
	}
	if _, err := c.Write([]byte("more\n")); err != nil {
		t.Fatal(err)
	}
	if line, err = br.ReadString('\n'); err != nil || line != "more\n" {
		t.Fatalf("the echo: %q, %v", line, err)
	}
	// the pairing ends at alpha: the session is cut
	if w := a.send("DELETE", PathPeers+"/beta", ""); w.Code != 200 && w.Code != 204 {
		t.Fatalf("removing the peer: %d %s", w.Code, w.Body)
	}
	rest, err := io.ReadAll(br)
	if err == nil {
		t.Fatalf("after the cut the program read %q and a plain end", rest)
	}
}

// A SOCKS5 rule through the API: made with socks, stored with its address, kept as it
// is by a caller that does not know the field and only switches the rule.
func TestSocksRuleRoutes(t *testing.T) {
	a, b := newPeerNode(t, "alpha"), newPeerNode(t, "beta")
	pairOver(t, a, b)
	_, port, _ := strings.Cut(freePort(t), ":")
	made := decode[Rule](t, b.send("POST", PathRules, `{"name":"web","peer":"alpha","socks":":`+port+`"}`), 201)
	if made.Socks != "127.0.0.1:"+port || made.Listen != "" || made.Destination != "" || !made.Listening {
		t.Fatalf("the rule as made: %+v", made)
	}
	// what a caller of an older version sends to switch a rule off and on: no socks
	off := decode[Rule](t, b.send("PUT", PathRules+"/web", `{"name":"web","peer":"alpha","enabled":false}`), 200)
	if off.Socks != made.Socks || off.Enabled || off.Listening {
		t.Fatalf("the rule switched off without naming socks: %+v", off)
	}
	on := decode[Rule](t, b.send("PUT", PathRules+"/web", `{"name":"web","peer":"alpha","enabled":true}`), 200)
	if on.Socks != made.Socks || !on.Listening {
		t.Fatalf("the rule switched on again: %+v", on)
	}
	wantError(t, "two kinds", b.send("POST", PathRules, `{"name":"x","peer":"alpha","socks":":1081","match":"192.0.2.0/24"}`), 400)
	wantError(t, "a range", b.send("POST", PathRules, `{"name":"x","peer":"alpha","socks":"127.0.0.1:1081-1082"}`), 400)
	wantError(t, "a name", b.send("POST", PathRules, `{"name":"x","peer":"alpha","socks":"localhost:1081"}`), 400)
	list := decode[[]Rule](t, b.send("GET", PathRules, ""), 200)
	if len(list) != 1 || list[0].Socks != made.Socks {
		t.Fatalf("the rules: %+v", list)
	}
}
