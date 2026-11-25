package api

// POST /api/v1/connect: a program on the host hands the node a connection to carry.
//
// The request is an ordinary one, with the token and a JSON body (ConnectRequest). The
// node checks both, then takes the connection the request came on out of the HTTP
// server's hands and carries it, as the application connection of one session, to the
// destination at the peer. At the peer the destination is held against the allow list
// the peer keeps for this node, like the destination of any rule.
//
// When the stream begins the node answers
//
//	HTTP/1.1 101 Switching Protocols
//	Connection: Upgrade
//	Upgrade: tcpraid-connect
//
// with one more header, Tcpraid-Session, that holds the id of the session (since
// version 0.11.0). From the empty line after it the connection carries the stream in
// both directions and nothing else. A stream that ended completely in both directions ends
// with an ordinary close; one that was cut short ends with a reset, so that its end is
// never taken for the end of the data.
//
// When no stream begins the node answers 502 (403 where the peer's allow list refused
// the destination) with the reason as {"error": "..."} and closes the connection.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/Claw-King/tcpraid/client"
	"github.com/Claw-King/tcpraid/internal/forward"
	"github.com/Claw-King/tcpraid/internal/node"
)

// ConnectUpgrade and ConnectRequest are those of package client.
const ConnectUpgrade = client.ConnectUpgrade

type ConnectRequest = client.ConnectRequest

func (s *Server) connect(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	var req ConnectRequest
	if !readBody(w, r, &req) {
		return
	}
	switch {
	case req.Peer == "":
		writeError(w, http.StatusBadRequest, "the request names no peer")
		return
	case req.Port < 1 || req.Port > 65535:
		writeError(w, http.StatusBadRequest, "the port is not a number from 1 to 65535")
		return
	case net.ParseIP(req.Host) == nil:
		if err := node.CheckHostName(req.Host); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("the host %q is neither an IP address nor a host name: %v", req.Host, err))
			return
		}
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		writeError(w, http.StatusInternalServerError, "this connection cannot be handed over to carry a stream")
		return
	}
	c, brw, err := hj.Hijack()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "this connection could not be handed over to carry a stream")
		return
	}
	c.SetDeadline(time.Time{}) // the HTTP server's deadlines are not the stream's
	begin := func(session string) error {
		c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		defer c.SetWriteDeadline(time.Time{})
		answer := "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + ConnectUpgrade + "\r\n"
		if session != "" {
			// with it the program can ask how the session ended (sessionEnd)
			answer += client.SessionHeader + ": " + session + "\r\n"
		}
		_, err := c.Write([]byte(answer + "\r\n"))
		return err
	}
	err = s.cfg.Rules.Connect(req.Peer, req.Host, req.Port, c, brw.Reader, begin)
	if err == nil {
		return // the stream began, and the transport has closed the connection
	}
	code := http.StatusBadGateway
	var refused *forward.ConnectRefused
	if errors.As(err, &refused) && refused.Allow {
		code = http.StatusForbidden
	}
	body, _ := json.Marshal(map[string]string{"error": err.Error()})
	c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", code, http.StatusText(code), len(body), body)
	c.Close()
}

// sessionEndWait is the longest that one request for the end of a session waits, and
// what it waits without a wait in the request.
const (
	sessionEndWait    = 60 * time.Second
	sessionEndDefault = 30 * time.Second
)

// GET /api/v1/sessions/{id}/end: how the session with the id ended at this node. The
// request waits until the session has ended, for the seconds its query names (wait,
// sessionEndDefault without one, sessionEndWait at most), and answers with ended false
// when it has not by then. A session that this node neither holds nor remembers is
// answered 404: the node keeps the ends of the last sessions for a few minutes.
func (s *Server) sessionEnd(w http.ResponseWriter, r *http.Request, p map[string]string) {
	raw, err := hex.DecodeString(p["id"])
	if err != nil || len(raw) != 16 {
		writeError(w, http.StatusBadRequest, "the id of a session is 32 hexadecimal digits")
		return
	}
	wait := sessionEndDefault
	if text := r.URL.Query().Get("wait"); text != "" {
		n, err := strconv.Atoi(text)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "wait is a number of seconds")
			return
		}
		wait = min(time.Duration(n)*time.Second, sessionEndWait)
	}
	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()
	end, known := s.cfg.Rules.SessionEnd(ctx, [16]byte(raw))
	if !known {
		writeError(w, http.StatusNotFound, "this node holds no session with that id and does not remember the end of one")
		return
	}
	writeJSON(w, http.StatusOK, client.SessionEnd{Ended: end.Ended, Complete: end.Complete, Reason: end.Reason})
}
