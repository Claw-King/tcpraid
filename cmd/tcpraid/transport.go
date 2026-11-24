package main

// The transport settings: the checks that the manual mode and "tcpraid run" share, the
// settings a node lets its operator change, and the flags that set them for a node, a
// peer or a rule.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Claw-King/tcpraid/internal/api"
	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/node"
)

// checkTransport reports what in the sizes of the transport cannot work; maxRSS is in
// MB, the others in bytes.
func checkTransport(chunk, window, budget, maxRSS int) error {
	if chunk < 4096 || chunk > engine.MaxChunk {
		return fmt.Errorf("-chunk must be 4096..%d", engine.MaxChunk)
	}
	if window < 4*chunk || window%(1<<20) != 0 || window>>20 > 65535 {
		return errors.New("-window must be whole MB, at least 4 chunks, below 64 GB")
	}
	// stream data is bounded by -budget twice (unacked sent data, out-of-order received
	// data): a lower -maxrss is reached in normal operation (-budget 512 MB under -maxrss
	// 600 was, in a test)
	if maxRSS > 0 && !engine.RSSSupported {
		return errors.New("-maxrss is only available on Linux, because this system does not let the program read its resident memory")
	}
	if need := 2*(budget>>20) + 100; maxRSS > 0 && maxRSS < need {
		return fmt.Errorf("-maxrss %d MB is below 2 x -budget + 100 MB = %d MB. Lower -budget or raise -maxrss.", maxRSS, need)
	}
	return nil
}

// sizeFlag is a flag that holds a number of bytes, written as in the configuration file
// of the manual mode: 256MB, 1GB, or a plain number of bytes.
type sizeFlag int

func (s *sizeFlag) String() string { return sizeString(int(*s)) }

func (s *sizeFlag) Set(v string) error {
	n, err := parseSize(v)
	if err != nil {
		return errors.New("want a size such as 256MB")
	}
	*s = sizeFlag(n)
	return nil
}

// sizeString writes a number of bytes the way a sizeFlag reads it.
func sizeString(n int) string {
	if n != 0 && n%(1<<20) == 0 {
		return fmt.Sprintf("%dMB", n>>20)
	}
	return fmt.Sprintf("%dB", n)
}

// transportOptions is the part of the transport's settings that "tcpraid run" lets the
// operator of a node change. Every other setting of the transport stays at its default.
type transportOptions struct {
	window, budget       int // bytes
	grace, hol, timeline time.Duration
	maxRSS               int // MB; 0: no limit
	// more holds the settings that came with 0.7.0 and were given: name to value, as
	// node.Transport writes them.
	more map[string]*string
	// given names the settings (node.Settings) that the command line gave: these are
	// not changed by a value stored in the node.
	given map[string]bool
}

// settingFlag is a flag that sets one transport setting by name: it checks the value
// and puts it into a map in the form it is stored in. With orDefault it also takes the
// word "default", which puts nil there: the setting goes back to what the layer below
// has.
type settingFlag struct {
	setting   node.Setting
	into      map[string]*string
	orDefault bool
	// shown is the default that the help of the command prints; empty for none.
	shown string
}

func (f settingFlag) String() string { return f.shown }

func (f settingFlag) Set(v string) error {
	if f.orDefault && v == "default" {
		f.into[f.setting.Name] = nil
		return nil
	}
	stored, err := node.ParseSetting(f.setting.Name, v)
	if err != nil {
		return err
	}
	f.into[f.setting.Name] = &stored
	return nil
}

// IsBoolFlag lets -share stand alone for -share=true, as the flag of the manual mode does.
func (f settingFlag) IsBoolFlag() bool { return f.setting.Bool }

// settingFlags declares, for a command that sets transport settings of a peer, of a
// rule or (nodeLayer) of the node, one flag for each setting that layer has. It returns
// the map the flags fill: a name with its value, or with nil where "default" was given.
func settingFlags(fs *flag.FlagSet, nodeLayer bool, below string) map[string]*string {
	change := map[string]*string{}
	for _, s := range node.Settings() {
		if s.NodeOnly && !nodeLayer {
			// declared, so that the answer says where the setting belongs
			fs.Var(sharedFlag(s.Name), s.Name, "all sessions of the node share this setting, so it is set with \"tcpraid transport set\" or \"tcpraid run\" and not here")
			continue
		}
		fs.Var(settingFlag{setting: s, into: change, orDefault: true}, s.Name, s.Help+". The word default takes the setting off again, and "+below+".")
	}
	return change
}

// sharedFlag is the flag of a setting that all sessions of a node share, on a command
// that sets the settings of a peer or a rule: it refuses every value.
type sharedFlag string

func (f sharedFlag) String() string { return "" }

func (f sharedFlag) Set(string) error {
	return fmt.Errorf("all sessions of the node share %s. Set it for the node, with \"tcpraid transport set -%s\".", string(f), string(f))
}

// defaultTransport returns the settings a node runs with when none is given.
func defaultTransport() transportOptions {
	d := engine.DefaultConfig()
	return transportOptions{window: d.Window, budget: d.Budget, grace: d.Grace, hol: d.Hol, timeline: d.Timeline, maxRSS: d.MaxRSS}
}

// flags declares the settings as flags of fs; t holds the defaults before the call and
// what was given after fs is parsed.
func (t *transportOptions) flags(fs *flag.FlagSet) {
	fs.Var((*sizeFlag)(&t.window), "window", "`size` of the send window of this node and of its reorder buffer when it receives, per session (whole MB, such as 256MB)")
	fs.Var((*sizeFlag)(&t.budget), "budget", "`size` of the data that all sessions together may hold unacknowledged, and as much again out of order (such as 256MB)")
	fs.DurationVar(&t.grace, "grace", t.grace, "how long a session is kept while nothing arrives from the peer")
	fs.DurationVar(&t.hol, "hol", t.hol, "send a missing chunk again once later data has waited for it this long plus the allowed queue (-target)")
	fs.DurationVar(&t.timeline, "timeline", t.timeline, "log one line of figures per session this often (0: off)")
	fs.IntVar(&t.maxRSS, "maxrss", t.maxRSS, "stop the node when its resident memory exceeds this many MB (0: no limit)")
	t.more = map[string]*string{}
	for _, s := range node.Settings() {
		if fs.Lookup(s.Name) == nil {
			fs.Var(settingFlag{setting: s, into: t.more, shown: s.Value(engine.DefaultConfig())}, s.Name, s.Help)
		}
	}
}

// noteGiven records which settings the parsed command line gave.
func (t *transportOptions) noteGiven(fs *flag.FlagSet) {
	t.given = map[string]bool{}
	fs.Visit(func(f *flag.Flag) {
		if _, ok := node.SettingByName(f.Name); ok {
			t.given[f.Name] = true
		}
	})
}

// check reports which setting cannot work.
func (t *transportOptions) check() error {
	chunk := engine.DefaultConfig().Chunk
	if t.budget < 4*chunk {
		return fmt.Errorf("-budget must be at least 4 chunks (%s)", sizeString(4*chunk))
	}
	if t.maxRSS < 0 {
		return errors.New("-maxrss must not be negative (0: no limit)")
	}
	if err := checkTransport(chunk, t.window, t.budget, t.maxRSS); err != nil {
		return err
	}
	if t.grace <= 0 {
		return errors.New("-grace must be above zero")
	}
	if t.hol <= 0 {
		return errors.New("-hol must be above zero")
	}
	if t.timeline < 0 {
		return errors.New("-timeline must not be negative (0: off)")
	}
	return nil
}

// apply puts the settings into cfg.
func (t *transportOptions) apply(cfg *engine.Config) {
	cfg.Window, cfg.Budget = t.window, t.budget
	cfg.Grace, cfg.Hol, cfg.Timeline = t.grace, t.hol, t.timeline
	cfg.MaxRSS = t.maxRSS
	for name, value := range t.more {
		if value != nil {
			node.Transport{name: *value}.Apply(cfg)
		}
	}
}

// oldSettings are the settings of the line "transport ..." that a node logs at its
// start, which scripts read: the line keeps these and their order.
var oldSettings = map[string]bool{"window": true, "budget": true, "grace": true, "hol": true, "timeline": true}

// tuned lists the settings that came with 0.7.0 and that cfg does not have at their
// default, as "name=value" separated by spaces; empty when all of them are.
func tuned(cfg engine.Config) string {
	d := engine.DefaultConfig()
	var parts []string
	for _, s := range node.Settings() {
		if v := s.Value(cfg); !oldSettings[s.Name] && v != s.Value(d) {
			parts = append(parts, s.Name+"="+v)
		}
	}
	return strings.Join(parts, " ")
}

// String is the settings as the node logs them at its start.
func (t *transportOptions) String() string {
	timeline, maxRSS := "off", "off"
	if t.timeline > 0 {
		timeline = t.timeline.String()
	}
	if t.maxRSS > 0 {
		maxRSS = fmt.Sprintf("%dMB", t.maxRSS)
	}
	return fmt.Sprintf("window=%s budget=%s grace=%v hol=%v timeline=%s maxrss=%s", sizeString(t.window), sizeString(t.budget), t.grace, t.hol, timeline, maxRSS)
}

// olderNodeTransport says, for a request about the node's transport settings that a
// node answered with "no such path", that the node runs a version from before 0.7.0.
func olderNodeTransport(err error) error {
	if err.Error() == "no such path" {
		return errors.New("the running node is older than this command and has no settings that can be read or changed while it runs. Restart the node to finish an update.")
	}
	return err
}

// printTransport writes the node's transport settings as a table.
func printTransport(w io.Writer, t api.Transport) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SETTING\tVALUE\tDEFAULT\tFROM\tFOR")
	for _, s := range t.Settings {
		from := s.Source
		if s.Source == "command line" && s.Stored != "" {
			from += " (" + s.Stored + " is stored and waits for a start without the flag)"
		}
		scope := "each session (a peer or a rule can set its own)"
		if s.Shared {
			scope = "all sessions of the node together"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.Name, s.Value, s.Default, from, scope)
	}
	return tw.Flush()
}

// cmdTransport is "tcpraid transport" and "tcpraid transport set": the transport
// settings of the node on this host, read and changed while it runs.
func cmdTransport(args []string, stdout, stderr io.Writer) int {
	var asJSON bool
	var change map[string]*string
	return dispatch("transport", "show", []string{"show", "set"}, map[string]command{
		"show": {
			name: "transport",
			flags: func(fs *flag.FlagSet) {
				fs.BoolVar(&asJSON, "json", false, "print the settings as the API returns them (JSON)")
			},
			run: func(dataDir string, _ []string, stdout, _ io.Writer) error {
				if asJSON {
					var raw json.RawMessage
					if err := apiCall(dataDir, http.MethodGet, api.PathTransport, nil, &raw, shortCall); err != nil {
						return olderNodeTransport(err)
					}
					_, err := fmt.Fprintf(stdout, "%s\n", raw)
					return err
				}
				var t api.Transport
				if err := apiCall(dataDir, http.MethodGet, api.PathTransport, nil, &t, shortCall); err != nil {
					return olderNodeTransport(err)
				}
				if err := printTransport(stdout, t); err != nil {
					return err
				}
				_, err := fmt.Fprintln(stdout, "\nThese are the node's values. A session takes them when it begins and keeps them until it ends.\n"+
					"\"tcpraid transport set\" changes them while the node runs. The memory limit (-maxrss) and the statistics line (-stats) are set only when the node starts.")
				return err
			},
		},
		"set": {
			name: "transport set", usage: "-SETTING VALUE ...",
			flags: func(fs *flag.FlagSet) {
				change = settingFlags(fs, true, "the node then uses the program's default")
			},
			run: func(dataDir string, _ []string, stdout, _ io.Writer) error {
				if len(change) == 0 {
					return errors.New("nothing to set. Give at least one setting, such as -window 256MB. \"tcpraid transport\" lists them.")
				}
				var before, after api.Transport
				if err := apiCall(dataDir, http.MethodGet, api.PathTransport, nil, &before, shortCall); err != nil {
					return olderNodeTransport(err)
				}
				if err := apiCall(dataDir, http.MethodPut, api.PathTransport, api.TransportUpdate{Set: change}, &after, shortCall); err != nil {
					return olderNodeTransport(err)
				}
				was := map[string]string{}
				for _, s := range before.Settings {
					was[s.Name] = s.Value
				}
				for _, s := range after.Settings {
					if _, asked := change[s.Name]; !asked {
						continue
					}
					switch {
					case s.Source == "command line":
						// only a stored value was taken off: the command line still decides
						fmt.Fprintf(stdout, "%s stays %s, which the command line of the node gives. No value is stored for it any more.\n", s.Name, s.Value)
					case was[s.Name] == s.Value:
						fmt.Fprintf(stdout, "%s is %s, as before (%s)\n", s.Name, s.Value, s.Source)
					default:
						fmt.Fprintf(stdout, "%s is now %s (%s), and was %s\n", s.Name, s.Value, s.Source, was[s.Name])
					}
				}
				_, err := fmt.Fprintln(stdout, "Sessions that begin from now on use this. Sessions that run now keep what they began with. The values are stored and hold after a restart.")
				return err
			},
		},
	}, args, stdout, stderr)
}

// transportText writes a layer of transport settings in the order the node shows them.
func transportText(t map[string]string) string { return node.Transport(t).String() }
