package node

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// mustOpen opens dir and releases its lock again, so a test can open it once more; what
// was loaded stays in the returned Node.
func mustOpen(t *testing.T, dir, name string) *Node {
	t.Helper()
	n, err := Open(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	return n
}

// snapshot returns every entry of dir with its mode and content.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		m[e.Name()] = fmt.Sprintf("%v %q", perm(t, path), read(t, path))
	}
	return m
}

func sameFiles(t *testing.T, what string, before, after map[string]string) {
	t.Helper()
	for name, b := range before {
		if a, ok := after[name]; !ok {
			t.Errorf("%s: %s is gone", what, name)
		} else if a != b {
			t.Errorf("%s: %s changed\n from %s\n   to %s", what, name, b, a)
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			t.Errorf("%s: %s was created", what, name)
		}
	}
}

func perm(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// wrongMode says whether the file at path has another mode than want. Where the mode
// bits do not express access (Windows), no mode is wrong.
func wrongMode(t *testing.T, path string, want os.FileMode) bool {
	t.Helper()
	return modesMeanAccess && perm(t, path) != want
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The first Open creates the directory and every file with its permissions; the second
// one reads the same identity and token back.
func TestOpenCreatesThenLoads(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub", "data")
	a := mustOpen(t, dir, "alpha")
	for name, want := range map[string]os.FileMode{
		".": 0o700, KeyFile: 0o600, CertFile: 0o644, StateFile: 0o600, TokenFile: 0o600,
	} {
		if wrongMode(t, filepath.Join(dir, name), want) {
			t.Errorf("%s: mode %o, want %o", name, perm(t, filepath.Join(dir, name)), want)
		}
	}
	fp := a.Identity.Fingerprint()
	if len(fp) != 64 || strings.ToLower(fp) != fp {
		t.Fatalf("fingerprint %q: want 64 lower-case hex digits", fp)
	}
	spki, err := x509.MarshalPKIXPublicKey(a.Identity.Key.Public())
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(spki); fp != hex.EncodeToString(sum[:]) {
		t.Fatalf("fingerprint %s is not the SHA-256 of the public key's SubjectPublicKeyInfo", fp)
	}
	if raw, err := hex.DecodeString(a.Token); err != nil || len(raw) != 32 {
		t.Fatalf("token %q: want 32 bytes in hex", a.Token)
	}
	if got := string(read(t, filepath.Join(dir, TokenFile))); got != a.Token {
		t.Fatalf("token file holds %q, the node uses %q", got, a.Token)
	}
	keyBefore := read(t, filepath.Join(dir, KeyFile))
	certBefore := read(t, filepath.Join(dir, CertFile))

	b := mustOpen(t, dir, "alpha")
	if b.Identity.Fingerprint() != fp {
		t.Fatalf("fingerprint changed on reload: %s then %s", fp, b.Identity.Fingerprint())
	}
	if b.Token != a.Token {
		t.Fatal("token changed on reload")
	}
	if !bytes.Equal(keyBefore, read(t, filepath.Join(dir, KeyFile))) || !bytes.Equal(certBefore, read(t, filepath.Join(dir, CertFile))) {
		t.Fatal("identity files were rewritten on reload")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.tmp*")); len(left) != 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
}

// The stored key file is PKCS#8 in PEM and the certificate is signed by that key.
func TestIdentityFileFormats(t *testing.T) {
	dir := t.TempDir()
	n := mustOpen(t, dir, "alpha")
	block, rest := pem.Decode(read(t, filepath.Join(dir, KeyFile)))
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatal("identity.key is not one PRIVATE KEY PEM block")
	}
	if _, err := x509.ParsePKCS8PrivateKey(block.Bytes); err != nil {
		t.Fatalf("identity.key is not PKCS#8: %v", err)
	}
	block, _ = pem.Decode(read(t, filepath.Join(dir, CertFile)))
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatal("identity.crt is not a CERTIFICATE PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	// the node's own key signed it (it is a leaf certificate, not a certificate authority)
	if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
		t.Fatalf("the certificate is not signed by its own key: %v", err)
	}
	if cert.IsCA {
		t.Fatal("the certificate claims to be a certificate authority")
	}
	if Fingerprint(cert) != n.Identity.Fingerprint() {
		t.Fatal("Fingerprint of the stored certificate differs from the node's")
	}
}

// A key file that cannot be used is an error and keeps its content.
func TestBadKeyIsNotOverwritten(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(ec)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		"garbage":       []byte("not a key\n"),
		"empty":         {},
		"truncated PEM": []byte("-----BEGIN PRIVATE KEY-----\nMC4CAQAwBQYDK2VwBCIE\n"),
		"wrong type":    pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER}),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			mustOpen(t, dir, "alpha")
			keyPath := filepath.Join(dir, KeyFile)
			if err := os.WriteFile(keyPath, content, 0o600); err != nil {
				t.Fatal(err)
			}
			certBefore := read(t, filepath.Join(dir, CertFile))
			if _, err := Open(dir, "alpha"); err == nil {
				t.Fatal("Open accepted the key file")
			}
			if !bytes.Equal(read(t, keyPath), content) {
				t.Fatal("the key file was changed")
			}
			if !bytes.Equal(read(t, filepath.Join(dir, CertFile)), certBefore) {
				t.Fatal("the certificate was changed")
			}
		})
	}
}

// A key file that exists but cannot be read is an error too, not a reason to make a new one.
func TestUnreadableKeyIsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	// a directory in the key's place cannot be read as a file, whoever runs the test
	if err := os.Mkdir(filepath.Join(dir, KeyFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, "alpha"); err == nil {
		t.Fatal("Open accepted an unreadable key")
	}
	if fi, err := os.Stat(filepath.Join(dir, KeyFile)); err != nil || !fi.IsDir() {
		t.Fatal("the unreadable key was replaced")
	}
	if _, err := os.Stat(filepath.Join(dir, CertFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a certificate was created without a usable key")
	}
}

func TestCertificateCases(t *testing.T) {
	// missing: issued again for the same key, same fingerprint
	dir := t.TempDir()
	fp := mustOpen(t, dir, "alpha").Identity.Fingerprint()
	keyBefore := read(t, filepath.Join(dir, KeyFile))
	if err := os.Remove(filepath.Join(dir, CertFile)); err != nil {
		t.Fatal(err)
	}
	if got := mustOpen(t, dir, "alpha").Identity.Fingerprint(); got != fp {
		t.Fatalf("fingerprint %s after the certificate was issued again, was %s", got, fp)
	}
	if !bytes.Equal(keyBefore, read(t, filepath.Join(dir, KeyFile))) {
		t.Fatal("the key changed when the certificate was issued again")
	}

	// a certificate of another key: refused, both files kept
	other := t.TempDir()
	mustOpen(t, other, "beta")
	foreign := read(t, filepath.Join(other, CertFile))
	if err := os.WriteFile(filepath.Join(dir, CertFile), foreign, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, "alpha"); err == nil {
		t.Fatal("Open accepted a certificate that does not belong to the key")
	}
	if !bytes.Equal(read(t, filepath.Join(dir, CertFile)), foreign) || !bytes.Equal(read(t, filepath.Join(dir, KeyFile)), keyBefore) {
		t.Fatal("identity files changed after a refused certificate")
	}

	// a certificate without its key: no new identity is made over it
	if err := os.Remove(filepath.Join(dir, KeyFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, "alpha"); err == nil {
		t.Fatal("Open made a new identity over a certificate without a key")
	}
	if _, err := os.Stat(filepath.Join(dir, KeyFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a key was created next to an existing certificate")
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), StateFile)
	s := NewState("alpha")
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	if wrongMode(t, path, 0o600) {
		t.Fatalf("mode %o, want 600", perm(t, path))
	}
	var generic map[string]any
	if err := json.Unmarshal(read(t, path), &generic); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"schema": float64(SchemaVersion), "node": map[string]any{"name": "alpha"}, "peers": []any{}, "pairing_codes": []any{}, "rules": []any{}}
	if got, _ := json.Marshal(generic); string(got) != string(mustJSON(t, want)) {
		t.Fatalf("state file holds %s", got)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Schema != SchemaVersion || got.Node.Name != "alpha" || got.Peers == nil || len(got.Peers) != 0 || got.Rules == nil || len(got.Rules) != 0 {
		t.Fatalf("loaded %+v", got)
	}

	// list elements pass through a load and a save untouched
	fp1, fp2 := strings.Repeat("a", 64), strings.Repeat("b", 64)
	secret := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" // 32 zero bytes
	withItems := `{"schema":3,"node":{"name":"alpha"},"peers":[` +
		`{"name":"one","fingerprint":"` + fp1 + `","pair_secret":"` + secret + `","addresses":["192.0.2.1:9851"],"paired_at":"2001-02-03T04:05:06Z",` +
		`"paths":[{"name":"default","type":"tcp","addr":"192.0.2.1:9851","security":"tls","roles":["data"],"subs":8}],` +
		`"allow":"198.51.100.0/24:873","udp_dial":"192.0.2.1:9852"},` +
		`{"name":"two","fingerprint":"` + fp2 + `","pair_secret":"` + secret + `","addresses":[],"paired_at":"2001-02-03T04:05:07Z","paths":[]}],` +
		`"pairing_codes":[{"id":"0a0b0c0d","secret_hash":"` + secret + `","expires":"2001-02-03T05:05:06Z","used":false}],` +
		`"rules":[{"name":"r1","listen":"0.0.0.0:5000-5002","destination":"198.51.100.7:6000-6002","peer":"one","enabled":true},` +
		`{"name":"t1","match":"198.51.100.0/24:22","peer":"two","enabled":false}]}`
	if err := os.WriteFile(path, []byte(withItems), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err = LoadState(path); err != nil {
		t.Fatal(err)
	}
	if len(got.Peers) != 2 || len(got.Rules) != 2 || len(got.PairingCodes) != 1 {
		t.Fatalf("%d peers, %d rules, %d pairing codes", len(got.Peers), len(got.Rules), len(got.PairingCodes))
	}
	if err := got.Save(path); err != nil {
		t.Fatal(err)
	}
	var a, b any
	json.Unmarshal([]byte(withItems), &a)
	json.Unmarshal(read(t, path), &b)
	if string(mustJSON(t, a)) != string(mustJSON(t, b)) {
		t.Fatalf("content changed across load and save: %s", read(t, path))
	}

	if _, err := LoadState(filepath.Join(t.TempDir(), "none.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: %v, want fs.ErrNotExist", err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A state file this version cannot take over is an error, for LoadState and for Open, and
// keeps its content; Open creates nothing next to it.
func TestStateRefusedAndKept(t *testing.T) {
	for name, content := range map[string]string{
		"newer schema":                        `{"schema":4,"node":{"name":"alpha"},"peers":[],"pairing_codes":[],"rules":[],"extra":{}}`,
		"unknown field in the current schema": `{"schema":3,"node":{"name":"alpha"},"peers":[],"pairing_codes":[],"rules":[],"extra":1}`,
		"unknown field in schema 2":           `{"schema":2,"node":{"name":"alpha"},"peers":[],"pairing_codes":[],"rules":[],"extra":1}`,
		"rules in schema 2":                   `{"schema":2,"node":{"name":"alpha"},"peers":[],"pairing_codes":[],"rules":[{"r":true}]}`,
		"rules in schema 1":                   `{"schema":1,"node":{"name":"alpha"},"peers":[],"rules":[{"r":true}]}`,
		"a rule that is not one":              `{"schema":3,"node":{"name":"alpha"},"peers":[],"pairing_codes":[],"rules":[{"name":"r","listen":"0.0.0.0:1","destination":"example..net:1","peer":"x","enabled":true}]}`,
		"peers in schema 1":                   `{"schema":1,"node":{"name":"alpha"},"peers":[{"k":1}],"rules":[]}`,
		"peer without a secret":               `{"schema":2,"node":{"name":"alpha"},"peers":[{"name":"x","fingerprint":"` + strings.Repeat("a", 64) + `","pair_secret":"","addresses":[],"paired_at":"2001-02-03T04:05:06Z","paths":[]}],"pairing_codes":[],"rules":[]}`,
		"schema zero":                         `{"schema":0,"node":{"name":"alpha"},"peers":[],"rules":[]}`,
		"no schema":                           `{"node":{"name":"alpha"},"peers":[],"rules":[]}`,
		"unknown field":                       `{"schema":1,"node":{"name":"alpha"},"peers":[],"rules":[],"extra":1}`,
		"not JSON":                            `schema: 1`,
		"empty":                               ``,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, StateFile)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadState(path); err == nil {
				t.Fatal("LoadState accepted the file")
			}
			if _, err := Open(dir, "beta"); err == nil {
				t.Fatal("Open accepted the file")
			}
			if got := string(read(t, path)); got != content {
				t.Fatalf("the file was changed to %q", got)
			}
			for name := range snapshot(t, dir) {
				if name != StateFile && name != LockFile {
					t.Errorf("Open created %s in a directory it refused", name)
				}
			}
		})
	}
}

// A name that is given is used, and renames a node that had another one. Without a name
// the node keeps the one it has; only a directory without a state file takes the host
// name, with a random suffix that it keeps from then on.
func TestNodeName(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skip("this host has no name")
	}
	stored := func(dir string) string {
		t.Helper()
		s, err := LoadState(filepath.Join(dir, StateFile))
		if err != nil {
			t.Fatal(err)
		}
		return s.Node.Name
	}

	dir := t.TempDir()
	if n := mustOpen(t, dir, "alpha"); n.Name() != "alpha" || stored(dir) != "alpha" {
		t.Fatalf("opened as alpha: node %q, state file %q", n.Name(), stored(dir))
	}
	before := snapshot(t, dir)
	if n := mustOpen(t, dir, ""); n.Name() != "alpha" {
		t.Fatalf("opened without a name: node %q, want the stored name alpha", n.Name())
	}
	sameFiles(t, "opening without a name", before, snapshot(t, dir))
	if n := mustOpen(t, dir, "beta"); n.Name() != "beta" || stored(dir) != "beta" {
		t.Fatalf("opened as beta: node %q, state file %q", n.Name(), stored(dir))
	}
	if n := mustOpen(t, dir, ""); n.Name() != "beta" {
		t.Fatalf("opened without a name after the rename: node %q, want beta", n.Name())
	}

	fresh := t.TempDir()
	if CheckName(host) != nil {
		// the host name of this machine cannot be a node's name (too long, for one):
		// a node without a name does not start, and says what to do
		if _, err := Open(fresh, ""); err == nil || !strings.Contains(err.Error(), "Set -name") {
			t.Fatalf("new directory without a name on a host called %q: %v, want an error that says to set -name", host, err)
		}
		return
	}
	// a host name that would be too long with the suffix gives up its end
	if room := MaxNameLen - 1 - nameSuffixLen; len(host) > room {
		host = strings.TrimRight(cutName(host, room), "-._")
	}
	suffixed := regexp.MustCompile("^" + regexp.QuoteMeta(host) + "-[a-z0-9]{4}$")
	first := mustOpen(t, fresh, "").Name()
	if !suffixed.MatchString(first) || stored(fresh) != first {
		t.Fatalf("new directory without a name: node %q, state file %q, want the host name %q with a suffix", first, stored(fresh), host)
	}
	// the name stays: a second start makes no new suffix and rewrites nothing
	before = snapshot(t, fresh)
	if n := mustOpen(t, fresh, ""); n.Name() != first {
		t.Fatalf("started again without a name: node %q, want %q as before", n.Name(), first)
	}
	sameFiles(t, "starting again without a name", before, snapshot(t, fresh))
	// another new directory on the same host: another name
	if other := mustOpen(t, t.TempDir(), "").Name(); !suffixed.MatchString(other) || other == first {
		t.Fatalf("a second new directory on this host: node %q, the first was %q", other, first)
	}
}

// DefaultName adds a hyphen and four characters out of a-z and 0-9, and differs from one
// call to the next.
func TestDefaultName(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		name, err := DefaultName("nas")
		if err != nil || !regexp.MustCompile(`^nas-[a-z0-9]{4}$`).MatchString(name) {
			t.Fatalf("DefaultName: %q, %v", name, err)
		}
		seen[name] = true
	}
	if len(seen) < 45 {
		t.Errorf("50 names, only %d different ones", len(seen))
	}
	// a host name that leaves no room for the suffix gives up its end, at a character
	// and not in the middle of one, and without a joining character before the suffix
	for host, want := range map[string]string{
		strings.Repeat("a", 64):               strings.Repeat("a", 59),
		strings.Repeat("a", 58) + "-bcdef":    strings.Repeat("a", 58),
		strings.Repeat("a", 57) + "\u00e9xyz": strings.Repeat("a", 57) + "\u00e9",
		strings.Repeat("a", 58) + "\u00e9xyz": strings.Repeat("a", 58),
	} {
		name, err := DefaultName(host)
		if err != nil || len(name) > MaxNameLen || CheckName(name) != nil || !strings.HasPrefix(name, want+"-") || len(name) != len(want)+1+nameSuffixLen {
			t.Errorf("DefaultName(%q): %q, %v, want %q with a suffix", host, name, err, want)
		}
	}
}

func TestOpenNeedsADirectory(t *testing.T) {
	if _, err := Open("", "alpha"); err == nil || !strings.Contains(err.Error(), "data directory") {
		t.Fatalf("empty directory name: %v, want an error that names the data directory", err)
	}
}

// A second Open of a directory that is in use fails, says why, and changes nothing --
// whatever name it was given. The lock ends with Close.
func TestLock(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, dir)
	for _, name := range []string{"alpha", "beta", ""} {
		_, err := Open(dir, name)
		if err == nil || !strings.Contains(err.Error(), "in use by another tcpraid") || !strings.Contains(err.Error(), dir) {
			t.Fatalf("second Open as %q: %v, want the directory reported in use", name, err)
		}
	}
	sameFiles(t, "refused Open", before, snapshot(t, dir))
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(dir, "")
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	defer again.Close()
	if again.Identity.Fingerprint() != first.Identity.Fingerprint() || again.Name() != "alpha" {
		t.Fatalf("after the lock was released: fingerprint %s name %q", again.Identity.Fingerprint(), again.Name())
	}

	// a failed Open does not keep the lock
	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, StateFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := Open(bad, "alpha"); err == nil || strings.Contains(err.Error(), "in use") {
			t.Fatalf("Open of a refused directory, attempt %d: %v, want the state file's error", i+1, err)
		}
	}
}

// Many starts on one new directory at the same moment: one gets it, the others are told
// it is in use, and the directory holds the identity of the one that got it.
func TestLockManyAtOnce(t *testing.T) {
	for round := 0; round < 20; round++ {
		dir := filepath.Join(t.TempDir(), "data")
		const starters = 8
		nodes := make([]*Node, starters)
		errs := make([]error, starters)
		var wg sync.WaitGroup
		begin := make(chan struct{})
		for i := range nodes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-begin
				nodes[i], errs[i] = Open(dir, fmt.Sprintf("node%d", i))
			}()
		}
		close(begin)
		wg.Wait()
		var winner *Node
		for i, n := range nodes {
			switch {
			case n != nil && winner != nil:
				t.Fatalf("round %d: two starters opened the directory", round)
			case n != nil:
				winner = n
			case !strings.Contains(errs[i].Error(), "in use by another tcpraid"):
				t.Fatalf("round %d: starter %d failed with %v", round, i, errs[i])
			}
		}
		if winner == nil {
			t.Fatalf("round %d: no starter opened the directory: %v", round, errs)
		}
		winner.Close()
		after := mustOpen(t, dir, "")
		if after.Identity.Fingerprint() != winner.Identity.Fingerprint() || after.Token != winner.Token || after.Name() != winner.Name() {
			t.Fatalf("round %d: the directory holds %s / %q, the starter that got it has %s / %q", round,
				after.Identity.Fingerprint(), after.Name(), winner.Identity.Fingerprint(), winner.Name())
		}
	}
}

// createFile puts a file in place only where there is none.
func TestCreateFileIsExclusive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := createFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if wrongMode(t, path, 0o600) || string(read(t, path)) != "first" {
		t.Fatalf("created with mode %o and content %q", perm(t, path), read(t, path))
	}
	err := createFile(path, []byte("second"), 0o600)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("creating over an existing file: %v, want fs.ErrExist", err)
	}
	if got := string(read(t, path)); got != "first" {
		t.Fatalf("the existing file now holds %q", got)
	}
	// writeFile, for the files that are meant to be replaced, does replace
	if err := writeFile(path, []byte("third"), 0o600); err != nil || string(read(t, path)) != "third" {
		t.Fatalf("writeFile over an existing file: %v, content %q", err, read(t, path))
	}
	if left := snapshot(t, dir); len(left) != 1 {
		t.Fatalf("temporary files left behind: %v", left)
	}
}

// A key or a token that appears between the check and the creation is not replaced:
// the creation fails. (The lock keeps a second node out; this is the second guard.)
func TestKeyAndTokenAreCreatedNotReplaced(t *testing.T) {
	dir := t.TempDir()
	// a dangling link: reading it says "does not exist", yet the name is taken
	for _, name := range []string{KeyFile, TokenFile} {
		if err := os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(dir, name)); err != nil {
			if runtime.GOOS == "windows" {
				t.Skipf("no symbolic link could be made (it takes a privilege on Windows): %v", err)
			}
			t.Fatal(err)
		}
	}
	if _, err := loadOrCreateIdentity(dir, true); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("key creation over a taken name: %v, want fs.ErrExist", err)
	}
	if _, err := loadOrCreateToken(dir); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("token creation over a taken name: %v, want fs.ErrExist", err)
	}
	for _, name := range []string{KeyFile, TokenFile} {
		if fi, err := os.Lstat(filepath.Join(dir, name)); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			t.Fatalf("%s was replaced", name)
		}
	}
}

// A directory with a state file and no identity file has lost its identity: Open fails
// and makes nothing.
func TestStateWithoutIdentity(t *testing.T) {
	dir := t.TempDir()
	mustOpen(t, dir, "alpha")
	for _, name := range []string{KeyFile, CertFile} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshot(t, dir)
	for _, name := range []string{"alpha", "beta", ""} {
		_, err := Open(dir, name)
		if err == nil || !strings.Contains(err.Error(), "identity") {
			t.Fatalf("Open as %q: %v, want an error about the lost identity", name, err)
		}
	}
	sameFiles(t, "refused Open", before, snapshot(t, dir))

	// the same with nothing but a state file
	bare := t.TempDir()
	if err := NewState("alpha").Save(filepath.Join(bare, StateFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(bare, ""); err == nil {
		t.Fatal("Open made an identity for a directory that has only a state file")
	}
	for name := range snapshot(t, bare) {
		if name != StateFile && name != LockFile {
			t.Errorf("Open created %s", name)
		}
	}
}

func TestWarnings(t *testing.T) {
	if !modesMeanAccess {
		t.Skip("the mode bits do not express access on this system; nothing is reported")
	}
	dir := filepath.Join(t.TempDir(), "data")
	n := mustOpen(t, dir, "alpha")
	if ws := n.Warnings(); len(ws) != 0 {
		t.Fatalf("fresh directory: warnings %q", ws)
	}
	// a directory others can read but not write, the usual mode of a volume, is fine
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if ws := n.Warnings(); len(ws) != 0 {
		t.Fatalf("world-readable directory: warnings %q", ws)
	}
	// one that others can write to is not: they could replace the key
	if err := os.Chmod(dir, 0o775); err != nil {
		t.Fatal(err)
	}
	if ws := n.Warnings(); len(ws) != 1 || !strings.Contains(ws[0], dir) || !strings.Contains(ws[0], "writable") || !strings.Contains(ws[0], "0775") {
		t.Fatalf("group-writable directory: warnings %q", ws)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, KeyFile), 0o604); err != nil {
		t.Fatal(err)
	}
	if ws := n.Warnings(); len(ws) != 1 || !strings.Contains(ws[0], KeyFile) || !strings.Contains(ws[0], "0604") {
		t.Fatalf("world-readable key: warnings %q", ws)
	}
	if err := os.Chmod(filepath.Join(dir, TokenFile), 0o640); err != nil {
		t.Fatal(err)
	}
	if ws := n.Warnings(); len(ws) != 2 || !strings.Contains(ws[1], TokenFile) || !strings.Contains(ws[1], "0640") {
		t.Fatalf("group-readable token: warnings %q", ws)
	}
	// a warning, not a refusal
	mustOpen(t, dir, "")
}

func TestBadTokenIsNotOverwritten(t *testing.T) {
	for name, content := range map[string]string{"empty": "", "short": "abcd", "not hex": strings.Repeat("z", 64)} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			mustOpen(t, dir, "alpha")
			path := filepath.Join(dir, TokenFile)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(dir, "alpha"); err == nil {
				t.Fatal("Open accepted the token file")
			}
			if got := string(read(t, path)); got != content {
				t.Fatalf("the token file was changed to %q", got)
			}
		})
	}
}

func TestAPIAddr(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadAPIAddr(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("no file: %v, want fs.ErrNotExist", err)
	}
	if err := WriteAPIAddr(dir, "127.0.0.1:9850"); err != nil {
		t.Fatal(err)
	}
	if err := WriteAPIAddr(dir, "[2001:db8::1]:9850"); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadAPIAddr(dir); err != nil || got != "[2001:db8::1]:9850" {
		t.Fatalf("read %q, %v", got, err)
	}
	if wrongMode(t, filepath.Join(dir, AddrFile), 0o600) {
		t.Fatalf("mode %o, want 600", perm(t, filepath.Join(dir, AddrFile)))
	}
}

// createDirect is what createFile falls back to where hard links do not exist: it
// creates, and does not replace.
func TestCreateDirectIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := createDirect(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || (modesMeanAccess && fi.Mode().Perm() != 0o600) {
		t.Fatalf("mode %v, err %v", fi.Mode().Perm(), err)
	}
	err := createDirect(path, []byte("two"), 0o600)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second create: %v, want an error matching fs.ErrExist", err)
	}
	if got := string(read(t, path)); got != "one" {
		t.Fatalf("the file was changed to %q", got)
	}
}

func TestCheckName(t *testing.T) {
	for _, name := range []string{"a", "nas-1", "office NAS", strings.Repeat("x", MaxNameLen), "caf\u00e9",
		strings.Repeat("\u00e9", MaxNameLen/2)} { // the limit is in bytes: 32 two-byte characters fit
		if err := CheckName(name); err != nil {
			t.Errorf("CheckName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{"", " a", "a ", "a\nb", "a\tb", "a\x1b[2Jb", "a\x00", "\xff", strings.Repeat("x", MaxNameLen+1),
		strings.Repeat("\u00e9", MaxNameLen/2+1), // 33 two-byte characters do not fit
		"a\u2028b", "a\u2029b",                   // line and paragraph separators
		"a\u202eb", "a\u200bb", "\ufeffa", // directional override, zero width, byte order mark
		"a\u00a0", "a\u00a0b", // no-break space
	} {
		if err := CheckName(name); err == nil {
			t.Errorf("CheckName(%q) = nil, want an error", name)
		}
	}
}

// A name that cannot be a node's name is refused when the directory is opened, and
// nothing is written for it.
func TestOpenRefusesBadName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if _, err := Open(dir, "two\nlines"); err == nil {
		t.Fatal("Open accepted a name with a line break")
	}
	for _, f := range []string{KeyFile, CertFile, StateFile, TokenFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("%s was created for a refused name", f)
		}
	}
	// the directory is usable afterwards
	mustOpen(t, dir, "alpha")
}

// A refused name leaves an existing node as it is, whether it is given with -name or
// already stored, and the message says where the name came from.
func TestBadNameOnExistingNode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	mustOpen(t, dir, "alpha")
	statePath := filepath.Join(dir, StateFile)
	before := read(t, statePath)

	_, err := Open(dir, "bad\nname")
	if err == nil || !strings.Contains(err.Error(), "-name") {
		t.Fatalf("Open with a bad -name: %v", err)
	}
	if !bytes.Equal(read(t, statePath), before) {
		t.Fatal("a refused rename changed the state file")
	}

	// a state file that already holds a name the check refuses
	stored := bytes.Replace(before, []byte(`"alpha"`), []byte(`" alpha"`), 1)
	if err := os.WriteFile(statePath, stored, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Open(dir, "")
	if err == nil || !strings.Contains(err.Error(), StateFile) || !strings.Contains(err.Error(), "Set -name") {
		t.Fatalf("Open with a bad stored name: %v", err)
	}
	if !bytes.Equal(read(t, statePath), stored) {
		t.Fatal("a refused stored name changed the state file")
	}
	// -name recovers the node
	n := mustOpen(t, dir, "beta")
	if n.Name() != "beta" {
		t.Fatalf("name %q after recovery", n.Name())
	}
}
