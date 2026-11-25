package engine

import (
	"net"
	"testing"
)

// lingerConn is a LAN-side connection that notes what it is told about its close, and
// whether it was closed.
type lingerConn struct {
	net.Conn // nil: only the methods below are used
	lingers  []int
	closed   bool
}

func (c *lingerConn) SetLinger(sec int) error { c.lingers = append(c.lingers, sec); return nil }
func (c *lingerConn) Close() error            { c.closed = true; return nil }

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

// A node that is stopped resets the LAN-side connection of every session, except of one
// that has said that it is done: the other end may have counted that session as
// confirmed, and a reset would lose what the connection has not sent yet. (Found in
// review with real processes: a node stopped within a second of its last write left
// the destination with a reset and a part of the data, and the command at the other
// node exited 0.)
func TestAStopLeavesASessionThatSaidDoneItsOrdinaryClose(t *testing.T) {
	e := New(DefaultConfig())
	done, alive := &lingerConn{}, &lingerConn{}
	sd := e.newSession([16]byte{1}, nil, false, "", keys{})
	sa := e.newSession([16]byte{2}, nil, false, "", keys{})
	sd.mu.Lock()
	sd.local = done
	sd.rxDone, sd.sendFin, sd.peerCum = true, 3, 3
	if !sd.buildSnapshot().allDone {
		t.Fatal("the session is not done")
	}
	sd.mu.Unlock()
	sa.mu.Lock()
	sa.local = alive
	sa.mu.Unlock()
	e.Shutdown()
	if done.closed || len(done.lingers) != 1 || done.lingers[0] != -1 {
		t.Errorf("the session that said done: closed %v, linger set to %v, want not closed and once -1", done.closed, done.lingers)
	}
	if !alive.closed || len(alive.lingers) != 1 || alive.lingers[0] != 0 {
		t.Errorf("the session that is alive: closed %v, linger set to %v, want a reset", alive.closed, alive.lingers)
	}
}
