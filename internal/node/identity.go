package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// Identity is the long-term key pair of a node and the self-signed certificate that
// carries its public key.
type Identity struct {
	Key  ed25519.PrivateKey
	Cert *x509.Certificate
}

// Fingerprint returns the fingerprint of the node's own certificate.
func (id *Identity) Fingerprint() string { return Fingerprint(id.Cert) }

// Fingerprint returns the SHA-256 of the certificate's public key (the DER encoding of
// its SubjectPublicKeyInfo) in lower-case hex. It names the key, not the certificate:
// another certificate for the same key has the same fingerprint.
func Fingerprint(cert *x509.Certificate) string {
	sum := FingerprintBytes(cert)
	return hex.EncodeToString(sum[:])
}

// FingerprintBytes returns the fingerprint of the certificate's public key as bytes.
func FingerprintBytes(cert *x509.Certificate) [sha256.Size]byte {
	return sha256.Sum256(cert.RawSubjectPublicKeyInfo)
}

// FingerprintBytes returns the fingerprint of the node's own certificate as bytes.
func (id *Identity) FingerprintBytes() [sha256.Size]byte { return FingerprintBytes(id.Cert) }

// TLSCertificate returns the identity in the form crypto/tls presents it to a peer.
func (id *Identity) TLSCertificate() tls.Certificate {
	return tls.Certificate{Certificate: [][]byte{id.Cert.Raw}, PrivateKey: id.Key, Leaf: id.Cert}
}

// loadOrCreateIdentity reads the identity in dir. The key is the identity: it is
// generated only when neither identity file exists and create allows it, it is written
// without replacing a file, and a key file that cannot be read or parsed is an error. A
// missing certificate is issued again for the existing key (the fingerprint does not
// change); a certificate without its key, or one that does not belong to the key, is an
// error.
func loadOrCreateIdentity(dir string, create bool) (*Identity, error) {
	keyPath := filepath.Join(dir, KeyFile)
	certPath := filepath.Join(dir, CertFile)
	const kept = "The file is left as it is."

	var key ed25519.PrivateKey
	keyPEM, err := os.ReadFile(keyPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if _, err := os.Lstat(certPath); err == nil {
			return nil, fmt.Errorf("%s exists without %s, so no new identity is created over it", certPath, keyPath)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if !create {
			return nil, fmt.Errorf("%s has a state file but neither %s nor %s. Its identity is lost, and a new one is not created in its place.", dir, KeyFile, CertFile)
		}
		if _, key, err = ed25519.GenerateKey(rand.Reader); err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, err
		}
		if err := createFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, fmt.Errorf("%w. %s", err, kept)
	default:
		if key, err = parseKey(keyPEM); err != nil {
			return nil, fmt.Errorf("%s: %w. %s", keyPath, err, kept)
		}
	}

	certPEM, err := os.ReadFile(certPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		der, err := selfSign(key)
		if err != nil {
			return nil, err
		}
		certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		if err := writeFile(certPath, certPEM, 0o644); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, fmt.Errorf("%w. %s", err, kept)
	}
	cert, err := parseCert(certPEM)
	if err != nil {
		return nil, fmt.Errorf("%s: %w. %s", certPath, err, kept)
	}
	if pub, ok := cert.PublicKey.(ed25519.PublicKey); !ok || !pub.Equal(key.Public()) {
		return nil, fmt.Errorf("%s does not belong to the key in %s. Both files are left as they are.", certPath, keyPath)
	}
	return &Identity{Key: key, Cert: cert}, nil
}

func parseKey(pemBytes []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("no PEM block of type PRIVATE KEY")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("the key is %T, not an Ed25519 key", k)
	}
	return key, nil
}

func parseCert(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no PEM block of type CERTIFICATE")
	}
	return x509.ParseCertificate(block.Bytes)
}

// selfSign issues a certificate for key, signed with key, and returns its DER encoding.
// Peers recognise a node by the fingerprint of its key, so the certificate only has to
// carry that key: it names no host and does not expire.
func selfSign(key ed25519.PrivateKey) ([]byte, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "tcpraid node"},
		NotBefore:    time.Now().Add(-time.Hour),
		// the value RFC 5280 (4.1.2.5) gives for a certificate with no well-defined expiry
		NotAfter:              time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	return x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
}
