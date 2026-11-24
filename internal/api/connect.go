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
// and from the empty line after it the connection carries the stream in both
// directions and nothing else. A stream that ended completely in both directions ends
// with an ordinary close; one that was cut short ends with a reset, so that its end is
// never taken for the end of the data.
//
// When no stream begins the node answers 502 (403 where the peer's allow list refused
// the destination) with the reason as {"error": "..."} and closes the connection.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/Claw-King/tcpraid/internal/forward"
	"github.com/Claw-King/tcpraid/internal/node"
)

// ConnectUpgrade names the protocol of a connection after a connect request was
// answered with 101: the stream itself.
const ConnectUpgrade = "tcpraid-connect"

// ConnectRequest asks for a connection to host:port as seen from the peer. Host is an
// IP address or a host name, which the peer looks up.
type ConnectRequest struct {
	Peer string `json:"peer"`
	Host string `json:"host"`
	Port int    `json:"port"`
}

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
	begin := func() error {
		c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		defer c.SetWriteDeadline(time.Time{})
		_, err := c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + ConnectUpgrade + "\r\n\r\n"))
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
