package node

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Claw-King/tcpraid/internal/engine"
)

// Rule is a forwarding rule: which connections this node carries to which peer.
//
// A fixed rule has Listen and Destination: the node listens there, and a connection
// that arrives goes to Destination, as seen from the peer. Both may name a range of
// ports ("0.0.0.0:50000-50050"): the ranges have the same length and correspond port
// by port. The destination may be a host name instead of an IP address
// ("nas.example.net:873"): the peer, whose network the destination is in, looks the
// name up for every connection.
//
// A transparent rule has Match instead: it applies to the connections that the host
// redirected to the node's transparent listener, by their original destination. The first transparent rule that matches, in the order of the list, takes
// the connection, which goes to that same destination as seen from the peer.
type Rule struct {
	Name        string `json:"name"`                  // unique among the node's rules
	Listen      string `json:"listen,omitempty"`      // fixed: IP:port or IP:first-last; the IP may be left out (":5000")
	Destination string `json:"destination,omitempty"` // fixed: HOST:port or HOST:first-last at the peer's end; HOST is an IP address or a host name
	Match       string `json:"match,omitempty"`       // transparent: networks in the form of engine.ParseAllow
	Peer        string `json:"peer"`                  // name of the peer the connections go to
	Enabled     bool   `json:"enabled"`
	// Paths names the paths the rule's connections use, of the paths between this node
	// and the peer that carry data (since 0.5.0): the names are the ones the node that
	// holds the paths gives them. Empty means every path. Subs, if not 0, is the most
	// data sub-connections a connection of the rule keeps on each path; a path never
	// keeps more than it is set to. Sub-connections for control and repair are not
	// affected by either.
	Paths []string `json:"paths,omitempty"`
	Subs  int      `json:"subs,omitempty"`
	// Dial says which node opens the sub-connections of the rule's connections (since
	// 0.6.0): DialSelf, DialPeer or DialBoth. Empty is DialAuto.
	Dial string `json:"dial,omitempty"`
	// Transport holds the transport settings of the rule's connections, at this node's
	// end (since 0.7.0). It goes before the peer's settings and the node's.
	Transport Transport `json:"transport,omitempty"`
}

// Who opens the sub-connections of a rule's connections (Rule.Dial).
const (
	// DialAuto: this node, if it holds a path to the peer and the path works; the
	// peer, if this node holds no path or its path is down while the peer's is not. A
	// rule is stored with an empty Dial for it.
	DialAuto = "auto"
	// DialSelf: this node only.
	DialSelf = "self"
	// DialPeer: the peer only, also where this node holds a path.
	DialPeer = "peer"
	// DialBoth: this node and the peer at the same time, for the same session. Each
	// of the two nodes needs a path to the other.
	DialBoth = "both"
)

// DialMode returns who opens the sub-connections of the rule's connections: one of
// DialAuto, DialSelf, DialPeer and DialBoth.
func (r Rule) DialMode() string {
	if r.Dial == "" {
		return DialAuto
	}
	return r.Dial
}

// CheckDial reports whether dial is a value Rule.Dial can have, and returns the value
// as a rule stores it: DialAuto is stored as the empty string.
func CheckDial(dial string) (string, error) {
	switch dial {
	case "", DialAuto:
		return "", nil
	case DialSelf, DialPeer, DialBoth:
		return dial, nil
	}
	return "", fmt.Errorf("dial is %q (want auto, self, peer or both)", dial)
}

// Clone returns a copy that shares no memory with r.
func (r Rule) Clone() Rule {
	if r.Paths != nil {
		r.Paths = append([]string{}, r.Paths...)
	}
	r.Transport = r.Transport.Clone()
	return r
}

// Equal reports whether the two rules say the same about which connections they take
// and where those go. The transport settings are left out: a session reads them when
// it is created, so a change of them needs no new listener.
func (r Rule) Equal(o Rule) bool {
	return r.Name == o.Name && r.Listen == o.Listen && r.Destination == o.Destination && r.Match == o.Match &&
		r.Peer == o.Peer && r.Enabled == o.Enabled && r.Subs == o.Subs && slices.Equal(r.Paths, o.Paths) &&
		r.DialMode() == o.DialMode()
}

// Narrowed reports whether the rule names paths or a number of sub-connections.
func (r Rule) Narrowed() bool { return len(r.Paths) > 0 || r.Subs != 0 }

// Limits of rules.
const (
	MaxRuleNameLen = 64   // bytes
	MaxRulePorts   = 1024 // ports in the range of one rule
	MaxRules       = 256
)

// Transparent reports whether the rule is a transparent rule.
func (r Rule) Transparent() bool { return r.Match != "" }

// PortRange is an address with a range of ports: Host:First to Host:Last.
type PortRange struct {
	// Host is an IP address; empty in a listen address means every address. In a
	// destination it may be a host name instead (Named).
	Host        string
	First, Last int
}

// Named reports whether the host of the range is a host name and not an IP address.
func (p PortRange) Named() bool { return p.Host != "" && net.ParseIP(p.Host) == nil }

// Ports returns the number of ports in the range.
func (p PortRange) Ports() int { return p.Last - p.First + 1 }

// Addr returns the address of the i-th port of the range, counting from 0.
func (p PortRange) Addr(i int) string { return net.JoinHostPort(p.Host, strconv.Itoa(p.First+i)) }

// MaxHostNameLen is the longest host name a destination may have, in bytes.
const MaxHostNameLen = 253

// CheckHostName reports whether name can be the host name in a rule's destination: at
// most MaxHostNameLen bytes, made of labels that are separated by dots, each 1 to 63
// letters, digits, hyphens or underscores and neither starting nor ending with a
// hyphen. The last label must not be all digits, so that an IP address with a mistake
// in it ("192.0.2") is not taken for a name.
func CheckHostName(name string) error {
	if name == "" || len(name) > MaxHostNameLen {
		return fmt.Errorf("a host name has 1 to %d characters", MaxHostNameLen)
	}
	labels := strings.Split(name, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return errors.New("each part of a host name between its dots has 1 to 63 characters and neither starts nor ends with a hyphen")
		}
		for _, c := range []byte(l) {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return errors.New("a host name is made of letters, digits, hyphens, underscores and dots")
			}
		}
	}
	if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return errors.New("it is neither an IP address nor a host name, whose last part is never a number")
	}
	return nil
}

// ParseDestination reads the destination of a fixed rule: "HOST:port" or
// "HOST:first-last", where HOST is an IP address (an IPv6 address in brackets) or a host
// name that passes CheckHostName.
func ParseDestination(s string) (PortRange, error) {
	return parsePortRange(s, false, true)
}

// ParsePortRange reads "IP:port" or "IP:first-last" (an IPv6 address in brackets). With
// anyHost, the IP may be left out. A host name is refused: see ParseDestination.
func ParsePortRange(s string, anyHost bool) (PortRange, error) {
	return parsePortRange(s, anyHost, false)
}

func parsePortRange(s string, anyHost, names bool) (PortRange, error) {
	host, ports, err := net.SplitHostPort(s)
	if err != nil {
		if names {
			return PortRange{}, fmt.Errorf("%q: want HOST:port or HOST:first-last, where HOST is an IP address or a host name", s)
		}
		return PortRange{}, fmt.Errorf("%q: want IP:port or IP:first-last", s)
	}
	switch {
	case host == "" && anyHost:
	case host == "" && names:
		return PortRange{}, fmt.Errorf("%q: no IP address and no host name", s)
	case host == "":
		return PortRange{}, fmt.Errorf("%q: no IP address", s)
	case net.ParseIP(host) != nil:
	case names:
		if err := CheckHostName(host); err != nil {
			return PortRange{}, fmt.Errorf("%q: %q cannot be used: %w", s, host, err)
		}
	default:
		return PortRange{}, fmt.Errorf("%q: %q is not an IP address. A listen address takes no host name.", s, host)
	}
	first, last, ranged := strings.Cut(ports, "-")
	if !ranged {
		last = first
	}
	p := PortRange{Host: host}
	if p.First, err = strconv.Atoi(first); err != nil || p.First < 1 || p.First > 65535 {
		return PortRange{}, fmt.Errorf("%q: the port is not a number from 1 to 65535", s)
	}
	if p.Last, err = strconv.Atoi(last); err != nil || p.Last < 1 || p.Last > 65535 {
		return PortRange{}, fmt.Errorf("%q: the last port is not a number from 1 to 65535", s)
	}
	if p.Last < p.First {
		return PortRange{}, fmt.Errorf("%q: the last port is below the first", s)
	}
	if p.Ports() > MaxRulePorts {
		return PortRange{}, fmt.Errorf("%q: %d ports (at most %d in one rule)", s, p.Ports(), MaxRulePorts)
	}
	return p, nil
}

// overlaps reports whether two listen ranges would need the same port of the same
// address. A range without an IP, or with 0.0.0.0 or ::, takes its ports on every
// address.
func (p PortRange) overlaps(q PortRange) bool {
	if p.Last < q.First || q.Last < p.First {
		return false
	}
	every := func(h string) bool {
		ip := net.ParseIP(h)
		return h == "" || ip.IsUnspecified()
	}
	return every(p.Host) || every(q.Host) || net.ParseIP(p.Host).Equal(net.ParseIP(q.Host))
}

// CheckRuleName reports whether name can be a rule's name: 1 to MaxRuleNameLen bytes
// of printable characters without white space.
func CheckRuleName(name string) error {
	switch {
	case name == "":
		return errors.New("the rule has no name")
	case len(name) > MaxRuleNameLen:
		return fmt.Errorf("the rule's name is longer than %d bytes", MaxRuleNameLen)
	case !utf8.ValidString(name):
		return errors.New("the rule's name is not valid UTF-8")
	}
	for _, r := range name {
		if !unicode.IsPrint(r) || unicode.IsSpace(r) {
			return errors.New("the rule's name holds a space or a character that is not printable")
		}
	}
	return nil
}

// Check reports whether the rule is well-formed on its own: a name, a peer, and either
// a listen address with a destination or a match. It does not look at other rules or at
// whether the peer exists.
func (r Rule) Check() error {
	if err := CheckRuleName(r.Name); err != nil {
		return err
	}
	if r.Peer == "" {
		return errors.New("the rule names no peer")
	}
	if err := r.checkNarrowing(); err != nil {
		return err
	}
	if err := r.Transport.Check(false); err != nil {
		return err
	}
	if stored, err := CheckDial(r.Dial); err != nil {
		return err
	} else if stored != r.Dial {
		return errors.New("dial \"auto\" is stored as no dial at all")
	}
	if r.Transparent() {
		if r.Listen != "" || r.Destination != "" {
			return errors.New("a rule has either match (transparent) or listen and destination (fixed), not both")
		}
		l, err := engine.ParseAllow(r.Match)
		if err != nil {
			return fmt.Errorf("match: %w", err)
		}
		if l.Empty() {
			return errors.New("match names no network")
		}
		return nil
	}
	if r.Listen == "" || r.Destination == "" {
		return errors.New("a fixed rule needs listen and destination, and a transparent rule needs match")
	}
	_, _, err := r.Ranges()
	return err
}

// checkNarrowing reports whether Paths and Subs are well-formed: names that a path can
// have, each once, and a number of sub-connections a path can have.
func (r Rule) checkNarrowing() error {
	if len(r.Paths) > MaxPaths {
		return fmt.Errorf("the rule names %d paths (at most %d)", len(r.Paths), MaxPaths)
	}
	seen := map[string]bool{}
	for _, name := range r.Paths {
		if err := (Path{Name: name}).checkName(); err != nil {
			return fmt.Errorf("paths: %w", err)
		}
		if seen[name] {
			return fmt.Errorf("paths: %q is there twice", name)
		}
		seen[name] = true
	}
	if r.Subs < 0 || r.Subs > MaxSubs {
		return fmt.Errorf("%d sub-connections for each path (want 1 to %d, or 0 for as many as the path has)", r.Subs, MaxSubs)
	}
	return nil
}

// Ranges returns the listen and destination ranges of a fixed rule.
func (r Rule) Ranges() (listen, destination PortRange, err error) {
	if listen, err = ParsePortRange(r.Listen, true); err != nil {
		return listen, destination, fmt.Errorf("listen %w", err)
	}
	if destination, err = ParseDestination(r.Destination); err != nil {
		return listen, destination, fmt.Errorf("destination %w", err)
	}
	if listen.Ports() != destination.Ports() {
		return listen, destination, fmt.Errorf("listen has %d ports and destination %d, but the two ranges must have the same length", listen.Ports(), destination.Ports())
	}
	return listen, destination, nil
}

// CheckRules reports whether rules can be the rules of one node: each passes Check, no
// two have the same name, and no two fixed rules listen on the same port of the same
// address (whether enabled or not: a rule that is switched on later must not collide).
func CheckRules(rules []Rule) error {
	if len(rules) > MaxRules {
		return fmt.Errorf("%d rules (at most %d)", len(rules), MaxRules)
	}
	names := map[string]bool{}
	type taken struct {
		name string
		r    PortRange
	}
	var listens []taken
	for _, r := range rules {
		if err := r.Check(); err != nil {
			return fmt.Errorf("rule %q: %w", r.Name, err)
		}
		if names[r.Name] {
			return fmt.Errorf("two rules are called %q", r.Name)
		}
		names[r.Name] = true
		if r.Transparent() {
			continue
		}
		l, _, _ := r.Ranges()
		for _, t := range listens {
			if l.overlaps(t.r) {
				return fmt.Errorf("rule %q listens on %s, which rule %q (%s) already takes", r.Name, r.Listen, t.name, formatRange(t.r))
			}
		}
		listens = append(listens, taken{r.Name, l})
	}
	return nil
}

func formatRange(p PortRange) string {
	if p.First == p.Last {
		return p.Addr(0)
	}
	return fmt.Sprintf("%s-%d", p.Addr(0), p.Last)
}

// checkPeerOptions reports whether allow and udpDial can be stored with a peer.
func checkPeerOptions(allow, udpDial string) error {
	if _, err := engine.ParseAllow(allow); err != nil {
		return fmt.Errorf("allow: %w", err)
	}
	if udpDial != "" {
		if err := CheckAddr(udpDial); err != nil {
			return fmt.Errorf("udp_dial: %w", err)
		}
	}
	return nil
}

// Rules returns a copy of the rules, in their order.
func (n *Node) Rules() []Rule {
	n.mu.Lock()
	defer n.mu.Unlock()
	rules := make([]Rule, len(n.state.Rules))
	for i, r := range n.state.Rules {
		rules[i] = r.Clone()
	}
	return rules
}

// AddRule stores a new rule at the end of the list. A rule that is not well-formed,
// collides with another, has a name that is taken or names a peer that is not there is
// the caller's doing (ErrInvalid) and changes nothing.
func (n *Node) AddRule(r Rule) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.update(func(st *State) error {
		st.Rules = append(st.Rules, r.Clone())
		return checkRulesFor(st, r)
	})
}

// ReplaceRule puts r in the place of the rule called name; r may have another name.
// What AddRule refuses is refused here too.
func (n *Node) ReplaceRule(name string, r Rule) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.update(func(st *State) error {
		for i := range st.Rules {
			if st.Rules[i].Name == name {
				st.Rules[i] = r.Clone()
				return checkRulesFor(st, r)
			}
		}
		return fmt.Errorf("rule %q: %w", name, ErrNotFound)
	})
}

// checkRulesFor checks the rules of st after r was put into them.
func checkRulesFor(st *State, r Rule) error {
	if err := CheckRules(st.Rules); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if !nameTaken(st, r.Peer) {
		return fmt.Errorf("%w: rule %q: no peer is called %q", ErrInvalid, r.Name, r.Peer)
	}
	return nil
}

// RemoveRule forgets the rule called name.
func (n *Node) RemoveRule(name string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.update(func(st *State) error {
		for i := range st.Rules {
			if st.Rules[i].Name == name {
				st.Rules = append(st.Rules[:i], st.Rules[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("rule %q: %w", name, ErrNotFound)
	})
}

// SetPeerOptions sets the allow list and the UDP feedback address of the peer called
// name and returns the peer as stored; see UpdatePeer.
func (n *Node) SetPeerOptions(name string, allow, udpDial *string) (Peer, error) {
	return n.UpdatePeer(name, nil, allow, udpDial, nil)
}
