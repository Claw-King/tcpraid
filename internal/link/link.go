// Package link makes and takes the connections between paired nodes, in the security
// tier each path asks for, and runs the pairing of two nodes.
//
// A node has up to two listeners. The TLS listener takes TLS 1.3 connections and tells
// them apart by protocol name (ALPN):
//
//	tcpraid-pair/1  a node that redeems a pairing code (package pair)
//	tcpraid/1       a paired node, known by the fingerprint of its identity key
//	tcpraid-own/1   a paired node that asks for the certificate the operator supplied
//
// Whatever else arrives is closed. The plain listener takes TCP connections without TLS,
// for paths that are already encrypted by something else. Both listeners keep running
// when accepting fails, and each holds only a bounded number of connections that have
// not yet proved who they are (Server.MaxUnauthenticated).
//
// The tiers of a path (node.Path.Security):
//
//	tls      TLS 1.3. Each end shows its identity certificate and checks that the
//	         other's key has the fingerprint recorded at the pairing.
//	owncert  TLS 1.3. The listening end shows the operator's certificate; the dialing
//	         end checks it by the fingerprint of its key (verify pin) or the way a web
//	         client would, by authority and host name (verify ca). The dialing end
//	         shows its identity certificate, as in tls.
//	off      Plain TCP: no encryption, and no authentication at this layer. What runs
//	         on the connection authenticates itself with keys from the pair secret
//	         (TransportKey).
//
// Every connection starts with an opening from the dialing end (opening.go): it says
// what the connection is for and proves, with a MAC under a key from the pair secret,
// that the dialing end holds the pair secret. A sub-connection of the transport follows
// one kind of opening; another kind asks the listening end to prove the same and is how
// a node checks a path -- in TLS 1.3 a client learns that the server refused its
// certificate only when it reads, and in the off tier nothing else says who answers.
package link

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/Claw-King/tcpraid/internal/node"
)

const (
	// ALPN is the TLS protocol name of a connection between paired nodes.
	ALPN = "tcpraid/1"
	// ALPNOwn is the same, where the dialing end expects the operator's certificate.
	ALPNOwn = "tcpraid-own/1"

	// HandshakeTimeout bounds a TLS handshake, on either end.
	HandshakeTimeout = 15 * time.Second
)

// ErrInvalid is matched (errors.Is) by errors that are the caller's doing: a pairing
// code that does not parse, an address that is not one, a request that lacks something.
var ErrInvalid = errors.New("invalid request")

// ErrTimeout is matched (errors.Is) by the error of an operation that was given up
// because the other node took too long.
var ErrTimeout = errors.New("timed out")

// inputError marks an error as the caller's doing without changing its text.
type inputError struct{ error }

func (inputError) Is(target error) bool { return target == ErrInvalid }
func (e inputError) Unwrap() error      { return e.error }

// TransportKey derives, from a pair secret, the key that the transport on a connection
// between the two nodes uses for its own authentication. Both nodes hold the same pair
// secret and so derive the same key; no other pair of nodes does. A pair secret of the
// wrong length is an error: a key derived from nothing would be the same for everyone.
func TransportKey(pairSecret []byte) ([]byte, error) {
	if len(pairSecret) != node.PairSecretLen {
		return nil, fmt.Errorf("the pair secret is %d bytes, not %d", len(pairSecret), node.PairSecretLen)
	}
	m := hmac.New(sha256.New, pairSecret)
	m.Write([]byte("tcpraid transport key v1"))
	return m.Sum(nil), nil
}
