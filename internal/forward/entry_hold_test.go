package forward

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// endReader gives the end of the data at once: what an entry reads from a client whose
// reset was reported to another call, and from a client that ended its sending side.
type endReader struct{}

func (endReader) Read([]byte) (int, error) { return 0, io.EOF }

// An end that an entry reads before its client has an answer is held back until the
// answer was written or the entry is over, and is the end of a stream only if the
// answer was written.
func TestAnEndBeforeTheAnswerWaitsForTheAnswer(t *testing.T) {
	rt := &Runtime{}
	read := func(oc *openConn) chan error {
		res := make(chan error, 1)
		go func() {
			_, err := oc.Read(make([]byte, 16))
			res <- err
		}()
		select {
		case err := <-res:
			t.Fatalf("a read returned %v before the client had its answer", err)
		case <-time.After(300 * time.Millisecond):
		}
		return res
	}
	pipe := func() net.Conn {
		a, b := net.Pipe()
		t.Cleanup(func() { a.Close(); b.Close() })
		return a
	}

	// the entry is over before there was an answer
	oc := rt.newOpenConn(pipe(), endReader{}, func() error { return nil })
	res := read(oc)
	oc.Close()
	if err := <-res; err != io.ErrUnexpectedEOF {
		t.Errorf("the entry was over before the client had an answer, and the read returned %v", err)
	}

	// the answer is written: a client that ended its sending side
	oc = rt.newOpenConn(pipe(), endReader{}, func() error { return nil })
	res = read(oc)
	if err := oc.open(); err != nil {
		t.Fatal(err)
	}
	if err := <-res; err != io.EOF {
		t.Errorf("the client had its answer, and the read returned %v, want the end of the data", err)
	}

	// the answer cannot be written: the client is gone
	oc = rt.newOpenConn(pipe(), endReader{}, func() error { return errors.New("broken pipe") })
	res = read(oc)
	if err := oc.open(); err == nil {
		t.Fatal("open returned no error")
	}
	if err := <-res; err != io.ErrUnexpectedEOF {
		t.Errorf("the answer could not be written, and the read returned %v", err)
	}
}
