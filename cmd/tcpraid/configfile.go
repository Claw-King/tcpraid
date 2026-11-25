package main

// The config commands: the node's configuration as one file, written out and applied.
//
//	tcpraid config export [-o FILE]
//	tcpraid config apply FILE [-dry-run] [-prune]
//
// The file is YAML and holds what describes the node without its secrets: its name, its
// peers with their paths, its rules, and the transport settings that were set while it
// ran. It holds no private key, no pair secret, no token, no password and no pairing
// code, so it can be kept where configuration is kept. It is not a backup: the identity
// of a node and its pairings are in the data directory and nowhere else.
//
// Both commands work through the local API of the running node (package client).
// Apply changes what differs and nothing else. It never pairs and never ends a pairing.

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Claw-King/tcpraid/client"
	"github.com/Claw-King/tcpraid/internal/api"
)

// configVersion is the version of the file's format that this program writes and reads.
const configVersion = 1

// proxyLoginKept stands in the file where a path's proxy has a user and a password in
// the node. They are not exported.
const proxyLoginKept = "kept in the node, not exported"

// configFile is the exported file.
type configFile struct {
	Version int          `yaml:"version"`
	Node    configNode   `yaml:"node"`
	Peers   []configPeer `yaml:"peers"`
	Rules   []configRule `yaml:"rules"`
}

type configNode struct {
	Name string `yaml:"name"`
	// Fingerprint says which node the file was exported from. Apply gives the node
	// the name of the file only when it is that node.
	Fingerprint string `yaml:"fingerprint,omitempty"`
	// Transport: the settings stored in the node while it ran, by name.
	Transport map[string]string `yaml:"transport,omitempty"`
}

type configPeer struct {
	Name        string            `yaml:"name"`
	Fingerprint string            `yaml:"fingerprint"`
	Allow       string            `yaml:"allow"`
	UDPDial     string            `yaml:"udp_dial"`
	Transport   map[string]string `yaml:"transport,omitempty"`
	Paths       []configPath      `yaml:"paths"`
}

type configPath struct {
	Name  string   `yaml:"name"`
	Type  string   `yaml:"type"`
	Addr  string   `yaml:"addr"`
	Addrs []string `yaml:"addrs,omitempty"`
	Proxy string   `yaml:"proxy,omitempty"`
	// ProxyLogin is proxyLoginKept where the proxy has a user and a password in the
	// node; apply reads it and does nothing with it.
	ProxyLogin string   `yaml:"proxy_login,omitempty"`
	Security   string   `yaml:"security"`
	Verify     string   `yaml:"verify,omitempty"`
	Pin        string   `yaml:"pin,omitempty"`
	Roles      []string `yaml:"roles"`
	Subs       int      `yaml:"subs"`
	Mark       int      `yaml:"mark,omitempty"`
}

type configRule struct {
	Name        string `yaml:"name"`
	Peer        string `yaml:"peer"`
	Listen      string `yaml:"listen,omitempty"`
	Destination string `yaml:"destination,omitempty"`
	Match       string `yaml:"match,omitempty"`
	Socks       string `yaml:"socks,omitempty"`
	// Enabled: a rule of the file that does not say is enabled, as one that is
	// added with the command.
	Enabled   *bool             `yaml:"enabled"`
	Paths     []string          `yaml:"paths,omitempty"`
	Subs      int               `yaml:"subs,omitempty"`
	Dial      string            `yaml:"dial,omitempty"`
	Transport map[string]string `yaml:"transport,omitempty"`
}

// nodeState is what the config commands read from the node.
type nodeState struct {
	file      configFile
	transport map[string]api.TransportSetting // by name
}

// readNodeState asks the node for everything a configFile holds.
func readNodeState(c *client.Client) (nodeState, error) {
	ctx := context.Background()
	var st nodeState
	status, err := c.Status(ctx)
	if err != nil {
		return st, err
	}
	peers, err := c.Peers(ctx)
	if err != nil {
		return st, err
	}
	rules, err := c.Rules(ctx)
	if err != nil {
		return st, err
	}
	tr, err := c.Transport(ctx)
	if err != nil {
		return st, olderNodeTransport(err)
	}
	f := configFile{Version: configVersion, Node: configNode{Name: status.Node.Name, Fingerprint: status.Node.Fingerprint}}
	st.transport = map[string]api.TransportSetting{}
	for _, s := range tr.Settings {
		st.transport[s.Name] = s
		if s.Stored != "" {
			if f.Node.Transport == nil {
				f.Node.Transport = map[string]string{}
			}
			f.Node.Transport[s.Name] = s.Stored
		}
	}
	for _, p := range peers {
		cp := configPeer{Name: p.Name, Fingerprint: p.Fingerprint, Allow: p.Allow, UDPDial: p.UDPDial, Transport: p.Transport}
		for _, path := range p.Paths {
			cpath := configPath{Name: path.Name, Type: path.Type, Addr: path.Addr, Addrs: path.Addrs, Proxy: path.Proxy,
				Security: path.Security, Verify: path.Verify, Pin: path.Pin, Roles: path.Roles, Subs: path.Subs, Mark: path.Mark}
			if path.ProxyLogin {
				cpath.ProxyLogin = proxyLoginKept
			}
			cp.Paths = append(cp.Paths, cpath)
		}
		f.Peers = append(f.Peers, cp)
	}
	for _, r := range rules {
		f.Rules = append(f.Rules, configRule{Name: r.Name, Peer: r.Peer, Listen: r.Listen, Destination: r.Destination, Match: r.Match,
			Socks: r.Socks, Enabled: &r.Enabled, Paths: r.Paths, Subs: r.Subs, Dial: r.Dial, Transport: r.Transport})
	}
	st.file = normalConfig(f)
	return st, nil
}

// normalConfig brings a configFile into the one form in which two that mean the same
// are equal: no empty lists and maps, "auto" for no dial, the peers and rules by name.
func normalConfig(f configFile) configFile {
	if len(f.Node.Transport) == 0 {
		f.Node.Transport = nil
	}
	peers := append([]configPeer{}, f.Peers...)
	for i := range peers {
		p := &peers[i]
		if len(p.Transport) == 0 {
			p.Transport = nil
		}
		paths := append([]configPath{}, p.Paths...)
		for j := range paths {
			if len(paths[j].Addrs) == 0 {
				paths[j].Addrs = nil
			}
			if len(paths[j].Roles) == 0 {
				paths[j].Roles = nil
			}
			paths[j].ProxyLogin = ""
		}
		if len(paths) == 0 {
			paths = nil
		}
		p.Paths = paths
	}
	sort.SliceStable(peers, func(i, j int) bool { return peers[i].Name < peers[j].Name })
	rules := append([]configRule{}, f.Rules...)
	for i := range rules {
		r := &rules[i]
		if len(r.Transport) == 0 {
			r.Transport = nil
		}
		if len(r.Paths) == 0 {
			r.Paths = nil
		}
		if r.Dial == "auto" {
			r.Dial = ""
		}
		enabled := r.Enabled == nil || *r.Enabled
		r.Enabled = &enabled
	}
	sort.SliceStable(rules, func(i, j int) bool { return rules[i].Name < rules[j].Name })
	if len(peers) == 0 {
		peers = nil
	}
	if len(rules) == 0 {
		rules = nil
	}
	f.Peers, f.Rules = peers, rules
	return f
}

const configHeader = `# The configuration of a tcpraid node, written by "tcpraid config export".
# "tcpraid config apply FILE" makes a running node match it.
# It holds no private key, no pair secret, no token, no password and no pairing code.
# The user and the password of a proxy are not exported either: where a path has them,
# proxy_login says so, and apply leaves them in the node as they are.
# It is not a backup. The identity of a node and its pairings are in its data
# directory, and a peer of this file has to be paired before apply can change it.
`

// writeConfig writes the file for st, with the note on proxies that have a login.
func writeConfig(w io.Writer, c *client.Client) error {
	st, err := readNodeState(c)
	if err != nil {
		return err
	}
	// the proxy_login notes were taken out for comparing; put them back for the file
	peers, err := c.Peers(context.Background())
	if err != nil {
		return err
	}
	login := map[[2]string]bool{}
	for _, p := range peers {
		for _, path := range p.Paths {
			if path.ProxyLogin {
				login[[2]string{p.Name, path.Name}] = true
			}
		}
	}
	for i := range st.file.Peers {
		for j := range st.file.Peers[i].Paths {
			if login[[2]string{st.file.Peers[i].Name, st.file.Peers[i].Paths[j].Name}] {
				st.file.Peers[i].Paths[j].ProxyLogin = proxyLoginKept
			}
		}
	}
	var b bytes.Buffer
	b.WriteString(configHeader)
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(st.file); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	_, err = w.Write(b.Bytes())
	return err
}

// parseConfig reads a file. A field that this program does not know is an error, and
// so is another version of the format.
func parseConfig(data []byte) (configFile, error) {
	var f configFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return f, errors.New("the file is empty")
		}
		return f, fmt.Errorf("the file cannot be read as a configuration: %v", err)
	}
	var more any
	if err := dec.Decode(&more); !errors.Is(err, io.EOF) {
		return f, errors.New("the file holds more than one YAML document. A configuration is one.")
	}
	if f.Version != configVersion {
		return f, fmt.Errorf("the file says version %d, and this program reads version %d", f.Version, configVersion)
	}
	if f.Node.Name == "" {
		return f, errors.New("the file names no node (node.name)")
	}
	seen := map[string]bool{}
	for _, p := range f.Peers {
		if p.Name == "" || p.Fingerprint == "" {
			return f, errors.New("a peer of the file lacks its name or its fingerprint")
		}
		if seen[p.Name] {
			return f, fmt.Errorf("the file has two peers called %q", p.Name)
		}
		seen[p.Name] = true
		for _, path := range p.Paths {
			if path.ProxyLogin != "" && path.ProxyLogin != proxyLoginKept {
				return f, fmt.Errorf("peer %q, path %q: proxy_login is a note that export writes (%q). To set a user and a password, put them into the proxy URL.", p.Name, path.Name, proxyLoginKept)
			}
		}
	}
	seen = map[string]bool{}
	for _, r := range f.Rules {
		if r.Name == "" {
			return f, errors.New("a rule of the file has no name")
		}
		if seen[r.Name] {
			return f, fmt.Errorf("the file has two rules called %q", r.Name)
		}
		seen[r.Name] = true
	}
	return normalConfig(f), nil
}

// configStep is one change that apply makes: what it is, in words, and how it is done.
type configStep struct {
	text string
	do   func(ctx context.Context, c *client.Client) error
}

// transportChange returns the request that turns the settings have into want: every
// name of want with its value, and every name that only have holds with nil.
func transportChange(have, want map[string]string) map[string]*string {
	change := map[string]*string{}
	for name, v := range want {
		v := v
		change[name] = &v
	}
	for name := range have {
		if _, ok := want[name]; !ok {
			change[name] = nil
		}
	}
	return change
}

// configPlan compares the file with the node and returns the changes that make the
// node match, in the order they are made, and the peers of the file that are not
// paired with the node. prune: rules and stored node settings that the file does not
// have are taken away. An error means that the file asks for something that cannot be
// done, and nothing should be changed. notes say what of the file is left out and why.
//
// The node gets the name of the file only when the file was exported from this node
// (node.fingerprint). A file of another node, or one that does not say, is taken as a
// pattern: its name stays with the node it came from.
//
// A rule whose peer the file has and this node is not paired with is left out: a peer
// of that name on this node would be another node.
func configPlan(want configFile, st nodeState, prune bool) (steps []configStep, unpaired []configPeer, notes []string, err error) {
	have := st.file
	if want.Node.Name != have.Node.Name {
		if want.Node.Fingerprint != "" && want.Node.Fingerprint == have.Node.Fingerprint {
			name := want.Node.Name
			steps = append(steps, configStep{fmt.Sprintf("node: the name %q becomes %q", have.Node.Name, name), func(ctx context.Context, c *client.Client) error {
				return c.Call(ctx, http.MethodPut, client.PathNode, client.NodeUpdate{Name: name}, nil)
			}})
		} else {
			notes = append(notes, fmt.Sprintf("The file is of the node %q and this node is %q. The name is left as it is, because the file does not say that it was exported from this node (node.fingerprint).", want.Node.Name, have.Node.Name))
		}
	}
	// the node's own transport settings
	set := map[string]*string{}
	var names []string
	for name, v := range want.Node.Transport {
		cur, known := st.transport[name]
		switch {
		case !known:
			return nil, nil, nil, fmt.Errorf("node.transport: the node has no setting called %q", name)
		case cur.Stored == v:
		case cur.Source == "command line":
			return nil, nil, nil, fmt.Errorf("node.transport: the file sets %s to %s, and the node was started with -%s on its command line, which goes before a stored value. Take %s out of the file, or start the node without -%s.", name, v, name, name, name)
		default:
			v := v
			set[name] = &v
			names = append(names, fmt.Sprintf("%s=%s", name, v))
		}
	}
	if prune {
		for name, cur := range st.transport {
			if _, ok := want.Node.Transport[name]; !ok && cur.Stored != "" {
				set[name] = nil
				names = append(names, name+"=default")
			}
		}
	}
	if len(set) > 0 {
		sort.Strings(names)
		steps = append(steps, configStep{"node: transport settings " + strings.Join(names, " "), func(ctx context.Context, c *client.Client) error {
			_, err := c.SetTransport(ctx, set)
			return err
		}})
	}
	// peers
	havePeers := map[string]configPeer{}
	for _, p := range have.Peers {
		havePeers[p.Name] = p
	}
	for _, p := range want.Peers {
		cur, ok := havePeers[p.Name]
		if !ok || cur.Fingerprint != p.Fingerprint {
			unpaired = append(unpaired, p)
			continue
		}
		if reflect.DeepEqual(cur, p) {
			continue
		}
		p := p
		var what []string
		if !reflect.DeepEqual(cur.Paths, p.Paths) {
			what = append(what, "paths")
		}
		if cur.Allow != p.Allow {
			what = append(what, "allow list")
		}
		if cur.UDPDial != p.UDPDial {
			what = append(what, "UDP address")
		}
		if !reflect.DeepEqual(cur.Transport, p.Transport) {
			what = append(what, "transport settings")
		}
		steps = append(steps, configStep{fmt.Sprintf("peer %q: %s", p.Name, strings.Join(what, ", ")), func(ctx context.Context, c *client.Client) error {
			paths := []api.Path{}
			for _, path := range p.Paths {
				paths = append(paths, api.Path{Name: path.Name, Type: path.Type, Addr: path.Addr, Addrs: path.Addrs, Proxy: path.Proxy,
					Security: path.Security, Verify: path.Verify, Pin: path.Pin, Roles: path.Roles, Subs: path.Subs, Mark: path.Mark})
			}
			u := api.PeerUpdate{Paths: &paths, Allow: &p.Allow, UDPDial: &p.UDPDial}
			if change := transportChange(cur.Transport, p.Transport); len(change) > 0 {
				u.Transport = change
			}
			_, err := c.UpdatePeer(ctx, p.Name, u)
			return err
		}})
	}
	// rules
	haveRules := map[string]configRule{}
	for _, r := range have.Rules {
		haveRules[r.Name] = r
	}
	inFile := map[string]bool{}
	for _, r := range want.Rules {
		inFile[r.Name] = true
	}
	// what is removed goes first, so that a rule of the file can take the address of
	// one that goes
	if prune {
		for _, r := range have.Rules {
			if !inFile[r.Name] {
				name := r.Name
				steps = append(steps, configStep{fmt.Sprintf("rule %q: removed, because the file does not have it", name), func(ctx context.Context, c *client.Client) error {
					return c.RemoveRule(ctx, name)
				}})
			}
		}
	}
	notPaired := map[string]bool{}
	for _, p := range unpaired {
		notPaired[p.Name] = true
	}
	for _, r := range want.Rules {
		if notPaired[r.Peer] {
			notes = append(notes, fmt.Sprintf("rule %q is left out, because its peer %q is in the file and not paired with this node.", r.Name, r.Peer))
			continue
		}
		cur, ok := haveRules[r.Name]
		if ok && reflect.DeepEqual(cur, r) {
			continue
		}
		r := r
		spec := func() api.RuleSpec {
			paths := append([]string{}, r.Paths...)
			subs := r.Subs
			dial := r.Dial
			if dial == "" {
				dial = "auto"
			}
			enabled := r.Enabled == nil || *r.Enabled
			s := api.RuleSpec{Name: r.Name, Listen: r.Listen, Destination: r.Destination, Match: r.Match, Socks: r.Socks, Peer: r.Peer,
				Enabled: &enabled, Paths: &paths, Subs: &subs, Dial: &dial}
			if change := transportChange(cur.Transport, r.Transport); len(change) > 0 {
				s.Transport = change
			}
			return s
		}
		if !ok {
			steps = append(steps, configStep{fmt.Sprintf("rule %q: added", r.Name), func(ctx context.Context, c *client.Client) error {
				_, err := c.AddRule(ctx, spec())
				return err
			}})
			continue
		}
		steps = append(steps, configStep{fmt.Sprintf("rule %q: changed", r.Name), func(ctx context.Context, c *client.Client) error {
			_, err := c.ReplaceRule(ctx, r.Name, spec())
			return err
		}})
	}
	return steps, unpaired, notes, nil
}

func cmdConfig(args []string, stdout, stderr io.Writer) int {
	var outFile string
	var dryRun, prune bool
	return dispatch("config", "", []string{"export", "apply"}, map[string]command{
		"export": {name: "config export", usage: "",
			flags: func(fs *flag.FlagSet) {
				fs.StringVar(&outFile, "o", "", "`file` to write (default: the standard output)")
			},
			run: func(dataDir string, _ []string, stdout, _ io.Writer) error {
				c := client.New(dataDir)
				c.Timeout = shortCall
				var b bytes.Buffer
				if err := writeConfig(&b, c); err != nil {
					return err
				}
				if outFile == "" {
					_, err := stdout.Write(b.Bytes())
					return err
				}
				return os.WriteFile(outFile, b.Bytes(), 0o644)
			}},
		"apply": {name: "config apply", usage: "FILE", nargs: 1,
			flags: func(fs *flag.FlagSet) {
				fs.BoolVar(&dryRun, "dry-run", false, "say what would be changed, and change nothing")
				fs.BoolVar(&prune, "prune", false, "also remove the rules, and take back the stored settings of the node, that the file does not have. A peer is never removed.")
			},
			run: func(dataDir string, args []string, stdout, _ io.Writer) error {
				data, err := os.ReadFile(args[0])
				if err != nil {
					return err
				}
				want, err := parseConfig(data)
				if err != nil {
					return fmt.Errorf("%s: %w", args[0], err)
				}
				c := client.New(dataDir)
				c.Timeout = shortCall
				st, err := readNodeState(c)
				if err != nil {
					return err
				}
				steps, unpaired, notes, err := configPlan(want, st, prune)
				if err != nil {
					return fmt.Errorf("%w Nothing was changed.", err)
				}
				for _, n := range notes {
					fmt.Fprintln(stdout, n)
				}
				for _, p := range unpaired {
					fmt.Fprintf(stdout, "peer %q with the fingerprint %s is in the file and not paired with this node. Apply does not pair. Pair the two nodes with \"tcpraid pair\", then apply the file again.\n", p.Name, p.Fingerprint)
				}
				if !prune {
					left := 0
					inFile := map[string]bool{}
					for _, r := range want.Rules {
						inFile[r.Name] = true
					}
					for _, r := range st.file.Rules {
						if !inFile[r.Name] {
							left++
						}
					}
					if left > 0 {
						fmt.Fprintf(stdout, "The node keeps %s that the file does not have. With -prune they are removed.\n", countOf(left, "rule"))
					}
				}
				if len(steps) == 0 {
					fmt.Fprintln(stdout, "nothing to change, because the node is as the file describes it")
				}
				for i, s := range steps {
					if dryRun {
						fmt.Fprintf(stdout, "would change: %s\n", s.text)
						continue
					}
					if err := s.do(context.Background(), c); err != nil {
						var refused *client.Error
						if !errors.As(err, &refused) {
							return fmt.Errorf("%s: %w. %d of %d changes were made before this one, and nothing is undone. The file can be applied again.", s.text, err, i, len(steps))
						}
						return fmt.Errorf("%s: the node did not take it: %v %d of %d changes were made before this one, and nothing is undone. The file can be applied again.", s.text, sentence(err.Error()), i, len(steps))
					}
					fmt.Fprintf(stdout, "changed: %s\n", s.text)
				}
				if len(unpaired) > 0 {
					return fmt.Errorf("the file is not applied in full, because it has %s that this node is not paired with", countOf(len(unpaired), "peer"))
				}
				return nil
			}},
	}, args, stdout, stderr)
}

// countOf writes a number with the thing it counts: "1 rule", "3 rules".
func countOf(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// sentence ends s with a full stop if it has none.
func sentence(s string) string {
	if strings.HasSuffix(s, ".") || strings.HasSuffix(s, "?") || strings.HasSuffix(s, "!") {
		return s
	}
	return s + "."
}
