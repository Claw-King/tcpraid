package engine

// UDP feedback channel. One end listens (-udplisten, the end with a
// reachable UDP port), the other sends to it (-udpdial); the listener answers each session at
// the last source address that authenticated, so an end behind carrier NAT works both ways.
//
// Datagram: session tag (8) | nonce (12) | AES-256-GCM(snapshot) (len + 16)
//   tag:   first 8 bytes of HMAC(header key, session id) -- finds the session without a lookup
//          by address (the address may change)
//   key:   HMAC(snapshot key of this direction, session id): one key per session and
//          direction, so the counter nonce below is never reused under one key
//   nonce: 4 zero bytes | 8-byte counter (per session and direction)
// A datagram whose counter is not above the highest accepted one is dropped: a replay (or a
// reordered one; the newest snapshot wins anyway) neither applies nor moves the reply address.
// A PSK is required. Datagrams that fail authentication are rate limited.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"log"
	"math/rand"
	"net"
	"time"
)

const (
	udpTagLen   = 8
	udpNonceLen = 12
	udpMax      = 1400
	udpFresh    = 3 * time.Second // the peer's UDP counts as working this long after a datagram
)

type udpState struct {
	tag      [8]byte
	aeadOut  cipher.AEAD
	aeadIn   cipher.AEAD
	ctr      uint64
	conn     *net.UDPConn // dialer: this session's own socket; listener: nil (s.e.udpLn)
	peer     *net.UDPAddr // where to send (dialer: fixed; listener: last authenticated source)
	lastIn   time.Time
	sentGen  uint64
	lastSend time.Time
	inCtr    uint64        // highest nonce counter accepted from the peer (replay guard)
	nIn      int64         // datagrams accepted for this session
	nOut     int64         // datagrams sent
	gapMax   time.Duration // longest time between accepted datagrams
}

func sessTag(ks keys, id [16]byte) (t [8]byte) {
	m := hmac.New(sha256.New, ks.hdr)
	m.Write([]byte("udp tag"))
	m.Write(id[:])
	copy(t[:], m.Sum(nil))
	return
}

func sessAEAD(dirKey []byte, id [16]byte) cipher.AEAD {
	m := hmac.New(sha256.New, dirKey)
	m.Write(id[:])
	b, _ := aes.NewCipher(m.Sum(nil))
	g, _ := cipher.NewGCM(b)
	return g
}

// udpSetup gives a session its UDP channel when one is configured (lock not held): the
// session sends to dial (host:port) from a socket of its own, or, with an empty dial,
// answers on the engine's listening socket.
func (s *session) udpSetup(ks keys, dial string) {
	if !ks.configured || (s.e.udpLn == nil && dial == "") {
		return
	}
	u := &udpState{tag: sessTag(ks, s.id)}
	out, in := ks.snapC2S, ks.snapS2C
	if !s.isClient {
		out, in = in, out
	}
	u.aeadOut, u.aeadIn = sessAEAD(out, s.id), sessAEAD(in, s.id)
	if dial != "" {
		ra, err := net.ResolveUDPAddr("udp", dial)
		if err != nil {
			log.Printf("[%x] -udpdial: %v", s.id[:3], err)
			return
		}
		c, err := net.DialUDP("udp", nil, ra)
		if err != nil {
			log.Printf("[%x] -udpdial: %v", s.id[:3], err)
			return
		}
		u.conn, u.peer = c, ra
		go s.udpRead(u)
	}
	s.mu.Lock()
	s.udp = u
	s.mu.Unlock()
	s.e.udpByTag.Store(u.tag, s)
	go s.udpSend(u)
}

// udpUp: the peer's UDP datagrams arrive (lock held).
func (s *session) udpUp() bool { return s.udp != nil && time.Since(s.udp.lastIn) < udpFresh }

func (s *session) udpClose() {
	if u := s.udp; u != nil {
		s.e.udpByTag.Delete(u.tag)
		if u.conn != nil {
			u.conn.Close()
		}
	}
}

// udpSend: a snapshot whenever the state changed (at most every -fbevery) and at least
// once a second, while the session lives.
func (s *session) udpSend(u *udpState) {
	tk := time.NewTicker(s.e.cfg.FbEvery)
	defer tk.Stop()
	for range tk.C {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		now := time.Now()
		if (s.changeN == u.sentGen && now.Sub(u.lastSend) < time.Second) || u.peer == nil {
			s.mu.Unlock()
			continue
		}
		u.sentGen = s.changeN
		u.lastSend = now
		body := s.buildSnapshot().marshal()
		s.mu.Unlock()
		if s.e.cfg.UDPLoss > 0 && rand.Float64() < s.e.cfg.UDPLoss {
			continue // test switch: emulate loss
		}
		s.udpWrite(u, body)
	}
}

// udpWrite seals one snapshot and sends it to the peer (lock not held).
func (s *session) udpWrite(u *udpState, body []byte) {
	s.mu.Lock()
	u.ctr++
	var nonce [udpNonceLen]byte
	binary.BigEndian.PutUint64(nonce[4:], u.ctr)
	conn, peer := u.conn, u.peer
	s.mu.Unlock()
	if peer == nil {
		return
	}
	pkt := make([]byte, 0, udpTagLen+udpNonceLen+len(body)+16)
	pkt = append(pkt, u.tag[:]...)
	pkt = append(pkt, nonce[:]...)
	pkt = u.aeadOut.Seal(pkt, nonce[:], body, u.tag[:])
	if len(pkt) > udpMax {
		s.e.mUDPBig.Add(1)
		return // too many ranges for one datagram: the TCP channels carry this one
	}
	var err error
	if conn != nil {
		_, err = conn.Write(pkt)
	} else if s.e.udpLn != nil {
		_, err = s.e.udpLn.WriteToUDP(pkt, peer)
	}
	if err == nil {
		s.e.mUDPOut.Add(1)
		s.mu.Lock()
		u.nOut++
		s.mu.Unlock()
	}
}

// udpRead: datagrams on a dialer's own socket.
func (s *session) udpRead(u *udpState) {
	b := make([]byte, 2048)
	for {
		n, err := u.conn.Read(b)
		if err != nil {
			return
		}
		s.udpIn(u, b[:n], nil)
	}
}

// udpListenLoop: the shared listener socket; datagrams find their session by tag.
func (e *Engine) udpListenLoop() {
	b := make([]byte, 2048)
	for {
		n, from, err := e.udpLn.ReadFromUDP(b)
		if err != nil {
			return
		}
		if n < udpTagLen+udpNonceLen+16 {
			e.udpBad(false)
			continue
		}
		var tag [8]byte
		copy(tag[:], b)
		v, ok := e.udpByTag.Load(tag)
		if !ok {
			e.udpBad(false)
			continue
		}
		s := v.(*session)
		s.mu.Lock()
		u := s.udp
		s.mu.Unlock()
		if u != nil {
			s.udpIn(u, append([]byte(nil), b[:n]...), from)
		}
	}
}

// udpIn authenticates and applies one datagram; from is the source (listener only).
func (s *session) udpIn(u *udpState, pkt []byte, from *net.UDPAddr) {
	if len(pkt) < udpTagLen+udpNonceLen+16 || !hmac.Equal(pkt[:udpTagLen], u.tag[:]) {
		s.e.udpBad(false)
		return
	}
	body, err := u.aeadIn.Open(nil, pkt[udpTagLen:udpTagLen+udpNonceLen], pkt[udpTagLen+udpNonceLen:], u.tag[:])
	if err != nil {
		s.e.udpBad(true)
		return
	}
	sn, err := parseSnapshot(body)
	if err != nil {
		s.e.udpBad(true)
		return
	}
	ctr := binary.BigEndian.Uint64(pkt[udpTagLen+4 : udpTagLen+udpNonceLen])
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if ctr <= u.inCtr {
		// replayed (or overtaken by a newer one): the newest snapshot already arrived, and an
		// old datagram sent from elsewhere must not move where we answer
		s.e.mUDPOld.Add(1)
		return
	}
	u.inCtr = ctr
	s.e.mUDPIn.Add(1)
	now := time.Now()
	if !u.lastIn.IsZero() {
		u.gapMax = max(u.gapMax, now.Sub(u.lastIn))
	}
	u.lastIn = now
	u.nIn++
	if from != nil && (u.peer == nil || !u.peer.IP.Equal(from.IP) || u.peer.Port != from.Port) {
		u.peer = from // a fresh authenticated datagram: answer here (the peer's NAT mapping may have changed)
	}
	s.onSnapshot(nil, sn)
}

// udpBad counts a datagram that failed authentication. Cheap rejects (too short, unknown
// session tag) cost a map lookup and are only counted: slowing the reader for them would
// let junk crowd out the genuine datagrams in the socket buffer (lab: a 2500/s flood
// let 56 of 668 genuine ones through). Failed decryptions (a known tag: an attacker on the
// path) cost an AEAD open; past 20000 a second the reader pauses briefly.
func (e *Engine) udpBad(costly bool) {
	e.mUDPBad.Add(1)
	if !costly {
		return
	}
	e.udpBadTok.Lock()
	now := time.Now()
	e.udpBadTok.tokens = min(20000, e.udpBadTok.tokens+now.Sub(e.udpBadTok.at).Seconds()*20000)
	e.udpBadTok.at = now
	e.udpBadTok.tokens--
	over := e.udpBadTok.tokens < 0
	e.udpBadTok.Unlock()
	if over {
		time.Sleep(time.Millisecond)
	}
}

// StartUDPListener opens the shared UDP feedback socket when Config.UDPListen is set.
// Call it before RunClient or RunServer, so that the socket exists before any session.
func (e *Engine) StartUDPListener() error {
	if e.cfg.UDPListen == "" {
		return nil
	}
	la, err := net.ResolveUDPAddr("udp", e.cfg.UDPListen)
	if err != nil {
		return err
	}
	e.udpLn, err = net.ListenUDP("udp", la)
	if err != nil {
		return err
	}
	go e.udpListenLoop()
	return nil
}
