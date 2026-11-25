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
	oc := rt.newOpenConn("", pipe(), endReader{}, func(string) error { return nil })
	res := read(oc)
	oc.Close()
	if err := <-res; err != io.ErrUnexpectedEOF {
		t.Errorf("the entry was over before the client had an answer, and the read returned %v", err)
	}

	// the answer is written: a client that ended its sending side
	oc = rt.newOpenConn("", pipe(), endReader{}, func(string) error { return nil })
	res = read(oc)
	if err := oc.open(); err != nil {
		t.Fatal(err)
	}
	if err := <-res; err != io.EOF {
		t.Errorf("the client had its answer, and the read returned %v, want the end of the data", err)
	}

	// the answer cannot be written: the client is gone
	oc = rt.newOpenConn("", pipe(), endReader{}, func(string) error { return errors.New("broken pipe") })
	res = read(oc)
	if err := oc.open(); err == nil {
		t.Fatal("open returned no error")
	}
	if err := <-res; err != io.ErrUnexpectedEOF {
		t.Errorf("the answer could not be written, and the read returned %v", err)
	}
}

// tlsLike is a client connection as an entry has it under -api-tls, as far as the
// entry looks: it can end one direction, which is a write that can fail, and it runs
// over another connection.
type tlsLike struct {
	net.Conn
	closeWrite error
}

func (c tlsLike) CloseWrite() error { return c.closeWrite }
func (c tlsLike) NetConn() net.Conn { return c.Conn }

// Over TLS the end of the stream toward the client is written, and a reset of the
// client can be reported to that write. The end of the client's data that is read
// after such a write failed is no end of a stream.
func TestAnEndAfterTheClosingRecordFailedIsNoEnd(t *testing.T) {
	rt := &Runtime{}
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	begin := func(string) error { return nil }

	oc := rt.newOpenConn("", tlsLike{a, errors.New("write: connection reset by peer")}, endReader{}, begin)
	if err := oc.CloseWrite(); err == nil {
		t.Fatal("CloseWrite returned no error")
	}
	if _, err := oc.Read(make([]byte, 16)); err != io.ErrUnexpectedEOF {
		t.Errorf("after the closing record could not be written, the read returned %v", err)
	}
	// the record was written: the client's end is the end of its stream
	oc = rt.newOpenConn("", tlsLike{a, nil}, endReader{}, begin)
	if err := oc.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err := oc.Read(make([]byte, 16)); err != io.EOF {
		t.Errorf("after the closing record was written, the read returned %v, want the end of the data", err)
	}
}
