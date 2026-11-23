package main

// The rules command: the forwarding rules of the node on this host, through its API.

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"text/tabwriter"

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
	case r.Error != "":
		return "listening; last connection: " + r.Error
	case r.Listening:
		return "listening"
	}
	return "not listening"
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
			name: "rules add", usage: "NAME -peer PEER (-listen IP:PORT[-PORT] -dest IP:PORT[-PORT] | -match NETWORKS)", nargs: 1,
			flags: func(fs *flag.FlagSet) {
				fs.StringVar(&spec.Peer, "peer", "", "name of the peer the connections go to")
				fs.StringVar(&spec.Listen, "listen", "", "fixed rule: where this node listens, IP:port or IP:first-last (the IP may be left out: every address)")
				fs.StringVar(&spec.Destination, "dest", "", "fixed rule: where the connections go at the peer's end, IP:port or IP:first-last (same number of ports as -listen)")
				fs.StringVar(&spec.Match, "match", "", "transparent rule: original destinations it takes, networks with optional ports (192.0.2.0/24:873,[2001:db8::/32]:5000-5010); needs tcpraid run -transparent")
				fs.BoolVar(&disabled, "disabled", false, "store the rule switched off")
			},
			run: func(dataDir string, args []string, stdout, _ io.Writer) error {
				if spec.Peer == "" {
					return errors.New("-peer is missing: name the peer the connections go to (tcpraid peers lists them)")
				}
				spec.Name = args[0]
				enabled := !disabled
				spec.Enabled = &enabled
				var out api.Rule
				if err := apiCall(dataDir, http.MethodPost, api.PathRules, spec, &out, shortCall); err != nil {
					return err
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
				fmt.Fprintf(stdout, "rule %s removed; connections it carries now run to their end\n", args[0])
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
