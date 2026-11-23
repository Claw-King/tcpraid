package engine

import (
	"bytes"
	"io"
	"math/rand"
	"net"
	"reflect"
	"strconv"
	"testing"
	"time"
)

func TestSnapshotRoundTrip(t *testing.T) {
	in := &snapshot{seqNo: 12345, sentNs: 987654321, cum: 1000,
		ranges:  []rng{{1002, 1005}, {1010, 1011}, {5000, 5100}},
		gaps:    []gap{{1000, 1002, 1500}, {1005, 1010, 20}},
		subs:    []subRx{{1, 1 << 30, 42}, {9, 7, 1 << 50}},
		budget:  256 << 20,
		fin:     -1,
		rxDone:  false,
		allDone: false}
	out, err := parseSnapshot(in.marshal())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip:\n in %+v\nout %+v", in, out)
	}
	in.fin, in.rxDone, in.allDone = 77, true, true
	out, err = parseSnapshot(in.marshal())
	if err != nil || out.fin != 77 || !out.rxDone || !out.allDone {
		t.Fatalf("flags: %+v %v", out, err)
	}
}

func TestSnapshotRejectsGarbage(t *testing.T) {
	good := (&snapshot{seqNo: 1, cum: 5, ranges: []rng{{7, 9}}, fin: -1}).marshal()
	for i := 0; i < len(good); i++ {
		if _, err := parseSnapshot(good[:i]); err == nil {
			t.Fatalf("truncated snapshot (%d of %d bytes) accepted", i, len(good))
		}
	}
	if _, err := parseSnapshot(append(append([]byte(nil), good...), 0)); err == nil {
		t.Fatal("trailing byte accepted")
	}
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ { // no panics on random input
		b := make([]byte, r.Intn(64))
		r.Read(b)
		parseSnapshot(b)
	}
}

// The receiver keeps only the ranges nearest its cumulative point; the sender must
// not take anything beyond the last range as missing.
func TestBuildSnapshotTruncates(t *testing.T) {
	s := te.newSession([16]byte{1}, nil, false, "", keys{})
	defer te.registry.Delete(s)
	s.cum = 0
	for q := uint64(1); q < 400; q += 2 { // every other chunk: 199 ranges, 199 gaps
		s.pending[q] = []byte{1}
		s.pendAt[q] = time.Duration(q) * time.Millisecond
	}
	sn := s.buildSnapshot()
	if len(sn.ranges) != maxRanges || len(sn.gaps) > maxGaps {
		t.Fatalf("ranges %d gaps %d", len(sn.ranges), len(sn.gaps))
	}
	if sn.ranges[0].lo != 1 || sn.gaps[0].lo != 0 || sn.gaps[0].hi != 1 {
		t.Fatalf("nearest first: %+v %+v", sn.ranges[0], sn.gaps[0])
	}
	last := sn.ranges[len(sn.ranges)-1].hi
	for _, g := range sn.gaps {
		if g.hi > last {
			t.Fatalf("gap %+v beyond the last reported range %d", g, last)
		}
	}
	if _, err := parseSnapshot(sn.marshal()); err != nil {
		t.Fatal(err)
	}
}

// Gap age = how long chunks above it have waited (the earliest arrival above it).
func TestGapAges(t *testing.T) {
	s := te.newSession([16]byte{2}, nil, false, "", keys{})
	defer te.registry.Delete(s)
	v := s.vnow()
	s.pending[3] = []byte{1}
	s.pendAt[3] = v - 900*time.Millisecond
	s.pending[7] = []byte{1}
	s.pendAt[7] = v - 100*time.Millisecond
	sn := s.buildSnapshot()
	if len(sn.gaps) != 2 || sn.gaps[0].lo != 0 || sn.gaps[0].hi != 3 || sn.gaps[1].lo != 4 || sn.gaps[1].hi != 7 {
		t.Fatalf("gaps %+v", sn.gaps)
	}
	if sn.gaps[0].ageMs < 850 || sn.gaps[1].ageMs < 50 || sn.gaps[1].ageMs > 400 {
		t.Fatalf("ages %+v", sn.gaps)
	}
}

func TestHeaderMAC(t *testing.T) {
	k := deriveKeys([]byte("0123456789abcdef-test-key"))
	h := subHeader{isNew: true, ctrl: true, repair: true, id: [16]byte{9, 8, 7}, dstIP: net.ParseIP("192.0.2.1"), dstPort: 873,
		path: 1, idx: 3, windowMB: 256, unix: time.Now().Unix()}
	b := h.marshal(k.hdr)
	got, err := parseHeader(b, k.hdr)
	if err != nil {
		t.Fatal(err)
	}
	if !got.isNew || !got.ctrl || !got.repair || got.id != h.id || !got.dstIP.Equal(h.dstIP) || got.dstPort != 873 || got.path != 1 || got.idx != 3 || got.windowMB != 256 {
		t.Fatalf("parsed %+v", got)
	}
	bad := append([]byte(nil), b...)
	bad[40] ^= 1
	if _, err := parseHeader(bad, k.hdr); err == nil {
		t.Fatal("tampered header accepted")
	}
	other := deriveKeys([]byte("another-key-0123456789"))
	if _, err := parseHeader(b, other.hdr); err == nil {
		t.Fatal("header with the wrong key accepted")
	}
	h.unix = time.Now().Add(-time.Hour).Unix()
	if _, err := parseHeader(h.marshal(k.hdr), k.hdr); err == nil {
		t.Fatal("stale header accepted")
	}
	// without a PSK the MAC is not checked
	if _, err := parseHeader(h.marshal(nil), nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k.snapC2S, deriveKeys([]byte("0123456789abcdef-test-key")).snapC2S) || bytes.Equal(k.snapC2S, k.snapS2C) {
		t.Fatal("key derivation")
	}
}

// A SOCKS5 proxy in the test: user/password auth, CONNECT to the target, then relay.
func TestSocks5Dial(t *testing.T) {
	target, _ := net.Listen("tcp", "127.0.0.1:0")
	defer target.Close()
	go func() {
		c, err := target.Accept()
		if err == nil {
			c.Write([]byte("hello"))
			c.Close()
		}
	}()
	px, _ := net.Listen("tcp", "127.0.0.1:0")
	defer px.Close()
	go func() {
		c, err := px.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		b := make([]byte, 512)
		io.ReadFull(c, b[:3]) // 5 1 2
		c.Write([]byte{5, 2})
		var l [1]byte
		io.ReadFull(c, b[:2]) // version 1, user length
		io.ReadFull(c, b[:b[1]])
		io.ReadFull(c, l[:]) // password length
		io.ReadFull(c, b[:l[0]])
		c.Write([]byte{1, 0})
		io.ReadFull(c, b[:4]) // 5 1 0 atyp
		io.ReadFull(c, b[:4+2])
		port := int(b[4])<<8 | int(b[5])
		up, err := net.Dial("tcp", net.JoinHostPort(net.IP(b[:4]).String(), strconv.Itoa(port)))
		if err != nil {
			c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
			return
		}
		c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
		io.Copy(c, up)
	}()
	p, err := parseSocks("socks5://alice:secret@" + px.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c, err := p.dial(&net.Dialer{Timeout: 5 * time.Second}, target.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(c)
	if string(got) != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestAllowRules(t *testing.T) {
	rules, err := parseAllow("192.0.2.10/32:9000-9009, 10.0.0.0/8, [2001:db8::/32]:22, fd00::/8")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		ip   string
		port int
		ok   bool
	}{
		{"192.0.2.10", 9005, true}, {"192.0.2.10", 22, false}, {"10.1.2.3", 1, true},
		{"2001:db8::1", 22, true}, {"2001:db8::1", 23, false}, {"fd00::5", 9000, true}, {"2001:db9::1", 22, false},
	}
	for _, c := range cases {
		if got := allowed(rules, net.ParseIP(c.ip), c.port); got != c.ok {
			t.Errorf("%s:%d allowed=%v, want %v", c.ip, c.port, got, c.ok)
		}
	}
	for _, bad := range []string{"[2001:db8::/32", "[2001:db8::/32]x", "10.0.0.0/8:abc"} {
		if _, err := parseAllow(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
