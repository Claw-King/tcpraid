package main

// Configuration: an optional YAML file; command-line flags still work
// and describe the common case (one data path, optionally one control path). Values set
// in the file override the flag defaults; flags given explicitly on the command line
// override the file.
//
// Example (client):
//
//	mode: client
//	listen: 0.0.0.0:9801
//	psk_file: /etc/tcpraid/psk
//	paths:
//	  - { name: relay,  type: tcp, addr: 127.0.0.1:9821, roles: [data], subs: 8 }
//	  - { name: direct, type: tcp, addr: 127.0.0.1:9822, roles: [control] }
//	  - { name: lab,    type: socks5, proxy: "socks5://127.0.0.1:1080", addr: 192.0.2.10:9901, roles: [data], subs: 4 }
//	control: { udp: { listen: 0.0.0.0:8445 } }
//	send: { window: 128MB, target: 300ms }
//	grace: 900s

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Claw-King/tcpraid/internal/engine"
)

type pathCfg struct {
	Name  string   `yaml:"name"`
	Type  string   `yaml:"type"`  // tcp (default) or socks5
	Addr  string   `yaml:"addr"`  // the tcpraid server as seen from the end of this path
	Proxy string   `yaml:"proxy"` // socks5://[user:pass@]host:port (type socks5)
	Mark  int      `yaml:"mark"`  // optional SO_MARK on its sockets (needs CAP_NET_ADMIN)
	Roles []string `yaml:"roles"` // data, repair, control
	Subs  int      `yaml:"subs"`  // data subs (default 8) or repair subs (default 2)
	Ctrl  int      `yaml:"ctrl"`  // control subs (default 1 when the path has the control role)
}

type fileCfg struct {
	Mode     string    `yaml:"mode"`
	Listen   string    `yaml:"listen"`
	PSKFile  string    `yaml:"psk_file"`
	Dst      string    `yaml:"dst"`
	Allow    []string  `yaml:"allow"`
	Paths    []pathCfg `yaml:"paths"`
	Metrics  string    `yaml:"metrics"`
	Timeline string    `yaml:"timeline"`
	Grace    string    `yaml:"grace"`
	Dead     string    `yaml:"dead"`
	Control  struct {
		UDP struct {
			Listen string `yaml:"listen"`
			Dial   string `yaml:"dial"`
		} `yaml:"udp"`
		FbSubs int `yaml:"piggyback_subs"`
	} `yaml:"control"`
	Send struct {
		Window    string `yaml:"window"`
		Target    string `yaml:"target"`
		PigTarget string `yaml:"stalled_target"`
		Chunk     string `yaml:"chunk"`
		Hol       string `yaml:"hol"`
		Budget    string `yaml:"budget"`
	} `yaml:"send"`
	Limits struct {
		MaxRSS  int `yaml:"maxrss_mb"`
		MaxSess int `yaml:"maxsess"`
	} `yaml:"limits"`
}

var cfgFile = flag.String("config", "", "YAML configuration file, overridden by flags given on the command line")

func parseSize(s string) (int, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	mul := 1
	for _, u := range []struct {
		s string
		m int
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(s, u.s) {
			mul, s = u.m, strings.TrimSpace(strings.TrimSuffix(s, u.s))
			break
		}
	}
	n, err := strconv.Atoi(s)
	return n * mul, err
}

// loadConfig applies the YAML file to the flag variables (unless set on the command line)
// and returns its paths (nil without a file or without paths).
func loadConfig() ([]pathCfg, error) {
	if *cfgFile == "" {
		return nil, nil
	}
	b, err := os.ReadFile(*cfgFile)
	if err != nil {
		return nil, err
	}
	var c fileCfg
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", *cfgFile, err)
	}
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	str := func(name string, v *string, val string) {
		if val != "" && !set[name] {
			*v = val
		}
	}
	dur := func(name string, v *time.Duration, val string) error {
		if val == "" || set[name] {
			return nil
		}
		d, err := time.ParseDuration(val)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		*v = d
		return nil
	}
	size := func(name string, v *int, val string) error {
		if val == "" || set[name] {
			return nil
		}
		n, err := parseSize(val)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		*v = n
		return nil
	}
	str("mode", mode, c.Mode)
	str("listen", listen, c.Listen)
	str("psk", pskFile, c.PSKFile)
	str("dst", fixedDst, c.Dst)
	str("metrics", metrics, c.Metrics)
	str("udplisten", udpListen, c.Control.UDP.Listen)
	str("udpdial", udpDial, c.Control.UDP.Dial)
	if len(c.Allow) > 0 && !set["allow"] {
		*allow = strings.Join(c.Allow, ",")
	}
	if c.Control.FbSubs > 0 && !set["fbsubs"] {
		*fbSubs = c.Control.FbSubs
	}
	if c.Limits.MaxRSS > 0 && !set["maxrss"] {
		*maxRSS = c.Limits.MaxRSS
	}
	if c.Limits.MaxSess > 0 && !set["maxsess"] {
		*maxSess = c.Limits.MaxSess
	}
	for _, e := range []error{
		dur("timeline", timelineT, c.Timeline), dur("grace", graceT, c.Grace), dur("dead", deadT, c.Dead),
		dur("target", target, c.Send.Target), dur("pigtarget", pigTarget, c.Send.PigTarget), dur("hol", holAge, c.Send.Hol),
		size("window", window, c.Send.Window), size("chunk", chunkSz, c.Send.Chunk), size("budget", budget, c.Send.Budget),
	} {
		if e != nil {
			return nil, e
		}
	}
	for i := range c.Paths {
		p := &c.Paths[i]
		if p.Name == "" {
			p.Name = fmt.Sprintf("path%d", i)
		}
		if p.Type == "" {
			p.Type = "tcp"
		}
		if p.Type != "tcp" && p.Type != "socks5" {
			return nil, fmt.Errorf("path %s: type %q (want tcp or socks5)", p.Name, p.Type)
		}
		if p.Addr == "" {
			return nil, fmt.Errorf("path %s: addr missing", p.Name)
		}
		if p.Type == "socks5" && p.Proxy == "" {
			return nil, fmt.Errorf("path %s: socks5 needs proxy", p.Name)
		}
		if len(p.Roles) == 0 {
			p.Roles = []string{"data"}
		}
		for _, r := range p.Roles {
			switch r {
			case "data":
				if p.Subs == 0 {
					p.Subs = 8
				}
			case "repair":
				if p.Subs == 0 {
					p.Subs = 2
				}
			case "control":
				if p.Ctrl == 0 {
					p.Ctrl = 1
				}
			default:
				return nil, fmt.Errorf("path %s: role %q (want data, repair or control)", p.Name, r)
			}
		}
		has := map[string]bool{}
		for _, r := range p.Roles {
			has[r] = true
		}
		if has["data"] && has["repair"] {
			return nil, fmt.Errorf("path %s: data and repair exclude each other, because a repair path is a separate path", p.Name)
		}
	}
	if len(c.Paths) > 250 {
		return nil, errors.New("too many paths")
	}
	return c.Paths, nil
}

// dialGroups: what the client keeps alive -- from the file's paths, or from the flags
// (path 0: data via -remote [-socks]; path 1: -ctrl control subs via -ctrlremote [-ctrlsocks, -ctrlmark]).
func dialGroups(paths []pathCfg) ([]engine.DialGroup, error) {
	mk := engine.NewDial
	var gs []engine.DialGroup
	if len(paths) == 0 {
		if *remote == "" {
			return nil, errors.New("-remote (or paths in -config) missing")
		}
		f, err := mk(*remote, *socksData, 0)
		if err != nil {
			return nil, fmt.Errorf("-socks: %w", err)
		}
		gs = append(gs, engine.DialGroup{Path: 0, Name: "data", Want: *nsub, Dial: f})
		if *ctrlN > 0 {
			a := *ctrlRem
			if a == "" {
				a = *remote
			}
			f, err := mk(a, *socksCtrl, *ctrlMark)
			if err != nil {
				return nil, fmt.Errorf("-ctrlsocks / -ctrlmark: %w", err)
			}
			gs = append(gs, engine.DialGroup{Path: 1, Name: "control", Ctrl: true, Want: *ctrlN, Dial: f})
		}
		return gs, nil
	}
	for i, p := range paths {
		f, err := mk(p.Addr, p.Proxy, p.Mark)
		if err != nil {
			return nil, fmt.Errorf("path %s: %w", p.Name, err)
		}
		for _, r := range p.Roles {
			if r == "data" || r == "repair" {
				gs = append(gs, engine.DialGroup{Path: uint8(i), Name: p.Name, Repair: r == "repair", Want: p.Subs, Dial: f})
			} else {
				gs = append(gs, engine.DialGroup{Path: uint8(i), Name: p.Name, Ctrl: true, Want: p.Ctrl, Dial: f})
			}
		}
	}
	return gs, nil
}
