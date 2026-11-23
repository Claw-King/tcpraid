package engine

import (
	"net"
	"testing"
	"time"
)

// te is the engine all tests share.
var te = New(DefaultConfig())

func testSession(t *testing.T) *session {
	s := te.newSession([16]byte{byte(time.Now().UnixNano())}, nil, false, "", keys{})
	t.Cleanup(func() { te.registry.Delete(s); te.nSessions.Add(-1) })
	return s
}

func testSub(idx uint16) *sub {
	c1, c2 := net.Pipe()
	c2.Close()
	return &sub{c: c1, idx: idx, born: time.Now(), lastRecv: time.Now(), replied: true}
}

// PROTOCOL.md, receiver rule 2: a chunk that fills a hole is taken even with the reorder budget
// spent; a new highest chunk beyond the budget is not.
func TestReorderBudgetHoleFill(t *testing.T) {
	s := testSession(t)
	s.recvBudget = 4
	old := te.cfg.Chunk
	te.cfg.Chunk = 2 // the per-session floor of the shared receive budget is 2 chunks
	t.Cleanup(func() { te.cfg.Chunk = old })
	sb := testSub(1)
	s.subs = []*sub{sb}
	big := make([]byte, maxData)
	s.onData(sb, 5, 1, big)             // far ahead: pending = 1 MB (within budget + maxData)
	s.onData(sb, 9, 1, make([]byte, 8)) // beyond the budget: dropped
	if s.pending[9] != nil {
		t.Fatal("chunk beyond the budget was taken")
	}
	s.onData(sb, 2, 1, make([]byte, 8)) // below the highest received: fills a hole, always taken
	if s.pending[2] == nil {
		t.Fatal("hole-filling chunk refused (would deadlock)")
	}
	s.onData(sb, 2, 1, make([]byte, 8)) // duplicate
	if s.pendingB != maxData+8 {
		t.Fatalf("pendingB %d", s.pendingB)
	}
}

func addChunks(s *session, sb *sub, from, to uint64, sentAgo time.Duration) {
	for q := from; q < to; q++ {
		s.chunks[q] = &chunk{data: make([]byte, 100), carrier: sb, sentAt: time.Now().Add(-sentAgo)}
		s.unackedB += 100
		sb.sentB += 100
	}
	s.nextSeq = to
}

func TestSnapshotReleases(t *testing.T) {
	s := testSession(t)
	sb := testSub(1)
	s.subs = []*sub{sb}
	addChunks(s, sb, 0, 10, 10*time.Second)
	s.onSnapshot(sb, &snapshot{seqNo: 5, cum: 3, ranges: []rng{{5, 7}}, fin: -1})
	for _, q := range []uint64{0, 1, 2, 5, 6} {
		if s.chunks[q] != nil {
			t.Fatalf("chunk %d not released", q)
		}
	}
	if s.unackedB != 500 || s.sackB != 200 || s.peerCum != 3 {
		t.Fatalf("unacked %d sacked %d peerCum %d", s.unackedB, s.sackB, s.peerCum)
	}
	// an older copy (lower seqNo) changes nothing
	s.onSnapshot(sb, &snapshot{seqNo: 4, cum: 9, fin: -1})
	if s.peerCum != 3 {
		t.Fatal("older snapshot applied")
	}
	// the cumulative point passes the sacked chunks: their accounting is dropped
	s.onSnapshot(sb, &snapshot{seqNo: 6, cum: 8, fin: -1})
	if s.sackB != 0 || len(s.sackSize) != 0 || s.unackedB != 200 {
		t.Fatalf("sacked %d (%d entries) unacked %d", s.sackB, len(s.sackSize), s.unackedB)
	}
}

// A missing chunk is re-sent only once later chunks have waited -hol beyond the queue
// depth, never back on the sub that was late with it, and not twice in a row.
func TestGapResend(t *testing.T) {
	s := testSession(t)
	a, b := testSub(1), testSub(2)
	s.subs = []*sub{a, b}
	addChunks(s, a, 0, 4, 5*time.Second)
	th := te.cfg.Hol + s.targetNow()
	s.onSnapshot(a, &snapshot{seqNo: 1, cum: 0, ranges: []rng{{1, 4}}, gaps: []gap{{0, 1, uint64((th / 2).Milliseconds())}}, fin: -1})
	if len(s.urgQ) != 0 {
		t.Fatal("re-sent before the threshold")
	}
	s.onSnapshot(a, &snapshot{seqNo: 2, cum: 0, ranges: []rng{{1, 4}}, gaps: []gap{{0, 1, uint64((th + 100*time.Millisecond).Milliseconds())}}, fin: -1})
	if len(s.urgQ) != 1 || s.urgQ[0] != 0 || s.chunks[0].avoid != a {
		t.Fatalf("urgQ %v", s.urgQ)
	}
	q, c, _ := s.takeData(a)
	if c != nil {
		t.Fatalf("the late sub took its own re-send (chunk %d)", q)
	}
	q, c, _ = s.takeData(b)
	if c == nil || q != 0 {
		t.Fatal("re-send not taken by the other sub")
	}
	s.onSnapshot(a, &snapshot{seqNo: 3, cum: 0, ranges: []rng{{1, 4}}, gaps: []gap{{0, 1, uint64((th + 200*time.Millisecond).Milliseconds())}}, fin: -1})
	if len(s.urgQ) != 0 {
		t.Fatal("re-sent again at once")
	}
}

// The virtual clock leaves out time blocked writing to the LAN side.
func TestVirtualClock(t *testing.T) {
	s := testSession(t)
	s.started = time.Now().Add(-10 * time.Second)
	s.blocked = 4 * time.Second
	if v := s.vnow(); v < 5900*time.Millisecond || v > 6100*time.Millisecond {
		t.Fatalf("vnow %v, want about 6 s", v)
	}
	s.writingSet = time.Now().Add(-time.Second)
	if v := s.vnow(); v > 5100*time.Millisecond {
		t.Fatalf("vnow %v counts the write in progress", v)
	}
}

func TestRedialBackoff(t *testing.T) {
	s := testSession(t)
	s.subs = []*sub{testSub(1)} // a fresh session: no flaps map yet
	if w := s.redialWait(0, false); w != 0 {
		t.Fatalf("fresh wait %v", w)
	}
	s.subs = nil
	for i := 0; i < 8; i++ {
		s.noteFlap(0, false, time.Now(), true) // eight subs failing together
	}
	if w := s.redialWait(0, false); w != time.Second {
		t.Fatalf("after one failure of eight subs %v, want 1s", w)
	}
	flap := func() { // a failure a while after the previous one
		s.flapAt[0] = s.flapAt[0].Add(-2 * time.Second)
		s.noteFlap(0, false, time.Now(), true)
	}
	flap()
	flap()
	if w := s.redialWait(0, false); w != 4*time.Second {
		t.Fatalf("after 3 flaps %v, want 4s", w)
	}
	if w := s.redialWait(0, true); w != 0 {
		t.Fatal("control role shares the data backoff")
	}
	for i := 0; i < 20; i++ {
		flap()
	}
	if w := s.redialWait(0, false); w != 8*time.Second {
		t.Fatalf("cap %v", w)
	}
	old := testSub(9)
	old.born = time.Now().Add(-60 * time.Second)
	s.subs = []*sub{old}
	if w := s.redialWait(0, false); w != 8*time.Second {
		t.Fatal("a sub from before the flaps cleared the backoff")
	}
	s.flapAt[0] = time.Now().Add(-7 * time.Second)
	fresh := testSub(10)
	fresh.born = time.Now().Add(-6 * time.Second)
	s.subs = []*sub{old, fresh}
	if w := s.redialWait(0, false); w != 0 {
		t.Fatal("a sub dialed after the flaps and alive for 5 s does not clear the backoff")
	}
	s.subs = nil
	for i := 0; i < 3; i++ {
		flap()
	}
	s.noteFlap(0, false, time.Now().Add(-60*time.Second), false)
	if w := s.redialWait(0, false); w != 4*time.Second {
		t.Fatalf("an old sub dying late reset the backoff (%v)", w)
	}
	s.flapAt[0] = time.Now().Add(-5 * time.Second)
	s.noteFlap(0, false, time.Now().Add(-4*time.Second), false)
	if w := s.redialWait(0, false); w != 0 {
		t.Fatal("a sub dialed after the flaps that lived does not reset the backoff")
	}
}

// Out-of-order data held by all sessions together stays within -budget.
func TestRecvCapShared(t *testing.T) {
	a, b := testSession(t), testSession(t)
	old := te.cfg.Budget
	te.cfg.Budget = 64 << 20
	t.Cleanup(func() { te.cfg.Budget = old; te.recvB.Store(0) })
	a.recvBudget, b.recvBudget = 48<<20, 48<<20
	a.pendingB = 40 << 20
	te.recvB.Store(40 << 20)
	if c := a.recvCap(); c != 48<<20 {
		t.Fatalf("a's own data counted against it: %d", c)
	}
	if c := b.recvCap(); c != 24<<20 {
		t.Fatalf("b offered %d MB, want 24", c>>20)
	}
	te.recvB.Store(70 << 20)
	if c := b.recvCap(); c != 2*te.cfg.Chunk {
		t.Fatalf("b offered %d with the budget spent, want the 2-chunk floor", c)
	}
}

func TestAbortFlagRoundTrip(t *testing.T) {
	sn := &snapshot{seqNo: 3, cum: 1, fin: -1, aborted: true}
	got, err := parseSnapshot(sn.marshal())
	if err != nil || !got.aborted || got.rxDone || got.allDone {
		t.Fatalf("%+v %v", got, err)
	}
}

// A repair sub takes re-sent chunks, and new data only once no ordinary data sub is up.
func TestRepairSub(t *testing.T) {
	s := testSession(t)
	d, r := testSub(1), testSub(2)
	r.path, r.repair = 1, true
	s.subs = []*sub{d, r}
	for q := uint64(0); q < 3; q++ {
		s.chunks[q] = &chunk{data: make([]byte, 100), queued: true}
		s.newQ = append(s.newQ, q)
	}
	if _, c, _ := s.takeData(r); c != nil {
		t.Fatal("repair sub took new data while a data sub is up")
	}
	q, c, _ := s.takeData(d)
	if c == nil || q != 0 {
		t.Fatal("data sub did not take new data")
	}
	s.requeue([]uint64{0}, d, cGap)
	if q, c, _ := s.takeData(r); c == nil || q != 0 {
		t.Fatal("repair sub did not take the re-send")
	}
	d.dead = true
	fresh := testSub(3) // dialed, the server has not answered (a path that accepts but forwards nothing)
	fresh.replied = false
	s.subs = append(s.subs, fresh)
	if q, c, _ := s.takeData(r); c == nil || q != 1 {
		t.Fatal("repair sub did not take new data with every answered data sub down")
	}
}

// A sub the server never answered counts toward the redial backoff however long it hung.
func TestUnansweredSubIsAFlap(t *testing.T) {
	s := testSession(t)
	z := testSub(1)
	z.replied = false
	z.born = time.Now().Add(-20 * time.Second)
	s.subs = []*sub{z}
	s.killSub(z, "silent", kSilent)
	if w := s.redialWait(0, false); w != time.Second {
		t.Fatalf("after an unanswered sub died at 20 s: wait %v, want 1s", w)
	}
}

// A chunk that went missing on one path is re-sent on another path when one is ready.
func TestResendPrefersOtherPath(t *testing.T) {
	s := testSession(t)
	a, b, c := testSub(1), testSub(2), testSub(3)
	c.path = 1
	s.subs = []*sub{a, b, c}
	s.chunks[0] = &chunk{data: make([]byte, 100), carrier: a, queued: false}
	s.requeue([]uint64{0}, a, cGap)
	if _, ch, _ := s.takeData(b); ch != nil {
		t.Fatal("re-send taken on the same path while another path is ready")
	}
	if q, ch, _ := s.takeData(c); ch == nil || q != 0 {
		t.Fatal("re-send not taken on the other path")
	}
	c.dead = true
	s.requeue([]uint64{0}, a, cGap)
	if q, ch, _ := s.takeData(b); ch == nil || q != 0 {
		t.Fatal("with no other path, the same path did not take the re-send")
	}
}

func TestDialBucket(t *testing.T) {
	b := newBucket(20, time.Minute)
	if got := b.take(8); got != 8 {
		t.Fatalf("first 8 dials: %d", got)
	}
	if got := b.take(20); got != 12 {
		t.Fatalf("burst beyond 20 a minute: %d", got)
	}
	b.at = b.at.Add(-6 * time.Second) // 6 s later: 2 more
	if got := b.take(5); got != 2 {
		t.Fatalf("after 6 s: %d, want 2", got)
	}
	b.give(1)
	if got := b.take(5); got != 1 {
		t.Fatalf("returned token: %d", got)
	}
	b.at = b.at.Add(-time.Hour)
	if got := b.take(100); got != 20 {
		t.Fatalf("refill capped at 20: %d", got)
	}
}
