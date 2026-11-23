package link

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/Claw-King/tcpraid/internal/node"
	"github.com/Claw-King/tcpraid/internal/pair"
)

// Service is what a running node can be asked to do about pairing and peers. The local
// API calls it; its HandlePair is the Server's handler for pairing connections.
type Service struct {
	Node   *node.Node
	Dialer *Dialer
	// Advertise are the addresses a pairing code carries when the request names none:
	// where other nodes reach this node's TLS listener.
	Advertise []string
	// Logf logs one line per pairing made, and per change an operator asked for. nil:
	// nothing is logged.
	Logf func(format string, args ...any)
	// RefusedLogf logs a pairing connection that failed or was refused. Anyone who can
	// reach the listener can cause as many of those as it likes, so this should be a
	// limited logger: Server.LogRefused. nil: Logf is used.
	RefusedLogf func(format string, args ...any)
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// JoinTimeout bounds a whole Join; 0 means DefaultJoinTimeout.
	JoinTimeout time.Duration
	// PeerRemoved, if not nil, is called after a peer was removed from the node's
	// state, with its name: whatever runs with that peer ends there.
	PeerRemoved func(name string)
}

// DefaultJoinTimeout bounds a whole join: every address that is tried and the pairing
// exchange at the one that answers.
const DefaultJoinTimeout = 60 * time.Second

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *Service) refusedf(format string, args ...any) {
	if s.RefusedLogf != nil {
		s.RefusedLogf(format, args...)
		return
	}
	s.logf(format, args...)
}

// Name returns the node's name.
func (s *Service) Name() string { return s.Node.Name() }

// SetName changes the node's name. A name that cannot be one is the caller's doing
// (ErrInvalid).
func (s *Service) SetName(name string) error {
	if err := s.Node.SetName(name); err != nil {
		if errors.Is(err, node.ErrInvalid) {
			err = inputError{err}
		}
		return err
	}
	s.logf("node renamed to %q", name)
	return nil
}

// Fingerprint returns the fingerprint of the node's identity key.
func (s *Service) Fingerprint() string { return s.Node.Fingerprint() }

// PeerCount returns the number of peers.
func (s *Service) PeerCount() int { return s.Node.PeerCount() }

// RuleCount returns the number of rules.
func (s *Service) RuleCount() int { return s.Node.RuleCount() }

// CreatePairingCode makes a pairing code for this node and records it. addresses are
// where the joining node will connect; empty means the node's advertised addresses. A
// ttl of 0 means pair.DefaultTTL. The text is the code to hand to the other node; it is
// returned here and kept nowhere.
func (s *Service) CreatePairingCode(addresses []string, ttl time.Duration) (text string, rec node.PairingCode, err error) {
	if len(addresses) == 0 {
		addresses = s.Advertise
	}
	if len(addresses) == 0 {
		return "", rec, inputError{errors.New("no address for the other node to connect to: name one in the request, or start the node with -advertise HOST:PORT")}
	}
	if ttl == 0 {
		ttl = pair.DefaultTTL
	}
	now := s.now()
	code, err := pair.New(s.Node.Identity.FingerprintBytes(), addresses, ttl, now)
	if err != nil {
		return "", rec, inputError{err}
	}
	hash := code.SecretHash()
	rec = node.PairingCode{ID: code.IDString(), SecretHash: hash[:], Expires: code.Expires}
	if err := s.Node.AddPairingCode(rec, now); err != nil {
		return "", node.PairingCode{}, err
	}
	rec.SecretHash = nil
	s.logf("pairing code %s made, valid until %s", rec.ID, rec.Expires.UTC().Format(time.RFC3339))
	return code.Text(), rec, nil
}

// PairingCodes returns the codes that have not expired, without their secret hashes.
func (s *Service) PairingCodes() []node.PairingCode { return s.Node.PairingCodes(s.now()) }

// RevokePairingCode removes the code with the id.
func (s *Service) RevokePairingCode(id string) error {
	if err := s.Node.RevokePairingCode(id); err != nil {
		return err
	}
	s.logf("pairing code %s revoked", id)
	return nil
}

// Join pairs this node with the node that made the pairing code. address, if not empty,
// is where to connect instead of the addresses in the code. The peer is stored with one
// path, to the address the pairing went through, in the tls tier. The returned peer
// carries no pair secret. The whole of it takes at most JoinTimeout; an error for having
// run out of that time matches ErrTimeout.
func (s *Service) Join(ctx context.Context, codeText, address string) (node.Peer, error) {
	code, err := pair.Parse(codeText, s.now())
	if err != nil {
		return node.Peer{}, inputError{err}
	}
	limit := s.JoinTimeout
	if limit <= 0 {
		limit = DefaultJoinTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	if code.Fingerprint == s.Node.Identity.FingerprintBytes() {
		return node.Peer{}, inputError{errors.New("this pairing code was made by this node itself: enter it on the other node")}
	}
	addrs := code.Addresses
	if address != "" {
		if err := pair.CheckAddress(address); err != nil {
			return node.Peer{}, inputError{err}
		}
		addrs = []string{address}
	}
	var res *pair.Result
	var used string
	var failures []error
	for _, addr := range addrs {
		res, err = s.joinAt(ctx, addr, code)
		if err == nil {
			used = addr
			break
		}
		failures = append(failures, fmt.Errorf("%s: %w", addr, err))
		if errors.Is(err, pair.ErrRefused) || ctx.Err() != nil {
			break // the answer would be the same at another address
		}
	}
	if res == nil {
		err := errors.Join(failures...)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("the pairing did not finish within %v (%w): %w", limit, ErrTimeout, err)
		}
		s.logf("joining with %s failed: %v", code, err)
		return node.Peer{}, err
	}
	peer, err := s.Node.PutPeer(node.Peer{
		Name:        res.Name,
		Fingerprint: res.Fingerprint,
		PairSecret:  res.Secret,
		Addresses:   []string{used},
		PairedAt:    s.now().UTC().Truncate(time.Second),
		Paths: []node.Path{{Name: node.DefaultPathName, Type: "tcp", Addr: used, Security: node.SecurityTLS,
			Roles: []string{"data"}, Subs: 8}},
	})
	if err != nil {
		s.logf("joining with %s failed: the other node accepted, but the peer could not be stored: %v", code, err)
		return node.Peer{}, fmt.Errorf("the other node accepted the pairing, but this node could not store it: %w", err)
	}
	s.logf("paired with %q (fingerprint %s) at %s, using %s", peer.Name, peer.Fingerprint, used, code)
	return withoutSecret(peer), nil
}

func (s *Service) joinAt(ctx context.Context, addr string, code *pair.Code) (*pair.Result, error) {
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	// the exchange has its own deadline; the join's limit, if it comes first, ends it by
	// closing the connection
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	return pair.Join(conn, s.Node.Identity.TLSCertificate(), code, s.Node.Name())
}

// HandlePair serves a pairing connection: it lets the joining node redeem a code and
// stores it as a peer, with no path, because nothing says how to reach it.
func (s *Service) HandlePair(conn *tls.Conn) {
	from := conn.RemoteAddr()
	var id string
	var stored node.Peer
	accepted := false // the code was redeemed and the peer is stored
	err := pair.Serve(conn, s.Node.Name(), func(req *pair.Request) error {
		id = req.ID
		var err error
		stored, err = s.Node.RedeemPairingCode(req.ID, s.now(), req.ProofOK, node.Peer{
			Name:        req.Name,
			Fingerprint: req.Fingerprint,
			PairSecret:  req.Secret,
			PairedAt:    s.now().UTC().Truncate(time.Second),
		})
		accepted = err == nil
		return err
	})
	switch {
	case err == nil:
		s.logf("paired with %q (fingerprint %s) from %s, using pairing code %s", stored.Name, stored.Fingerprint, from, id)
	case accepted:
		// This node has a new peer and the code is used, whatever happened to the
		// connection afterwards: that is always on record. Only a holder of a valid
		// code can cause this line, so it needs no limit.
		s.logf("paired with %q (fingerprint %s) from %s, using pairing code %s, but the answer did not reach it (%v): it will report a failure; pair the two nodes again", stored.Name, stored.Fingerprint, from, id, err)
	case errors.Is(err, net.ErrClosed):
		// this node closed the connection itself, to make room or to stop, before
		// anything was accepted: that has its own line
	case id != "":
		s.refusedf("refused pairing code %s from %s: %v", id, from, err)
	default:
		s.refusedf("pairing connection from %s failed: %v", from, err)
	}
}

func withoutSecret(p node.Peer) node.Peer {
	p.PairSecret = nil
	return p
}

// Peers returns the peers, without their pair secrets.
func (s *Service) Peers() []node.Peer {
	ps := s.Node.Peers()
	for i := range ps {
		ps[i] = withoutSecret(ps[i])
	}
	return ps
}

// Peer returns the peer called name, without its pair secret.
func (s *Service) Peer(name string) (node.Peer, error) {
	p, ok := s.Node.Peer(name)
	if !ok {
		return node.Peer{}, fmt.Errorf("peer %q: %w", name, node.ErrNotFound)
	}
	return withoutSecret(p), nil
}

// SetPeerPaths replaces the paths of the peer called name; see UpdatePeer.
func (s *Service) SetPeerPaths(name string, paths []node.Path) (node.Peer, error) {
	return s.UpdatePeer(name, &paths, nil, nil)
}

// SetPeerOptions sets the allow list and the UDP feedback address of the peer called
// name; see UpdatePeer.
func (s *Service) SetPeerOptions(name string, allow, udpDial *string) (node.Peer, error) {
	return s.UpdatePeer(name, nil, allow, udpDial)
}

// UpdatePeer changes settings of the peer called name in one step and returns the peer,
// without its pair secret: its paths (replaced as a whole), its allow list, its UDP
// feedback address. A nil argument leaves that setting as it is. Values that cannot be
// stored are the caller's doing (ErrInvalid) and change nothing.
//
// A proxy given without user and password keeps the user and password that the path of
// the same name has now, if it goes to the same proxy: the API shows a proxy without
// them, so a caller that sends back what it was shown changes nothing.
func (s *Service) UpdatePeer(name string, paths *[]node.Path, allow, udpDial *string) (node.Peer, error) {
	old, ok := s.Node.Peer(name)
	if !ok {
		return node.Peer{}, fmt.Errorf("peer %q: %w", name, node.ErrNotFound)
	}
	if paths != nil {
		kept := append([]node.Path{}, *paths...)
		for i := range kept {
			for _, was := range old.Paths {
				if was.Name == kept[i].Name {
					kept[i].Proxy = keepProxyUser(kept[i].Proxy, was.Proxy)
				}
			}
		}
		paths = &kept
	}
	stored, err := s.Node.UpdatePeer(name, paths, allow, udpDial)
	if err != nil {
		if errors.Is(err, node.ErrInvalid) {
			err = inputError{err}
		}
		return node.Peer{}, err
	}
	if paths != nil {
		var desc []string
		for _, p := range stored.Paths {
			desc = append(desc, fmt.Sprintf("%s to %s (%s)", p.Name, p.Addr, p.Security))
		}
		s.logf("peer %q: paths set: %s", name, strings.Join(desc, ", "))
	}
	if allow != nil || udpDial != nil {
		s.logf("peer %q: allow list %q, UDP feedback to %q", name, stored.Allow, stored.UDPDial)
	}
	return withoutSecret(stored), nil
}

// keepProxyUser returns proxy, with the user and password of was put into it when proxy
// has none and both name the same proxy host.
func keepProxyUser(proxy, was string) string {
	u, err := url.Parse(proxy)
	if err != nil || u.User != nil || proxy == "" {
		return proxy
	}
	w, err := url.Parse(was)
	if err != nil || w.User == nil || w.Scheme != u.Scheme || w.Host != u.Host {
		return proxy
	}
	return was
}

// RemovePeer ends the pairing with the peer called name on this node: its connections
// are refused from now on, and what runs with it is ended (PeerRemoved). The other node
// is not told.
func (s *Service) RemovePeer(name string) error {
	if err := s.Node.RemovePeer(name); err != nil {
		return err
	}
	s.logf("peer %q removed", name)
	if s.PeerRemoved != nil {
		s.PeerRemoved(name)
	}
	return nil
}

// PathResult is the outcome of checking one path.
type PathResult struct {
	Name     string
	Addr     string
	Security string
	OK       bool
	Error    string // why not, when OK is false
}

// CheckTimeout bounds the check of one path.
const CheckTimeout = 20 * time.Second

// CheckPeer opens a connection on every path of the peer called name, in the path's
// tier, checks it (Dialer.Check) and closes it. A peer without paths gives an empty list: this node
// has no way to reach it.
func (s *Service) CheckPeer(ctx context.Context, name string) ([]PathResult, error) {
	peer, ok := s.Node.Peer(name)
	if !ok {
		return nil, fmt.Errorf("peer %q: %w", name, node.ErrNotFound)
	}
	results := []PathResult{}
	for _, path := range peer.Paths {
		r := PathResult{Name: path.Name, Addr: path.Addr, Security: path.Security, OK: true}
		pctx, cancel := context.WithTimeout(ctx, CheckTimeout)
		if err := s.Dialer.Check(pctx, peer, path); err != nil {
			r.OK, r.Error = false, err.Error()
		}
		cancel()
		results = append(results, r)
	}
	return results, nil
}
