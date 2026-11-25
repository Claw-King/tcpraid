package engine

// Read-only views of an Engine for callers outside the package. Nothing here changes a
// session or a counter.

import (
	"bytes"
	"sort"
	"time"
)

// SessionInfo describes one live session at the moment it was listed.
type SessionInfo struct {
	ID          [16]byte
	Destination string    // where the application connection goes at the far end
	Client      bool      // this end took the application connection and dials the sub-connections
	Started     time.Time // when this end created the session
	Peer        string    // the other end of the pair the session belongs to (Owner.Peer)
	Rule        string    // what made this end carry the connection (Owner.Rule)
	Test        bool      // the session carries test data and no application connection (Owner.Test)

	TxBytes      int64 // stream bytes sent; a chunk sent again is not counted again
	RxBytes      int64 // stream bytes received in DATA frames, duplicates included
	UnackedBytes int64 // read from the application and not yet reported received by the peer: sent, or still queued
	ResentBytes  int64 // stream bytes queued for sending again (a chunk counts each time)
	Subs         int   // live sub-connections, data and control
	Dialed       int   // of those, the ones this end opened; the other end opened the rest

	// UnsentBytes is the part of UnackedBytes that was never sent yet: read from the
	// application and still waiting for a sub-connection. The rest was sent at least
	// once and is not reported received.
	UnsentBytes int64
	// Up: the other end has answered on a sub-connection at least once. NoReply is how
	// the last sub-connection that it never answered ended (the error of reading its
	// answer); empty when every one was answered.
	Up      bool
	NoReply string
	// Silent is how long ago the last frame from the other end arrived (for a session
	// that never had one: how long ago it was created). GraceLeft is how much longer
	// the session lasts if the silence goes on; it ends with a reset at zero.
	Silent, GraceLeft time.Duration
	// Window is the send window the session was created with, in bytes: the engine's
	// or the one its creator gave it.
	Window int
	// IdleShare is the share of the last seconds in which the session's data
	// sub-connections had room and nothing to send, 0 to 1, without those of a repair
	// path; -1 while the session is too young to say. AppWait: the share is high,
	// nothing is unsent, and this end is the one that sends: the session waits for
	// its application to give it data.
	IdleShare float64
	AppWait   bool
	// Config is the copy of the settings the session was created with.
	Config Config
}

// SessionList returns the live sessions, oldest first.
func (e *Engine) SessionList() []SessionInfo {
	list := []SessionInfo{}
	now := time.Now()
	e.registry.Range(func(k, _ any) bool {
		s := k.(*session)
		s.mu.Lock()
		data, ctrl := s.counts()
		dialed := 0
		for _, x := range s.subs {
			if !x.dead && x.dialed {
				dialed++
			}
		}
		var unsent int64 // as in the timeline line
		for _, q := range s.newQ {
			if c := s.chunks[q]; c != nil {
				unsent += int64(len(c.data))
			}
		}
		silent := max(now.Sub(s.lastPeer), 0)
		list = append(list, SessionInfo{
			ID:           s.id,
			Destination:  s.dstAddr,
			Client:       s.isClient,
			Started:      s.started,
			Peer:         s.owner.Peer,
			Rule:         s.owner.Rule,
			Test:         s.owner.Test,
			TxBytes:      s.st.txB,
			RxBytes:      s.st.rxB,
			UnackedBytes: int64(s.unackedB),
			ResentBytes:  s.st.resentB,
			Subs:         data + ctrl,
			Dialed:       dialed,
			UnsentBytes:  unsent,
			Up:           s.everUp,
			NoReply:      s.noReply,
			Silent:       silent,
			GraceLeft:    max(s.grace()-silent, 0),
			Window:       s.cfg.Window,
			IdleShare:    s.idleShare(),
			AppWait:      s.idle.appWait,
			Config:       s.cfg,
		})
		s.mu.Unlock()
		return true
	})
	sort.Slice(list, func(i, j int) bool {
		if !list[i].Started.Equal(list[j].Started) {
			return list[i].Started.Before(list[j].Started)
		}
		return bytes.Compare(list[i].ID[:], list[j].ID[:]) < 0
	})
	return list
}

// Counter is one value of the engine's counters.
type Counter struct {
	Name  string // the name PublishExpvar publishes it under
	Label string // for an entry of a keyed counter: what the key says (a reason, a cause)
	Key   string // ... and the key; both empty for a plain counter
	Value int64
	Gauge bool // a level that goes up and down, not a running total
}

// Counters returns the current value of everything PublishExpvar publishes, in a fixed
// order. A keyed counter (send_wait_ms, resend_by) gives one entry per key.
func (e *Engine) Counters() []Counter {
	cs := []Counter{
		{Name: "sessions_total", Value: e.mSessions.Load()},
		{Name: "sessions_aborted", Value: e.mAborts.Load()},
		{Name: "sessions_active", Value: e.nSessions.Load(), Gauge: true},
		{Name: "dialfail", Value: e.mDialFail.Load()},
		{Name: "dials_capped", Value: e.mDialCapped.Load()},
		{Name: "subs_up", Value: e.mSubsUp.Load()},
		{Name: "subs_lost", Value: e.mSubsLost.Load()},
		{Name: "kicks_slow", Value: e.mKickSlow.Load()},
		{Name: "kicks_silent", Value: e.mKickSilent.Load()},
		{Name: "kicks_write", Value: e.mKickWrite.Load()},
		{Name: "chunks_tx", Value: e.mChunksTx.Load()},
		{Name: "bytes_tx", Value: e.mBytesTx.Load()},
		{Name: "resend_chunks", Value: e.mResend.Load()},
		{Name: "rx_drop", Value: e.mRxDrop.Load()},
		{Name: "rx_dup", Value: e.mRxDup.Load()},
		{Name: "local_write_stall_ms", Value: e.mStallMs.Load()},
		{Name: "fb_gaps_1s", Value: e.mFbGaps.Load()},
		{Name: "bad_snapshots", Value: e.mBadSnap.Load()},
		{Name: "unacked_mb", Value: e.globalB.Load() >> 20, Gauge: true},
		{Name: "rss_mb", Value: int64(rssMB()), Gauge: true}, // taken out below where it is not read
		{Name: "sojourn_ms_max", Value: e.maxSojourn(), Gauge: true},
		{Name: "udp_in", Value: e.mUDPIn.Load()},
		{Name: "udp_out", Value: e.mUDPOut.Load()},
		{Name: "udp_bad", Value: e.mUDPBad.Load()},
		{Name: "udp_too_big", Value: e.mUDPBig.Load()},
		{Name: "udp_old", Value: e.mUDPOld.Load()},
		{Name: "send_write_ms", Value: e.writeUs.Load() / 1000},
	}
	if !RSSSupported {
		// no figure is better than a zero that reads as one
		kept := cs[:0]
		for _, c := range cs {
			if c.Name != "rss_mb" {
				kept = append(kept, c)
			}
		}
		cs = kept
	}
	for i, n := range waitNames {
		cs = append(cs, Counter{Name: "send_wait_ms", Label: "reason", Key: n, Value: e.waitUs[i].Load() / 1000})
	}
	for i, n := range causeNames {
		cs = append(cs, Counter{Name: "resend_by", Label: "cause", Key: n, Value: e.resendBy[i].Load()})
	}
	return cs
}
