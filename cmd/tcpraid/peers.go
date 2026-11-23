package main

// The pair and peers commands. They ask the node on this host through its local API,
// like status does.
//
//	tcpraid pair create [-addr HOST:PORT]... [-ttl DURATION]
//	tcpraid pair list
//	tcpraid pair revoke ID
//	tcpraid pair join [CODE] [-addr HOST:PORT]
//	tcpraid peers
//	tcpraid peers show NAME
//	tcpraid peers remove NAME
//	tcpraid peers check NAME
//	tcpraid peers path set NAME PATH [-addr A] [-type T] [-proxy P] [-security S] [-verify V] [-pin F] [-roles R,...] [-subs N]
//	tcpraid peers path remove NAME PATH
//
// Each takes -data DIR, the node's data directory. Flags may stand before or after the
// code, id or name. "pair join" without a code, or with "-" for it, reads the code from
// standard input, which keeps it out of the process list and the shell's history.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Claw-King/tcpraid/internal/api"
	"github.com/Claw-King/tcpraid/internal/link"
	"github.com/Claw-King/tcpraid/internal/node"
	"github.com/Claw-King/tcpraid/internal/pair"
)

// errNoNode: the node could not be asked at all.
var errNoNode = errors.New("the node is not running or -data is not its data directory")

// errTimeout: the node was asked and did not answer in time.
var errTimeout = errors.New("timed out")

// apiCall sends one request to the node whose data directory is dataDir and decodes the
// answer into out (nil: the answer has no body that matters). in, if not nil, is sent as
// the JSON body. An error that matches errNoNode means the node could not be asked; any
// other error is what the node answered.
func apiCall(dataDir, method, path string, in, out any, timeout time.Duration) error {
	token, err := node.ReadToken(dataDir)
	if err != nil {
		return fmt.Errorf("%w (%v)", errNoNode, err)
	}
	addr, err := node.ReadAPIAddr(dataDir)
	if err != nil {
		return fmt.Errorf("%w (%v)", errNoNode, err)
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://"+addr+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// no proxy from the environment: the token goes to the node and nowhere else
	tr := &http.Transport{Proxy: nil}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr, Timeout: timeout}).Do(req)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			// the node is there and busy: not the same as no node
			return fmt.Errorf("%w: the node did not answer within %v", errTimeout, timeout)
		}
		return fmt.Errorf("%w (%v)", errNoNode, err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(answer, &e) != nil || e.Error == "" {
			// not this API: something else listens at the recorded address
			return fmt.Errorf("%w (%s answered %s)", errNoNode, addr, resp.Status)
		}
		return errors.New(e.Error)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(answer, out); err != nil {
		return fmt.Errorf("%w (%s did not answer as the node does: %v)", errNoNode, addr, err)
	}
	return nil
}

// command is one of the pair or peers commands: its flags, the number of arguments it
// takes besides them, and what it does with both.
type command struct {
	name     string // as typed, for messages: "pair join"
	usage    string // the arguments, for messages: "CODE"
	nargs    int    // arguments it must have
	optional int    // and how many more it may have
	flags    func(fs *flag.FlagSet)
	run      func(dataDir string, args []string, stdout, stderr io.Writer) error
}

// execute parses the flags of c from args, wherever they stand among the arguments,
// runs c and returns the exit code: 0, 1 when the node refused or could not be asked, 2
// for a command line that is wrong.
func (c command) execute(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tcpraid "+c.name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data", defaultDataDir, "data directory of the node")
	if c.flags != nil {
		c.flags(fs)
	}
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: tcpraid %s %s [flags]\n", c.name, c.usage)
		fs.PrintDefaults()
	}
	var rest []string
	for {
		switch err := fs.Parse(args); {
		case errors.Is(err, flag.ErrHelp):
			return 0
		case err != nil:
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		// a flag ends at the first argument that is not one: take it, go on behind it
		rest = append(rest, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(rest) < c.nargs || len(rest) > c.nargs+c.optional {
		want := strconv.Itoa(c.nargs)
		if c.optional > 0 {
			want = fmt.Sprintf("%d to %d", c.nargs, c.nargs+c.optional)
		}
		fmt.Fprintf(stderr, "tcpraid %s: takes %s argument(s) besides flags, got %d\nusage: tcpraid %s %s [flags]\n", c.name, want, len(rest), c.name, c.usage)
		return 2
	}
	if *dataDir == "" {
		fmt.Fprintf(stderr, "tcpraid %s: -data is empty: name the data directory\n", c.name)
		return 2
	}
	if err := c.run(*dataDir, rest, stdout, stderr); err != nil {
		if errors.Is(err, errNoNode) {
			fmt.Fprintf(stderr, "tcpraid %s: %v (-data %s)\n", c.name, err, *dataDir)
		} else {
			fmt.Fprintf(stderr, "tcpraid %s: %v\n", c.name, err)
		}
		return 1
	}
	return 0
}

// dispatch runs the command of group ("pair", "peers", "peers path") that args name.
// def, if not empty, is the command when args name none. names lists what the group
// has, for the message when args name something else.
func dispatch(group, def string, names []string, cmds map[string]command, args []string, stdout, stderr io.Writer) int {
	sub := def
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	c, ok := cmds[sub]
	if !ok {
		if sub == "" {
			fmt.Fprintf(stderr, "tcpraid %s: which one? %s\n", group, strings.Join(names, ", "))
		} else {
			fmt.Fprintf(stderr, "tcpraid %s: unknown command %q; there are: %s\n", group, sub, strings.Join(names, ", "))
		}
		return 2
	}
	return c.execute(args, stdout, stderr)
}

const (
	shortCall = 10 * time.Second
	// a check waits for another node on every path: a handshake and its timeouts
	longCall = 2 * time.Minute
	// a join is given up by the node after link.DefaultJoinTimeout; the command waits
	// longer than that, so that it is the node's answer the user reads
	joinCall = link.DefaultJoinTimeout + 30*time.Second

	// maxCodeInput bounds what "pair join" reads from standard input for a code.
	maxCodeInput = 4096
)

func cmdPair(args []string, stdout, stderr io.Writer) int {
	var addrs stringList
	var ttl time.Duration
	var joinAddr string
	return dispatch("pair", "", []string{"create", "list", "revoke", "join"}, map[string]command{
		"create": {
			name: "pair create",
			flags: func(fs *flag.FlagSet) {
				fs.Var(&addrs, "addr", "HOST:PORT at which the other node reaches this one; may be given more than once (default: the node's -advertise)")
				fs.DurationVar(&ttl, "ttl", 0, "how long the code is valid (default 1h)")
			},
			run: func(dataDir string, _ []string, stdout, stderr io.Writer) error {
				// 0 is the flag not given: the node's default
				if ttl != 0 && (ttl < pair.MinTTL || ttl > pair.MaxTTL || ttl%time.Second != 0) {
					return fmt.Errorf("-ttl %v: a code is valid for whole seconds, from %v to %v", ttl, pair.MinTTL, pair.MaxTTL)
				}
				var made api.PairingCode
				req := api.PairingCodeRequest{Addresses: addrs, TTLSeconds: int64(ttl / time.Second)}
				if err := apiCall(dataDir, http.MethodPost, api.PathPairingCodes, req, &made, shortCall); err != nil {
					return err
				}
				// the code alone on standard output, so that it can be piped; the rest is for the person
				fmt.Fprintln(stdout, made.Code)
				fmt.Fprintf(stderr, "pairing code %s, valid until %s, for one use.\n"+
					"Enter it on the other node: tcpraid pair join (it then reads the code from standard input)\n"+
					"It is shown this once and is a secret until it is used.\n", made.ID, made.Expires.Local().Format(time.RFC1123))
				return nil
			},
		},
		"list": {
			name: "pair list",
			run: func(dataDir string, _ []string, stdout, _ io.Writer) error {
				var list []api.PairingCode
				if err := apiCall(dataDir, http.MethodGet, api.PathPairingCodes, nil, &list, shortCall); err != nil {
					return err
				}
				if len(list) == 0 {
					fmt.Fprintln(stdout, "no pairing codes")
					return nil
				}
				tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tVALID UNTIL\tUSED")
				for _, pc := range list {
					used := "no"
					if pc.Used {
						used = "yes"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\n", pc.ID, pc.Expires.Local().Format(time.RFC3339), used)
				}
				return tw.Flush()
			},
		},
		"revoke": {
			name: "pair revoke", usage: "ID", nargs: 1,
			run: func(dataDir string, args []string, stdout, _ io.Writer) error {
				if err := apiCall(dataDir, http.MethodDelete, api.PathPairingCodes+"/"+url.PathEscape(args[0]), nil, nil, shortCall); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "pairing code %s revoked\n", args[0])
				return nil
			},
		},
		"join": {
			name: "pair join", usage: "[CODE]", optional: 1,
			flags: func(fs *flag.FlagSet) {
				fs.StringVar(&joinAddr, "addr", "", "HOST:PORT to connect to, instead of the addresses in the code")
			},
			run: func(dataDir string, args []string, stdout, _ io.Writer) error {
				var code string
				if len(args) == 1 && args[0] != "-" {
					code = args[0]
				} else {
					// not on the command line: not in the process list, not in the shell's history
					b, err := io.ReadAll(io.LimitReader(stdin, maxCodeInput+1))
					if err != nil {
						return fmt.Errorf("reading the pairing code from standard input: %w", err)
					}
					if len(b) > maxCodeInput {
						return fmt.Errorf("standard input holds more than %d bytes: that is not a pairing code", maxCodeInput)
					}
					if code = strings.TrimSpace(string(b)); code == "" {
						return errors.New("no pairing code: give it as the argument, or on standard input")
					}
				}
				var p api.Peer
				if err := apiCall(dataDir, http.MethodPost, api.PathPeers, api.JoinRequest{Code: code, Address: joinAddr}, &p, joinCall); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "paired with %s (fingerprint %s)\n", p.Name, p.Fingerprint)
				return nil
			},
		},
	}, args, stdout, stderr)
}

// peerPath is the path of a peer in an API request.
func peerPath(name string) string { return api.PathPeers + "/" + url.PathEscape(name) }

// peerState says in words what the control connections with a peer say.
func peerState(p api.Peer) string {
	s := "offline: no control connection with it is up"
	if p.Online {
		s = "online"
		if p.Version != "" {
			s += ", runs " + p.Version
		}
	}
	if p.LastMessage != nil {
		s += ", last heard " + p.LastMessage.Local().Format(time.RFC3339)
	}
	return s
}

// printPaths writes a table of paths.
func printPaths(w io.Writer, paths []api.Path) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PATH\tTYPE\tADDRESS\tPROXY\tSECURITY\tVERIFY\tPIN\tROLES\tSUBS")
	dash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	for _, p := range paths {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\n", p.Name, p.Type, p.Addr, dash(p.Proxy), p.Security, dash(p.Verify), dash(p.Pin),
			strings.Join(p.Roles, ","), p.Subs)
	}
	return tw.Flush()
}

// cmdPeersPath is "tcpraid peers path set|remove". Both read the peer, change its list
// of paths and send the list back: the node replaces the paths as a whole.
func cmdPeersPath(args []string, stdout, stderr io.Writer) int {
	var set *flag.FlagSet
	var f struct {
		addr, typ, proxy, security, verify, pin, roles string
		subs                                           int
	}
	return dispatch("peers path", "", []string{"set", "remove"}, map[string]command{
		"set": {
			name: "peers path set", usage: "NAME PATH", nargs: 2,
			flags: func(fs *flag.FlagSet) {
				set = fs
				fs.StringVar(&f.addr, "addr", "", "HOST:PORT of the peer's listener as seen from the end of the path (needed for a new path)")
				fs.StringVar(&f.typ, "type", "", "tcp or socks5 (a new path: tcp)")
				fs.StringVar(&f.proxy, "proxy", "", "socks5://[user:pass@]host:port, with -type socks5")
				fs.StringVar(&f.security, "security", "", "tls, owncert or off (a new path: tls)")
				fs.StringVar(&f.verify, "verify", "", "with -security owncert: pin or ca")
				fs.StringVar(&f.pin, "pin", "", "with -security owncert and -verify pin: fingerprint of the listener's certificate key")
				fs.StringVar(&f.roles, "roles", "", "comma-separated: data, repair, control (a new path: data)")
				fs.IntVar(&f.subs, "subs", 0, "sub-connections, 1 to 64 (a new path: 8)")
			},
			run: func(dataDir string, args []string, stdout, _ io.Writer) error {
				peer, pathName := args[0], args[1]
				var p api.Peer
				if err := apiCall(dataDir, http.MethodGet, peerPath(peer), nil, &p, shortCall); err != nil {
					return err
				}
				given := map[string]bool{}
				set.Visit(func(fl *flag.Flag) { given[fl.Name] = true })
				idx := -1
				for i := range p.Paths {
					if p.Paths[i].Name == pathName {
						idx = i
					}
				}
				verb := "changed"
				if idx < 0 {
					if !given["addr"] {
						return fmt.Errorf("%s has no path %s: a new path needs -addr", peer, pathName)
					}
					p.Paths = append(p.Paths, api.Path{Name: pathName, Type: "tcp", Security: node.SecurityTLS, Roles: []string{node.RoleData}, Subs: 8})
					idx, verb = len(p.Paths)-1, "added"
				}
				path := &p.Paths[idx]
				if given["addr"] {
					path.Addr = f.addr
				}
				if given["type"] {
					path.Type = f.typ
					if f.typ != "socks5" && !given["proxy"] {
						path.Proxy = "" // a proxy goes with socks5 only
					}
				}
				if given["proxy"] {
					path.Proxy = f.proxy
				}
				if given["security"] {
					path.Security = f.security
					if f.security != node.SecurityOwnCert {
						// verify and pin go with owncert only
						if !given["verify"] {
							path.Verify = ""
						}
						if !given["pin"] {
							path.Pin = ""
						}
					}
				}
				if given["verify"] {
					path.Verify = f.verify
					if f.verify == node.VerifyCA && !given["pin"] {
						path.Pin = ""
					}
				}
				if given["pin"] {
					path.Pin = f.pin
				}
				if given["roles"] {
					path.Roles = strings.Split(f.roles, ",")
				}
				if given["subs"] {
					path.Subs = f.subs
				}
				var stored api.Peer
				if err := apiCall(dataDir, http.MethodPut, peerPath(peer), api.PeerUpdate{Paths: &p.Paths}, &stored, shortCall); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "path %s of %s %s\n", pathName, peer, verb)
				return printPaths(stdout, stored.Paths)
			},
		},
		"remove": {
			name: "peers path remove", usage: "NAME PATH", nargs: 2,
			run: func(dataDir string, args []string, stdout, _ io.Writer) error {
				peer, pathName := args[0], args[1]
				var p api.Peer
				if err := apiCall(dataDir, http.MethodGet, peerPath(peer), nil, &p, shortCall); err != nil {
					return err
				}
				kept := []api.Path{}
				for _, path := range p.Paths {
					if path.Name != pathName {
						kept = append(kept, path)
					}
				}
				if len(kept) == len(p.Paths) {
					return fmt.Errorf("%s has no path %s", peer, pathName)
				}
				if err := apiCall(dataDir, http.MethodPut, peerPath(peer), api.PeerUpdate{Paths: &kept}, nil, shortCall); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "path %s of %s removed\n", pathName, peer)
				if len(kept) == 0 {
					fmt.Fprintf(stdout, "%s has no path left: this node cannot reach it\n", peer)
				}
				return nil
			},
		},
	}, args, stdout, stderr)
}

func cmdPeers(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "path" {
		return cmdPeersPath(args[1:], stdout, stderr)
	}
	return dispatch("peers", "list", []string{"list", "show", "remove", "check", "path"}, map[string]command{
		"show": {
			name: "peers show", usage: "NAME", nargs: 1,
			run: func(dataDir string, args []string, stdout, _ io.Writer) error {
				var p api.Peer
				if err := apiCall(dataDir, http.MethodGet, peerPath(args[0]), nil, &p, shortCall); err != nil {
					return err
				}
				addrs := "none: it joined this node, which was not told where it listens"
				if len(p.Addresses) > 0 {
					addrs = strings.Join(p.Addresses, ", ")
				}
				fmt.Fprintf(stdout, "name         %s\n", p.Name)
				fmt.Fprintf(stdout, "fingerprint  %s\n", p.Fingerprint)
				fmt.Fprintf(stdout, "paired       %s\n", p.PairedAt.Local().Format(time.RFC3339))
				fmt.Fprintf(stdout, "addresses    %s\n", addrs)
				fmt.Fprintf(stdout, "state        %s\n", peerState(p))
				if p.Allow != "" {
					fmt.Fprintf(stdout, "allow        %s\n", p.Allow)
				}
				if p.UDPDial != "" {
					fmt.Fprintf(stdout, "udp feedback %s\n", p.UDPDial)
				}
				if len(p.Paths) == 0 {
					fmt.Fprintln(stdout, "paths        none: this node cannot reach it (tcpraid peers path set adds one)")
					return nil
				}
				fmt.Fprintln(stdout)
				return printPaths(stdout, p.Paths)
			},
		},
		"list": {
			name: "peers",
			run: func(dataDir string, _ []string, stdout, _ io.Writer) error {
				var list []api.Peer
				if err := apiCall(dataDir, http.MethodGet, api.PathPeers, nil, &list, shortCall); err != nil {
					return err
				}
				if len(list) == 0 {
					fmt.Fprintln(stdout, "no peers")
					return nil
				}
				tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tSTATE\tFINGERPRINT\tPAIRED\tPATHS")
				for _, p := range list {
					var paths []string
					for _, path := range p.Paths {
						paths = append(paths, fmt.Sprintf("%s (%s, %s)", path.Addr, path.Security, strings.Join(path.Roles, "+")))
					}
					if paths == nil {
						paths = []string{"none: this node cannot reach it"}
					}
					state := "offline"
					if p.Online {
						state = "online"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.Name, state, p.Fingerprint, p.PairedAt.Local().Format(time.RFC3339), strings.Join(paths, "; "))
				}
				return tw.Flush()
			},
		},
		"remove": {
			name: "peers remove", usage: "NAME", nargs: 1,
			run: func(dataDir string, args []string, stdout, _ io.Writer) error {
				if err := apiCall(dataDir, http.MethodDelete, peerPath(args[0]), nil, nil, shortCall); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "peer %s removed; the other node is not told and still lists this one\n", args[0])
				return nil
			},
		},
		"check": {
			name: "peers check", usage: "NAME", nargs: 1,
			run: func(dataDir string, args []string, stdout, _ io.Writer) error {
				var chk api.PeerCheck
				if err := apiCall(dataDir, http.MethodPost, peerPath(args[0])+"/check", nil, &chk, longCall); err != nil {
					return err
				}
				if len(chk.Paths) == 0 {
					return fmt.Errorf("this node has no path to %s: it can only be reached by it", chk.Peer)
				}
				failed := 0
				for _, p := range chk.Paths {
					if p.OK {
						fmt.Fprintf(stdout, "%s  %s (%s): ok\n", p.Name, p.Addr, p.Security)
					} else {
						failed++
						fmt.Fprintf(stdout, "%s  %s (%s): FAILED: %s\n", p.Name, p.Addr, p.Security, p.Error)
					}
				}
				if failed > 0 {
					return fmt.Errorf("%d of %d path(s) to %s failed", failed, len(chk.Paths), chk.Peer)
				}
				return nil
			},
		},
	}, args, stdout, stderr)
}
