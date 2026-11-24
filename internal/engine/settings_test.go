package engine

import (
	"crypto/rand"
	"crypto/sha256"
	"net"
	"testing"
	"time"
)

// sessionsOf returns the sessions of e by the rule of their owner (the carrying end) or
// by the window their other end named in its header (the taking end, in MB).
func sessionsOf(e *Engine) (byRule map[string]*session, list []*session) {
	byRule = map[string]*session{}
	e.registry.Range(func(k, _ any) bool {
		s := k.(*session)
		byRule[s.owner.Rule] = s
		list = append(list, s)
		return true
	})
	return byRule, list
}

// A session runs with the settings it was created with: the ones its creator gave, or
// the engine's. Two sessions of one engine can differ, at the end that carries the
// connection and at the end that takes the sub-connections, and each end tells the
// other the window of that session, not the engine's.
func TestSessionsRunWithTheirOwnSettings(t *testing.T) {
	dst, got := sinkListener(t)
	k := NewKeys([]byte("key material of the pair, 32 b.."))
	exitCfg := DefaultConfig()
	exitCfg.Window, exitCfg.Grace = 8<<20, 2*time.Minute
	exit := New(DefaultConfig())
	addr := subListener(t, exit, SubOptions{Keys: k, Peer: "alpha", Config: &exitCfg})
	entry := New(DefaultConfig())
	route := entry.NewRoute([]DialGroup{{Name: "p", Want: 2, Dial: dialTo(addr)}}, k, "")
	small := DefaultConfig()
	small.Window, small.Target, small.PigTarget, small.Grace = 4<<20, 700*time.Millisecond, 3*time.Second, time.Minute

	done := make(chan struct{}, 2)
	appSmall, farSmall := net.Pipe()
	go func() {
		route.CarryWith(&small, farSmall, dst.IP, dst.Port, Owner{Peer: "beta", Rule: "small"}, nil)
		done <- struct{}{}
	}()
	appPlain, farPlain := net.Pipe()
	go func() {
		route.Carry(farPlain, dst.IP, dst.Port, Owner{Peer: "beta", Rule: "plain"}, nil)
		done <- struct{}{}
	}()
	// the settings given to CarryWith are copied: a change afterwards reaches no session
	deadline := time.Now().Add(10 * time.Second)
	for {
		up := 0
		for _, i := range append(entry.SessionList(), exit.SessionList()...) {
			if i.Up && i.Subs == 2 {
				up++
			}
		}
		if up == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the two sessions did not come up at both ends: %+v / %+v", entry.SessionList(), exit.SessionList())
		}
		time.Sleep(5 * time.Millisecond)
	}
	small.Window, exitCfg.Window = 64<<20, 64<<20

	at, _ := sessionsOf(entry)
	for rule, want := range map[string]struct {
		window int
		target time.Duration
		grace  time.Duration
	}{
		"small": {4 << 20, 700 * time.Millisecond, time.Minute},
		"plain": {entry.cfg.Window, entry.cfg.Target, entry.cfg.Grace},
	} {
		s := at[rule]
		if s == nil {
			t.Fatalf("the entry has no session of %q", rule)
		}
		s.mu.Lock()
		if s.cfg.Window != want.window || s.sendWin != want.window || s.recvBudget != want.window {
			t.Errorf("%s at the entry: window %d, send window %d, reorder budget %d, want %d for each", rule, s.cfg.Window, s.sendWin, s.recvBudget, want.window)
		}
		if g := s.targetNow(); g != want.target {
			t.Errorf("%s at the entry: target %v, want %v", rule, g, want.target)
		}
		if g := s.grace(); g != want.grace {
			t.Errorf("%s at the entry: grace %v, want %v", rule, g, want.grace)
		}
		s.mu.Unlock()
	}
	// the exit was told 128 MB by one session and 4 MB by the other, and holds both at
	// the 8 MB it was given for the sessions of this peer
	_, list := sessionsOf(exit)
	if len(list) != 2 {
		t.Fatalf("the exit has %d sessions, want 2", len(list))
	}
	sawSmall := 0
	for _, s := range list {
		s.mu.Lock()
		if s.cfg.Window != 8<<20 || s.sendWin != 8<<20 || s.recvBudget != 8<<20 || s.grace() != 2*time.Minute {
			t.Errorf("at the exit: window %d, send window %d, reorder budget %d, grace %v, want 8 MB and 2 minutes", s.cfg.Window, s.sendWin, s.recvBudget, s.grace())
		}
		// the window of the entry's small session is below the exit's, so the exit
		// may send that session no more than it; first from the header, then from
		// the snapshots
		if s.peerBudget == 4<<20 {
			sawSmall++
		}
		s.mu.Unlock()
	}
	if sawSmall != 1 {
		t.Errorf("%d sessions at the exit were told a window of 4 MB by the entry, want 1", sawSmall)
	}
	// the entry's plain session was told the exit's 8 MB, in the reply and in snapshots
	plain := at["plain"]
	plain.mu.Lock()
	if plain.peerBudget != 8<<20 {
		t.Errorf("plain at the entry: the exit takes %d bytes out of order, want its window of 8 MB", plain.peerBudget)
	}
	plain.mu.Unlock()

	// both still carry data, more than the smaller window holds
	data := make([]byte, 6<<20)
	rand.Read(data)
	for _, app := range []net.Conn{appSmall, appPlain} {
		go func() { app.Write(data); app.Close() }()
	}
	for range 2 {
		select {
		case b := <-got:
			if sha256.Sum256(b) != sha256.Sum256(data) {
				t.Fatalf("%d bytes arrived, %d sent, or they differ", len(b), len(data))
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the data did not arrive")
		}
	}
	for range 2 {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("a session did not end")
		}
	}
}
