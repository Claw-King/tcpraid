package main

// Subcommands: tcpraid run, status, version, pair, peers, rules and sessions. A first argument
// that does not start with "-" names one; any other command line is the manual mode in
// main.go.
//
// "run" starts a node: a long-running process with a data directory (identity, state),
// a local HTTP API, its forwarding rules and, if asked for, listeners for other nodes.
// The other commands ask the node on this host through that API; "pair" and "peers" are
// in peers.go, "rules" in rules.go, "sessions" in sessions.go.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Claw-King/tcpraid/internal/api"
	"github.com/Claw-King/tcpraid/internal/control"
	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/forward"
	"github.com/Claw-King/tcpraid/internal/link"
	"github.com/Claw-King/tcpraid/internal/node"
	"github.com/Claw-King/tcpraid/internal/pair"
	"github.com/Claw-King/tcpraid/internal/web"
)

const (
	defaultAPIAddr = "127.0.0.1:9850"
	// defaultSpares is how many spare connections a node asks for and keeps when
	// -spares does not say.
	defaultSpares = 2

	// shutdownWait is how long a stopping node waits for connections from other nodes
	// that are still being handled (a pairing, the answer to a path check) before it
	// gives up the lock on its data directory.
	shutdownWait = 5 * time.Second
)

// stdin is where a command reads what it is not given as an argument.
var stdin io.Reader = os.Stdin

// subcommand runs the subcommand that args select and returns its exit code. ok is false
// when args select the manual mode instead: no arguments, or a first argument that
// starts with "-".
func subcommand(args []string, stdout, stderr io.Writer) (code int, ok bool) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return 0, false
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:], stderr), true
	case "status":
		return cmdStatus(args[1:], stdout, stderr), true
	case "version":
		return cmdVersion(args[1:], stdout, stderr), true
	case "pair":
		return cmdPair(args[1:], stdout, stderr), true
	case "peers":
		return cmdPeers(args[1:], stdout, stderr), true
	case "rules":
		return cmdRules(args[1:], stdout, stderr), true
	case "sessions":
		return cmdSessions(args[1:], stdout, stderr), true
	}
	fmt.Fprintf(stderr, "tcpraid: unknown command %q\n"+
		"commands: run, status, version, pair, peers, rules, sessions (tcpraid COMMAND -h lists a command's flags)\n"+
		"without a command, tcpraid takes the manual-mode flags (tcpraid -h)\n", args[0])
	return 2, true
}

// parseFlags parses the flags of a subcommand, which takes no other arguments. done is
// true when the command ends here with the given exit code: help was asked for, or the
// arguments are wrong (the message is already written).
func parseFlags(fs *flag.FlagSet, args []string) (code int, done bool) {
	switch err := fs.Parse(args); {
	case errors.Is(err, flag.ErrHelp):
		return 0, true
	case err != nil:
		return 2, true
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "%s: unexpected argument %q\n", fs.Name(), fs.Arg(0))
		return 2, true
	}
	return 0, false
}

// versionLine is what -version and the version command print.
func versionLine() string {
	return fmt.Sprintf("tcpraid %s (wire protocol %d)", version, engine.WireVersion)
}

func cmdVersion(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tcpraid version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if code, done := parseFlags(fs, args); done {
		return code
	}
	fmt.Fprintln(stdout, versionLine())
	return 0
}

func cmdRun(args []string, stderr io.Writer) int {
	o, code, done := runFlags(args, stderr)
	if done {
		return code
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runNodeWith(ctx, o); err != nil {
		fmt.Fprintf(stderr, "tcpraid run: %v\n", err)
		return 1
	}
	return 0
}

// runFlags reads and checks the command line of "tcpraid run". done is true when the
// command ends here with the given exit code: help was asked for, or the command line
// cannot work (the message is already written).
func runFlags(args []string, stderr io.Writer) (o runOptions, code int, done bool) {
	fs := flag.NewFlagSet("tcpraid run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data", defaultDataDir, "data directory (created if missing)")
	apiAddr := fs.String("api", defaultAPIAddr, "listen address of the local API")
	name := fs.String("name", "", "name of this node (default: the name it already has, or at the first start the host name with four random characters after it)")
	fs.StringVar(&o.listen, "listen", "", "listen address for other nodes, TLS (empty: this node can join others, but not be joined)")
	fs.StringVar(&o.listenPlain, "listen-plain", "", "listen address for peers without TLS, for paths that are already encrypted (empty: off)")
	fs.StringVar(&o.certFile, "cert", "", "certificate (PEM) shown to peers that use the owncert tier (needs -key and -listen)")
	fs.StringVar(&o.keyFile, "key", "", "private key (PEM) of -cert")
	fs.Var(&o.advertise, "advertise", "HOST:PORT at which other nodes reach -listen, put into pairing codes (may be given more than once)")
	fs.StringVar(&o.transparent, "transparent", "", "listen address for connections that an nftables or iptables redirect rule of the host sends to this node, to be routed by transparent rules (empty: off)")
	fs.StringVar(&o.udpListen, "udp-listen", "", "listen address (UDP) for feedback from peers that are told to send it here (empty: off)")
	spares := fs.Int("spares", defaultSpares, "spare connections: how many this node asks a peer that it has no path to for, and the most it keeps to a peer that asks (0 to "+strconv.Itoa(control.MaxSpares)+", and 0 for none). A session that the peer opens the connections of begins on one at once.")
	fs.BoolVar(&o.noUI, "no-ui", false, "switch the web interface off: its pages and the login answer 404, and the local API with its token stays")
	fs.StringVar(&o.uiPasswordFile, "ui-password-file", "", "file with the password of the web interface (default: the environment variable "+uiPasswordEnv+", or else a password made at the first start and logged once)")
	transport := defaultTransport()
	transport.flags(fs)
	fs.DurationVar(&o.stats, "stats", 0, "log one line of the node's totals, and one for each session, this often (0: off)")
	if code, done := parseFlags(fs, args); done {
		return o, code, true
	}
	if *dataDir == "" {
		fmt.Fprintln(stderr, "tcpraid run: -data is empty. Name the data directory.")
		return o, 2, true
	}
	o.dataDir, o.apiAddr, o.name, o.transport, o.spares = *dataDir, *apiAddr, *name, &transport, spares
	if err := o.check(); err != nil {
		fmt.Fprintf(stderr, "tcpraid run: %v\n", err)
		return o, 2, true
	}
	return o, 0, false
}

// stringList is a flag that may be given more than once.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// runOptions is what "tcpraid run" was asked for.
type runOptions struct {
	dataDir, apiAddr, name string
	listen, listenPlain    string
	certFile, keyFile      string
	advertise              stringList
	transparent, udpListen string
	uiPasswordFile         string
	noUI                   bool              // no web interface
	spares                 *int              // spare connections asked for and kept; nil: defaultSpares
	transport              *transportOptions // nil: the defaults
	stats                  time.Duration     // how often the totals are logged; 0: never
}

// check reports what in the options cannot work, before anything is opened.
func (o *runOptions) check() error {
	if (o.certFile == "") != (o.keyFile == "") {
		return errors.New("-cert and -key go together")
	}
	if o.certFile != "" && o.listen == "" {
		return errors.New("-cert is shown at -listen. Set -listen as well.")
	}
	for _, a := range o.advertise {
		if err := pair.CheckAddress(a); err != nil {
			return fmt.Errorf("-advertise: %w", err)
		}
	}
	if o.stats < 0 {
		return errors.New("-stats must not be negative (0: off)")
	}
	if o.spares != nil && (*o.spares < 0 || *o.spares > control.MaxSpares) {
		return fmt.Errorf("-spares is %d. Give 0 to %d.", *o.spares, control.MaxSpares)
	}
	if o.noUI && o.uiPasswordFile != "" {
		return errors.New("-no-ui and -ui-password-file do not go together, because a web interface that is switched off has no password")
	}
	if o.transparent != "" && !engine.TransparentSupported {
		return errors.New("-transparent is only available on Linux, because it needs the host to redirect connections to the node")
	}
	if o.transport != nil {
		return o.transport.check()
	}
	return nil
}

// engineConfig returns the settings of the node's transport: the defaults, with what
// the options change.
func (o *runOptions) engineConfig() engine.Config {
	cfg := engine.DefaultConfig()
	cfg.Version = version
	cfg.UDPListen = o.udpListen
	cfg.Stats = o.stats
	if o.transport != nil {
		o.transport.apply(&cfg)
	}
	return cfg
}

// runNode runs a node with the local API only; see runNodeWith.
func runNode(ctx context.Context, dataDir, apiAddr, name string) error {
	return runNodeWith(ctx, runOptions{dataDir: dataDir, apiAddr: apiAddr, name: name})
}

// runNodeWith opens the data directory, serves the local API and the listeners for other
// nodes, and returns nil once ctx is done and everything has shut down. It holds the
// lock on the data directory for as long as it runs. An empty name leaves the node's
// name as it is. The node pairs with other nodes, runs its forwarding rules and takes
// the sub-connections of its peers.
func runNodeWith(ctx context.Context, o runOptions) error {
	n, err := node.Open(o.dataDir, o.name)
	if err != nil {
		return err
	}
	defer n.Close()
	for _, w := range n.Warnings() {
		log.Printf("warning: %s", w)
	}
	var ownCert *tls.Certificate
	if o.certFile != "" {
		c, err := tls.LoadX509KeyPair(o.certFile, o.keyFile)
		if err != nil {
			return fmt.Errorf("-cert / -key: %w", err)
		}
		ownCert = &c
		// what a peer puts into a path of the owncert tier that verifies by pin
		log.Printf("certificate of -cert: key fingerprint %s", node.Fingerprint(c.Leaf))
	}
	cfg := o.engineConfig()
	e := engine.New(cfg)

	dialer := &link.Dialer{Identity: n.Identity}
	rules := &forward.Runtime{Node: n, Engine: e, Dialer: dialer, Version: version, Logf: log.Printf, Spares: defaultSpares}
	if o.spares != nil {
		rules.Spares = *o.spares
	}
	svc := &link.Service{Node: n, Dialer: dialer, Advertise: o.advertise, Logf: log.Printf, PeerRemoved: rules.PeerRemoved}
	lsrv := &link.Server{
		Identity:          n.Identity,
		OwnCert:           ownCert,
		PeerByFingerprint: n.PeerByFingerprint,
		HandlePair:        svc.HandlePair,
		Logf:              log.Printf,
	}
	acc := &link.Acceptor{PeerByFingerprint: n.PeerByFingerprint, HandleSub: rules.HandleSub, HandleReverse: rules.HandleReverse,
		HandleControl: rules.HandleControl, HandleSpare: rules.HandleSpare, LogRefused: lsrv.LogRefused}
	lsrv.HandlePeer, lsrv.HandlePlain = acc.HandlePeer, acc.HandlePlain
	// lines that anyone who reaches the listener can cause go through the limiter
	svc.RefusedLogf = lsrv.LogRefused

	// Every listener is bound before anything is served or recorded, so that a taken
	// address ends the start with nothing half up.
	type listener struct {
		flag  string
		addr  string
		ln    net.Listener
		serve func(context.Context, net.Listener) error
	}
	// Without a web interface no password is made, read or stored: the environment
	// variable is ignored, and a hash that an earlier start stored is left as it is.
	ui := &api.UI{Files: web.Files, Off: o.noUI}
	if !o.noUI {
		if ui.PasswordHash, err = uiPassword(n.Dir, o.uiPasswordFile); err != nil {
			return err
		}
	}
	apiSrv := api.New(api.Config{Version: version, Token: n.Token, Node: svc, Engine: e, Rules: rules, UI: ui})
	listeners := []*listener{{flag: "-api", addr: o.apiAddr, serve: apiSrv.Serve}}
	if o.listen != "" {
		listeners = append(listeners, &listener{flag: "-listen", addr: o.listen, serve: lsrv.ServeTLS})
	}
	if o.listenPlain != "" {
		listeners = append(listeners, &listener{flag: "-listen-plain", addr: o.listenPlain, serve: lsrv.ServePlain})
	}
	if o.transparent != "" {
		listeners = append(listeners, &listener{flag: "-transparent", addr: o.transparent, serve: rules.ServeTransparent})
	}
	closeAll := func() {
		for _, l := range listeners {
			if l.ln != nil {
				l.ln.Close()
			}
		}
	}
	bound := map[string]string{"-listen": "off", "-listen-plain": "off", "-transparent": "off", "-udp-listen": "off"}
	for _, l := range listeners {
		if l.ln, err = net.Listen("tcp", l.addr); err != nil {
			closeAll()
			return fmt.Errorf("%s: %w", l.flag, err)
		}
		bound[l.flag] = l.ln.Addr().String()
	}
	if o.udpListen != "" {
		if err := e.StartUDPListener(); err != nil {
			closeAll()
			return fmt.Errorf("-udp-listen: %w", err)
		}
		bound["-udp-listen"] = o.udpListen
	}
	// recorded where "tcpraid status" finds it: the address actually bound (the port may
	// have been left to the system), in a form that connects from this host
	if err := node.WriteAPIAddr(n.Dir, reachableAddr(listeners[0].ln.Addr())); err != nil {
		closeAll()
		return err
	}
	log.Printf("node %s name=%q fingerprint=%s data=%s api=%s listen=%s listen-plain=%s transparent=%s udp-listen=%s", version, n.Name(), n.Identity.Fingerprint(),
		n.Dir, bound["-api"], bound["-listen"], bound["-listen-plain"], bound["-transparent"], bound["-udp-listen"])
	transport := transportOptions{window: cfg.Window, budget: cfg.Budget, grace: cfg.Grace, hol: cfg.Hol, timeline: cfg.Timeline, maxRSS: cfg.MaxRSS}
	log.Printf("transport %s", &transport)

	// where the web page is, as something to type into a browser: the line above says
	// only "api=", and a person who starts a node for the first time looks for this
	if a, ok := listeners[0].ln.Addr().(*net.TCPAddr); ok && !o.noUI {
		host := a.IP.String()
		if a.IP.IsUnspecified() {
			host = "127.0.0.1" // every address of this machine: its own is the one sure to work
		}
		log.Printf("web interface: open http://%s in a browser on this machine", net.JoinHostPort(host, strconv.Itoa(a.Port)))
	}
	if a, ok := listeners[0].ln.Addr().(*net.TCPAddr); ok && !a.IP.IsLoopback() {
		if o.noUI {
			log.Printf("the API at %s is plain HTTP and reachable from other hosts. Use it on a network you trust, or put a reverse proxy with TLS in front.", a)
		} else {
			log.Printf("the web interface and the API at %s are plain HTTP and reachable from other hosts. Use them on a network you trust, or put a reverse proxy with TLS in front.", a)
		}
	}
	// One listener that fails stops the others; the first error is the node's.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The rules open their own listeners. One that cannot is reported in the rule's
	// status and does not stop the node.
	rules.Start(ctx)
	// Past -maxrss the node stops the way it stops on a signal, and reports why. The
	// same loop logs the totals of -stats; without -maxrss it never ends the node.
	var overLimit atomic.Pointer[error]
	if cfg.MaxRSS > 0 || cfg.Stats > 0 {
		go e.GuardWith(func(format string, a ...interface{}) {
			err := fmt.Errorf(format, a...)
			log.Printf("stopping: %v", err)
			overLimit.Store(&err)
			cancel()
		})
	}
	errs := make(chan error, len(listeners))
	for _, l := range listeners {
		go func() {
			err := l.serve(ctx, l.ln)
			if err != nil {
				err = fmt.Errorf("%s: %w", l.flag, err)
				cancel()
			}
			errs <- err
		}()
	}
	var first error
	for range listeners {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
	}
	// The listeners are closed. The sessions end here: the peers are told, and the
	// application connections are reset, so that no cut-off stream looks complete.
	e.Shutdown()
	// What the listeners handed out may still run and may still change the state file
	// (a pairing stores a peer): it gets a moment to finish before the lock on the data
	// directory goes.
	if !lsrv.Wait(shutdownWait) {
		log.Printf("connections from other nodes were still open after %v, stopping anyway", shutdownWait)
	}
	if first != nil {
		return first
	}
	if err := overLimit.Load(); err != nil {
		return *err
	}
	log.Print("node stopped")
	return nil
}

// reachableAddr returns the address a program on this host connects to in order to reach
// a listener bound to addr. A listener on every address (0.0.0.0, ::) has no address of
// its own to connect to; it is reached over loopback.
func reachableAddr(addr net.Addr) string {
	if tcp, ok := addr.(*net.TCPAddr); ok && (len(tcp.IP) == 0 || tcp.IP.IsUnspecified()) {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(tcp.Port))
	}
	return addr.String()
}

func cmdStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tcpraid status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data", defaultDataDir, "data directory of the node")
	asJSON := fs.Bool("json", false, "print the status as the API returns it (JSON)")
	if code, done := parseFlags(fs, args); done {
		return code
	}
	if *dataDir == "" {
		fmt.Fprintln(stderr, "tcpraid status: -data is empty. Name the data directory.")
		return 2
	}
	st, body, err := queryStatus(*dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "tcpraid status: the node is not running or -data %s is not its data directory (%v)\n", *dataDir, err)
		return 1
	}
	if *asJSON {
		stdout.Write(body)
		return 0
	}
	fmt.Fprintf(stdout, "node         %s\n", st.Node.Name)
	fmt.Fprintf(stdout, "fingerprint  %s\n", st.Node.Fingerprint)
	fmt.Fprintf(stdout, "version      %s (wire protocol %d)\n", st.Version, st.WireVersion)
	fmt.Fprintf(stdout, "uptime       %v\n", time.Duration(st.UptimeSeconds)*time.Second)
	fmt.Fprintf(stdout, "sessions     %d\n", st.Sessions)
	fmt.Fprintf(stdout, "peers        %d\n", st.Peers)
	fmt.Fprintf(stdout, "rules        %d\n", st.Rules)
	return 0
}

// queryStatus asks the node whose data directory is dataDir for its status: the token
// and the API address are read from files the node keeps there. It returns the decoded
// status and the answer as received.
func queryStatus(dataDir string) (*api.Status, []byte, error) {
	token, err := node.ReadToken(dataDir)
	if err != nil {
		return nil, nil, err
	}
	addr, err := node.ReadAPIAddr(dataDir)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+api.PathStatus, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	// no proxy from the environment: the token goes to the node and nowhere else
	tr := &http.Transport{Proxy: nil}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("%s answered %s", addr, resp.Status)
	}
	var st api.Status
	if err := json.Unmarshal(body, &st); err != nil {
		return nil, nil, fmt.Errorf("%s did not answer with a status: %w", addr, err)
	}
	// JSON from something else decodes without error and leaves the fields empty; a
	// node always has a version and a fingerprint
	if st.Version == "" || st.Node.Fingerprint == "" {
		return nil, nil, fmt.Errorf("%s did not answer with a status", addr)
	}
	return &st, body, nil
}
