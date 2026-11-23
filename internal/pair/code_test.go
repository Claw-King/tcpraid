package pair

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)

func testFingerprint() [sha256.Size]byte { return sha256.Sum256([]byte("a key")) }

func newCode(t *testing.T, addrs ...string) *Code {
	t.Helper()
	c, err := New(testFingerprint(), addrs, DefaultTTL, testNow)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCodeRoundTrip(t *testing.T) {
	c := newCode(t, "nas.example.org:9851", "192.0.2.7:9851", "[2001:db8::7]:9851")
	text := c.Text()
	if !strings.HasPrefix(text, "tcpraid-pair-1-") {
		t.Fatalf("code %q", text[:20])
	}
	if text != strings.ToLower(text) || strings.ContainsAny(text, "= \n") {
		t.Fatal("the code is not lower case without padding or space")
	}
	got, err := Parse(text, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, c) {
		t.Fatalf("parsed %+v\nwant   %+v", got, c)
	}
	if !got.Expires.Equal(testNow.Add(time.Hour)) {
		t.Fatalf("expires %v, want an hour after %v", got.Expires, testNow)
	}
	if len(got.IDString()) != 8 {
		t.Fatalf("id %q", got.IDString())
	}
	// printing a Code by accident shows its id and nothing else
	if s := fmt.Sprintf("%v %+v %s", c, c, c); strings.Contains(s, text[20:40]) || !strings.Contains(s, c.IDString()) {
		t.Fatalf("a printed Code: %q", s)
	}
	if got.SecretHash() != sha256.Sum256(c.Secret[:]) {
		t.Fatal("the secret hash is not the SHA-256 of the secret")
	}
	// two codes differ in id and secret
	d := newCode(t, "nas.example.org:9851")
	if d.ID == c.ID || d.Secret == c.Secret {
		t.Fatal("two codes share an id or a secret")
	}
}

// A code survives being pasted with white space in it and in upper case.
func TestCodeAsPasted(t *testing.T) {
	c := newCode(t, "nas.example.org:9851")
	text := c.Text()
	mangled := "  " + strings.ToUpper(text[:30]) + " \n\t" + text[30:60] + "\r\n" + text[60:] + "\n"
	got, err := Parse(mangled, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, c) {
		t.Fatal("the pasted code parsed to something else")
	}
}

// One wrong character is caught by the checksum, wherever it is.
func TestCodeOneWrongCharacter(t *testing.T) {
	text := newCode(t, "nas.example.org:9851").Text()
	body := len("tcpraid-pair-1-")
	for i := body; i < len(text); i++ {
		wrong := byte('a')
		if text[i] == 'a' {
			wrong = 'b'
		}
		bad := text[:i] + string(wrong) + text[i+1:]
		_, err := Parse(bad, testNow)
		// The last character of a base32 text can hold bits that are not part of the
		// data; changing only those is the one change that is "malformed" or harmless
		// and not a checksum error.
		if i == len(text)-1 && (err == nil || errors.Is(err, ErrMalformed)) {
			continue
		}
		if !errors.Is(err, ErrChecksum) {
			t.Fatalf("character %d changed: %v, want the checksum error", i, err)
		}
	}
	// a character that is not in the alphabet, a missing one, an extra one
	for name, bad := range map[string]string{
		"not in the alphabet": text[:body+5] + "1" + text[body+6:],
		"one missing":         text[:body+5] + text[body+6:],
		"one extra":           text[:body+5] + "a" + text[body+5:],
		"cut short":           text[:len(text)-10],
	} {
		if _, err := Parse(bad, testNow); !errors.Is(err, ErrChecksum) && !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v, want the checksum or the malformed error", name, err)
		}
	}
}

func TestCodeErrors(t *testing.T) {
	text := newCode(t, "nas.example.org:9851").Text()
	for _, c := range []struct {
		name string
		text string
		now  time.Time
		want error
	}{
		{"expired", text, testNow.Add(time.Hour), ErrExpired},
		{"long expired", text, testNow.Add(48 * time.Hour), ErrExpired},
		{"unknown version", strings.Replace(text, "tcpraid-pair-1-", "tcpraid-pair-2-", 1), testNow, ErrVersion},
		{"unknown prefix", strings.Replace(text, "tcpraid-pair-", "tcpraid-join-", 1), testNow, ErrNotACode},
		{"something else", "hello", testNow, ErrNotACode},
		{"empty", "", testNow, ErrNotACode},
		{"prefix only", "tcpraid-pair-", testNow, ErrMalformed},
		{"no body", "tcpraid-pair-1-", testNow, ErrMalformed},
	} {
		_, err := Parse(c.text, c.now)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
		for _, other := range []error{ErrExpired, ErrVersion, ErrNotACode, ErrMalformed, ErrChecksum} {
			if other != c.want && errors.Is(err, other) {
				t.Errorf("%s: the error also matches %v", c.name, other)
			}
		}
	}
	// just before the expiry the code is still good
	if _, err := Parse(text, testNow.Add(time.Hour-time.Second)); err != nil {
		t.Errorf("one second before the expiry: %v", err)
	}
}

func TestNewCodeRefuses(t *testing.T) {
	fp := testFingerprint()
	for name, addrs := range map[string][]string{
		"no address":       nil,
		"no port":          {"nas.example.org"},
		"port zero":        {"nas.example.org:0"},
		"port not numeric": {"nas.example.org:http"},
		"no host":          {":9851"},
		"space":            {"nas example:9851"},
		"too long":         {strings.Repeat("a", 251) + ":9851"},
		"too many":         {"a:1", "b:1", "c:1", "d:1", "e:1", "f:1", "g:1", "h:1", "i:1"},
	} {
		if _, err := New(fp, addrs, DefaultTTL, testNow); err == nil {
			t.Errorf("%s: a code was made", name)
		}
	}
	for _, ttl := range []time.Duration{0, -time.Minute, MaxTTL + time.Second} {
		if _, err := New(fp, []string{"a:1"}, ttl, testNow); err == nil {
			t.Errorf("ttl %v: a code was made", ttl)
		}
	}
}

// No error of Parse repeats any part of the code: with a dash missing, what stands in
// the version's place is the body, secret and all; an address is part of the code too.
func TestParseErrorsDoNotEchoTheCode(t *testing.T) {
	c := newCode(t, "nas.example.org:9851")
	text := c.Text()
	body := strings.TrimPrefix(text, "tcpraid-pair-1-")
	for name, in := range map[string]string{
		"no dash after the version": "tcpraid-pair-1" + body,
		"another version":           "tcpraid-pair-2-" + body,
		"dash inside the body":      "tcpraid-pair-" + body[:30] + "-" + body[30:],
		"bad character":             text[:40] + "!" + text[41:],
		"cut short":                 text[:len(text)-9],
	} {
		_, err := Parse(in, testNow)
		if err == nil {
			t.Errorf("%s: parsed", name)
			continue
		}
		for i := 0; i+12 <= len(body); i += 6 {
			if strings.Contains(err.Error(), body[i:i+12]) {
				t.Errorf("%s: the error repeats part of the code: %v", name, err)
				break
			}
		}
	}
	// a well-formed code whose address is not one: the address is not repeated
	bad := *c
	bad.Addresses = []string{"no-port.example.org"}
	_, err := Parse(bad.Text(), testNow)
	if !errors.Is(err, ErrMalformed) || strings.Contains(err.Error(), "no-port") {
		t.Fatalf("a code with a bad address: %v", err)
	}
}

// An address that is cut off is a malformed code, not a shorter address.
func TestParseTruncatedAddress(t *testing.T) {
	c := newCode(t, "nas.example.org:9851")
	payload := append([]byte{}, c.ID[:]...)
	payload = append(payload, c.Secret[:]...)
	payload = append(payload, c.Fingerprint[:]...)
	payload = append(payload, 0, 0, 0, 0, 0x7f, 0xff, 0xff, 0xff) // expiry, far away
	payload = append(payload, 1, 20)                              // one address of 20 bytes
	payload = append(payload, "192.0.2.7:9851"...)                // of which 14 follow
	sum := sha256.Sum256(payload)
	text := prefix + version + "-" + strings.ToLower(encoding.EncodeToString(append(payload, sum[:sumLen]...)))
	if _, err := Parse(text, testNow); !errors.Is(err, ErrMalformed) {
		t.Fatalf("got %v, want an error matching ErrMalformed", err)
	}
}

// A Code printed by value shows as little as one printed by pointer, with every verb.
func TestCodeValueDoesNotPrintTheSecret(t *testing.T) {
	c := newCode(t, "nas.example.org:9851")
	secretHex, secretDec := fmt.Sprintf("%x", c.Secret[:]), fmt.Sprint(c.Secret[:4])
	secretDec = strings.Trim(secretDec, "[]")
	for _, s := range []string{
		fmt.Sprintf("%v", *c), fmt.Sprintf("%+v", *c), fmt.Sprintf("%s", *c), fmt.Sprintf("%#v", *c),
		fmt.Sprintf("%v", c), fmt.Sprintf("%#v", c), fmt.Sprint(*c), fmt.Errorf("with %w", errors.New(c.String())).Error(),
	} {
		if strings.Contains(s, secretHex) || strings.Contains(s, secretDec) || !strings.Contains(s, c.IDString()) {
			t.Errorf("a printed Code shows %q", s)
		}
	}
}

// A code is valid for whole seconds, at least one, at most MaxTTL.
func TestNewCodeTTL(t *testing.T) {
	addrs := []string{"nas.example.org:9851"}
	for _, ttl := range []time.Duration{0, -time.Second, 500 * time.Millisecond, 1500 * time.Millisecond, MaxTTL + time.Second} {
		if _, err := New(testFingerprint(), addrs, ttl, testNow); err == nil {
			t.Errorf("a code valid for %v was made", ttl)
		}
	}
	for _, ttl := range []time.Duration{time.Second, MaxTTL} {
		if _, err := New(testFingerprint(), addrs, ttl, testNow); err != nil {
			t.Errorf("a code valid for %v: %v", ttl, err)
		}
	}
}
