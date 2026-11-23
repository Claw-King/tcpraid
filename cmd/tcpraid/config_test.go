package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/internal/engine"
)

func TestConfigFile(t *testing.T) {
	f := t.TempDir() + "/c.yaml"
	os.WriteFile(f, []byte(`mode: client
listen: 127.0.0.1:9801
paths:
  - { name: relay, addr: 127.0.0.1:9821, roles: [data], subs: 6 }
  - { name: direct, addr: 127.0.0.1:9822, roles: [control] }
  - { name: lab, type: socks5, proxy: "socks5://127.0.0.1:1080", addr: 192.0.2.10:9901, roles: [data, control], subs: 2, ctrl: 2 }
  - { name: fix, addr: 127.0.0.1:9823, roles: [repair] }
send: { window: 64MB, target: 250ms }
grace: 120s
`), 0o600)
	old := *cfgFile
	*cfgFile = f
	defer func() { *cfgFile = old }()
	paths, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if *window != 64<<20 || *target != 250*time.Millisecond || *graceT != 120*time.Second || *mode != "client" {
		t.Fatalf("window %d target %v grace %v mode %s", *window, *target, *graceT, *mode)
	}
	gs, err := dialGroups(paths)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, g := range gs {
		got = append(got, fmt.Sprintf("%d/%s/%v/%v/%d", g.Path, g.Name, g.Ctrl, g.Repair, g.Want))
	}
	want := "0/relay/false/false/6 1/direct/true/false/1 2/lab/false/false/2 2/lab/true/false/2 3/fix/false/true/2"
	if strings.Join(got, " ") != want {
		t.Fatalf("groups %v, want %s", got, want)
	}
	os.WriteFile(f, []byte("paths:\n  - { name: x, addr: a:1, roles: [bogus] }\n"), 0o600)
	if _, err := loadConfig(); err == nil {
		t.Fatal("bad role accepted")
	}
	os.WriteFile(f, []byte("unknown_key: 1\n"), 0o600)
	if _, err := loadConfig(); err == nil {
		t.Fatal("unknown key accepted")
	}
	*window, *target, *graceT, *mode = 128<<20, 300*time.Millisecond, 900*time.Second, ""
}

// The flag defaults and the engine's defaults describe the same settings. The declared
// defaults are compared, not the current values, which other tests change.
func TestFlagDefaultsMatchEngine(t *testing.T) {
	d := engine.DefaultConfig()
	want := map[string]string{
		"chunk":     strconv.Itoa(d.Chunk),
		"window":    strconv.Itoa(d.Window),
		"budget":    strconv.Itoa(d.Budget),
		"target":    d.Target.String(),
		"pigtarget": d.PigTarget.String(),
		"coalesce":  d.Coalesce.String(),
		"dead":      d.Dead.String(),
		"grace":     d.Grace.String(),
		"kickratio": strconv.FormatFloat(d.KickRatio, 'g', -1, 64),
		"kickfloor": strconv.Itoa(d.KickFloor),
		"share":     strconv.FormatBool(d.Share),
		"fbsubs":    strconv.Itoa(d.FbSubs),
		"fbevery":   d.FbEvery.String(),
		"hol":       d.Hol.String(),
		"maxsess":   strconv.Itoa(d.MaxSess),
		"maxrss":    strconv.Itoa(d.MaxRSS),
		"timeline":  d.Timeline.String(),
		"stats":     d.Stats.String(),
		"chaos":     d.Chaos.String(),
		"life":      d.Life.String(),
		"udploss":   strconv.FormatFloat(d.UDPLoss, 'g', -1, 64),
		"v":         strconv.FormatBool(d.Verbose),
	}
	for name, w := range want {
		f := flag.Lookup(name)
		if f == nil {
			t.Fatalf("flag -%s is not defined", name)
		}
		if f.DefValue != w {
			t.Errorf("-%s defaults to %s, the engine to %s", name, f.DefValue, w)
		}
	}
}
