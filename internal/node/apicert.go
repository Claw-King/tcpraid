package node

// The certificate of the local API, for a node that serves its API and its web
// interface over TLS.
//
//	api.key  ECDSA P-256 private key of the node's own API certificate, PKCS#8, PEM (0600)
//	api.crt  self-signed X.509 certificate for that key, PEM (0644)
//	api.tls  the certificate the API presents now, PEM (0600). It is there only while
//	         the node was last started with TLS: the commands on this host read it to
//	         know that the API speaks TLS, and they talk only to an API that holds the
//	         key of this certificate.
//
// The API has a key of its own and does not present the node's identity. The identity
// is an Ed25519 key. RFC 8446 (section 9.1) obliges every implementation of TLS 1.3 to
// support signatures with ECDSA and P-256 and does not oblige it to support Ed25519,
// so the API presents a key of the first kind to the programs that connect to it.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
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
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// More file names in the data directory.
const (
	APIKeyFile  = "api.key"
	APICertFile = "api.crt"
	APITLSFile  = "api.tls"
)

const (
	// apiCertLife is how long the node's own API certificate is valid.
	apiCertLife = 800 * 24 * time.Hour
	// apiCertRenew: a start that finds less than this left issues a new certificate
	// for the same key.
	apiCertRenew = 30 * 24 * time.Hour
)

// APICertificate returns the node's own certificate for its API, with the key, made if
// the data directory dir has none. The certificate names the given host names and IP
// addresses. The one in the directory is kept as long as it belongs to the key, names
// all of them and has more than 30 days left; otherwise a new one is issued for the
// same key, so the fingerprint of the key stays. The key is written without replacing a
// file, and a key file that cannot be read or parsed is an error.
func APICertificate(dir string, names []string, now time.Time) (tls.Certificate, error) {
	keyPath, certPath := filepath.Join(dir, APIKeyFile), filepath.Join(dir, APICertFile)
	const kept = "The file is left as it is."
	var key *ecdsa.PrivateKey
	keyPEM, err := readBusy(keyPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			return tls.Certificate{}, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return tls.Certificate{}, err
		}
		if err := createFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
			return tls.Certificate{}, err
		}
	case err != nil:
		return tls.Certificate{}, fmt.Errorf("%w. %s", err, kept)
	default:
		block, _ := pem.Decode(keyPEM)
		if block == nil || block.Type != "PRIVATE KEY" {
			return tls.Certificate{}, fmt.Errorf("%s: no PEM block of type PRIVATE KEY. %s", keyPath, kept)
		}
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("%s: %w. %s", keyPath, err, kept)
		}
		var ok bool
		if key, ok = k.(*ecdsa.PrivateKey); !ok {
			return tls.Certificate{}, fmt.Errorf("%s: the key is %T, not an ECDSA key. %s", keyPath, k, kept)
		}
	}

	var dns []string
	var ips []net.IP
	for _, name := range names {
		if name == "" {
			continue
		}
		if ip := net.ParseIP(name); ip != nil {
			if !slices.ContainsFunc(ips, ip.Equal) {
				ips = append(ips, ip)
			}
		} else if !slices.Contains(dns, name) {
			dns = append(dns, name)
		}
	}
	if certPEM, err := readBusy(certPath); err == nil {
		if cert, err := parseCert(certPEM); err == nil && apiCertFits(cert, key, dns, ips, now) {
			return tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key, Leaf: cert}, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return tls.Certificate{}, fmt.Errorf("%w. %s", err, kept)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "tcpraid API"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(apiCertLife),
		// it is its own issuer, so that a program which is given the file as the
		// certificate to trust accepts the chain
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dns,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := writeFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return tls.Certificate{}, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}, nil
}

// apiCertFits reports whether a stored certificate can go on being used: it is for the
// key, it names every wanted name, and it is valid now and for a while yet.
func apiCertFits(cert *x509.Certificate, key *ecdsa.PrivateKey, dns []string, ips []net.IP, now time.Time) bool {
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(key.Public()) {
		return false
	}
	if now.Before(cert.NotBefore) || cert.NotAfter.Sub(now) < apiCertRenew {
		return false
	}
	for _, name := range dns {
		if !slices.Contains(cert.DNSNames, name) {
			return false
		}
	}
	for _, ip := range ips {
		if !slices.ContainsFunc(cert.IPAddresses, ip.Equal) {
			return false
		}
	}
	return true
}

// WriteAPITLS records the certificate the API presents, for the commands on this host.
func WriteAPITLS(dir string, leaf *x509.Certificate) error {
	return writeFile(filepath.Join(dir, APITLSFile), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw}), 0o600)
}

// RemoveAPITLS takes the record of WriteAPITLS away: the API is served without TLS.
func RemoveAPITLS(dir string) error {
	err := os.Remove(filepath.Join(dir, APITLSFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// ReadAPITLS returns the certificate recorded by WriteAPITLS. An error that matches
// fs.ErrNotExist means there is none: the API of the node is served without TLS.
func ReadAPITLS(dir string) (*x509.Certificate, error) {
	path := filepath.Join(dir, APITLSFile)
	b, err := readBusy(path)
	if err != nil {
		return nil, err
	}
	cert, err := parseCert(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w. The file is left as it is.", path, err)
	}
	return cert, nil
}

// CertificateSHA256 returns the SHA-256 of the whole certificate, in upper-case hex
// with a colon between the bytes: the form in which programs that show a certificate
// print its fingerprint. It names the certificate, where Fingerprint names its key.
func CertificateSHA256(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	var b strings.Builder
	for i := 0; i < len(h); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(h[i : i+2])
	}
	return b.String()
}
