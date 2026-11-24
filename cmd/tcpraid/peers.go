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
//	tcpraid peers remove NAME [-with-rules]
//	tcpraid peers check NAME
//	tcpraid peers test NAME [-seconds N] [-path PATH] [-json]
//	tcpraid peers set NAME [-allow LIST] [-udp-dial HOST:PORT]
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
	"github.com/Claw-King/tcpraid/internal/forward"
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
	if out == nil || len(bytes.TrimSpace(answer)) == 0 {
		// nothing to read, or a node that answers this request without a body: out
		// stays as it was
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
		fmt.Fprintf(stderr, "tcpraid %s: -data is empty. Name the data directory.\n", c.name)
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
			fmt.Fprintf(stderr, "tcpraid %s: unknown command %q. The commands are: %s\n", group, sub, strings.Join(names, ", "))
		}
		return 2
	}
	return c.execute(args, stdout, stderr)
}

// printPeerTest writes the result of a path test: a table of the rates, then one
// sentence for each path. It returns an error when a path could not be measured.
func printPeerTest(w io.Writer, res api.PeerTest) error {
	if res.PeerDials {
		fmt.Fprintf(w, "This node has no path to %s. The paths below are the ones %s holds to this node.\n", res.Peer, res.Peer)
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PATH\tSUB-CONNECTIONS\tONE ALONE\tALL TOGETHER\tTIMES")
	rate := func(v float64) string { return fmt.Sprintf("%.1f MB/s", v/(1<<20)) }
	failed := 0
	for _, p := range res.Paths {
		switch {
		case p.Error != "":
			failed++
			fmt.Fprintf(tw, "%s\t%d\t-\t-\t-\n", p.Name, p.Subs)
		case p.Subs < 2:
			fmt.Fprintf(tw, "%s\t%d\t%s\t-\t-\n", p.Name, p.Subs, rate(p.One))
		default:
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%.1f\n", p.Name, p.Subs, rate(p.One), rate(p.All), p.All/p.One)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, p := range res.Paths {
		switch {
		case p.Error != "":
			fmt.Fprintf(w, "%s: not measured. %s\n", p.Name, p.Error)
		case p.Subs < 2:
			fmt.Fprintf(w, "%s: the path has one sub-connection, so there is nothing to compare.\n", p.Name)
		case p.LittleGain:
			fmt.Fprintf(w, "%s: %d sub-connections together carried little more than one alone. Either one connection already fills this line, or something on the path puts the connections into one. \"Connecting two sites\" in the documentation (docs/en/connecting.md) says how to tell them apart.\n", p.Name, p.Subs)
		default:
			fmt.Fprintf(w, "%s: %d sub-connections together carried %.1f times what one carried alone.\n", p.Name, p.Subs, p.All/p.One)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d path(s) could not be measured", failed, len(res.Paths))
	}
	return nil
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
				fs.Var(&addrs, "addr", "HOST:PORT at which the other node reaches this one (may be given more than once, default: the node's -advertise)")
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
					"Enter it on the other node with \"tcpraid pair join\", which then reads the code from standard input.\n"+
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
						return fmt.Errorf("standard input holds more than %d bytes, which is not a pairing code", maxCodeInput)
					}
					if code = strings.TrimSpace(string(b)); code == "" {
						return errors.New("no pairing code was given. Give it as the argument, or on standard input.")
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
	fmt.Fprintln(tw, "PATH\tTYPE\tADDRESS\tPROXY\tSECURITY\tVERIFY\tPIN\tROLES\tSUBS\tMARK")
	dash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	for _, p := range paths {
		mark := "-"
		if p.Mark != 0 {
			mark = strconv.Itoa(p.Mark)
		}
		// a path with several addresses shows them in the order they are tried
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n", p.Name, p.Type, pathAddrs(p), dash(p.Proxy), p.Security, dash(p.Verify), dash(p.Pin),
			strings.Join(p.Roles, ","), p.Subs, mark)
	}
	return tw.Flush()
}

// olderNode says, for a request that a node refused because it does not know one of the
// fields (their JSON names, each with what it is for), that the node runs an older
// version than this command: the binary was replaced and the node not restarted yet.
// Any other error is returned as it is.
func olderNode(err error, fields map[string]string) error {
	for field, what := range fields {
		if strings.Contains(err.Error(), fmt.Sprintf("unknown field %q", field)) {
			return fmt.Errorf("the running node is older than this command and does not know %s. Restart the node to finish an update. (%w)", what, err)
		}
	}
	return err
}

// pathAddrs writes the addresses of a path, in the order they are tried.
func pathAddrs(p api.Path) string {
	return strings.Join(append([]string{p.Addr}, p.Addrs...), ",")
}

// cmdPeersPath is "tcpraid peers path set|remove". Each asks the node to change the one
// path: the node reads and writes the peer's paths in one step, so two of these commands
// that run at the same time for two paths both take effect.
func cmdPeersPath(args []string, stdout, stderr io.Writer) int {
	var set *flag.FlagSet
	var f struct {
		addr, typ, proxy, security, verify, pin, roles string
		subs, mark                                     int
	}
	// pathURL is the path of one path of a peer in an API request.
	pathURL := func(peer, path string) string { return peerPath(peer) + "/paths/" + url.PathEscape(path) }
	return dispatch("peers path", "", []string{"set", "remove"}, map[string]command{
		"set": {
			name: "peers path set", usage: "NAME PATH", nargs: 2,
			flags: func(fs *flag.FlagSet) {
				set = fs
				fs.StringVar(&f.addr, "addr", "", "HOST:PORT of the peer's listener as seen from the end of the path (needed for a new path). Several, separated by commas, are tried in that order when a connection cannot be made.")
				fs.IntVar(&f.mark, "mark", 0, "socket mark of the path's connections, on Linux only and with the capability CAP_NET_ADMIN (0: none)")
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
				// only what was given is sent: the node leaves the rest of the path as it is
				var req api.PathUpdate
				set.Visit(func(fl *flag.Flag) {
					switch fl.Name {
					case "addr":
						// the first is the path's address, the others its further ones. One
						// address alone is sent as before: the node then takes any further
						// ones away itself.
						addrs := strings.Split(f.addr, ",")
						req.Addr = &addrs[0]
						if rest := addrs[1:]; len(rest) > 0 {
							req.Addrs = &rest
						}
					case "mark":
						req.Mark = &f.mark
					case "type":
						req.Type = &f.typ
					case "proxy":
						req.Proxy = &f.proxy
					case "security":
						req.Security = &f.security
					case "verify":
						req.Verify = &f.verify
					case "pin":
						req.Pin = &f.pin
					case "roles":
						roles := strings.Split(f.roles, ",")
						req.Roles = &roles
					case "subs":
						req.Subs = &f.subs
					}
				})
				needsAddr := fmt.Errorf("%s has no path %s, and a new path needs -addr", peer, pathName)
				if req == (api.PathUpdate{}) {
					// nothing to change: the path is shown as it is, as this command always did
					var p api.Peer
					if err := apiCall(dataDir, http.MethodGet, peerPath(peer), nil, &p, shortCall); err != nil {
						return err
					}
					for _, path := range p.Paths {
						if path.Name == pathName {
							fmt.Fprintf(stdout, "path %s of %s changed\n", pathName, peer)
							return printPaths(stdout, p.Paths)
						}
					}
					return needsAddr
				}
				var done api.PathSet
				if err := apiCall(dataDir, http.MethodPut, pathURL(peer, pathName), req, &done, shortCall); err != nil {
					if strings.Contains(err.Error(), "a new path needs an address") {
						return needsAddr // in the words of this command
					}
					return olderNode(err, map[string]string{"addrs": "several addresses for a path", "mark": "a socket mark for a path"})
				}
				verb := "changed"
				if done.Created {
					verb = "added"
				}
				fmt.Fprintf(stdout, "path %s of %s %s\n", pathName, peer, verb)
				return printPaths(stdout, done.Peer.Paths)
			},
		},
		"remove": {
			name: "peers path remove", usage: "NAME PATH", nargs: 2,
			run: func(dataDir string, args []string, stdout, _ io.Writer) error {
				peer, pathName := args[0], args[1]
				var left api.Peer
				if err := apiCall(dataDir, http.MethodDelete, pathURL(peer, pathName), nil, &left, shortCall); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "path %s of %s removed\n", pathName, peer)
				if len(left.Paths) == 0 {
					fmt.Fprintf(stdout, "%s has no path left, so this node cannot reach it\n", peer)
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
	var set *flag.FlagSet
	var allow, udpDial string
	var test api.PeerTestRequest
	var testJSON, withRules bool
	return dispatch("peers", "list", []string{"list", "show", "remove", "check", "test", "set", "path"}, map[string]command{
		"test": {
			name: "peers test", usage: "NAME", nargs: 1,
			flags: func(fs *flag.FlagSet) {
				fs.IntVar(&test.Seconds, "seconds", forward.TestDefaultSeconds, fmt.Sprintf("how long each run sends test data (1 to %d)", forward.TestMaxSeconds))
				fs.StringVar(&test.Path, "path", "", "test only the path of this name (\"\": every path that carries data)")
				fs.BoolVar(&testJSON, "json", false, "print the result as the API returns it (JSON)")
			},
			run: func(dataDir string, args []string, stdout, stderr io.Writer) error {
				if test.Seconds < 1 || test.Seconds > forward.TestMaxSeconds {
					return fmt.Errorf("-seconds is out of range. Give 1 to %d.", forward.TestMaxSeconds)
				}
				// two runs for each path, and no peer has more than node.MaxPaths paths
				wait := time.Duration(2*test.Seconds*node.MaxPaths)*time.Second + longCall
				fmt.Fprintf(stderr, "path test with %s: two runs of up to %d seconds for each path\n", args[0], test.Seconds)
				if testJSON {
					var raw json.RawMessage
					if err := apiCall(dataDir, http.MethodPost, peerPath(args[0])+"/test", test, &raw, wait); err != nil {
						return err
					}
					_, err := fmt.Fprintf(stdout, "%s\n", raw)
					return err
				}
				var res api.PeerTest
				if err := apiCall(dataDir, http.MethodPost, peerPath(args[0])+"/test", test, &res, wait); err != nil {
					return err
				}
				return printPeerTest(stdout, res)
			},
		},
		"set": {
			name: "peers set", usage: "NAME", nargs: 1,
			flags: func(fs *flag.FlagSet) {
				set = fs
				fs.StringVar(&allow, "allow", "", "destinations the peer may reach through this node, as networks with an optional port or range, separated by commas, such as 192.0.2.0/24:873,[2001:db8::/32]:5000-5010 (\"\": any destination)")
				fs.StringVar(&udpDial, "udp-dial", "", "HOST:PORT of the peer's -udp-listen, to which this node sends feedback for sessions with it (\"\": none)")
			},
			run: func(dataDir string, args []string, stdout, _ io.Writer) error {
				var req api.PeerUpdate
				set.Visit(func(fl *flag.Flag) {
					switch fl.Name {
					case "allow":
						req.Allow = &allow
					case "udp-dial":
						req.UDPDial = &udpDial
					}
				})
				if req.Allow == nil && req.UDPDial == nil {
					return errors.New("nothing to set. Give -allow, -udp-dial or both. An empty value clears the setting.")
				}
				var stored api.Peer
				if err := apiCall(dataDir, http.MethodPut, peerPath(args[0]), req, &stored, shortCall); err != nil {
					return err
				}
				if req.Allow != nil {
					if stored.Allow == "" {
						fmt.Fprintf(stdout, "%s may reach any destination through this node\n", stored.Name)
					} else {
						fmt.Fprintf(stdout, "%s may reach through this node: %s\n", stored.Name, stored.Allow)
					}
				}
				if req.UDPDial != nil {
					if stored.UDPDial == "" {
						fmt.Fprintf(stdout, "no UDP feedback is sent to %s\n", stored.Name)
					} else {
						fmt.Fprintf(stdout, "UDP feedback for sessions with %s goes to %s\n", stored.Name, stored.UDPDial)
					}
				}
				return nil
			},
		},
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
				if p.Spares > 0 {
					fmt.Fprintf(stdout, "spares       %d connections wait for a session to begin on them\n", p.Spares)
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
						paths = append(paths, fmt.Sprintf("%s (%s, %s)", pathAddrs(path), path.Security, strings.Join(path.Roles, "+")))
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
			flags: func(fs *flag.FlagSet) {
				fs.BoolVar(&withRules, "with-rules", false, "remove the rules that name the peer as well")
			},
			run: func(dataDir string, args []string, stdout, _ io.Writer) error {
				query := "?rules=keep"
				if withRules {
					query = "?rules=remove"
				}
				var gone api.PeerRemoval
				if err := apiCall(dataDir, http.MethodDelete, peerPath(args[0])+query, nil, &gone, shortCall); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "peer %s removed. The other node is not told and still lists this one.\n", args[0])
				if gone.Peer == "" {
					// the answer of a node from before 0.4.0, which names no rules
					fmt.Fprintln(stdout, "The running node is older than this command. It did not say which rules name the peer, and it removed none. \"tcpraid rules\" lists them. Restart the node to finish an update.")
					return nil
				}
				switch names := strings.Join(gone.Rules, ", "); {
				case len(gone.Rules) == 0:
				case gone.RulesRemoved:
					fmt.Fprintf(stdout, "The rules that named it were removed with it: %s\n", names)
				default:
					fmt.Fprintf(stdout, "These rules still name %s: %s\n"+
						"A node that is paired with this one under the same name later takes them over. To prevent that, remove them with \"tcpraid rules remove\".\n", args[0], names)
				}
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
					return fmt.Errorf("this node has no path to %s and can only be reached by it", chk.Peer)
				}
				// one line for each address of each path; a path with several addresses
				// works while one of them does
				failed := 0
				addrs, works := map[string]int{}, map[string]bool{}
				var names []string
				for _, p := range chk.Paths {
					if addrs[p.Name] == 0 {
						names = append(names, p.Name)
					}
					addrs[p.Name]++
					if p.OK {
						works[p.Name] = true
						fmt.Fprintf(stdout, "%s  %s (%s): ok\n", p.Name, p.Addr, p.Security)
					} else {
						failed++
						fmt.Fprintf(stdout, "%s  %s (%s): FAILED: %s\n", p.Name, p.Addr, p.Security, p.Error)
					}
				}
				if failed == 0 {
					return nil
				}
				if len(names) == len(chk.Paths) {
					return fmt.Errorf("%d of %d path(s) to %s failed", failed, len(chk.Paths), chk.Peer)
				}
				dead := 0
				for _, name := range names {
					if !works[name] {
						dead++
					}
				}
				if dead == 0 {
					return fmt.Errorf("%d of %d address(es) to %s failed. Every path still has an address that works.", failed, len(chk.Paths), chk.Peer)
				}
				return fmt.Errorf("%d of %d address(es) to %s failed, and %d of %d path(s) have no address that works", failed, len(chk.Paths), chk.Peer, dead, len(names))
			},
		},
	}, args, stdout, stderr)
}
