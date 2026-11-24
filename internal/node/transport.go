package node

// Transport settings in layers.
//
// A session runs with settings of the transport (internal/engine). A node has them in
// three layers: the node's own, the ones stored with a peer, and the ones stored with a
// rule. A session takes each setting from the rule that carries it, if the rule sets
// it, else from the peer, else from the node. A setting that all sessions of a node
// share has the node layer only.
//
// Every setting here acts at this node's end of a session. The other end of the same
// session uses its own layers; nothing is sent to it.

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Claw-King/tcpraid/internal/engine"
)

// Transport holds transport settings by name, each value written as on the command
// line: {"window": "256MB", "target": "500ms", "share": "false"}. A name that is not
// there is not set at this layer.
type Transport map[string]string

// Setting describes one transport setting.
type Setting struct {
	Name string
	// NodeOnly: all sessions of a node share it, so it has the node layer only.
	NodeOnly bool
	// Bool: the value is true or false.
	Bool bool
	// Help is one line for the help text of a command: what the setting governs and
	// the values it takes.
	Help string

	// parse reads a value and checks it, and returns it as it is stored.
	parse func(text string) (stored string, err error)
	// put writes a value that parse accepted into cfg.
	put func(cfg *engine.Config, stored string)
	// get reads the value from cfg, as it is stored.
	get func(cfg *engine.Config) string
}

// Value reads the setting from cfg, written as it is stored.
func (s Setting) Value(cfg engine.Config) string { return s.get(&cfg) }

// FormatSize writes a number of bytes the way ParseSize reads it, in the largest unit
// that leaves no fraction.
func FormatSize(n int) string {
	switch {
	case n != 0 && n%(1<<30) == 0:
		return strconv.Itoa(n>>30) + "GB"
	case n != 0 && n%(1<<20) == 0:
		return strconv.Itoa(n>>20) + "MB"
	case n != 0 && n%(1<<10) == 0:
		return strconv.Itoa(n>>10) + "KB"
	}
	return strconv.Itoa(n) + "B"
}

// ParseSize reads a number of bytes: 256MB, 1GB, 64KB, 4096B, or a plain number.
func ParseSize(text string) (int, error) {
	s := strings.ToUpper(strings.TrimSpace(text))
	mul := 1
	for _, u := range []struct {
		suffix string
		mul    int
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(s, u.suffix) {
			mul, s = u.mul, strings.TrimSpace(strings.TrimSuffix(s, u.suffix))
			break
		}
	}
	// at most 1 TB, and no more than an int holds where it has 32 bits
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || n > (int64(1)<<40)/int64(mul) || n*int64(mul) > math.MaxInt {
		return 0, fmt.Errorf("%q is not a size. Write it like 256MB.", text)
	}
	return int(n * int64(mul)), nil
}

// durationSetting is a setting that holds a time: lo to hi, where a hi of 0 means no
// upper limit and a lo of 1 "above zero".
func durationSetting(name, help string, lo, hi time.Duration, field func(*engine.Config) *time.Duration) Setting {
	return Setting{Name: name, Help: help,
		parse: func(text string) (string, error) {
			d, err := time.ParseDuration(strings.TrimSpace(text))
			if err != nil {
				return "", fmt.Errorf("%q is not a duration. Write it like 500ms, 20s or 15m.", text)
			}
			switch {
			case hi == 0 && lo == 1 && d < lo:
				return "", fmt.Errorf("%s must be above zero", name)
			case hi == 0 && d < lo:
				return "", fmt.Errorf("%s must not be negative", name)
			case hi != 0 && (d < lo || d > hi):
				return "", fmt.Errorf("%s must be %v to %v", name, lo, hi)
			}
			return d.String(), nil
		},
		put: func(cfg *engine.Config, stored string) {
			d, _ := time.ParseDuration(stored)
			*field(cfg) = d
		},
		get: func(cfg *engine.Config) string { return field(cfg).String() },
	}
}

func sizeSetting(name, help string, check func(n int) error, field func(*engine.Config) *int) Setting {
	return Setting{Name: name, Help: help,
		parse: func(text string) (string, error) {
			n, err := ParseSize(text)
			if err != nil {
				return "", err
			}
			if err := check(n); err != nil {
				return "", err
			}
			return FormatSize(n), nil
		},
		put: func(cfg *engine.Config, stored string) {
			n, _ := ParseSize(stored)
			*field(cfg) = n
		},
		get: func(cfg *engine.Config) string { return FormatSize(*field(cfg)) },
	}
}

// The limits of the settings. The chunk size is not a setting of a node: every node
// uses the default.
const (
	transportChunk = 256 << 10
	// MinWindow is the smallest window and the smallest budget: four chunks.
	MinWindow = 4 * transportChunk
)

// CheckWindow reports whether n bytes can be a session's window: whole MB, at least
// four chunks, below 64 GB.
func CheckWindow(n int) error {
	if n < MinWindow || n%(1<<20) != 0 || n>>20 > 65535 {
		return errors.New("window must be whole MB, at least 1MB, below 64GB")
	}
	return nil
}

// settings lists every setting, in the order they are shown.
var settings = []Setting{
	sizeSetting("window", "`size` of the send window and of the reorder buffer of a session at this node (whole MB, such as 256MB)",
		CheckWindow, func(c *engine.Config) *int { return &c.Window }),
	durationSetting("grace", "how long a session is kept while nothing arrives from the peer (a `duration` above zero)",
		1, 0, func(c *engine.Config) *time.Duration { return &c.Grace }),
	durationSetting("hol", "send a missing chunk again once later data has waited for it this long plus the allowed queue (a `duration` above zero)",
		1, 0, func(c *engine.Config) *time.Duration { return &c.Hol }),
	durationSetting("target", "the allowed queue: how much data a sub-connection may hold beyond its feedback loop, as a `duration` at its rate (20ms to 5s)",
		20*time.Millisecond, 5*time.Second, func(c *engine.Config) *time.Duration { return &c.Target }),
	durationSetting("pigtarget", "the allowed queue for 10 seconds after the peer's reports stalled for over a second (a `duration` from 20ms to 30s)",
		20*time.Millisecond, 30*time.Second, func(c *engine.Config) *time.Duration { return &c.PigTarget }),
	durationSetting("dead", "replace a sub-connection that has data on the way and makes no progress, or cannot be written to, for this long (a `duration` from 5s to 10m)",
		5*time.Second, 10*time.Minute, func(c *engine.Config) *time.Duration { return &c.Dead }),
	{Name: "kickratio", Help: "replace a busy sub-connection that delivers less than the median of its path divided by this `number` for 10 seconds (1.5 to 100)",
		parse: func(text string) (string, error) {
			f, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
			if err != nil || f != f {
				return "", fmt.Errorf("%q is not a number", text)
			}
			if f < 1.5 || f > 100 {
				return "", errors.New("kickratio must be 1.5 to 100")
			}
			return strconv.FormatFloat(f, 'g', -1, 64), nil
		},
		put: func(cfg *engine.Config, stored string) { cfg.KickRatio, _ = strconv.ParseFloat(stored, 64) },
		get: func(cfg *engine.Config) string { return strconv.FormatFloat(cfg.KickRatio, 'g', -1, 64) },
	},
	sizeSetting("kickfloor", "replace no sub-connection while the median rate of its path is below this many bytes a second (a `size` from 1KB to 1GB)",
		func(n int) error {
			if n < 1<<10 || n > 1<<30 {
				return errors.New("kickfloor must be 1KB to 1GB")
			}
			return nil
		}, func(c *engine.Config) *int { return &c.KickFloor }),
	{Name: "share", Bool: true, Help: "hold each sub-connection to its share of the window, by the rate it delivers at (true or false, written -share=false)",
		parse: func(text string) (string, error) {
			switch strings.ToLower(strings.TrimSpace(text)) {
			case "true":
				return "true", nil
			case "false":
				return "false", nil
			}
			return "", fmt.Errorf("%q is neither true nor false", text)
		},
		put: func(cfg *engine.Config, stored string) { cfg.Share = stored == "true" },
		get: func(cfg *engine.Config) string { return strconv.FormatBool(cfg.Share) },
	},
	{Name: "fbsubs", Help: "while a session has no control sub-connection, send each report on this `number` of data sub-connections (1 to 16)",
		parse: func(text string) (string, error) {
			n, err := strconv.Atoi(strings.TrimSpace(text))
			if err != nil {
				return "", fmt.Errorf("%q is not a whole number", text)
			}
			if n < 1 || n > 16 {
				return "", errors.New("fbsubs must be 1 to 16")
			}
			return strconv.Itoa(n), nil
		},
		put: func(cfg *engine.Config, stored string) { cfg.FbSubs, _ = strconv.Atoi(stored) },
		get: func(cfg *engine.Config) string { return strconv.Itoa(cfg.FbSubs) },
	},
	durationSetting("fbevery", "how often a report on what arrived is sent while something changes, where one goes every second in any case (a `duration` from 5ms to 1s)",
		5*time.Millisecond, time.Second, func(c *engine.Config) *time.Duration { return &c.FbEvery }),
	durationSetting("coalesce", "wait this long for more data from the application before a chunk that is not full is sent (a `duration` from 0s to 1s)",
		0, time.Second, func(c *engine.Config) *time.Duration { return &c.Coalesce }),
	func() Setting {
		s := sizeSetting("budget", "`size` of the data that all sessions together may hold unacknowledged, and as much again out of order (such as 256MB)",
			func(n int) error {
				if n < MinWindow {
					return errors.New("budget must be at least 1MB")
				}
				return nil
			}, func(c *engine.Config) *int { return &c.Budget })
		s.NodeOnly = true
		return s
	}(),
	func() Setting {
		s := durationSetting("timeline", "log one line of figures for each session this often (a `duration`, and 0s for off)",
			0, 0, func(c *engine.Config) *time.Duration { return &c.Timeline })
		s.NodeOnly = true
		return s
	}(),
}

// Settings returns every transport setting, in the order they are shown.
func Settings() []Setting { return append([]Setting{}, settings...) }

// SettingByName returns the setting called name.
func SettingByName(name string) (Setting, bool) {
	for _, s := range settings {
		if s.Name == name {
			return s, true
		}
	}
	return Setting{}, false
}

// ParseSetting checks a value for the setting called name and returns it as it is
// stored: "0.5s" becomes "500ms", "268435456" becomes "256MB".
func ParseSetting(name, text string) (string, error) {
	s, ok := SettingByName(name)
	if !ok {
		return "", fmt.Errorf("there is no transport setting called %q", name)
	}
	return s.parse(text)
}

// Check reports whether t can be stored: every name is a setting, every value is one
// the setting takes and is written as it is stored. For a peer or a rule (nodeLayer
// false) it must name no setting that all sessions of a node share.
func (t Transport) Check(nodeLayer bool) error {
	for _, name := range t.names() {
		s, ok := SettingByName(name)
		if !ok {
			return fmt.Errorf("there is no transport setting called %q", name)
		}
		if s.NodeOnly && !nodeLayer {
			return fmt.Errorf("%s is shared by all sessions of the node and can only be set for the node", name)
		}
		stored, err := s.parse(t[name])
		if err != nil {
			return err
		}
		if stored != t[name] {
			return fmt.Errorf("%s is written %q where it is stored, not %q", name, stored, t[name])
		}
	}
	return nil
}

// names returns the names in t, sorted.
func (t Transport) names() []string {
	names := make([]string, 0, len(t))
	for name := range t {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Clone returns a copy that shares no memory with t; nil for an empty one, so that a
// layer without settings is stored as no layer at all.
func (t Transport) Clone() Transport {
	if len(t) == 0 {
		return nil
	}
	return maps.Clone(t)
}

// Apply writes every setting of t into cfg. t must have passed Check.
func (t Transport) Apply(cfg *engine.Config) {
	for name, stored := range t {
		if s, ok := SettingByName(name); ok {
			s.put(cfg, stored)
		}
	}
}

// String writes the settings of t in the order they are shown: "window=256MB
// target=500ms". An empty t gives the empty string.
func (t Transport) String() string {
	var parts []string
	for _, s := range settings {
		if v, ok := t[s.Name]; ok {
			parts = append(parts, s.Name+"="+v)
		}
	}
	return strings.Join(parts, " ")
}

// Merge returns t changed by change: a name with a value is set to it, after the value
// was checked and brought into the form it is stored in, and a name with nil is taken
// out. t itself is not changed. For a peer or a rule (nodeLayer false) a setting that
// all sessions of a node share is refused.
func (t Transport) Merge(change map[string]*string, nodeLayer bool) (Transport, error) {
	out := Transport{}
	maps.Copy(out, t)
	names := make([]string, 0, len(change))
	for name := range change {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		s, ok := SettingByName(name)
		if !ok {
			return nil, fmt.Errorf("there is no transport setting called %q", name)
		}
		if s.NodeOnly && !nodeLayer {
			return nil, fmt.Errorf("%s is shared by all sessions of the node and can only be set for the node", name)
		}
		if change[name] == nil {
			delete(out, name)
			continue
		}
		stored, err := s.parse(*change[name])
		if err != nil {
			return nil, err
		}
		out[name] = stored
	}
	return out.Clone(), nil
}

// ResolveTransport returns the settings of a session: base, changed by each layer in
// turn, so that a rule's setting goes before a peer's and a peer's before the node's.
// pinned names the settings that the node layer must leave as base has them, because
// the command line of the running node gave them; a peer's and a rule's still apply.
// The layers must have passed Check.
func ResolveTransport(base engine.Config, pinned map[string]bool, nodeLayer, peerLayer, ruleLayer Transport) engine.Config {
	cfg := base
	for name, stored := range nodeLayer {
		if s, ok := SettingByName(name); ok && !pinned[name] {
			s.put(&cfg, stored)
		}
	}
	for _, layer := range []Transport{peerLayer, ruleLayer} {
		for name, stored := range layer {
			if s, ok := SettingByName(name); ok && !s.NodeOnly {
				s.put(&cfg, stored)
			}
		}
	}
	return cfg
}

// CheckTransportFit reports what in the settings of a node cannot work together: the
// memory limit of the node (maxRSS in MB, 0 for none) must leave room for the budget
// twice.
func CheckTransportFit(cfg engine.Config) error {
	if need := 2*(cfg.Budget>>20) + 100; cfg.MaxRSS > 0 && cfg.MaxRSS < need {
		return fmt.Errorf("-maxrss %d MB is below 2 x budget + 100 MB = %d MB. Lower the budget or raise -maxrss.", cfg.MaxRSS, need)
	}
	return nil
}
