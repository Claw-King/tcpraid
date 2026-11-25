package engine

import (
	"net"
	"testing"
)

// lingerConn is a LAN-side connection that notes what it is told about its close.
type lingerConn struct {
	net.Conn // nil: only SetLinger is used
	lingers  []int
}

func (c *lingerConn) SetLinger(sec int) error { c.lingers = append(c.lingers, sec); return nil }

// An end that says "I am done" in a snapshot has taken back the reset on close of its
// LAN-side connection before the snapshot exists: the other end counts the session as
// confirmed on that word, and a process that died right after it would otherwise make
// the system throw away what the connection had not sent yet. (Found in review with
// real processes: a node killed within a second of its last write left the destination
// with a reset and a part of the data, and the command at the other node exited 0.)
func TestAnEndThatSaysDoneHasTakenTheResetBack(t *testing.T) {
	s := testSession(t)
	local := &lingerConn{}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.local = local
	// not done yet: the other end's stream is delivered, this end's is not all taken
	s.rxDone, s.sendFin, s.peerCum = true, 3, 2
	if sn := s.buildSnapshot(); sn.allDone || len(local.lingers) != 0 {
		t.Fatalf("an end that is not done: allDone %v, linger set to %v", sn.allDone, local.lingers)
	}
	// done
	s.peerCum = 3
	if sn := s.buildSnapshot(); !sn.allDone {
		t.Fatal("the snapshot of an end that is done does not say so")
	}
	if len(local.lingers) != 1 || local.lingers[0] != -1 {
		t.Fatalf("the snapshot says done, and the linger of the LAN-side connection was set to %v, want once to -1", local.lingers)
	}
	// once is enough
	s.buildSnapshot()
	if len(local.lingers) != 1 {
		t.Fatalf("the reset on close was taken back %d times", len(local.lingers))
	}
}
