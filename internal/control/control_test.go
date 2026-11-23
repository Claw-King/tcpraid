package control

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"testing"
)

func testKey(t *testing.T, fill byte) []byte {
	t.Helper()
	key, err := Key(bytes.Repeat([]byte{fill}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestKey(t *testing.T) {
	if _, err := Key(make([]byte, 31)); err == nil {
		t.Fatal("a pair secret of the wrong length gave a key")
	}
	a, b := testKey(t, 1), testKey(t, 2)
	if bytes.Equal(a, b) || len(a) != 32 {
		t.Fatalf("keys: %x %x", a, b)
	}
}

// A message survives the trip; every field does.
func TestEncodeRead(t *testing.T) {
	key := testKey(t, 1)
	for _, m := range []Message{
		{Type: TypeHello, Name: "alpha", Version: "0.1.0", Protocols: []int{1}},
		{Type: TypePing, Seq: 7},
		{Type: TypePong, Seq: 7},
		{Type: TypeOpen, Request: 1<<63 + 5, Session: "0a0b"},
		{Type: TypeOpenFail, Request: 9, Reason: "no path"},
	} {
		b, err := Encode(m, key)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Read(bytes.NewReader(b), key)
		if err != nil {
			t.Fatalf("%+v: %v", m, err)
		}
		if got.Type != m.Type || got.Name != m.Name || got.Version != m.Version || len(got.Protocols) != len(m.Protocols) ||
			got.Seq != m.Seq || got.Request != m.Request || got.Session != m.Session || got.Reason != m.Reason {
			t.Fatalf("sent %+v, read %+v", m, got)
		}
	}
}

// frame builds a message around a JSON text, with a right MAC.
func frame(key []byte, body string) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, uint16(len(body)))
	b = append(b, body...)
	return append(b, sum(key, b)...)
}

func TestReadRefuses(t *testing.T) {
	key, other := testKey(t, 1), testKey(t, 2)
	good, err := Encode(Message{Type: TypePing, Seq: 1}, key)
	if err != nil {
		t.Fatal(err)
	}
	// another pair's key
	if _, err := Read(bytes.NewReader(good), other); !errors.Is(err, ErrMAC) {
		t.Fatalf("a message under another key: %v", err)
	}
	// one bit changed, anywhere after the length
	for i := 2; i < len(good); i++ {
		bad := bytes.Clone(good)
		bad[i] ^= 1
		if _, err := Read(bytes.NewReader(bad), key); !errors.Is(err, ErrMAC) {
			t.Fatalf("byte %d changed: %v", i, err)
		}
	}
	// a length above the limit is refused before anything more is read
	long := []byte{0xff, 0xff}
	if _, err := Read(bytes.NewReader(long), key); !errors.Is(err, ErrTooLong) {
		t.Fatalf("a length of 65535: %v", err)
	}
	if _, err := Encode(Message{Type: TypeOpenFail, Reason: strings.Repeat("x", MaxMessage)}, key); !errors.Is(err, ErrTooLong) {
		t.Fatalf("encoding a message that is too long: %v", err)
	}
	// the longest that is allowed passes
	body := `{"type":"x","reason":"` + strings.Repeat("y", MaxMessage-len(`{"type":"x","reason":""}`)) + `"}`
	if len(body) != MaxMessage {
		t.Fatalf("test body is %d bytes", len(body))
	}
	if _, err := Read(bytes.NewReader(frame(key, body)), key); err != nil {
		t.Fatalf("a message of exactly the limit: %v", err)
	}
	// cut off
	if _, err := Read(bytes.NewReader(good[:len(good)-1]), key); err == nil {
		t.Fatal("a message that was cut off was read")
	}
	// a right MAC over something that is not a JSON object
	if _, err := Read(bytes.NewReader(frame(key, "[1,2")), key); err == nil || errors.Is(err, ErrMAC) {
		t.Fatalf("broken JSON: %v", err)
	}
}

// A type and fields that this version does not know are read without an error: the
// caller ignores the type.
func TestUnknownTypeAndFields(t *testing.T) {
	key := testKey(t, 1)
	m, err := Read(bytes.NewReader(frame(key, `{"type":"from-the-future","seq":3,"colour":"blue","nested":{"a":[1]}}`)), key)
	if err != nil {
		t.Fatal(err)
	}
	if m.Type != "from-the-future" || m.Seq != 3 {
		t.Fatalf("read %+v", m)
	}
}

func TestConn(t *testing.T) {
	key := testKey(t, 1)
	x, y := net.Pipe()
	defer x.Close()
	defer y.Close()
	a, b := NewConn(x, key), NewConn(y, key)
	go func() {
		a.Send(Message{Type: TypePing, Seq: 1})
		a.Send(Message{Type: TypeOpen, Request: 2})
	}()
	for _, want := range []string{TypePing, TypeOpen} {
		m, err := b.Receive(Timeout)
		if err != nil || m.Type != want {
			t.Fatalf("received %+v, %v; want type %s", m, err, want)
		}
	}
}
