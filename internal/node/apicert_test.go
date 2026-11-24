package node

import (
	"crypto/x509"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The node's own API certificate is made once and kept: a second start gets the same
// one. It names what it was asked to name, and a program that trusts the file accepts
// it for those names.
func TestAPICertificateIsMadeOnceAndKept(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	names := []string{"localhost", "127.0.0.1", "::1", "nas-a"}
	first, err := APICertificate(dir, names, now)
	if err != nil {
		t.Fatal(err)
	}
	again, err := APICertificate(dir, names, now.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !first.Leaf.Equal(again.Leaf) {
		t.Fatal("a second start made another certificate")
	}
	// fewer names than it has: still the same one
	if fewer, err := APICertificate(dir, []string{"localhost"}, now); err != nil || !fewer.Leaf.Equal(first.Leaf) {
		t.Fatalf("asked for fewer names: another certificate, or %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(first.Leaf)
	for _, name := range names {
		if _, err := first.Leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: name, CurrentTime: now}); err != nil {
			t.Errorf("the certificate is not accepted for %s: %v", name, err)
		}
	}
	if _, err := first.Leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "other.example", CurrentTime: now}); err == nil {
		t.Error("the certificate is accepted for a name it was not made for")
	}
	if got := first.Leaf.NotAfter.Sub(now); got < 700*24*time.Hour || got > 825*24*time.Hour {
		t.Errorf("the certificate is valid for %v", got)
	}
	if fi, err := os.Stat(filepath.Join(dir, APIKeyFile)); err != nil || (fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/') {
		t.Errorf("the key file: %v, mode %v", err, fi.Mode())
	}
}

// A new certificate is issued for the same key when the one in the directory lacks a
// name, has little time left, or is gone.
func TestAPICertificateIsIssuedAgainForTheSameKey(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	first, err := APICertificate(dir, []string{"localhost"}, now)
	if err != nil {
		t.Fatal(err)
	}
	more, err := APICertificate(dir, []string{"localhost", "192.0.2.7"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if more.Leaf.Equal(first.Leaf) || Fingerprint(more.Leaf) != Fingerprint(first.Leaf) {
		t.Fatal("one more name: want another certificate for the same key")
	}
	if len(more.Leaf.IPAddresses) != 1 || more.Leaf.IPAddresses[0].String() != "192.0.2.7" {
		t.Fatalf("addresses in the certificate: %v", more.Leaf.IPAddresses)
	}
	late := now.Add(apiCertLife - apiCertRenew + time.Hour)
	renewed, err := APICertificate(dir, []string{"localhost", "192.0.2.7"}, late)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Leaf.Equal(more.Leaf) || Fingerprint(renewed.Leaf) != Fingerprint(first.Leaf) || !renewed.Leaf.NotAfter.After(late.Add(apiCertRenew)) {
		t.Fatal("little time left: want another certificate for the same key, valid for long")
	}
	if err := os.Remove(filepath.Join(dir, APICertFile)); err != nil {
		t.Fatal(err)
	}
	back, err := APICertificate(dir, []string{"localhost"}, late)
	if err != nil || Fingerprint(back.Leaf) != Fingerprint(first.Leaf) {
		t.Fatalf("the certificate file gone: %v, or another key", err)
	}
}

// A key file that is not an ECDSA key is an error and stays as it is.
func TestAPICertificateLeavesAStrangeKeyAlone(t *testing.T) {
	dir := t.TempDir()
	n, err := Open(dir, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	n.Close()
	// the identity key is an Ed25519 key
	b, err := os.ReadFile(filepath.Join(dir, KeyFile))
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{"another kind of key": b, "no key": []byte("not a key\n")} {
		if err := os.WriteFile(filepath.Join(dir, APIKeyFile), content, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := APICertificate(dir, []string{"localhost"}, time.Now()); err == nil || !strings.Contains(err.Error(), "left as it is") {
			t.Errorf("%s: %v", name, err)
		}
		if got, _ := os.ReadFile(filepath.Join(dir, APIKeyFile)); string(got) != string(content) {
			t.Errorf("%s: the file was changed", name)
		}
	}
}

func TestAPITLSRecord(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadAPITLS(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("no record: %v, want an error that says so", err)
	}
	if err := RemoveAPITLS(dir); err != nil {
		t.Fatalf("taking away a record that is not there: %v", err)
	}
	c, err := APICertificate(dir, []string{"localhost"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteAPITLS(dir, c.Leaf); err != nil {
		t.Fatal(err)
	}
	got, err := ReadAPITLS(dir)
	if err != nil || !got.Equal(c.Leaf) {
		t.Fatalf("the record read back: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, APITLSFile), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAPITLS(dir); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a record that is no certificate: %v", err)
	}
	if err := RemoveAPITLS(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAPITLS(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("after taking the record away: %v", err)
	}
	if fp := CertificateSHA256(c.Leaf); !regexp.MustCompile(`^([0-9A-F]{2}:){31}[0-9A-F]{2}$`).MatchString(fp) {
		t.Errorf("fingerprint %q", fp)
	}
}
