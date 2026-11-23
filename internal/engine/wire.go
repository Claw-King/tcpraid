package engine

// Wire format (specified in PROTOCOL.md).
//
// Sub-connection header, client -> server, once per TCP sub-connection (69 bytes):
//
//	"TCPR" | version (1) | flags (1) | session id (16) | dst ip (16, v4-mapped) | dst port (2) |
//	path (1) | sub index (2) | window MB (2) | unix time (8) | MAC (16)
//	flags bit 0: new session (only such a sub may create a session on the server)
//	flags bit 1: control sub (carries no DATA)
//	MAC: first 16 bytes of HMAC-SHA256(psk-derived header key, all preceding bytes); all zero
//	     without a PSK. With a PSK the server rejects a bad MAC or a clock skew over 10 minutes.
//
// Server reply on the same sub (8 bytes): "TCPR" | status (1) | reserved (1) | budget MB (2)
//
//	status 0 ok, 1 gone (session ended cleanly), 2 gone (no such session / ended badly),
//	3 refused (session limit), 4 bad header / MAC
//
// Frames after that, both directions, any sub:
//
//	DATA     1 | seq (8) | send time (8) | len (4) | payload      (data subs only)
//	SNAP     2 | len (2) | snapshot | MAC (16)                    (feedback, see snapshot)
//	ABORT    3                                                    (the sender's LAN side failed)
//	DIALFAIL 4                                                    (server: destination unreachable / denied)
//
// Send times are the sender's monotonic clock (ns since process start); only the sender
// compares them (round trips) or maps them onto its own clock (rates), so the two ends
// need no clock agreement.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net"
	"time"
)

const (
	magic      = "TCPR"
	wireVer    = 1
	headerLen  = 69
	replyLen   = 8
	macLen     = 16
	maxData    = 1 << 20 // largest DATA payload accepted (and largest -chunk)
	maxSnap    = 4096    // largest snapshot accepted
	maxRanges  = 64      // received ranges per snapshot (the nearest to the cumulative point)
	maxGaps    = 32      // missing ranges with their ages per snapshot
	hdrSkewMax = 10 * time.Minute

	fData     = 1
	fSnap     = 2
	fAbort    = 3
	fDialFail = 4

	hNew    = 1
	hCtrl   = 2
	hRepair = 4 // a repair sub: re-sent chunks only, new data only while no other data sub is up

	stOK      = 0
	stGoneOK  = 1
	stGoneBad = 2
	stRefused = 3
	stBadHdr  = 4
)

type subHeader struct {
	isNew, ctrl bool
	repair      bool
	id          [16]byte
	dstIP       net.IP
	dstPort     int
	path        uint8
	idx         uint16
	windowMB    uint16
	unix        int64
}

func (h *subHeader) marshal(key []byte) []byte {
	b := make([]byte, headerLen)
	copy(b[0:4], magic)
	b[4] = wireVer
	if h.isNew {
		b[5] |= hNew
	}
	if h.ctrl {
		b[5] |= hCtrl
	}
	if h.repair {
		b[5] |= hRepair
	}
	copy(b[6:22], h.id[:])
	copy(b[22:38], h.dstIP.To16())
	binary.BigEndian.PutUint16(b[38:40], uint16(h.dstPort))
	b[40] = h.path
	binary.BigEndian.PutUint16(b[41:43], h.idx)
	binary.BigEndian.PutUint16(b[43:45], h.windowMB)
	binary.BigEndian.PutUint64(b[45:53], uint64(h.unix))
	if key != nil {
		copy(b[53:], mac(key, b[:53]))
	}
	return b
}

func parseHeader(b []byte, key []byte) (*subHeader, error) {
	if len(b) != headerLen || string(b[0:4]) != magic {
		return nil, errors.New("not a tcpraid header")
	}
	if b[4] != wireVer {
		return nil, errors.New("unsupported version")
	}
	h := &subHeader{isNew: b[5]&hNew != 0, ctrl: b[5]&hCtrl != 0, repair: b[5]&hRepair != 0}
	copy(h.id[:], b[6:22])
	h.dstIP = net.IP(append([]byte(nil), b[22:38]...))
	h.dstPort = int(binary.BigEndian.Uint16(b[38:40]))
	h.path = b[40]
	h.idx = binary.BigEndian.Uint16(b[41:43])
	h.windowMB = binary.BigEndian.Uint16(b[43:45])
	h.unix = int64(binary.BigEndian.Uint64(b[45:53]))
	if key != nil {
		if !hmac.Equal(b[53:], mac(key, b[:53])) {
			return nil, errors.New("bad header MAC")
		}
		if d := time.Since(time.Unix(h.unix, 0)); d > hdrSkewMax || d < -hdrSkewMax {
			return nil, errors.New("header clock skew")
		}
	}
	return h, nil
}

func reply(status byte, budgetMB uint16) []byte {
	b := make([]byte, replyLen)
	copy(b[0:4], magic)
	b[4] = status
	binary.BigEndian.PutUint16(b[6:8], budgetMB)
	return b
}

func mac(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)[:macLen]
}

// keys derived from the PSK: one for headers, one per direction for snapshots
type keys struct {
	hdr        []byte
	snapC2S    []byte // snapshots sent by the client
	snapS2C    []byte // snapshots sent by the server
	configured bool
}

func deriveKeys(psk []byte) keys {
	if len(psk) == 0 {
		return keys{}
	}
	d := func(label string) []byte {
		m := hmac.New(sha256.New, psk)
		m.Write([]byte("tcpraid v1 " + label))
		return m.Sum(nil)
	}
	return keys{hdr: d("header"), snapC2S: d("snapshot c2s"), snapS2C: d("snapshot s2c"), configured: true}
}

// ---------- snapshot ----------

// snapshot: one end's complete state, sent on every feedback channel; the newest
// (highest seqNo) wins at the reader. Fields as receiver of the peer's data (cum,
// ranges, gaps, per-sub counters, budget) and as sender of its own (fin, done).
type snapshot struct {
	seqNo   uint64
	sentNs  uint64 // sender clock
	cum     uint64 // next chunk expected: everything below was delivered in order
	ranges  []rng  // received chunk ranges above cum, ascending, at most maxRanges
	gaps    []gap  // missing ranges between cum and the highest received, with their ages
	subs    []subRx
	budget  uint64 // free bytes in the reorder buffer
	fin     int64  // our own end-of-stream chunk seq, -1 = not yet
	rxDone  bool   // the peer's stream was delivered up to its fin
	allDone bool   // rxDone and everything we sent was delivered at the peer
	aborted bool   // the sender's application connection failed or its process is exiting: reset yours
}

type rng struct{ lo, hi uint64 } // [lo, hi)

type gap struct {
	lo, hi uint64
	ageMs  uint64 // how long chunks after it have waited (virtual clock)
}

type subRx struct {
	idx   uint16
	bytes uint64 // DATA payload bytes received on this sub so far
	echo  uint64 // send time of the latest DATA received on it (the peer's clock)
}

func putUvarint(b []byte, v uint64) []byte { return binary.AppendUvarint(b, v) }

func (s *snapshot) marshal() []byte {
	b := make([]byte, 0, 256)
	b = putUvarint(b, s.seqNo)
	b = binary.BigEndian.AppendUint64(b, s.sentNs)
	b = putUvarint(b, s.cum)
	b = putUvarint(b, uint64(len(s.ranges)))
	prev := s.cum
	for _, r := range s.ranges {
		b = putUvarint(b, r.lo-prev)
		b = putUvarint(b, r.hi-r.lo)
		prev = r.hi
	}
	b = putUvarint(b, uint64(len(s.gaps)))
	prev = s.cum
	for _, g := range s.gaps {
		b = putUvarint(b, g.lo-prev)
		b = putUvarint(b, g.hi-g.lo)
		b = putUvarint(b, g.ageMs)
		prev = g.hi
	}
	b = putUvarint(b, uint64(len(s.subs)))
	for _, x := range s.subs {
		b = putUvarint(b, uint64(x.idx))
		b = putUvarint(b, x.bytes)
		b = binary.BigEndian.AppendUint64(b, x.echo)
	}
	b = putUvarint(b, s.budget)
	b = putUvarint(b, uint64(s.fin+1))
	var fl byte
	if s.rxDone {
		fl |= 1
	}
	if s.allDone {
		fl |= 2
	}
	if s.aborted {
		fl |= 4
	}
	return append(b, fl)
}

var errSnap = errors.New("bad snapshot")

type reader struct {
	b   []byte
	err error
}

func (r *reader) uv() uint64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.err = errSnap
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *reader) u64() uint64 {
	if r.err != nil || len(r.b) < 8 {
		r.err = errSnap
		return 0
	}
	v := binary.BigEndian.Uint64(r.b)
	r.b = r.b[8:]
	return v
}

func parseSnapshot(b []byte) (*snapshot, error) {
	r := &reader{b: b}
	s := &snapshot{}
	s.seqNo = r.uv()
	s.sentNs = r.u64()
	s.cum = r.uv()
	n := r.uv()
	if n > maxRanges {
		return nil, errSnap
	}
	prev := s.cum
	for i := uint64(0); i < n && r.err == nil; i++ {
		lo := prev + r.uv()
		hi := lo + r.uv()
		if hi <= lo {
			return nil, errSnap
		}
		s.ranges = append(s.ranges, rng{lo, hi})
		prev = hi
	}
	n = r.uv()
	if n > maxGaps {
		return nil, errSnap
	}
	prev = s.cum
	for i := uint64(0); i < n && r.err == nil; i++ {
		lo := prev + r.uv()
		hi := lo + r.uv()
		age := r.uv()
		if hi <= lo {
			return nil, errSnap
		}
		s.gaps = append(s.gaps, gap{lo, hi, age})
		prev = hi
	}
	n = r.uv()
	if n > 1024 {
		return nil, errSnap
	}
	for i := uint64(0); i < n && r.err == nil; i++ {
		s.subs = append(s.subs, subRx{idx: uint16(r.uv()), bytes: r.uv(), echo: r.u64()})
	}
	s.budget = r.uv()
	s.fin = int64(r.uv()) - 1
	if r.err != nil || len(r.b) != 1 {
		return nil, errSnap
	}
	s.rxDone = r.b[0]&1 != 0
	s.allDone = r.b[0]&2 != 0
	s.aborted = r.b[0]&4 != 0
	return s, nil
}

func macOK(key, msg, tag []byte) bool { return hmac.Equal(tag, mac(key, msg)) }
