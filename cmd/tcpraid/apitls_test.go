package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/api"
	"github.com/Claw-King/tcpraid/internal/node"
)

// trusting returns a client that trusts the certificate cert the way a program does
// that is given the file as the one certificate to trust: the chain and the name are
// checked.
func trusting(cert *x509.Certificate) *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}, Timeout: 5 * time.Second}
}

// A node started with -api-tls serves its API and its page over TLS with a certificate
// of its own. The commands on the host follow without being told, a program that trusts
// the recorded certificate reaches the API by the loopback address, a request without
// TLS is not served, and the cookie of a login is for TLS only. Started again without
// the switch, the node is as before.
func TestAPIOverTLS(t *testing.T) {
	t.Setenv(uiPasswordEnv, "a password for the test")
	var logged lockedBuffer
	old := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(old)

	dir := t.TempDir()
	run := func(o runOptions) (stop func()) {
		t.Helper()
		os.Remove(filepath.Join(dir, node.AddrFile)) // so that the wait sees the new one
		stop = runFor(t, o)
		waitAddr(t, dir)
		return stop
	}
	stop := run(runOptions{dataDir: dir, apiAddr: "127.0.0.1:0", name: "alpha", apiTLS: true})
	addr := waitAddr(t, dir)
	cert, err := node.ReadAPITLS(dir)
	if err != nil {
		t.Fatalf("the certificate the node recorded: %v", err)
	}

	// the commands
	if out := must(t, "status", "-data", dir); !strings.Contains(out, "alpha") {
		t.Errorf("tcpraid status: %q", out)
	}
	must(t, "peers", "-data", dir)
	must(t, "rules", "-data", dir)
	must(t, "transport", "-data", dir)
	must(t, "transport", "set", "-window", "64MB", "-data", dir)

	// a program that is given the file
	client := trusting(cert)
	resp, err := client.Get("https://" + addr + api.PathHealth)
	if err != nil {
		t.Fatalf("with the recorded certificate as the one to trust: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "ok" || resp.TLS == nil {
		t.Fatalf("health over TLS: %d %q", resp.StatusCode, body)
	}
	// without TLS nothing of the API is served
	plain, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + addr + api.PathHealth)
	if err == nil {
		b, _ := io.ReadAll(plain.Body)
		plain.Body.Close()
		if plain.StatusCode == 200 || string(b) == "ok" {
			t.Fatalf("a request without TLS was served: %d %q", plain.StatusCode, b)
		}
	}
	// the login over TLS
	req, _ := http.NewRequest("POST", "https://"+addr+api.PathLogin, strings.NewReader(`{"password":"a password for the test"}`))
	req.Header.Set(api.RequestHeader, "1")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	cookies := resp.Cookies()
	if resp.StatusCode != 204 || len(cookies) != 1 || !cookies[0].Secure || cookies[0].Name != api.SessionCookieName(addr) {
		t.Fatalf("login over TLS: %d, cookies %+v", resp.StatusCode, cookies)
	}

	out := logged.String()
	for _, want := range []string{"open https://" + addr, "SHA-256 fingerprint of the certificate: " + node.CertificateSHA256(cert)} {
		if !strings.Contains(out, want) {
			t.Errorf("the log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "plain HTTP") || strings.Contains(out, "open http://") {
		t.Errorf("the log of a node with TLS speaks of plain HTTP:\n%s", out)
	}
	for _, secret := range []string{"a password for the test", "PRIVATE KEY"} {
		if strings.Contains(out, secret) {
			t.Errorf("the log holds %q", secret)
		}
	}
	stop()

	// again with TLS: the same certificate, so what a browser was told to accept holds
	stop = run(runOptions{dataDir: dir, apiAddr: "127.0.0.1:0", name: "alpha", apiTLS: true})
	if again, err := node.ReadAPITLS(dir); err != nil || !again.Equal(cert) {
		t.Fatalf("a second start with TLS: another certificate, or %v", err)
	}
	must(t, "status", "-data", dir)
	stop()

	// without the switch: no record, plain HTTP, the commands follow again
	stop = run(runOptions{dataDir: dir, apiAddr: "127.0.0.1:0", name: "alpha"})
	defer stop()
	if _, err := os.Stat(filepath.Join(dir, node.APITLSFile)); !os.IsNotExist(err) {
		t.Fatalf("the record of the certificate after a start without TLS: %v", err)
	}
	must(t, "status", "-data", dir)
	resp, err = http.Get("http://" + waitAddr(t, dir) + api.PathHealth)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("health without TLS after the switch was taken off: %v", err)
	}
	resp.Body.Close()
}

// A command talks only to an API that holds the key of the certificate in the data
// directory. When the certificate there is another one, or the API speaks no TLS at
// all, the command fails and no request reaches the API: the token does not leave.
func TestCommandsSendNothingToAnotherCertificate(t *testing.T) {
	var requests atomic.Int32
	var sawToken atomic.Bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "" {
			sawToken.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.Status{Version: "9.9.9", Node: api.StatusNode{Name: "other", Fingerprint: strings.Repeat("a", 64)}})
	})
	srv := httptest.NewTLSServer(handler)
	defer srv.Close()
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the handshakes that fail below

	dir := t.TempDir()
	write := func(name string, content []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(node.AddrFile, []byte(strings.TrimPrefix(srv.URL, "https://")))
	write(node.TokenFile, []byte(strings.Repeat("ab", 32)))
	// a certificate that is not the server's
	other, err := node.APICertificate(t.TempDir(), []string{"localhost", "127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	write(node.APITLSFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: other.Leaf.Raw}))

	for _, args := range [][]string{{"status", "-data", dir}, {"peers", "-data", dir}, {"rules", "-data", dir}, {"transport", "-data", dir}} {
		code, _, stdout, stderr := call(args...)
		if code == 0 || stdout != "" || stderr == "" {
			t.Errorf("tcpraid %s against another certificate: code %d, stdout %q, stderr %q", strings.Join(args, " "), code, stdout, stderr)
		}
		if strings.Contains(stderr, strings.Repeat("ab", 32)) {
			t.Errorf("tcpraid %s: the message holds the token", strings.Join(args, " "))
		}
	}
	if _, _, _, stderr := call("status", "-data", dir); !strings.Contains(stderr, node.APITLSFile) {
		t.Errorf("the message does not name the file with the certificate: %q", stderr)
	}
	if n := requests.Load(); n != 0 || sawToken.Load() {
		t.Fatalf("%d requests reached an API with another certificate (token seen: %v)", n, sawToken.Load())
	}

	// the same directory with the certificate the server has: the commands get through
	write(node.APITLSFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	if out := must(t, "status", "-data", dir); !strings.Contains(out, "other") {
		t.Errorf("status with the right certificate: %q", out)
	}
	if requests.Load() == 0 || !sawToken.Load() {
		t.Fatal("with the right certificate no request with the token arrived")
	}

	// a certificate on record, and an API that speaks no TLS: nothing arrives there
	requests.Store(0)
	sawToken.Store(false)
	plain := httptest.NewServer(handler)
	defer plain.Close()
	plain.Config.ErrorLog = log.New(io.Discard, "", 0)
	write(node.AddrFile, []byte(strings.TrimPrefix(plain.URL, "http://")))
	if code, _, _, stderr := call("status", "-data", dir); code == 0 || stderr == "" {
		t.Errorf("status against an API without TLS while a certificate is on record: code %d", code)
	}
	if n := requests.Load(); n != 0 || sawToken.Load() {
		t.Fatalf("%d requests reached an API without TLS (token seen: %v)", n, sawToken.Load())
	}
}

// With -api-cert and -api-key the API presents the certificate that was given, the
// node makes none of its own, and the commands follow.
func TestAPIWithAGivenCertificate(t *testing.T) {
	given := t.TempDir()
	c, err := node.APICertificate(given, []string{"localhost", "127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	startNodeWith(t, runOptions{dataDir: dir, name: "alpha", apiCertFile: filepath.Join(given, node.APICertFile), apiKeyFile: filepath.Join(given, node.APIKeyFile)})
	recorded, err := node.ReadAPITLS(dir)
	if err != nil || !recorded.Equal(c.Leaf) {
		t.Fatalf("the certificate on record is not the one given: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, node.APIKeyFile)); !os.IsNotExist(err) {
		t.Errorf("the node made a key of its own beside the one given: %v", err)
	}
	if out := must(t, "status", "-data", dir); !strings.Contains(out, "alpha") {
		t.Errorf("tcpraid status: %q", out)
	}
	resp, err := trusting(c.Leaf).Get("https://" + waitAddr(t, dir) + api.PathHealth)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("health with the given certificate as the one to trust: %v", err)
	}
	resp.Body.Close()

	// one without the other is refused before anything starts, and so is a file that
	// is no certificate
	for _, args := range [][]string{{"-api-cert", "/x"}, {"-api-key", "/y"}} {
		if _, code, done, stderr := runFlagsOf(append([]string{"-data", "/d"}, args...)...); !done || code != 2 || !strings.Contains(stderr, "-api-cert and -api-key go together") {
			t.Errorf("%v: done=%v code=%d stderr=%q", args, done, code, stderr)
		}
	}
	junk := filepath.Join(t.TempDir(), "junk.pem")
	if err := os.WriteFile(junk, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = runNodeWith(t.Context(), runOptions{dataDir: t.TempDir(), apiAddr: "127.0.0.1:0", apiCertFile: junk, apiKeyFile: junk})
	if err == nil || !strings.Contains(err.Error(), "-api-cert") {
		t.Errorf("a certificate file that is none: %v", err)
	}
	if o, _, done, _ := runFlagsOf("-data", "/d", "-api-tls"); done || !o.apiTLS {
		t.Errorf("-api-tls alone: done=%v apiTLS=%v", done, o.apiTLS)
	}
}

// The status names the address the node takes other nodes at and the addresses it was
// started with for them, which the page puts into the form for a pairing code.
func TestStatusNamesTheAddresses(t *testing.T) {
	dir := t.TempDir()
	startNodeWith(t, runOptions{dataDir: dir, name: "alpha", listen: "127.0.0.1:0", advertise: stringList{"nas.example:9851", "192.0.2.5:9851"}})
	var st api.Status
	if err := json.Unmarshal([]byte(must(t, "status", "-data", dir, "-json")), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Listen) != 1 || !strings.HasPrefix(st.Listen[0], "127.0.0.1:") || strings.HasSuffix(st.Listen[0], ":0") {
		t.Errorf("listen %v, want the bound address", st.Listen)
	}
	if strings.Join(st.Advertise, " ") != "nas.example:9851 192.0.2.5:9851" {
		t.Errorf("advertise %v", st.Advertise)
	}

	bare := t.TempDir()
	startNodeWith(t, runOptions{dataDir: bare, name: "beta"})
	var raw map[string]any
	out := must(t, "status", "-data", bare, "-json")
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"listen", "advertise"} {
		if l, ok := raw[field].([]any); !ok || len(l) != 0 {
			t.Errorf("a node that cannot be joined: %s is %v, want an empty list\n%s", field, raw[field], out)
		}
	}
}
