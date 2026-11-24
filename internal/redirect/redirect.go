// Package redirect installs the redirect rules that send connections to a node's
// transparent listener, with the nft program of the host. It is used on Linux.
//
// The node owns one table, "inet tcpraid", and touches nothing else. The table holds a
// rule for every network that an enabled transparent rule of the node names: TCP
// connections to it are redirected to the port of the transparent listener. The whole
// table is written anew, from one file given to nft, whenever the node's rules change,
// and is deleted when the node stops.
package redirect

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/node"
)

const (
	// Table is the name of the node's table, in the family inet.
	Table = "tcpraid"
	// Priority is the priority of the node's chains: they come before chains at the
	// standard priority of destination NAT, which is -100.
	Priority = -110
)

// Which connections are redirected.
const (
	Forwarded = "forwarded" // those that pass through this host (hook prerouting)
	Local     = "local"     // those that programs on this host open (hook output)
	Both      = "both"
)

// CheckMode reports whether mode is one of Forwarded, Local and Both.
func CheckMode(mode string) error {
	switch mode {
	case Forwarded, Local, Both:
		return nil
	}
	return fmt.Errorf("%q is none of %s, %s and %s", mode, Forwarded, Local, Both)
}

// Script returns the text for "nft -f" that puts the node's table in place for the
// transparent rules among rules: the enabled ones, in their order. port is the port of
// the transparent listener. With mode Local or Both, connections of processes that run
// as the user uid are left alone: those are the node's own.
func Script(rules []node.Rule, port int, mode string, uid int) (string, error) {
	if err := CheckMode(mode); err != nil {
		return "", err
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("the transparent listener has no port to redirect to (%d)", port)
	}
	var lines []string
	for _, r := range rules {
		if !r.Enabled || !r.Transparent() {
			continue
		}
		list, err := engine.ParseAllow(r.Match)
		if err != nil {
			return "", fmt.Errorf("rule %q: %w", r.Name, err)
		}
		for _, e := range list.Entries() {
			family := "ip"
			if e.Net.IP.To4() == nil {
				family = "ip6"
			}
			// A match may hold ports that no connection has. The numbers are brought
			// into the range nft takes, and an entry that then holds no port, which
			// no connection can meet, gets no line.
			first, last := max(e.First, 1), min(e.Last, 65535)
			if first > last {
				continue
			}
			ports := ""
			switch {
			case first == last:
				ports = fmt.Sprintf(" tcp dport %d", first)
			case first > 1 || last < 65535:
				ports = fmt.Sprintf(" tcp dport %d-%d", first, last)
			}
			if ports == "" {
				ports = " meta l4proto tcp"
			}
			lines = append(lines, fmt.Sprintf("%s daddr %s%s redirect to :%d", family, e.Net, ports, port))
		}
	}
	var b strings.Builder
	// The first line makes sure the table exists, so that the second can delete it.
	// The three statements go to nft as one file.
	fmt.Fprintf(&b, "table inet %s\ndelete table inet %s\ntable inet %s {\n", Table, Table, Table)
	chain := func(name, hook, first string) {
		fmt.Fprintf(&b, "\tchain %s {\n\t\ttype nat hook %s priority %d; policy accept;\n", name, hook, Priority)
		if first != "" {
			fmt.Fprintf(&b, "\t\t%s\n", first)
		}
		for _, l := range lines {
			fmt.Fprintf(&b, "\t\t%s\n", l)
		}
		b.WriteString("\t}\n")
	}
	if mode == Forwarded || mode == Both {
		chain("prerouting", "prerouting", "")
	}
	if mode == Local || mode == Both {
		chain("output", "output", fmt.Sprintf("meta skuid %d return", uid))
	}
	b.WriteString("}\n")
	return b.String(), nil
}

// Installer keeps the node's table in line with its rules.
type Installer struct {
	Nft  string // the nft program; empty: "nft" from the PATH
	Port int    // of the transparent listener
	Mode string // Forwarded, Local or Both
	UID  int    // the user the node runs as
	// Print, if not nil, gets the script in the place of nft: nothing is installed.
	Print func(script string)

	mu   sync.Mutex
	last string // the script that is in place
}

func (in *Installer) nft() string {
	if in.Nft != "" {
		return in.Nft
	}
	return "nft"
}

// run runs nft with args and the script on its standard input.
func (in *Installer) run(script string, args ...string) error {
	path, err := exec.LookPath(in.nft())
	if err != nil {
		return fmt.Errorf("the program nft was not found (%v). The node installs its redirect rules with it.", err)
	}
	cmd := exec.Command(path, args...)
	cmd.Stdin = strings.NewReader(script)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		said := strings.TrimSpace(out.String())
		if len(said) > 400 {
			said = said[:400]
		}
		return fmt.Errorf("nft did not take the rules: %v: %s. Where nft says that the operation is not permitted, the node lacks the capability CAP_NET_ADMIN.", err, said)
	}
	return nil
}

// Apply writes the table for the rules that rules returns, unless the one in place is
// the same. The rules are read while no other Apply runs, so that of two changes close
// together the later one is the one whose table stays.
func (in *Installer) Apply(rules func() []node.Rule) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	script, err := Script(rules(), in.Port, in.Mode, in.UID)
	if err != nil {
		return err
	}
	if script == in.last {
		return nil
	}
	if in.Print != nil {
		in.Print(script)
	} else if err := in.run(script, "-f", "-"); err != nil {
		return err
	}
	in.last = script
	return nil
}

// Remove deletes the node's table. A table that is not there is no error.
func (in *Installer) Remove() error {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.last = ""
	if in.Print != nil {
		return nil
	}
	// declared first, so that the delete does not fail on a table that is gone
	return in.run(fmt.Sprintf("table inet %s\ndelete table inet %s\n", Table, Table), "-f", "-")
}
