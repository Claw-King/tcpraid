// Package hook runs a command of the user's when something happens in the node: a peer
// comes online or goes offline, a session begins or ends, a connection of a rule fails.
//
// The command is executed directly, never through a shell, and it gets no arguments.
// What happened is given to it twice: as environment variables (TCPRAID_EVENT and the
// others below) and as one line of JSON on its standard input. Text that comes from a
// peer, such as a name or a reason, is in those values only. They hold no key, no
// token and no pairing code.
//
// A hook runs beside the node and cannot hold it up. At most MaxRunning runs go on at
// one time; an event that arrives while that many run is not given to the hook, and is
// counted and logged. A run that lasts longer than the Runner's Timeout is ended. What
// a run returns is logged when it is not zero and changes nothing in the node.
package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The events.
const (
	PeerOnline   = "peer_online"   // the first control connection with a peer is up
	PeerOffline  = "peer_offline"  // the last control connection with a peer is gone
	SessionStart = "session_start" // a session was created at this node
	SessionEnd   = "session_end"   // a session ended at this node
	RuleFailure  = "rule_failure"  // a connection of a rule, or of tcpraid connect, was not carried
)

// Event is what a hook is told. A field that does not belong to the event is empty and
// left out of the JSON; its environment variable is not set.
type Event struct {
	Event string    `json:"event"` // TCPRAID_EVENT
	Time  time.Time `json:"time"`  // TCPRAID_TIME, RFC 3339
	Peer  string    `json:"peer,omitempty"`
	Rule  string    `json:"rule,omitempty"`
	// Of a session: its id in hex, "client" where this node took the application's
	// connection and "server" where the peer brought the session, and the destination.
	Session     string `json:"session,omitempty"`
	Role        string `json:"role,omitempty"`
	Destination string `json:"destination,omitempty"`
	// Of a session that ended: the stream bytes sent and received, whether the stream
	// ended completely in both directions, and the reason as the log has it.
	TxBytes *int64 `json:"tx_bytes,omitempty"`
	RxBytes *int64 `json:"rx_bytes,omitempty"`
	Clean   *bool  `json:"clean,omitempty"`
	// Reason: why a session ended, or why a connection was not carried.
	Reason string `json:"reason,omitempty"`
}

// MaxRunning is how many runs of the hook go on at one time.
const MaxRunning = 4

// DefaultTimeout is how long a run of the hook may last.
const DefaultTimeout = 30 * time.Second

// Runner runs the hook. Make one with New.
type Runner struct {
	path    string
	timeout time.Duration
	logf    func(format string, args ...any)
	slots   chan struct{}
	wg      sync.WaitGroup

	runs     atomic.Int64 // runs that were started
	dropped  atomic.Int64 // events that were not given to the hook
	failed   atomic.Int64 // runs that did not return 0, or could not be started
	timeouts atomic.Int64 // runs that were ended because they lasted too long

	mu       sync.Mutex
	loggedAt time.Time // when a dropped event was logged last
	unlogged int64     // events dropped since
}

// New returns a Runner for command, which is looked up like a command that is typed:
// a path as it is, a bare name in the directories of PATH. A command that is not there,
// or cannot be executed, is an error. timeout is how long a run may last (0:
// DefaultTimeout); logf, if not nil, takes the lines a Runner logs.
func New(command string, timeout time.Duration, logf func(format string, args ...any)) (*Runner, error) {
	if command == "" {
		return nil, fmt.Errorf("the hook names no command")
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return nil, fmt.Errorf("the hook %q cannot be run: %w", command, err)
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Runner{path: path, timeout: timeout, logf: logf, slots: make(chan struct{}, MaxRunning)}, nil
}

// Path returns the command as it was found.
func (r *Runner) Path() string { return r.path }

// Counts returns how many runs were started, how many events were not given to the
// hook, how many runs failed, and how many were ended for lasting too long.
func (r *Runner) Counts() (runs, dropped, failed, timeouts int64) {
	return r.runs.Load(), r.dropped.Load(), r.failed.Load(), r.timeouts.Load()
}

// Running returns how many runs go on now.
func (r *Runner) Running() int { return len(r.slots) }

// Emit gives the event to the hook and returns at once. When MaxRunning runs are going
// on, the event is dropped.
func (r *Runner) Emit(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	select {
	case r.slots <- struct{}{}:
	default:
		r.dropped.Add(1)
		r.mu.Lock()
		r.unlogged++
		if time.Since(r.loggedAt) >= 10*time.Second {
			were := "events were"
			if r.unlogged == 1 {
				were = "event was"
			}
			r.logf("hook: %d %s not given to the hook, because %d runs of it were going on", r.unlogged, were, MaxRunning)
			r.loggedAt, r.unlogged = time.Now(), 0
		}
		r.mu.Unlock()
		return
	}
	r.runs.Add(1)
	r.wg.Add(1)
	go func() {
		defer func() { <-r.slots; r.wg.Done() }()
		r.run(e)
	}()
}

// Wait returns when no run goes on, or after d.
func (r *Runner) Wait(d time.Duration) {
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// oneLine makes a value fit an environment variable and a log line: what is not
// printable becomes a space.
func oneLine(s string) string {
	return strings.Map(func(c rune) rune {
		if c < 0x20 || c == 0x7f {
			return ' '
		}
		return c
	}, s)
}

// Env returns the environment variables that describe e, each as NAME=value.
func Env(e Event) []string {
	env := []string{"TCPRAID_EVENT=" + e.Event, "TCPRAID_TIME=" + e.Time.UTC().Format(time.RFC3339)}
	add := func(name, value string) {
		if value != "" {
			env = append(env, name+"="+oneLine(value))
		}
	}
	add("TCPRAID_PEER", e.Peer)
	add("TCPRAID_RULE", e.Rule)
	add("TCPRAID_SESSION", e.Session)
	add("TCPRAID_ROLE", e.Role)
	add("TCPRAID_DESTINATION", e.Destination)
	if e.TxBytes != nil {
		add("TCPRAID_TX_BYTES", strconv.FormatInt(*e.TxBytes, 10))
	}
	if e.RxBytes != nil {
		add("TCPRAID_RX_BYTES", strconv.FormatInt(*e.RxBytes, 10))
	}
	if e.Clean != nil {
		add("TCPRAID_CLEAN", strconv.FormatBool(*e.Clean))
	}
	add("TCPRAID_REASON", e.Reason)
	return env
}

func (r *Runner) run(e Event) {
	line, err := json.Marshal(e)
	if err != nil {
		r.failed.Add(1)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	// the command and nothing else: no shell reads it, and it gets no arguments
	cmd := exec.CommandContext(ctx, r.path)
	// The node's own environment, without the variables that begin with TCPRAID_:
	// those belong to the node (the password of the web page can be given in one), and
	// the ones of the event take their place.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TCPRAID_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, Env(e)...)
	cmd.Stdin = bytes.NewReader(append(line, '\n'))
	// a run that is ended must not leave the node waiting for what its children hold
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	switch {
	case err == nil:
	case ctx.Err() != nil:
		r.timeouts.Add(1)
		r.failed.Add(1)
		r.logf("hook: the run for %s lasted longer than %v and was ended", e.Event, r.timeout)
	default:
		r.failed.Add(1)
		r.logf("hook: the run for %s failed: %v", e.Event, err)
	}
}
