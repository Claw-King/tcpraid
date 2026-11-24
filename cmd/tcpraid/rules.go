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
		// the paths and sub-connections of the rule are not sent: the node keeps what
		// the rule has
		spec := api.RuleSpec{Name: r.Name, Listen: r.Listen, Destination: r.Destination, Match: r.Match, Peer: r.Peer, Enabled: &enabled}
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
	var paths string
	var subs int
	return dispatch("rules", "list", []string{"list", "add", "remove", "enable", "disable"}, map[string]command{
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
					if r.Match != "" {
						takes, dest = "redirected to "+r.Match, "(as redirected)"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", r.Name, takes, r.Peer, dest, r.Sessions, ruleState(r))
				}
				return tw.Flush()
			},
		},
		"add": {
			name: "rules add", usage: "NAME -peer PEER (-listen IP:PORT[-PORT] -dest IP:PORT[-PORT] | -match NETWORKS) [-paths NAMES] [-subs N]", nargs: 1,
			flags: func(fs *flag.FlagSet) {
				fs.StringVar(&spec.Peer, "peer", "", "name of the peer the connections go to")
				fs.StringVar(&spec.Listen, "listen", "", "fixed rule: where this node listens, IP:port or IP:first-last (without the IP: every address)")
				fs.StringVar(&spec.Destination, "dest", "", "fixed rule: where the connections go at the peer's end, HOST:port or HOST:first-last, with an IP address or a host name that the peer looks up (same number of ports as -listen)")
				fs.StringVar(&spec.Match, "match", "", "transparent rule: original destinations it takes, networks with optional ports such as 192.0.2.0/24:873,[2001:db8::/32]:5000-5010 (needs tcpraid run -transparent)")
				fs.BoolVar(&disabled, "disabled", false, "store the rule switched off")
				fs.StringVar(&paths, "paths", "", "names of the paths the rule's connections use, separated by commas, as the node that holds the paths calls them (\"\": every path)")
				fs.IntVar(&subs, "subs", 0, "the most data sub-connections a connection of the rule keeps on each path (0: as many as the path has)")
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
				var out api.Rule
				if err := apiCall(dataDir, http.MethodPost, api.PathRules, spec, &out, shortCall); err != nil {
					return olderNode(err, map[string]string{"paths": "paths for a rule", "subs": "a number of sub-connections for a rule"})
				}
				fmt.Fprintf(stdout, "rule %s added: %s\n", out.Name, ruleState(out))
				if out.Enabled && !out.Listening {
					return errors.New("the rule is stored but does not take connections")
				}
				return nil
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
