package forward

// The transport settings of sessions: the node's, a peer's and a rule's (node.Transport),
// read when a session is created. A session keeps what it began with.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/node"
)

// Where a setting of the node comes from.
const (
	SourceDefault     = "default"
	SourceCommandLine = "command line" // given when the node was started
	SourceStored      = "stored"       // set while a node ran, and kept in its state file
)

// TransportValue is one transport setting of the node as it is in force for sessions
// that are created from now on, before what their peer and their rule set.
type TransportValue struct {
	Name    string
	Value   string
	Default string
	Source  string // SourceDefault, SourceCommandLine or SourceStored
	// Stored is the value kept in the state file, also where the command line goes
	// before it; empty when there is none.
	Stored string
	// Shared: all sessions of the node share the setting, so a peer and a rule have
	// none of it.
	Shared bool
}

// settingsFor returns the settings of a session with the peer that is created now. rule
// is the name of the rule that carries it; empty for a session that the peer carries
// and for one that carries test data.
func (rt *Runtime) settingsFor(peer node.Peer, rule string) *engine.Config {
	var ruleLayer node.Transport
	if rule != "" {
		ruleLayer = rt.Node.RuleTransport(rule)
	}
	cfg := node.ResolveTransport(rt.Engine.Config(), rt.Pinned, rt.Node.Transport(), peer.Transport, ruleLayer)
	return &cfg
}

// Transport returns the transport settings of the node, every one, in the order they
// are shown.
func (rt *Runtime) Transport() []TransportValue {
	base, stored := rt.Engine.Config(), rt.Node.Transport()
	now := node.ResolveTransport(base, rt.Pinned, stored, nil, nil)
	defaults := engine.DefaultConfig()
	var out []TransportValue
	for _, s := range node.Settings() {
		v := TransportValue{Name: s.Name, Value: s.Value(now), Default: s.Value(defaults), Source: SourceDefault, Stored: stored[s.Name], Shared: s.NodeOnly}
		switch _, has := stored[s.Name]; {
		case rt.Pinned[s.Name]:
			v.Source = SourceCommandLine
		case has:
			v.Source = SourceStored
		}
		out = append(out, v)
	}
	return out
}

// SetTransport changes the transport settings stored for the node and returns all of
// them as they are in force then: each name in change is set to its value, or goes back
// to its default where the value is nil. The change acts on sessions that are created
// afterwards; a session that runs keeps what it began with. A setting that the command
// line of the running node gives cannot be set, because the command line goes before
// what is stored. What cannot be stored is the caller's doing (node.ErrInvalid) and
// changes nothing.
func (rt *Runtime) SetTransport(change map[string]*string) ([]TransportValue, error) {
	base := rt.Engine.Config()
	var pinned []string
	for name, value := range change {
		if value != nil && rt.Pinned[name] {
			pinned = append(pinned, name)
		}
	}
	if len(pinned) > 0 {
		sort.Strings(pinned)
		name := pinned[0]
		s, _ := node.SettingByName(name)
		return nil, fmt.Errorf("%w: the node was started with -%s %s on its command line, which goes before a stored value. Start the node without -%s to set it while the node runs.",
			node.ErrInvalid, name, s.Value(base), name)
	}
	stored, err := rt.Node.SetTransport(change, func(next node.Transport) error {
		return node.CheckTransportFit(node.ResolveTransport(base, rt.Pinned, next, nil, nil))
	})
	if err != nil {
		return nil, err
	}
	var names []string
	for name := range change {
		names = append(names, name)
	}
	sort.Strings(names)
	text := stored.String()
	if text == "" {
		text = "none"
	}
	rt.logf("settings of the node changed (%s). Stored now: %s. Sessions that begin from now on use them.", strings.Join(names, ", "), text)
	return rt.Transport(), nil
}
