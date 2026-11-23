// tcpraid stripes ONE TCP connection over several TCP sub-connections and puts it back
// together in order at the far end, for long, lossy paths where a single TCP connection
// is slow or erratic.
//
// This command reads the flags and the optional configuration file, checks them and runs
// one transport engine. The design is described in package internal/engine.
//
// That is the manual mode, which carries the traffic. A first argument that names a
// subcommand (run, status, version) selects the node commands in node.go instead.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Claw-King/tcpraid/internal/engine"
)

// version is set at build time: go build -ldflags "-X main.version=0.1.2"
var version = "0.1.2-dev"

var (
	showVer   = flag.Bool("version", false, "print the version and exit")
	checkOnly = flag.Bool("check", false, "validate the flags and -config file (paths, PSK, memory rule), print the result and exit")
	mode      = flag.String("mode", "", "client or server")
	listen    = flag.String("listen", "", "listen address")
	remote    = flag.String("remote", "", "client: server address for data subs")
	nsub      = flag.Int("n", 8, "client: data sub-connections to keep alive")
	ctrlN     = flag.Int("ctrl", 0, "client: control sub-connections to keep alive (snapshots only)")
	ctrlRem   = flag.String("ctrlremote", "", "client: server address the control subs dial (default -remote)")
	udpListen = flag.String("udplisten", "", "listen for UDP feedback here (the end with a reachable UDP port; needs -psk)")
	udpDial   = flag.String("udpdial", "", "send UDP feedback to the peer's -udplisten address (needs -psk)")
	udpLoss   = flag.Float64("udploss", 0, "drop this share of our outgoing UDP feedback (testing)")
	socksData = flag.String("socks", "", "client: reach -remote through this SOCKS5 proxy (socks5://[user:pass@]host:port)")
	socksCtrl = flag.String("ctrlsocks", "", "client: reach the control address through this SOCKS5 proxy")
	ctrlMark  = flag.Int("ctrlmark", 0, "client: SO_MARK on control sub sockets (test convenience; prefer a fixed-target proxy inbound)")
	chunkSz   = flag.Int("chunk", 256*1024, "chunk size")
	window    = flag.Int("window", 128<<20, "send window and reorder budget (bytes, whole MB)")
	target    = flag.Duration("target", 300*time.Millisecond, "data each sub may hold on top of its feedback loop, as time at its rate")
	pigTarget = flag.Duration("pigtarget", time.Second, "... for 10 s after feedback stalled for over 1 s")
	coalesce  = flag.Duration("coalesce", 20*time.Millisecond, "wait this long to fill a chunk after a short LAN read")
	deadT     = flag.Duration("dead", 20*time.Second, "a sub with data in flight and no progress (or a silent control sub, or a blocked write) this long is dead")
	graceT    = flag.Duration("grace", 900*time.Second, "keep a session whose peer is silent this long")
	kickRatio = flag.Float64("kickratio", 5, "a busy sub delivering under the median of its path / this for 10 s is replaced")
	kickFloor = flag.Int("kickfloor", 256*1024, "never replace subs while the median rate is below this (bytes/s)")
	share     = flag.Bool("share", true, "cap each sub at its delivery-rate share of the window")
	fbSubs    = flag.Int("fbsubs", 3, "without a control sub: send each snapshot on this many data subs")
	fbEvery   = flag.Duration("fbevery", 20*time.Millisecond, "snapshot interval while something changes (1 s otherwise)")
	holAge    = flag.Duration("hol", time.Second, "re-send a missing chunk once later ones have waited this long for it (x attempts)")
	timelineT = flag.Duration("timeline", 0, "log one line per session this often (diagnostics; 0 = off)")
	allow     = flag.String("allow", "", "server: allowed destinations, comma-separated CIDR[:port[-port]] (empty = any)")
	pskFile   = flag.String("psk", "", "file with the pre-shared key (both ends): MACs on headers and snapshots")
	metrics   = flag.String("metrics", "", "serve expvar metrics at http://ADDR/debug/vars (empty = off)")
	fixedDst  = flag.String("dst", "", "client: fixed destination (testing)")
	chaos     = flag.Duration("chaos", 0, "client: kill a random data sub this often (testing)")
	verbose   = flag.Bool("v", false, "verbose")
	maxRSS    = flag.Int("maxrss", 0, "exit if resident memory exceeds this many MB (0 = off)")
	maxSess   = flag.Int("maxsess", 64, "refuse new sessions beyond this many")
	statsT    = flag.Duration("stats", 0, "log sessions / memory / per-sub state this often (0 = off)")
	budget    = flag.Int("budget", 256<<20, "max unacked bytes summed over all sessions")
	life      = flag.Duration("life", 0, "exit after this long (test deployments; 0 = never)")
)

// loadPSK reads the pre-shared key file (nil without -psk).
func loadPSK() []byte {
	if *pskFile == "" {
		return nil
	}
	b, err := os.ReadFile(*pskFile)
	if err != nil {
		log.Fatalf("-psk: %v", err)
	}
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) < 16 {
		log.Fatal("-psk: key shorter than 16 bytes")
	}
	return b
}

// engineConfig copies the flag values (after the configuration file was applied) into the
// engine's settings.
func engineConfig() engine.Config {
	return engine.Config{
		Version:   version,
		Listen:    *listen,
		FixedDst:  *fixedDst,
		Allow:     *allow,
		UDPListen: *udpListen,
		UDPDial:   *udpDial,
		UDPLoss:   *udpLoss,
		Chunk:     *chunkSz,
		Window:    *window,
		Budget:    *budget,
		Target:    *target,
		PigTarget: *pigTarget,
		Coalesce:  *coalesce,
		Dead:      *deadT,
		Grace:     *graceT,
		KickRatio: *kickRatio,
		KickFloor: *kickFloor,
		Share:     *share,
		FbSubs:    *fbSubs,
		FbEvery:   *fbEvery,
		Hol:       *holAge,
		Timeline:  *timelineT,
		Stats:     *statsT,
		Verbose:   *verbose,
		Chaos:     *chaos,
		MaxRSS:    *maxRSS,
		MaxSess:   *maxSess,
		Life:      *life,
	}
}

func main() {
	if code, ok := subcommand(os.Args[1:], os.Stdout, os.Stderr); ok {
		os.Exit(code)
	}
	flag.Parse()
	if *showVer {
		fmt.Println(versionLine())
		return
	}
	paths, err := loadConfig()
	if err != nil {
		log.Fatalf("-config: %v", err)
	}
	if err := checkTransport(*chunkSz, *window, *budget, *maxRSS); err != nil {
		log.Fatal(err)
	}
	if (*udpListen != "" || *udpDial != "") && *pskFile == "" {
		log.Fatal("-udplisten / -udpdial need -psk")
	}
	if *checkOnly {
		psk := loadPSK()
		desc := "server"
		if *mode == "client" {
			gs, err := dialGroups(paths)
			if err != nil {
				log.Fatal(err)
			}
			var d []string
			for _, g := range gs {
				role := "data"
				if g.Ctrl {
					role = "control"
				} else if g.Repair {
					role = "repair"
				}
				d = append(d, fmt.Sprintf("%s/%s x%d", g.Name, role, g.Want))
			}
			desc = "client paths [" + strings.Join(d, ", ") + "]"
		}
		fmt.Printf("ok: %s %s, window %d MB, budget %d MB, maxrss %d MB, grace %v, psk %v, udp listen %q dial %q\n",
			version, desc, *window>>20, *budget>>20, *maxRSS, *graceT, len(psk) > 0, *udpListen, *udpDial)
		return
	}
	e := engine.New(engineConfig())
	e.PublishExpvar()
	if err := e.StartUDPListener(); err != nil {
		log.Fatalf("-udplisten: %v", err)
	}
	go e.Guard()
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		v := <-sig
		log.Printf("%v: telling %d peers, exiting", v, e.Sessions())
		e.Fatal("exit on %v", v)
	}()
	if *metrics != "" {
		go func() { log.Print(http.ListenAndServe(*metrics, nil)) }()
	}
	switch *mode {
	case "client":
		e.SetPSK(loadPSK())
		groups, err := dialGroups(paths)
		if err != nil {
			log.Fatal(err)
		}
		log.Fatal(e.RunClient(groups))
	case "server":
		e.SetPSK(loadPSK())
		log.Fatal(e.RunServer())
	default:
		log.Fatal("-mode client|server")
	}
}
