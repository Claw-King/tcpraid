package main

// The transport settings: the checks that the manual mode and "tcpraid run" share, and
// the few settings a node lets its operator change.

import (
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/Claw-King/tcpraid/internal/engine"
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
	fs.DurationVar(&t.grace, "grace", t.grace, "how long a session is kept while no sub-connection to the peer is up")
	fs.DurationVar(&t.hol, "hol", t.hol, "send a missing chunk again once later data has waited for it this long plus the allowed queue (300ms)")
	fs.DurationVar(&t.timeline, "timeline", t.timeline, "log one line of figures per session this often (0: off)")
	fs.IntVar(&t.maxRSS, "maxrss", t.maxRSS, "stop the node when its resident memory exceeds this many MB (0: no limit)")
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
