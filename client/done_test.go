package client_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Claw-King/tcpraid/client"
)

// exchange dials through the stand-in, sends a line, ends its side and reads the answer.
func exchange(t *testing.T, dataDir string) *client.Conn {
	t.Helper()
	c := client.New(dataDir)
	c.Timeout = 10 * time.Second
	conn, err := c.Dial(context.Background(), "nas-b", "192.0.2.10:7")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.Write([]byte("hello\n"))
	conn.CloseWrite()
	if answer, err := io.ReadAll(conn); err != nil || string(answer) != "hello\n" {
		t.Fatalf("the answer: %q, %v", answer, err)
	}
	return conn
}

// Done says how the node ended the session of a connection: nil for a stream that
// ended complete, a *CutError with the node's reason for another end, ErrNoWord when
// the node names no session, the node's own answer when it does not remember the
// session, and the context's error when the context ends before the session does.
func TestDone(t *testing.T) {
	session := strings.Repeat("0f", 16)
	for _, c := range []struct {
		what    string
		session string
		status  int
		end     client.SessionEnd
		check   func(error) bool
	}{
		{"a stream that ended complete", session, http.StatusOK, client.SessionEnd{Ended: true, Complete: true}, func(err error) bool { return err == nil }},
		{"a stream that was cut off", session, http.StatusOK, client.SessionEnd{Ended: true, Reason: "peer aborted"}, func(err error) bool {
			var cut *client.CutError
			return errors.As(err, &cut) && cut.Reason == "peer aborted"
		}},
		{"a node that names no session", "", http.StatusOK, client.SessionEnd{}, func(err error) bool { return errors.Is(err, client.ErrNoWord) }},
		{"a node that does not remember the session", session, http.StatusNotFound, client.SessionEnd{}, func(err error) bool {
			var apiErr *client.Error
			return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound && !errors.Is(err, client.ErrNoWord)
		}},
	} {
		dir, done := standInWith(c.session, c.status, c.end)
		if err := exchange(t, dir).Done(context.Background()); !c.check(err) {
			t.Errorf("%s: Done returned %v", c.what, err)
		}
		done()
	}
	// a session that has not ended: Done asks again until the context ends
	dir, done := standInWith(session, http.StatusOK, client.SessionEnd{})
	defer done()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := exchange(t, dir).Done(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a session that does not end: Done returned %v, want the context's error", err)
	}
}
