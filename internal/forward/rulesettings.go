package forward

// The transport settings of a rule at the other node of its sessions (since 0.12.0).
//
// A rule's transport settings act at the node that stores the rule. Where the data of
// a session flow mostly from the other node, the settings that count are the ones the
// other node sends with, and that node keeps settings only for the peer as a whole. So
// a node names to each peer that takes them (control.FeatureRuleSettings) the sets of
// settings its rules have for that peer, each under a number, and a session of such a
// rule tells the peer its number: in the open that asks the peer for sub-connections,
// and in the opening of each sub-connection this node dials. The peer creates the
// session with the set in the place of what it keeps for this node; a setting that the
// peer's command line gives stays as it is there.
//
// Only the settings that each session has for itself and that take no more memory at
// the peer travel (travelling). A set is named again after every hello of the peer and
// whenever the rules change, whole: the last rule_settings takes the place of the ones
// before. A number is used once in the life of a process, and begins at a random value,
// so a number that the peer still holds from an earlier set, or from an earlier process
// of this node, names nothing that this node means.

import (
	"crypto/rand"
	"encoding/binary"
	"maps"
	"slices"
	"strings"

	"github.com/Claw-King/tcpraid/internal/control"
	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/node"
)

// travelling are the settings of a rule that its sessions take at the peer too.
var travelling = []string{"coalesce", "hol", "target", "dead", "kickratio", "kickfloor", "share", "fbsubs", "fbevery"}

// travellingOf returns the settings of t that travel; nil when there are none.
func travellingOf(t node.Transport) node.Transport {
	var out node.Transport
	for _, name := range travelling {
		if v, ok := t[name]; ok {
			if out == nil {
				out = node.Transport{}
			}
			out[name] = v
		}
	}
	return out
}

// outSet is the set a rule of this node names to its peer (rt.mu).
type outSet struct {
	id   uint64
	text string // the set as Transport.String writes it, to see whether it changed
	t    node.Transport
}

// ruleSet returns the number of the set that the sessions of rule r take at its peer,
// and 0 when they take none: the rule has no setting that travels, or the peer does
// not take such sets.
//
// The settings are the rule's as the node stores them now, as for the session at this
// node (settingsFor): a listener can still hold the rule as it was when it started.
func (rt *Runtime) ruleSet(r node.Rule) uint64 {
	if !rt.peerSays(r.Peer, control.FeatureRuleSettings) {
		return 0
	}
	t := rt.Node.RuleTransport(r.Name)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.setOfLocked(r.Name, t)
}

// setOfLocked returns the number of the set of the rule called name whose settings are
// all, a new one when they changed since the number was given, and 0 for a rule
// without settings that travel (rt.mu held).
func (rt *Runtime) setOfLocked(name string, all node.Transport) uint64 {
	t := travellingOf(all)
	if len(t) == 0 {
		delete(rt.outSets, name)
		return 0
	}
	if rt.outSets == nil {
		rt.outSets = map[string]outSet{}
	}
	text := t.String()
	if o, ok := rt.outSets[name]; ok && o.text == text {
		return o.id
	}
	if rt.setSeq == 0 {
		var b [8]byte
		rand.Read(b[:])
		// room above for every number the process will give
		rt.setSeq = binary.BigEndian.Uint64(b[:])>>2 | 1
	}
	rt.setSeq++
	rt.outSets[name] = outSet{id: rt.setSeq, text: text, t: t}
	return rt.setSeq
}

// setsFor returns the sets that the rules of this node have for the peer called name,
// MaxRuleSets at most, in the order of the rules, and the names of the rules that did
// not fit. It forgets the sets of rules that are gone.
func (rt *Runtime) setsFor(name string) (sets []control.RuleSet, left []string) {
	rules := rt.Node.Rules()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for rule := range rt.outSets {
		if !slices.ContainsFunc(rules, func(r node.Rule) bool { return r.Name == rule }) {
			delete(rt.outSets, rule)
		}
	}
	for _, r := range rules {
		if r.Peer != name {
			continue
		}
		id := rt.setOfLocked(r.Name, r.Transport)
		if id == 0 {
			continue
		}
		if len(sets) == control.MaxRuleSets {
			left = append(left, r.Name)
			continue
		}
		sets = append(sets, control.RuleSet{ID: id, Settings: maps.Clone(rt.outSets[r.Name].t)})
	}
	return sets, left
}

// announceSets names to the peer, on cc, the sets of this node's rules for it, if the
// peer takes them. It is called when the peer's hello has arrived on cc.
func (rt *Runtime) announceSets(cc *ctlConn) {
	if !rt.peerSays(cc.peer, control.FeatureRuleSettings) {
		return
	}
	sets, left := rt.setsFor(cc.peer)
	if len(left) > 0 {
		rt.logf("peer %q: the transport settings of the rules %s are not named to the peer, which takes %d sets at most. Their sessions take the peer's own settings at the peer.",
			cc.peer, strings.Join(left, ", "), control.MaxRuleSets)
	}
	cc.Send(control.Message{Type: control.TypeRuleSettings, Sets: sets})
}

// announceSetsAll names the sets anew to every peer that is connected and takes them,
// after the rules changed.
func (rt *Runtime) announceSetsAll() {
	rt.mu.Lock()
	var ccs []*ctlConn
	for _, pc := range rt.ctl {
		if len(pc.conns) > 0 {
			ccs = append(ccs, pc.conns[0])
		}
	}
	rt.mu.Unlock()
	for _, cc := range ccs {
		rt.announceSets(cc)
	}
}

// setsNamed takes a peer's rule_settings: the sets it names take the place of the ones
// it named before. Each value is checked by this node's own rules for the setting; a
// setting that does not travel, or whose value does not pass, is left out and logged,
// and a set without a number or beyond MaxRuleSets is passed over.
func (rt *Runtime) setsNamed(peer string, m control.Message) {
	got := map[uint64]node.Transport{}
	var dropped []string
	for _, set := range m.Sets[:min(len(m.Sets), control.MaxRuleSets)] {
		if set.ID == 0 {
			continue
		}
		t := node.Transport{}
		names := slices.Sorted(maps.Keys(set.Settings))
		for _, name := range names[:min(len(names), 2*len(travelling))] {
			value := set.Settings[name]
			if !slices.Contains(travelling, name) {
				dropped = append(dropped, printable(name, 32))
				continue
			}
			stored, err := node.ParseSetting(name, value)
			if err != nil {
				dropped = append(dropped, name+"="+printable(value, 32))
				continue
			}
			t[name] = stored
		}
		got[set.ID] = t
	}
	rt.mu.Lock()
	if rt.inSets == nil {
		rt.inSets = map[string]map[uint64]node.Transport{}
	}
	rt.inSets[peer] = got
	rt.mu.Unlock()
	if len(dropped) > 0 {
		rt.logf("peer %q: of the transport settings its rules name for their sessions at this node, these are not taken: %s. The sessions take this node's settings for the peer in their place.",
			peer, strings.Join(dropped[:min(len(dropped), 8)], ", "))
	}
}

// settingsWithSet returns the settings of a session with the peer that the peer
// created, under the set with the number set that the peer named, if there is one:
// the set goes before what this node keeps for the peer and for itself, except for a
// setting that this node's command line gives.
func (rt *Runtime) settingsWithSet(peer node.Peer, set uint64) *engine.Config {
	cfg := rt.settingsFor(peer, "")
	if set == 0 {
		return cfg
	}
	rt.mu.Lock()
	t := rt.inSets[peer.Name][set]
	rt.mu.Unlock()
	for name, stored := range t {
		if !rt.Pinned[name] {
			node.Transport{name: stored}.Apply(cfg)
		}
	}
	return cfg
}

// peerLacks reports whether the peer called name is connected and its hello did not
// name the feature.
func (rt *Runtime) peerLacks(name, feature string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	pc := rt.ctl[name]
	return pc != nil && len(pc.conns) > 0 && pc.greeted && !slices.Contains(pc.features, feature)
}
