package main

// The sessions command: what the node on this host is carrying right now.
//
//	tcpraid sessions [-json]

import (
	"encoding/json"
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

func cmdSessions(args []string, stdout, stderr io.Writer) int {
	var asJSON bool
	return command{
		name: "sessions",
		flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&asJSON, "json", false, "print the sessions as the API returns them (JSON), without the current speed")
		},
		run: func(dataDir string, _ []string, stdout, _ io.Writer) error {
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
	fmt.Fprintln(tw, "ID\tROLE\tRULE\tPEER\tDESTINATION\tAGE\tSENT\tRECEIVED\tAVERAGE\tNOW\tSENT AGAIN\tSUBS")
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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%v\t%s\t%s\t%s\t%s\t%s\t%d\n", id, s.Role, dash(s.Rule), dash(s.Peer), s.Destination,
			age.Truncate(time.Second), size(s.TxBytes), size(s.RxBytes), average, current, again, s.Subs)
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
