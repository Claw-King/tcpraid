package main

// The sessions command: what the node on this host is carrying right now.
//
//	tcpraid sessions [-json] [-watch [-interval DURATION]]

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"text/tabwriter"
	"time"

	"github.com/Claw-King/tcpraid/internal/api"
)

// sessionsInterval is the time between the two looks at the sessions from which the
// command works out their current speed.
var sessionsInterval = time.Second

// watchRounds is how many tables "sessions -watch" prints before it ends; 0: it goes on
// until it is interrupted. Tests set it.
var watchRounds = 0

// silentAfter is how long nothing must have arrived from the peer before the state of
// a session says that it waits. The two ends report to each other at least once a
// second while a session is connected.
const silentAfter = 5 * time.Second

func cmdSessions(args []string, stdout, stderr io.Writer) int {
	var asJSON, watch bool
	interval := 2 * time.Second
	return command{
		name: "sessions",
		flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&asJSON, "json", false, "print the sessions as the API returns them (JSON), without the current speed")
			fs.BoolVar(&watch, "watch", false, "print the table again every -interval until interrupted")
			fs.DurationVar(&interval, "interval", interval, "with -watch: the time between two tables (1s to 1h)")
		},
		run: func(dataDir string, _ []string, stdout, _ io.Writer) error {
			if asJSON && watch {
				return errors.New("-json and -watch do not go together. Give one of them.")
			}
			if asJSON {
				var raw json.RawMessage
				if err := apiCall(dataDir, http.MethodGet, api.PathSessions, nil, &raw, shortCall); err != nil {
					return err
				}
				_, err := fmt.Fprintf(stdout, "%s\n", raw)
				return err
			}
			var before, after []api.Session
			if err := apiCall(dataDir, http.MethodGet, api.PathSessions, nil, &before, shortCall); err != nil {
				return err
			}
			if watch {
				if interval < time.Second || interval > time.Hour {
					return errors.New("-interval is out of range. Give 1s to 1h.")
				}
				at := time.Now()
				for round := 1; ; round++ {
					time.Sleep(interval)
					if err := apiCall(dataDir, http.MethodGet, api.PathSessions, nil, &after, shortCall); err != nil {
						return err
					}
					now := time.Now()
					fmt.Fprintf(stdout, "%s\n", now.Format("15:04:05"))
					if err := printSessions(stdout, before, after, now.Sub(at), now); err != nil {
						return err
					}
					if round == watchRounds {
						return nil
					}
					fmt.Fprintln(stdout)
					// a fresh slice: decoding into the old one would write over before
					before, after, at = after, nil, now
				}
			}
			if len(before) == 0 {
				fmt.Fprintln(stdout, "no sessions")
				return nil
			}
			at := time.Now()
			time.Sleep(sessionsInterval)
			if err := apiCall(dataDir, http.MethodGet, api.PathSessions, nil, &after, shortCall); err != nil {
				return err
			}
			now := time.Now()
			return printSessions(stdout, before, after, now.Sub(at), now)
		},
	}.execute(args, stdout, stderr)
}

// sessionState says in a few words how a session is doing: whether the peer has
// answered at all, and whether anything arrives from it. A session from which nothing
// arrives is kept for the grace time and reset then. A session whose sub-connections
// were opened partly by this node and partly by the peer says how many each opened.
func sessionState(s api.Session) string {
	silent := time.Duration(s.SilentMs) * time.Millisecond
	left := (time.Duration(s.GraceLeftMs) * time.Millisecond).Truncate(time.Second)
	state := "ok"
	switch {
	case !s.Up:
		state = "connecting"
	case silent >= silentAfter:
		state = fmt.Sprintf("waiting for the peer for %v, gives up in %v", silent.Truncate(time.Second), left)
	case s.WaitsForApplication:
		// no fault: the application gives this node no more data for now
		state = "ok, waiting for the application"
	}
	if s.SubsDialed > 0 && s.SubsDialed < s.Subs {
		state += fmt.Sprintf(", %d sub-connections opened by this node and %d by the peer", s.SubsDialed, s.Subs-s.SubsDialed)
	}
	return state
}

// printSessions writes one line for each session of after. before is the same list
// taken apart earlier; the bytes a session moved in between give its current speed.
func printSessions(w io.Writer, before, after []api.Session, apart time.Duration, now time.Time) error {
	if len(after) == 0 {
		fmt.Fprintln(w, "no sessions")
		return nil
	}
	moved := map[string]int64{}
	for _, s := range before {
		moved[s.ID] = s.TxBytes + s.RxBytes
	}
	dash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tROLE\tRULE\tPEER\tDESTINATION\tAGE\tSENT\tRECEIVED\tAVERAGE\tNOW\tSENT AGAIN\tSUBS\tNOT SENT YET\tIN FLIGHT\tSTATE")
	for _, s := range after {
		id := s.ID
		if len(id) > 6 {
			id = id[:6] // as in the node's log
		}
		age := now.Sub(s.Started)
		average, current, again := "-", "-", "-"
		if age >= time.Second {
			average = speed(s.TxBytes+s.RxBytes, age)
		}
		if b, ok := moved[s.ID]; ok && apart > 0 {
			current = speed(s.TxBytes+s.RxBytes-b, apart)
		}
		if s.TxBytes > 0 {
			again = fmt.Sprintf("%.1f%%", 100*float64(s.ResentBytes)/float64(s.TxBytes))
		}
		rule, dest := dash(s.Rule), s.Destination
		if s.Test {
			// a path test: it has no rule, and its data ends inside the other node
			rule, dest = "(path test)", "(path test)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%v\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", id, s.Role, rule, dash(s.Peer), dest,
			age.Truncate(time.Second), size(s.TxBytes), size(s.RxBytes), average, current, again, s.Subs,
			size(s.UnsentBytes), size(s.InflightBytes), sessionState(s))
	}
	return tw.Flush()
}

// size writes a number of bytes for a person.
func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// speed writes n bytes in d as MB/s.
func speed(n int64, d time.Duration) string {
	return fmt.Sprintf("%.1f MB/s", float64(n)/(1<<20)/d.Seconds())
}
