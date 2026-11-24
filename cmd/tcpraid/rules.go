package main

// The rules command: the forwarding rules of the node on this host, through its API.

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Claw-King/tcpraid/internal/api"
	"github.com/Claw-King/tcpraid/internal/forward"
	"github.com/Claw-King/tcpraid/internal/node"
)

func rulePath(name string) string { return api.PathRules + "/" + url.PathEscape(name) }

// ruleState says in a word or two how a rule is doing.
func ruleState(r api.Rule) string {
	switch {
	case !r.Enabled:
		return "disabled"
	case r.Error != "" && !r.Listening:
		return "FAILED: " + r.Error
	case r.Error != "" && r.ErrorAt != nil:
		return "listening" + ruleUses(r) + ", last connection: " + r.Error + " (" + ago(time.Since(*r.ErrorAt)) + ")"
	case r.Error != "":
		return "listening" + ruleUses(r) + ", last connection: " + r.Error
	case r.Listening:
		return "listening" + ruleUses(r)
	}
	return "not listening"
}

// ruleUses says, after the word "listening", what is particular about the way the
// rule's connections go: the paths and sub-connections it keeps to, and that the peer
// opens the connections for now. It is empty for a rule that uses everything in the
// usual way, so that the state of such a rule reads as it always did.
func ruleUses(r api.Rule) string {
	s := ""
	if len(r.Paths) > 0 {
		s += ", over the paths " + strings.Join(r.Paths, ",")
	}
	if r.Subs > 0 {
		s += fmt.Sprintf(", with at most %d sub-connections on a path", r.Subs)
	}
	if r.PeerDials {
		s += ", the peer opens the connections while this node's path is down"
	}
	if len(r.Transport) > 0 {
		s += ", with " + transportText(r.Transport)
	}
	switch r.Dial {
	case "self":
		s += ", only this node opens the connections"
	case "peer":
		s += ", only the peer opens the connections"
	case "both":
		s += ", both nodes open the connections"
	}
	return s
}

// ago writes how long ago something happened, to the unit that matters.
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", max(int(d/time.Second), 0))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%dm ago", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
	return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
}

// setRuleEnabled switches the rule called name on or off: it reads the rule and puts it
// back with the switch changed.
func setRuleEnabled(dataDir, name string, enabled bool, stdout io.Writer) error {
	var list []api.Rule
	if err := apiCall(dataDir, http.MethodGet, api.PathRules, nil, &list, shortCall); err != nil {
		return err
	}
	for _, r := range list {
		if r.Name != name {
			continue
		}
		// the paths and sub-connections of the rule and who dials are not sent: the
		// node keeps what the rule has
		spec := api.RuleSpec{Name: r.Name, Listen: r.Listen, Destination: r.Destination, Match: r.Match, Socks: r.Socks, Peer: r.Peer, Enabled: &enabled}
		var out api.Rule
		if err := apiCall(dataDir, http.MethodPut, rulePath(name), spec, &out, shortCall); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "rule %s: %s\n", out.Name, ruleState(out))
		return nil
	}
	return fmt.Errorf("no rule is called %q", name)
}

func cmdRules(args []string, stdout, stderr io.Writer) int {
	var spec api.RuleSpec
	var disabled bool
	var paths, dial string
	var subs int
	var ruleTransport map[string]*string
	const below = "the rule's connections then use the value of the peer or of the node"
	return dispatch("rules", "list", []string{"list", "add", "set", "remove", "enable", "disable"}, map[string]command{
		"list": {
			name: "rules",
			run: func(dataDir string, _ []string, stdout, _ io.Writer) error {
				var list []api.Rule
				if err := apiCall(dataDir, http.MethodGet, api.PathRules, nil, &list, shortCall); err != nil {
					return err
				}
				if len(list) == 0 {
					fmt.Fprintln(stdout, "no rules")
					return nil
				}
				tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tTAKES\tPEER\tDESTINATION\tSESSIONS\tSTATE")
				for _, r := range list {
					takes, dest := r.Listen, r.Destination
					switch {
					case r.Match != "":
						takes, dest = "redirected to "+r.Match, "(as redirected)"
					case r.Socks != "":
						takes, dest = "SOCKS5 on "+r.Socks, "(as the client asks)"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", r.Name, takes, r.Peer, dest, r.Sessions, ruleState(r))
				}
				return tw.Flush()
			},
		},
		"add": {
			name: "rules add", usage: "NAME -peer PEER (-listen IP:PORT[-PORT] -dest IP:PORT[-PORT] | -match NETWORKS | -socks [IP]:PORT) [-paths NAMES] [-subs N] [-dial auto|self|peer|both] [-SETTING VALUE]...", nargs: 1,
			flags: func(fs *flag.FlagSet) {
				fs.StringVar(&spec.Peer, "peer", "", "name of the peer the connections go to")
				fs.StringVar(&spec.Listen, "listen", "", "fixed rule: where this node listens, IP:port or IP:first-last (without the IP: every address)")
				fs.StringVar(&spec.Destination, "dest", "", "fixed rule: where the connections go at the peer's end, HOST:port or HOST:first-last, with an IP address or a host name that the peer looks up (same number of ports as -listen)")
				fs.StringVar(&spec.Match, "match", "", "transparent rule: original destinations it takes, networks with optional ports such as 192.0.2.0/24:873,[2001:db8::/32]:5000-5010 (needs tcpraid run -transparent)")
				fs.StringVar(&spec.Socks, "socks", "", "SOCKS5 rule: where this node listens for SOCKS5 clients, IP:port, or :port for the loopback address 127.0.0.1. Each connection goes to the destination its client asks for, as seen from the peer.")
				fs.BoolVar(&disabled, "disabled", false, "store the rule switched off")
				fs.StringVar(&paths, "paths", "", "names of the paths the rule's connections use, separated by commas, as the node that holds the paths calls them (\"\": every path)")
				fs.IntVar(&subs, "subs", 0, "the most data sub-connections a connection of the rule keeps on each path (0: as many as the path has)")
				fs.StringVar(&dial, "dial", "", "which node opens the connections between the nodes: auto, self, peer or both (default auto: this node where it has a path that works, else the peer)")
				ruleTransport = settingFlags(fs, false, below)
			},
			run: func(dataDir string, args []string, stdout, _ io.Writer) error {
				if spec.Peer == "" {
					return errors.New("-peer is missing. Name the peer the connections go to. \"tcpraid peers\" lists them.")
				}
				spec.Name = args[0]
				enabled := !disabled
				spec.Enabled = &enabled
				// sent only when given: a node of an older version does not know them
				if paths != "" {
					names := strings.Split(paths, ",")
					spec.Paths = &names
				}
				if subs != 0 {
					spec.Subs = &subs
				}
				if dial != "" && dial != "auto" {
					spec.Dial = &dial
				}
				for name, value := range ruleTransport {
					if value != nil {
						if spec.Transport == nil {
							spec.Transport = map[string]*string{}
						}
						spec.Transport[name] = value
					}
				}
				var out api.Rule
				if err := apiCall(dataDir, http.MethodPost, api.PathRules, spec, &out, shortCall); err != nil {
					return olderNode(err, map[string]string{"paths": "paths for a rule", "subs": "a number of sub-connections for a rule", "dial": "the dial of a rule",
						"transport": "transport settings for a rule", "socks": "SOCKS5 rules"})
				}
				fmt.Fprintf(stdout, "rule %s added: %s\n", out.Name, ruleState(out))
				if out.Socks != "" && !node.SocksLoopback(out.Socks) {
					fmt.Fprintf(stdout, "Note: %s\n", forward.SocksOpenWarning(out.Socks, out.Peer))
				}
				if out.Enabled && !out.Listening {
					return errors.New("the rule is stored but does not take connections")
				}
				return nil
			},
		},
		"set": {
			name: "rules set", usage: "NAME -SETTING VALUE ...", nargs: 1,
			flags: func(fs *flag.FlagSet) {
				ruleTransport = settingFlags(fs, false, below)
			},
			run: func(dataDir string, args []string, stdout, _ io.Writer) error {
				if len(ruleTransport) == 0 {
					return errors.New("nothing to set. Give at least one transport setting, such as -window 256MB. The rest of a rule is changed by removing it and adding it again.")
				}
				var list []api.Rule
				if err := apiCall(dataDir, http.MethodGet, api.PathRules, nil, &list, shortCall); err != nil {
					return err
				}
				for _, r := range list {
					if r.Name != args[0] {
						continue
					}
					// what the request does not name stays as the node has it: paths,
					// sub-connections, who dials, and the settings not given here
					spec := api.RuleSpec{Name: r.Name, Listen: r.Listen, Destination: r.Destination, Match: r.Match, Socks: r.Socks, Peer: r.Peer, Enabled: &r.Enabled, Transport: ruleTransport}
					var out api.Rule
					if err := apiCall(dataDir, http.MethodPut, rulePath(r.Name), spec, &out, shortCall); err != nil {
						return olderNode(err, map[string]string{"transport": "transport settings for a rule"})
					}
					if len(out.Transport) == 0 {
						fmt.Fprintf(stdout, "the connections of rule %s use the transport settings of the peer and the node\n", out.Name)
					} else {
						fmt.Fprintf(stdout, "the connections of rule %s use, in the place of the peer's and the node's settings: %s\n", out.Name, transportText(out.Transport))
					}
					fmt.Fprintln(stdout, "Connections that arrive from now on use this. Sessions that run now keep what they began with.")
					return nil
				}
				return fmt.Errorf("no rule is called %q", args[0])
			},
		},
		"remove": {
			name: "rules remove", usage: "NAME", nargs: 1,
			run: func(dataDir string, args []string, stdout, _ io.Writer) error {
				if err := apiCall(dataDir, http.MethodDelete, rulePath(args[0]), nil, nil, shortCall); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "rule %s removed. Connections it carries now run to their end.\n", args[0])
				return nil
			},
		},
		"enable": {
			name: "rules enable", usage: "NAME", nargs: 1,
			run: func(dataDir string, args []string, stdout, _ io.Writer) error {
				return setRuleEnabled(dataDir, args[0], true, stdout)
			},
		},
		"disable": {
			name: "rules disable", usage: "NAME", nargs: 1,
			run: func(dataDir string, args []string, stdout, _ io.Writer) error {
				return setRuleEnabled(dataDir, args[0], false, stdout)
			},
		},
	}, args, stdout, stderr)
}
