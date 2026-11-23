package pair

// The pairing exchange, on a TLS 1.3 connection with the protocol name ALPN.
//
// The joining node connects, shows its identity certificate and checks that the
// certificate it is shown has the fingerprint in the pairing code: it is talking to the
// node that made the code. It then sends
//
//	id (4) | proof (32) | n (1) | name (n)
//
// where the proof is HMAC-SHA256 keyed with the SHA-256 of the code's one-time secret
// over 32 bytes of keying material exported from this TLS connection. The proof shows
// that the joining node holds the code and is good for this connection only. The node
// that made the code answers
//
//	status (1) | n (1) | name (n)
//
// with status 0 when it accepted the proof (the name is its own), anything else when it
// did not (no name follows). Both ends then take the pair secret, 32 more bytes exported
// from the same connection under another label: they share it without ever sending it.

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/Claw-King/tcpraid/internal/node"
)

const (
	// ALPN is the TLS protocol name of a pairing connection.
	ALPN = "tcpraid-pair/1"
	// Timeout is how long a pairing connection may take from start to end.
	Timeout = 15 * time.Second

	proofLabel  = "tcpraid pairing proof v1"
	secretLabel = "tcpraid pair secret v1"

	statusOK      = 0
	statusRefused = 1
)

// ErrRefused: the node that made the code did not accept it. It does not say why: the
// code may be wrong, used, expired or revoked.
var ErrRefused = errors.New("the other node did not accept the pairing code. It is wrong, was used, has expired or was revoked.")

// Result is what a completed pairing gives each node about the other.
type Result struct {
	Name        string // the other node's name, checked with node.CheckName
	Fingerprint string // of the other node's identity key
	Secret      []byte // the pair secret, node.PairSecretLen bytes
}

// exported returns the proof material and the pair secret of an established connection.
func exported(conn *tls.Conn) (proofMaterial, secret []byte, err error) {
	cs := conn.ConnectionState()
	if proofMaterial, err = cs.ExportKeyingMaterial(proofLabel, nil, sha256.Size); err != nil {
		return nil, nil, err
	}
	if secret, err = cs.ExportKeyingMaterial(secretLabel, nil, node.PairSecretLen); err != nil {
		return nil, nil, err
	}
	return proofMaterial, secret, nil
}

func proof(secretHash, material []byte) []byte {
	m := hmac.New(sha256.New, secretHash)
	m.Write(material)
	return m.Sum(nil)
}

// Join runs the joining node's side of a pairing on conn, a fresh TCP connection to one
// of the code's addresses, and closes conn. cert is this node's identity, name its name.
func Join(conn net.Conn, cert tls.Certificate, code *Code, name string) (*Result, error) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(Timeout))
	tc := tls.Client(conn, &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{ALPN},
		// The certificate is not checked against authorities or a host name: the node is
		// known by its key, and VerifyConnection compares that with the code.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || node.FingerprintBytes(cs.PeerCertificates[0]) != code.Fingerprint {
				return errors.New("the node at this address is not the one that made the pairing code")
			}
			return nil
		},
	})
	if err := tc.Handshake(); err != nil {
		if errors.Is(err, io.EOF) || IsReset(err) {
			return nil, fmt.Errorf("%w. The other node closed the connection during the handshake and may be busy. Try again.", err)
		}
		return nil, err
	}
	cs := tc.ConnectionState()
	if cs.NegotiatedProtocol != ALPN {
		return nil, errors.New("the node at this address does not take pairing connections")
	}
	material, secret, err := exported(tc)
	if err != nil {
		return nil, err
	}
	hash := code.SecretHash()
	req := append([]byte{}, code.ID[:]...)
	req = append(req, proof(hash[:], material)...)
	req = append(req, byte(len(name)))
	req = append(req, name...)
	if _, err := tc.Write(req); err != nil {
		return nil, err
	}
	var head [2]byte
	if _, err := io.ReadFull(tc, head[:1]); err != nil {
		return nil, fmt.Errorf("no answer to the pairing code: %w", err)
	}
	if head[0] != statusOK {
		return nil, ErrRefused
	}
	if _, err := io.ReadFull(tc, head[1:]); err != nil {
		return nil, err
	}
	peerName := make([]byte, head[1])
	if _, err := io.ReadFull(tc, peerName); err != nil {
		return nil, err
	}
	if err := node.CheckName(string(peerName)); err != nil {
		return nil, fmt.Errorf("the other node's name cannot be used: %w", err)
	}
	return &Result{Name: string(peerName), Fingerprint: node.Fingerprint(cs.PeerCertificates[0]), Secret: secret}, nil
}

// Request is what the node that made a code learns from a joining node's request.
type Request struct {
	ID          string // the code's id, 8 hex digits
	Name        string // the joining node's name, checked with node.CheckName
	Fingerprint string // of the joining node's identity key
	Secret      []byte // the pair secret

	proof, material []byte
}

// ProofOK reports whether the request's proof was made with the secret hash, in
// constant time.
func (r *Request) ProofOK(secretHash []byte) bool {
	return hmac.Equal(r.proof, proof(secretHash, r.material))
}

// Serve runs the side of the node that made the code, on a connection whose TLS
// handshake is done with the protocol ALPN, and closes it. The caller sets the deadline.
// Serve reads the request and hands it to redeem, which decides: nil accepts the
// pairing (the peer is stored by then), an error refuses it. The joining node is told
// which of the two, not why. Serve returns redeem's error, or its own.
func Serve(conn *tls.Conn, name string, redeem func(*Request) error) error {
	defer conn.Close()
	cs := conn.ConnectionState()
	if cs.NegotiatedProtocol != ALPN || len(cs.PeerCertificates) == 0 {
		return errors.New("not a pairing connection")
	}
	var head [idLen + sha256.Size + 1]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return fmt.Errorf("reading the request: %w", err)
	}
	peerName := make([]byte, head[len(head)-1])
	if _, err := io.ReadFull(conn, peerName); err != nil {
		return fmt.Errorf("reading the request: %w", err)
	}
	material, secret, err := exported(conn)
	if err != nil {
		return err
	}
	req := &Request{
		ID:          hex.EncodeToString(head[:idLen]),
		Name:        string(peerName),
		Fingerprint: node.Fingerprint(cs.PeerCertificates[0]),
		Secret:      secret,
		proof:       head[idLen : idLen+sha256.Size],
		material:    material,
	}
	if err = node.CheckName(req.Name); err != nil {
		err = fmt.Errorf("the joining node's name cannot be used: %w", err)
	} else {
		err = redeem(req)
	}
	if err != nil {
		conn.Write([]byte{statusRefused})
		return err
	}
	answer := append([]byte{statusOK, byte(len(name))}, name...)
	if _, werr := conn.Write(answer); werr != nil {
		return fmt.Errorf("the pairing was accepted, but the answer could not be sent: %w", werr)
	}
	return nil
}
