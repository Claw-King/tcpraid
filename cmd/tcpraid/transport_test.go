package main

import (
	"bytes"
	"context"
	"math"
	"reflect"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/engine"
)

// defaultTransportLine is what a node logs after "transport " when no setting is given.
const defaultTransportLine = "window=128MB budget=256MB grace=15m0s hol=1s timeline=off maxrss=off"

// runFlagsOf parses a command line of "tcpraid run" and returns what it gives.
func runFlagsOf(args ...string) (o runOptions, code int, done bool, stderr string) {
	var errb bytes.Buffer
	o, code, done = runFlags(args, &errb)
	return o, code, done, errb.String()
}

// Without a transport flag a node runs the transport with the engine's defaults, and so
// does a node that is started without options at all.
func TestRunTransportDefaults(t *testing.T) {
	want := engine.DefaultConfig()
	want.Version = version
	o, code, done, stderr := runFlagsOf("-data", "/x")
	if done {
		t.Fatalf("run -data /x: ended with code %d: %s", code, stderr)
	}
	if got := o.engineConfig(); !reflect.DeepEqual(got, want) {
		t.Errorf("no transport flag:\n got  %+v\n want %+v", got, want)
	}
	bare := runOptions{dataDir: "/x"}
	if got := bare.engineConfig(); !reflect.DeepEqual(got, want) {
		t.Errorf("no options:\n got  %+v\n want %+v", got, want)
	}
	if err := bare.check(); err != nil {
		t.Errorf("no options: %v", err)
	}
}

// Each transport flag reaches the engine's settings, and nothing else changes.
func TestRunTransportFlags(t *testing.T) {
	args := []string{"-data", "/x", "-udp-listen", "192.0.2.1:8446",
		"-window", "256MB", "-budget", "384MB", "-grace", "3600s", "-hol", "500ms", "-timeline", "2s"}
	want := engine.DefaultConfig()
	want.Version, want.UDPListen = version, "192.0.2.1:8446"
	want.Window, want.Budget = 256<<20, 384<<20
	want.Grace, want.Hol, want.Timeline = time.Hour, 500*time.Millisecond, 2*time.Second
	logged := "window=256MB budget=384MB grace=1h0m0s hol=500ms timeline=2s maxrss=off"
	if engine.RSSSupported {
		// -maxrss exists only where the resident memory can be read
		args = append(args, "-maxrss", "900")
		want.MaxRSS = 900
		logged = "window=256MB budget=384MB grace=1h0m0s hol=500ms timeline=2s maxrss=900MB"
	}
	o, code, done, stderr := runFlagsOf(args...)
	if done {
		t.Fatalf("ended with code %d: %s", code, stderr)
	}
	if got := o.engineConfig(); !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
	if got := o.transport.String(); got != logged {
		t.Errorf("logged as %q, want %q", got, logged)
	}
	d := defaultTransport()
	if got, want := d.String(), defaultTransportLine; got != want {
		t.Errorf("defaults logged as %q, want %q", got, want)
	}
	// a size may be a plain number of bytes, as in the manual mode
	o, _, done, stderr = runFlagsOf("-data", "/x", "-window", "268435456", "-budget", "1GB")
	if done || o.transport.window != 256<<20 || o.transport.budget != 1<<30 {
		t.Errorf("-window 268435456 -budget 1GB: done=%v options=%+v stderr=%q", done, o.transport, stderr)
	}
}

// A transport setting that cannot work ends the command before the node starts, with a
// message that names the flag.
func TestRunTransportRefused(t *testing.T) {
	for _, c := range []struct {
		args []string
		flag string
	}{
		{[]string{"-window", "0"}, "-window"},
		{[]string{"-window", "512KB"}, "-window"},   // below 4 chunks
		{[]string{"-window", "1500KB"}, "-window"},  // not whole MB
		{[]string{"-window", "65536MB"}, "-window"}, // 64 GB
		{[]string{"-window", "plenty"}, "-window"},  // not a size
		{[]string{"-window", "-128MB"}, "-window"},  // negative
		{[]string{"-budget", "0"}, "-budget"},       // nothing may be sent
		{[]string{"-budget", "512KB"}, "-budget"},   // below 4 chunks
		{[]string{"-budget", "much"}, "-budget"},    // not a size
		{[]string{"-grace", "0"}, "-grace"},         // a session would end at once
		{[]string{"-grace", "-5s"}, "-grace"},       //
		{[]string{"-grace", "long"}, "-grace"},      // not a duration
		{[]string{"-hol", "0"}, "-hol"},             //
		{[]string{"-hol", "-1s"}, "-hol"},           //
		{[]string{"-timeline", "-2s"}, "-timeline"}, //
		{[]string{"-maxrss", "-1"}, "-maxrss"},      //
		{[]string{"-maxrss", "611"}, "-maxrss"},     // below 2 x 256 MB + 100 MB
		{[]string{"-maxrss", "500"}, "-maxrss"},     // the same, with the default budget
		{[]string{"-budget", "512MB", "-maxrss", "1123"}, "-maxrss"},
		{[]string{"-maxrss", "lots"}, "-maxrss"}, // not a number
	} {
		_, code, done, stderr := runFlagsOf(append([]string{"-data", "/x"}, c.args...)...)
		if !done || code != 2 || !strings.Contains(stderr, c.flag) {
			t.Errorf("%q: done=%v code=%d stderr=%q, want exit 2 with a message that names %s", c.args, done, code, stderr, c.flag)
		}
	}
	// the lowest limit that fits the budget is taken
	accepted := [][]string{
		{"-budget", "1MB", "-window", "1MB"},
		{"-timeline", "0"},
	}
	if engine.RSSSupported {
		accepted = append(accepted, []string{"-maxrss", "612"}, []string{"-budget", "192MB", "-maxrss", "500"})
	} else if _, code, done, stderr := runFlagsOf("-data", "/x", "-maxrss", "612"); !done || code != 2 || !strings.Contains(stderr, "-maxrss") {
		// where the resident memory cannot be read, every limit is refused
		t.Errorf("-maxrss 612 on a system without it: done=%v code=%d stderr=%q", done, code, stderr)
	}
	for _, args := range accepted {
		if _, code, done, stderr := runFlagsOf(append([]string{"-data", "/x"}, args...)...); done {
			t.Errorf("%q: refused with code %d: %s", args, code, stderr)
		}
	}
}

// -stats reaches the engine's settings and changes nothing else; a negative one is
// refused before the node starts.
func TestRunStatsFlag(t *testing.T) {
	o, code, done, stderr := runFlagsOf("-data", "/x", "-stats", "5m")
	if done {
		t.Fatalf("ended with code %d: %s", code, stderr)
	}
	want := engine.DefaultConfig()
	want.Version, want.Stats = version, 5*time.Minute
	if got := o.engineConfig(); !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
	// it is not one of the settings of the line "transport ..." that a node logs
	if got, want := o.transport.String(), defaultTransportLine; got != want {
		t.Errorf("the transport line is %q, want %q", got, want)
	}
	if o, _, done, stderr = runFlagsOf("-data", "/x", "-stats", "0"); done || o.engineConfig().Stats != 0 {
		t.Errorf("-stats 0: done=%v stats=%v stderr=%q", done, o.engineConfig().Stats, stderr)
	}
	for _, v := range []string{"-1s", "often"} {
		_, code, done, stderr := runFlagsOf("-data", "/x", "-stats", v)
		if !done || code != 2 || !strings.Contains(stderr, "-stats") {
			t.Errorf("-stats %s: done=%v code=%d stderr=%q, want exit 2 with a message that names -stats", v, done, code, stderr)
		}
	}
}

// The checks that the manual mode and the node share.
func TestCheckTransport(t *testing.T) {
	d := engine.DefaultConfig()
	if err := checkTransport(d.Chunk, d.Window, d.Budget, d.MaxRSS); err != nil {
		t.Errorf("the defaults: %v", err)
	}
	for _, c := range []struct {
		chunk, window, budget, maxRSS int
		flag                          string
	}{
		{1024, d.Window, d.Budget, 0, "-chunk"},
		{engine.MaxChunk + 1, d.Window, d.Budget, 0, "-chunk"},
		{d.Chunk, 3 * d.Chunk, d.Budget, 0, "-window"},
		{d.Chunk, d.Window + 1, d.Budget, 0, "-window"},
		{d.Chunk, d.Window, d.Budget, 600, "-maxrss"},
	} {
		if err := checkTransport(c.chunk, c.window, c.budget, c.maxRSS); err == nil || !strings.Contains(err.Error(), c.flag) {
			t.Errorf("chunk %d window %d budget %d maxrss %d: %v, want an error that names %s", c.chunk, c.window, c.budget, c.maxRSS, err, c.flag)
		}
	}
}

// A node whose memory is above -maxrss stops by itself, the way it stops on a signal,
// and says why. The limit here is one that any process is above; the command refuses
// such a value, so the options are made by hand.
func TestRunStopsAboveMaxRSS(t *testing.T) {
	if !engine.RSSSupported {
		t.Skip("this system does not let the program read its resident memory; -maxrss is refused")
	}
	// the limit also sets the memory limit of the Go runtime: put it back
	defer debug.SetMemoryLimit(math.MaxInt64)
	tr := defaultTransport()
	tr.maxRSS = 1
	dir := t.TempDir()
	done := make(chan error, 1)
	go func() {
		done <- runNodeWith(context.Background(), runOptions{dataDir: dir, apiAddr: "127.0.0.1:0", name: "alpha", transport: &tr})
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "-maxrss 1 MB") {
			t.Fatalf("the node ended with %v, want an error that names -maxrss", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the node did not stop above -maxrss")
	}
	// it went through its ordinary stop: the data directory is free again
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runNode(ctx, dir, "127.0.0.1:0", ""); err != nil {
		t.Fatalf("a node on the same directory afterwards: %v", err)
	}
}
