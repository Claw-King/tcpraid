package engine

// How much of the last seconds the data sub-connections of a session stood idle: they
// had room and nothing to send. The timeline has the same figure as "starve", for a node
// that logs timelines. Here it is kept for every session, for the session list.
//
// The sub-connections are idle too while the other end takes the data slowly and the
// window is full: nothing is queued for them then either. So the session list is also
// told how much of the time the session waited in its read of the application's
// connection. Idle sub-connections, a session that waits for its application nearly
// all the time, and nothing unsent, mean together that the application gives the
// session no more data for now: the limit of the transfer is the application, not the
// path. A program that asks for a file piece by piece and waits for each answer looks
// like this at the end that sends the file.

import "time"

const (
	// idleWindow is the time the figure looks back over, and idleMin the shortest
	// stretch that says anything.
	idleWindow = 5 * time.Second
	idleMin    = 3 * time.Second
	// idleHigh is the share from which a session counts as waiting for its
	// application, if the other conditions hold (appWait).
	idleHigh = 0.9
)

// idleSample is the state of a session's counters at one tick of its monitor.
type idleSample struct {
	at     time.Time
	idle   time.Duration // summed over the ordinary data subs: time with room and nothing to send
	room   time.Duration // summed over the ticks: length of the tick x ordinary data subs alive
	read   time.Duration // time spent in the read of the application's connection
	tx, rx int64         // stream bytes sent and received
}

// idleState is what a session keeps for the figure (session.mu).
type idleState struct {
	acc  time.Duration // idle so far; the writers of the ordinary data subs add to it
	room time.Duration
	// read is the time pumpLocal has spent in reads of the application's connection
	// that returned; readSince is when the read that is under way began (zero: none)
	read      time.Duration
	readSince time.Time
	samples   []idleSample
	share     float64 // of the window; -1 while not known
	appWait   bool
}

// sampleIdle is called by the monitor at every tick with the live data sub-connections
// (lock held). Sub-connections of a repair path are left out: they carry re-sent chunks
// and are idle most of the time whatever the application does.
func (s *session) sampleIdle(data []*sub, now time.Time) {
	st := &s.idle
	ordinary := 0
	for _, x := range data {
		if !x.repair {
			ordinary++
		}
	}
	if n := len(st.samples); n > 0 {
		st.room += now.Sub(st.samples[n-1].at) * time.Duration(ordinary)
	}
	read := st.read
	if !st.readSince.IsZero() {
		read += now.Sub(st.readSince)
	}
	st.samples = append(st.samples, idleSample{at: now, idle: st.acc, room: st.room, read: read, tx: s.st.txB, rx: s.st.rxB})
	// keep the samples of the window, and the one before it as its beginning
	for len(st.samples) > 2 && now.Sub(st.samples[1].at) >= idleWindow {
		st.samples = st.samples[1:]
	}
	first := st.samples[0]
	st.share, st.appWait = -1, false
	if now.Sub(first.at) < idleMin || st.room == first.room {
		return
	}
	st.share = min(max(float64(st.acc-first.idle)/float64(st.room-first.room), 0), 1)
	// Waiting for the application: the sub-connections stood idle nearly all the
	// time, the session spent nearly all the time in its read of the application's
	// connection (so it was not held by a full window), nothing that the application
	// gave is unsent, and this end is the one that sends the data of the session at
	// the moment. An end that sends nothing at all is not waiting to send.
	waited := float64(read-first.read) / float64(now.Sub(first.at))
	sent, got := s.st.txB-first.tx, s.st.rxB-first.rx
	st.appWait = s.everUp && st.share >= idleHigh && waited >= idleHigh && len(s.newQ) == 0 && sent > 0 && sent >= got
}

// idleShare returns the figure for the session list (lock held): -1 before the first
// tick of the monitor that could say.
func (s *session) idleShare() float64 {
	if len(s.idle.samples) == 0 {
		return -1
	}
	return s.idle.share
}
