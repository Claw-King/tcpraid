package node

import (
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

// Errors of the state methods. Callers match them with errors.Is.
var (
	ErrNotFound = errors.New("not found")
	// ErrInvalid: what was asked for cannot be stored, for example a path that is not one.
	ErrInvalid = errors.New("invalid")
	// ErrSelf: a node cannot be its own peer.
	ErrSelf = errors.New("that is this node itself")
	// A pairing code that cannot be redeemed.
	ErrCodeUnknown = errors.New("no such pairing code")
	ErrCodeUsed    = errors.New("the pairing code was used already")
	ErrCodeExpired = errors.New("the pairing code has expired")
	ErrCodeProof   = errors.New("the proof does not match the pairing code")
)

// MaxProofFailures is how many wrong proofs in a row a pairing code survives; the next
// one removes the code.
const MaxProofFailures = 5

// Name returns the node's name.
func (n *Node) Name() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.state.Node.Name
}

// SetName changes the node's name. A name that CheckName refuses is an error that
// matches ErrInvalid; when the state file cannot be written, the name stays as it was.
// Peers keep the name this node had when they were paired with it.
func (n *Node) SetName(name string) error {
	if err := CheckName(name); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.update(func(st *State) error {
		st.Node.Name = name
		return nil
	})
}

// Fingerprint returns the fingerprint of the node's identity key.
func (n *Node) Fingerprint() string { return n.Identity.Fingerprint() }

// PeerCount returns the number of peers.
func (n *Node) PeerCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.state.Peers)
}

// RuleCount returns the number of rules.
func (n *Node) RuleCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.state.Rules)
}

// Peers returns a copy of the peers, in the order they were paired.
func (n *Node) Peers() []Peer {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]Peer, len(n.state.Peers))
	for i, p := range n.state.Peers {
		out[i] = p.clone()
	}
	return out
}

// Peer returns a copy of the peer called name.
func (n *Node) Peer(name string) (Peer, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, p := range n.state.Peers {
		if p.Name == name {
			return p.clone(), true
		}
	}
	return Peer{}, false
}

// PeerByFingerprint returns a copy of the peer whose identity key has the fingerprint.
func (n *Node) PeerByFingerprint(fingerprint string) (Peer, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, p := range n.state.Peers {
		if p.Fingerprint == fingerprint {
			return p.clone(), true
		}
	}
	return Peer{}, false
}

// update applies change to a copy of the state and, if change returns nil, writes the
// copy to the state file and makes it the node's state. When the write fails the node's
// state stays what it was: memory and file never disagree. The caller holds n.mu.
func (n *Node) update(change func(st *State) error) error {
	next := n.state.clone()
	if err := change(next); err != nil {
		return err
	}
	if err := next.Save(n.statePath); err != nil {
		return fmt.Errorf("the state file could not be written, nothing was changed: %w", err)
	}
	n.state = next
	return nil
}

// PutPeer stores a peer after a pairing and returns it as stored.
//
// A peer with the same fingerprint is the same node paired again: its record keeps its
// name and paths and takes the new pair secret, addresses and pairing time; a path
// called "default" follows the new first address. Otherwise the peer is added; if its
// name is taken by another peer it gets a suffix ("-2", "-3", ...), because peers are
// addressed by name.
func (n *Node) PutPeer(p Peer) (Peer, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var stored Peer
	err := n.update(func(st *State) error {
		var err error
		stored, err = putPeer(st, n.Identity.Fingerprint(), p)
		return err
	})
	return stored, err
}

func putPeer(st *State, self string, p Peer) (Peer, error) {
	if err := CheckName(p.Name); err != nil {
		return Peer{}, fmt.Errorf("the peer's name: %w", err)
	}
	if !IsFingerprint(p.Fingerprint) || len(p.PairSecret) != PairSecretLen {
		return Peer{}, errors.New("the peer record has no proper fingerprint or pair secret")
	}
	if p.Fingerprint == self {
		return Peer{}, ErrSelf
	}
	if err := CheckPaths(p.Paths); err != nil {
		return Peer{}, err
	}
	p = p.clone()
	for i := range st.Peers {
		old := &st.Peers[i]
		if old.Fingerprint != p.Fingerprint {
			continue
		}
		old.PairSecret, old.Addresses, old.PairedAt = p.PairSecret, p.Addresses, p.PairedAt
		if len(old.Paths) == 0 {
			old.Paths = p.Paths
		} else if len(p.Addresses) > 0 {
			for j := range old.Paths {
				if old.Paths[j].Name == DefaultPathName {
					old.Paths[j].Addr = p.Addresses[0]
					old.Paths[j].Addrs = nil
					if len(p.Addresses) > 1 {
						old.Paths[j].Addrs = append([]string{}, p.Addresses[1:]...)
					}
				}
			}
		}
		return old.clone(), nil
	}
	base := p.Name
	for k := 2; nameTaken(st, p.Name); k++ {
		p.Name = withSuffix(base, fmt.Sprintf("-%d", k))
		if err := CheckName(p.Name); err != nil {
			return Peer{}, fmt.Errorf("no free name for the peer: its name %q is taken, and %q cannot be a name: %w", base, p.Name, err)
		}
	}
	st.Peers = append(st.Peers, p)
	return p.clone(), nil
}

// withSuffix returns name with suffix appended, within MaxNameLen bytes: a name that is
// too long for it gives up its end, whole characters at a time.
func withSuffix(name, suffix string) string {
	room := MaxNameLen - len(suffix)
	if len(name) > room {
		cut := room
		for cut > 0 && !utf8.RuneStart(name[cut]) {
			cut--
		}
		name = name[:cut]
	}
	return name + suffix
}

// PathChange says what to set on one path of a peer. A nil field leaves that setting as
// it is.
//
// Addr sets the address of the path and takes its further addresses away, unless Addrs
// is set too. Addrs sets the further addresses (an empty list: none).
type PathChange struct {
	Addr, Type, Proxy, Security, Verify, Pin *string
	Addrs, Roles                             *[]string
	Subs, Mark                               *int
}

// Empty reports whether the change sets nothing.
func (c PathChange) Empty() bool {
	return c.Addr == nil && c.Addrs == nil && c.Type == nil && c.Proxy == nil && c.Security == nil && c.Verify == nil && c.Pin == nil &&
		c.Roles == nil && c.Subs == nil && c.Mark == nil
}

// Apply sets on p what the change names. What only goes with a setting that the change
// takes away goes with it, unless the change names it too: the proxy when the type is
// no longer socks5, verify and pin when the security is no longer owncert, the pin when
// verify becomes ca.
func (c PathChange) Apply(p *Path) {
	if c.Addr != nil {
		p.Addr = *c.Addr
		if c.Addrs == nil {
			p.Addrs = nil
		}
	}
	if c.Addrs != nil {
		p.Addrs = nil
		if len(*c.Addrs) > 0 {
			p.Addrs = append([]string{}, *c.Addrs...)
		}
	}
	if c.Mark != nil {
		p.Mark = *c.Mark
	}
	if c.Type != nil {
		p.Type = *c.Type
		if p.Type != "socks5" && c.Proxy == nil {
			p.Proxy = ""
		}
	}
	if c.Proxy != nil {
		p.Proxy = *c.Proxy
	}
	if c.Security != nil {
		p.Security = *c.Security
		if p.Security != SecurityOwnCert {
			if c.Verify == nil {
				p.Verify = ""
			}
			if c.Pin == nil {
				p.Pin = ""
			}
		}
	}
	if c.Verify != nil {
		p.Verify = *c.Verify
		if p.Verify == VerifyCA && c.Pin == nil {
			p.Pin = ""
		}
	}
	if c.Pin != nil {
		p.Pin = *c.Pin
	}
	if c.Roles != nil {
		p.Roles = append([]string{}, *c.Roles...)
	}
	if c.Subs != nil {
		p.Subs = *c.Subs
	}
}

// NewPath returns a path called name with the settings a new path has when nothing else
// is said: type tcp, security tls, the data role, 8 sub-connections. It has no address
// yet.
func NewPath(name string) Path {
	return Path{Name: name, Type: "tcp", Security: SecurityTLS, Roles: []string{RoleData}, Subs: 8}
}

// ChangePaths changes the paths of the peer called name in one step and returns the
// peer as stored: change is given a copy of the paths as they are at that moment and
// returns the paths to store. Nothing can change the peer between the two, so two
// callers that each change another path both take effect. An error of change is
// returned as it is and changes nothing; paths that CheckPaths refuses are an error
// that matches ErrInvalid.
func (n *Node) ChangePaths(name string, change func(paths []Path) ([]Path, error)) (Peer, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var stored Peer
	err := n.update(func(st *State) error {
		for i := range st.Peers {
			p := &st.Peers[i]
			if p.Name != name {
				continue
			}
			paths, err := change(p.clone().Paths)
			if err != nil {
				return err
			}
			if err := CheckPaths(paths); err != nil {
				return fmt.Errorf("%w: %w", ErrInvalid, err)
			}
			p.Paths = Peer{Paths: paths}.clone().Paths
			stored = p.clone()
			return nil
		}
		return fmt.Errorf("peer %q: %w", name, ErrNotFound)
	})
	return stored, err
}

// SetPeerPaths replaces the paths of the peer called name by paths and returns the peer
// as stored; see UpdatePeer.
func (n *Node) SetPeerPaths(name string, paths []Path) (Peer, error) {
	return n.UpdatePeer(name, &paths, nil, nil)
}

// UpdatePeer changes settings of the peer called name, in one step, and returns the
// peer as stored: its paths (replaced as a whole), its allow list, its UDP feedback
// address. A nil argument leaves that setting as it is. Everything is checked before
// anything changes; an error of those checks matches ErrInvalid. When the state file
// cannot be written, the peer stays as it was.
func (n *Node) UpdatePeer(name string, paths *[]Path, allow, udpDial *string) (Peer, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var stored Peer
	err := n.update(func(st *State) error {
		for i := range st.Peers {
			p := &st.Peers[i]
			if p.Name != name {
				continue
			}
			if paths != nil {
				if err := CheckPaths(*paths); err != nil {
					return fmt.Errorf("%w: %w", ErrInvalid, err)
				}
				p.Paths = Peer{Paths: *paths}.clone().Paths
			}
			if allow != nil {
				p.Allow = *allow
			}
			if udpDial != nil {
				p.UDPDial = *udpDial
			}
			if err := checkPeerOptions(p.Allow, p.UDPDial); err != nil {
				return fmt.Errorf("%w: %w", ErrInvalid, err)
			}
			stored = p.clone()
			return nil
		}
		return fmt.Errorf("peer %q: %w", name, ErrNotFound)
	})
	return stored, err
}

// DefaultPathName is the name of the path a pairing creates.
const DefaultPathName = "default"

func nameTaken(st *State, name string) bool {
	for _, p := range st.Peers {
		if p.Name == name {
			return true
		}
	}
	return false
}

// RemovePeer forgets the peer called name.
func (n *Node) RemovePeer(name string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.update(func(st *State) error {
		for i, p := range st.Peers {
			if p.Name == name {
				st.Peers = append(st.Peers[:i], st.Peers[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("peer %q: %w", name, ErrNotFound)
	})
}

// AddPairingCode records a pairing code that was handed out. Codes that have expired
// are dropped on the way.
func (n *Node) AddPairingCode(pc PairingCode, now time.Time) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.update(func(st *State) error {
		dropExpired(st, now)
		for _, old := range st.PairingCodes {
			if old.ID == pc.ID {
				return fmt.Errorf("pairing code id %s is taken", pc.ID)
			}
		}
		pc.SecretHash = append([]byte{}, pc.SecretHash...)
		st.PairingCodes = append(st.PairingCodes, pc)
		return (&State{PairingCodes: []PairingCode{pc}}).validate()
	})
}

func dropExpired(st *State, now time.Time) {
	kept := st.PairingCodes[:0]
	for _, pc := range st.PairingCodes {
		if pc.Expires.After(now) {
			kept = append(kept, pc)
		}
	}
	st.PairingCodes = kept
}

// PairingCodes returns the pairing codes that have not expired, used or not, oldest
// first. The copies hold no secret hash.
func (n *Node) PairingCodes(now time.Time) []PairingCode {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := []PairingCode{}
	for _, pc := range n.state.PairingCodes {
		if pc.Expires.After(now) {
			pc.SecretHash = nil
			out = append(out, pc)
		}
	}
	return out
}

// RevokePairingCode removes the pairing code with the id.
func (n *Node) RevokePairingCode(id string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.update(func(st *State) error {
		for i, pc := range st.PairingCodes {
			if pc.ID == id {
				st.PairingCodes = append(st.PairingCodes[:i], st.PairingCodes[i+1:]...)
				delete(n.failures, id)
				return nil
			}
		}
		return fmt.Errorf("pairing code %s: %w", id, ErrNotFound)
	})
}

// RedeemPairingCode is the generating node's side of a pairing: if the code with the id
// is known, unused and not expired, and proofOK accepts its secret hash, the code is
// marked used and the peer is stored (as PutPeer does), in one step. The stored peer is
// returned.
//
// proofOK is called with the mutex held and must only compare. A wrong proof counts
// against the code: after MaxProofFailures wrong proofs in a row the code is removed,
// so the secret cannot be searched for through this method.
func (n *Node) RedeemPairingCode(id string, now time.Time, proofOK func(secretHash []byte) bool, p Peer) (Peer, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	idx := -1
	for i, pc := range n.state.PairingCodes {
		if pc.ID == id {
			idx = i
		}
	}
	switch {
	case idx < 0:
		return Peer{}, ErrCodeUnknown
	case n.state.PairingCodes[idx].Used:
		return Peer{}, ErrCodeUsed
	case !n.state.PairingCodes[idx].Expires.After(now):
		return Peer{}, ErrCodeExpired
	}
	if !proofOK(n.state.PairingCodes[idx].SecretHash) {
		n.failures[id]++
		if n.failures[id] >= MaxProofFailures {
			delete(n.failures, id)
			if err := n.update(func(st *State) error {
				st.PairingCodes = append(st.PairingCodes[:idx], st.PairingCodes[idx+1:]...)
				return nil
			}); err != nil {
				return Peer{}, err
			}
			return Peer{}, fmt.Errorf("%w. That was wrong proof number %d, and the code is revoked.", ErrCodeProof, MaxProofFailures)
		}
		return Peer{}, ErrCodeProof
	}
	var stored Peer
	err := n.update(func(st *State) error {
		var err error
		if stored, err = putPeer(st, n.Identity.Fingerprint(), p); err != nil {
			return err
		}
		st.PairingCodes[idx].Used = true
		return nil
	})
	if err != nil {
		return Peer{}, err
	}
	delete(n.failures, id)
	return stored, nil
}
