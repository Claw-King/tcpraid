package node

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"
)

// SchemaVersion is the version of the state file layout this program writes. It reads
// this version and versions 1 and 2, which it brings up to date.
const SchemaVersion = 3

// State is the content of the state file.
type State struct {
	Schema       int           `json:"schema"`
	Node         NodeState     `json:"node"`
	Peers        []Peer        `json:"peers"`
	PairingCodes []PairingCode `json:"pairing_codes"`
	// Rules are the forwarding rules, in the order they were made. The order matters
	// for transparent rules: the first that matches a connection takes it.
	Rules []Rule `json:"rules"`
}

// NodeState is what the state file records about the node itself.
type NodeState struct {
	Name string `json:"name"`
}

// Peer is a node this node is paired with.
type Peer struct {
	Name        string    `json:"name"`        // unique among this node's peers
	Fingerprint string    `json:"fingerprint"` // of the peer's identity key, lower-case hex
	PairSecret  []byte    `json:"pair_secret"` // PairSecretLen bytes shared with the peer; base64 in the file
	Addresses   []string  `json:"addresses"`   // where the peer can be reached; empty if it cannot
	PairedAt    time.Time `json:"paired_at"`
	Paths       []Path    `json:"paths"`
	// Allow lists the destinations the peer may reach through this node, in the form
	// of engine.ParseAllow; empty means any: a paired node is trusted.
	Allow string `json:"allow,omitempty"`
	// UDPDial is the host:port of the peer's UDP feedback listener; empty means this
	// node sends the peer no UDP feedback of its own accord.
	UDPDial string `json:"udp_dial,omitempty"`
}

// PairSecretLen is the length of a pair secret in bytes.
const PairSecretLen = 32

// Security tiers of a path.
const (
	SecurityTLS     = "tls"     // TLS 1.3 with the node identities; each end checks the other's key fingerprint
	SecurityOwnCert = "owncert" // TLS 1.3 where the listening end shows a certificate its operator supplied
	SecurityOff     = "off"     // plain TCP, for a path that is already encrypted
)

// How the dialing end checks the listening end's certificate in the owncert tier.
const (
	VerifyPin = "pin" // the certificate's key has the fingerprint in Path.Pin
	VerifyCA  = "ca"  // the certificate is valid for the host name under the system's authorities
)

// Path is one way to reach a peer.
type Path struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`            // "tcp" or "socks5"
	Addr     string   `json:"addr"`            // host:port of the peer's listener as seen from the end of the path
	Proxy    string   `json:"proxy,omitempty"` // socks5://[user:pass@]host:port (type socks5)
	Security string   `json:"security"`        // SecurityTLS, SecurityOwnCert or SecurityOff
	Verify   string   `json:"verify,omitempty"`
	Pin      string   `json:"pin,omitempty"` // owncert with VerifyPin: fingerprint of the listener's certificate key
	Roles    []string `json:"roles"`         // data, repair, control
	Subs     int      `json:"subs"`
}

// PairingCode is what the node keeps of a pairing code it handed out. The one-time
// secret itself is not kept, only its hash.
type PairingCode struct {
	ID         string    `json:"id"`          // 8 hex digits; public
	SecretHash []byte    `json:"secret_hash"` // SHA-256 of the one-time secret; base64 in the file
	Expires    time.Time `json:"expires"`
	Used       bool      `json:"used"`
}

// NewState returns the state of a node called name with no peers and no rules.
func NewState(name string) *State {
	s := &State{Schema: SchemaVersion, Node: NodeState{Name: name}}
	s.fill()
	return s
}

// fill replaces nil lists by empty ones, so that they are written as [] and not null.
func (s *State) fill() {
	if s.Peers == nil {
		s.Peers = []Peer{}
	}
	if s.PairingCodes == nil {
		s.PairingCodes = []PairingCode{}
	}
	if s.Rules == nil {
		s.Rules = []Rule{}
	}
	for i := range s.Peers {
		p := &s.Peers[i]
		if p.Addresses == nil {
			p.Addresses = []string{}
		}
		if p.Paths == nil {
			p.Paths = []Path{}
		}
		for j := range p.Paths {
			if p.Paths[j].Roles == nil {
				p.Paths[j].Roles = []string{}
			}
		}
	}
}

// clone returns a copy that shares no memory with s.
func (s *State) clone() *State {
	c := *s
	c.Peers = make([]Peer, len(s.Peers))
	for i, p := range s.Peers {
		c.Peers[i] = p.clone()
	}
	c.PairingCodes = make([]PairingCode, len(s.PairingCodes))
	for i, pc := range s.PairingCodes {
		pc.SecretHash = bytes.Clone(pc.SecretHash)
		c.PairingCodes[i] = pc
	}
	c.Rules = append([]Rule{}, s.Rules...)
	return &c
}

func (p Peer) clone() Peer {
	p.PairSecret = bytes.Clone(p.PairSecret)
	p.Addresses = append([]string{}, p.Addresses...)
	paths := make([]Path, len(p.Paths))
	for i, path := range p.Paths {
		path.Roles = append([]string{}, path.Roles...)
		paths[i] = path
	}
	p.Paths = paths
	return p
}

// stateV1 is the layout of schema version 1. Its lists never had elements this program
// could interpret, so a version 1 file is taken over only with empty lists.
type stateV1 struct {
	Schema int               `json:"schema"`
	Node   NodeState         `json:"node"`
	Peers  []json.RawMessage `json:"peers"`
	Rules  []json.RawMessage `json:"rules"`
}

// stateV2 is the layout of schema version 2: version 3 without rules (the list was
// there and always empty) and without the allow list and UDP address of a peer.
type stateV2 struct {
	Schema       int               `json:"schema"`
	Node         NodeState         `json:"node"`
	Peers        []Peer            `json:"peers"`
	PairingCodes []PairingCode     `json:"pairing_codes"`
	Rules        []json.RawMessage `json:"rules"`
}

// LoadState reads the state file at path. A file that is not valid, has a schema version
// this program does not read, holds unknown fields or holds a peer or pairing code that
// makes no sense is an error; LoadState never writes. A file of schema version 1 or 2
// is returned in the current layout. A missing file is reported with an error that matches
// fs.ErrNotExist.
func LoadState(path string) (*State, error) {
	s, _, err := loadState(path)
	return s, err
}

// loadState is LoadState; old reports that the file is of an older schema version and
// has to be written again in the current one.
func loadState(path string) (s *State, old bool, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	const kept = "The file is left as it is."
	// The schema version is read on its own first: a file of another version is reported
	// as that, whatever else in it this version would not understand.
	var probe struct {
		Schema *int `json:"schema"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, false, fmt.Errorf("%s: %w. %s", path, err, kept)
	}
	if probe.Schema == nil {
		return nil, false, fmt.Errorf("%s: no schema version. %s", path, kept)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	// a field this version does not know would be lost at the next save
	dec.DisallowUnknownFields()
	switch *probe.Schema {
	case 1:
		var v1 stateV1
		if err := dec.Decode(&v1); err != nil {
			return nil, false, fmt.Errorf("%s: %w. %s", path, err, kept)
		}
		if len(v1.Peers) > 0 || len(v1.Rules) > 0 {
			return nil, false, fmt.Errorf("%s: schema version 1 with peers or rules, which no version 1 program wrote. %s", path, kept)
		}
		s, old = &State{Schema: SchemaVersion, Node: v1.Node}, true
	case 2:
		var v2 stateV2
		if err := dec.Decode(&v2); err != nil {
			return nil, false, fmt.Errorf("%s: %w. %s", path, err, kept)
		}
		if len(v2.Rules) > 0 {
			return nil, false, fmt.Errorf("%s: schema version 2 with rules, which no version 2 program wrote. %s", path, kept)
		}
		for _, p := range v2.Peers {
			if p.Allow != "" || p.UDPDial != "" {
				return nil, false, fmt.Errorf("%s: schema version 2 with fields of version 3. %s", path, kept)
			}
		}
		s, old = &State{Schema: SchemaVersion, Node: v2.Node, Peers: v2.Peers, PairingCodes: v2.PairingCodes}, true
	case SchemaVersion:
		s = &State{}
		if err := dec.Decode(s); err != nil {
			return nil, false, fmt.Errorf("%s: %w. %s", path, err, kept)
		}
	default:
		return nil, false, fmt.Errorf("%s: schema version %d, this program reads versions 1 to %d. %s", path, *probe.Schema, SchemaVersion, kept)
	}
	s.fill()
	if err := s.validate(); err != nil {
		return nil, false, fmt.Errorf("%s: %w. %s", path, err, kept)
	}
	return s, old, nil
}

// validate checks what the rest of the program relies on in a state that was read from
// a file: unique, well-formed peer names and fingerprints, pair secrets of the right
// length, paths of known kinds, pairing codes with an id and a hash, rules that are
// well-formed and do not collide. A rule may name a peer that is not there: the pairing
// was ended after the rule was made.
func (s *State) validate() error {
	names, fps := map[string]bool{}, map[string]bool{}
	for _, p := range s.Peers {
		if err := CheckName(p.Name); err != nil {
			return fmt.Errorf("peer %q: %w", p.Name, err)
		}
		if names[p.Name] {
			return fmt.Errorf("two peers are called %q", p.Name)
		}
		names[p.Name] = true
		if !IsFingerprint(p.Fingerprint) {
			return fmt.Errorf("peer %q: the fingerprint is not 64 lower-case hex digits", p.Name)
		}
		if fps[p.Fingerprint] {
			return fmt.Errorf("two peers have the fingerprint of %q", p.Name)
		}
		fps[p.Fingerprint] = true
		if len(p.PairSecret) != PairSecretLen {
			return fmt.Errorf("peer %q: the pair secret is not %d bytes", p.Name, PairSecretLen)
		}
		if err := CheckPaths(p.Paths); err != nil {
			return fmt.Errorf("peer %q, %w", p.Name, err)
		}
		if err := checkPeerOptions(p.Allow, p.UDPDial); err != nil {
			return fmt.Errorf("peer %q: %w", p.Name, err)
		}
	}
	if err := CheckRules(s.Rules); err != nil {
		return err
	}
	ids := map[string]bool{}
	for _, pc := range s.PairingCodes {
		if b, err := hex.DecodeString(pc.ID); err != nil || len(b) != 4 || ids[pc.ID] {
			return fmt.Errorf("pairing code %q: the id is not 8 hex digits, or is there twice", pc.ID)
		}
		ids[pc.ID] = true
		if len(pc.SecretHash) != 32 {
			return fmt.Errorf("pairing code %s: the secret hash is not 32 bytes", pc.ID)
		}
	}
	return nil
}

// Limits of a path.
const (
	MaxPathNameLen = 32 // bytes
	MaxSubs        = 64 // sub-connections of one path
	MaxAddrLen     = 255
)

// Roles of a path.
const (
	RoleData    = "data"
	RoleRepair  = "repair"
	RoleControl = "control"
)

// CheckAddr reports whether addr is a host:port that a node can dial: a host, a port
// from 1 to 65535, printable ASCII without spaces, at most MaxAddrLen bytes.
func CheckAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("address %q: want host:port", addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("address %q: the port is not a number from 1 to 65535", addr)
	}
	if host == "" {
		return fmt.Errorf("address %q: no host", addr)
	}
	if len(addr) > MaxAddrLen {
		return fmt.Errorf("address %q: longer than %d bytes", addr, MaxAddrLen)
	}
	for _, r := range addr {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) || r == ' ' {
			return fmt.Errorf("address %q: only printable ASCII without spaces", addr)
		}
	}
	return nil
}

// errProxy is the one thing said about a proxy that cannot be used. The proxy itself is
// never part of a message: it may hold a password.
var errProxy = errors.New("the proxy is not of the form socks5://[user:pass@]host:port")

// checkProxy reports whether proxy is a SOCKS5 proxy URL: the scheme socks5, a host and
// a port, optionally a user and a password, and nothing else.
func checkProxy(proxy string) error {
	u, err := url.Parse(proxy)
	if err != nil || u.Scheme != "socks5" || u.Opaque != "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return errProxy
	}
	if CheckAddr(u.Host) != nil {
		return errProxy
	}
	return nil
}

// Check reports whether the path can be stored and dialed: a name, a type, an address, a
// proxy where the type needs one, a security tier with what goes with it, roles and a
// number of sub-connections. No message it returns holds the proxy.
func (p Path) Check() error {
	switch {
	case p.Name == "":
		return errors.New("the path has no name")
	case len(p.Name) > MaxPathNameLen:
		return fmt.Errorf("the path's name is longer than %d bytes", MaxPathNameLen)
	case !utf8.ValidString(p.Name):
		return errors.New("the path's name is not valid UTF-8")
	}
	for _, r := range p.Name {
		if !unicode.IsPrint(r) || unicode.IsSpace(r) {
			return errors.New("the path's name holds a space or a character that is not printable")
		}
	}
	switch p.Type {
	case "tcp":
		if p.Proxy != "" {
			return errors.New("a proxy belongs to type socks5 only")
		}
	case "socks5":
		if p.Proxy == "" {
			return errors.New("type socks5 needs a proxy")
		}
		if err := checkProxy(p.Proxy); err != nil {
			return err
		}
	default:
		return fmt.Errorf("type %q (want tcp or socks5)", p.Type)
	}
	if p.Addr == "" {
		return errors.New("no address")
	}
	if err := CheckAddr(p.Addr); err != nil {
		return err
	}
	switch p.Security {
	case SecurityTLS, SecurityOff:
		if p.Verify != "" || p.Pin != "" {
			return fmt.Errorf("verify and pin belong to security %s only", SecurityOwnCert)
		}
	case SecurityOwnCert:
		switch p.Verify {
		case "", VerifyPin:
			if !IsFingerprint(p.Pin) {
				return errors.New("security owncert with verify pin needs pin, the fingerprint of the listener's certificate key as 64 lower-case hex digits")
			}
		case VerifyCA:
			if p.Pin != "" {
				return errors.New("pin does not go with verify ca")
			}
		default:
			return fmt.Errorf("verify %q (want pin or ca)", p.Verify)
		}
	default:
		return fmt.Errorf("security %q (want tls, owncert or off)", p.Security)
	}
	if len(p.Roles) == 0 {
		return errors.New("the path has no role (want data, repair or control)")
	}
	seen := map[string]bool{}
	for _, r := range p.Roles {
		switch r {
		case RoleData, RoleRepair, RoleControl:
		default:
			return fmt.Errorf("role %q (want data, repair or control)", r)
		}
		if seen[r] {
			return fmt.Errorf("role %s is there twice", r)
		}
		seen[r] = true
	}
	if seen[RoleData] && seen[RoleRepair] {
		return errors.New("a path cannot have both the data and the repair role, because a repair path is a separate path")
	}
	if p.Subs < 1 || p.Subs > MaxSubs {
		return fmt.Errorf("%d sub-connections (want 1 to %d)", p.Subs, MaxSubs)
	}
	return nil
}

// MaxPaths is the largest number of paths a peer can have.
const MaxPaths = 16

// CheckPaths reports whether paths can be the paths of one peer: each passes Check and
// no two have the same name.
func CheckPaths(paths []Path) error {
	if len(paths) > MaxPaths {
		return fmt.Errorf("%d paths (at most %d)", len(paths), MaxPaths)
	}
	names := map[string]bool{}
	for _, path := range paths {
		if err := path.Check(); err != nil {
			return fmt.Errorf("path %q: %w", path.Name, err)
		}
		if names[path.Name] {
			return fmt.Errorf("two paths are called %q", path.Name)
		}
		names[path.Name] = true
	}
	return nil
}

// IsFingerprint reports whether s has the form of a fingerprint: 64 lower-case hex
// digits.
func IsFingerprint(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Save writes the state file at path (0600), replacing it in one step.
func (s *State) Save(path string) error {
	out := s.clone()
	out.fill()
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(b, '\n'), 0o600)
}
