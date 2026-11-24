package main

// The connect command: one connection to a destination at a peer, on the standard input
// and output of the command. It is made for programs that run a command to reach a
// server, and it goes through the node on this host, by its API (api/connect.go).

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/Claw-King/tcpraid/internal/api"
)

// Exit statuses of tcpraid connect.
const (
	connectDone    = 0 // the stream from the destination ended completely, and sending did not fail
	connectNoNode  = 1 // the node could not be asked, or did not take the request
	connectUsage   = 2 // the command line is wrong
	connectRefused = 3 // the node took the request, and no connection to the destination began
	connectCut     = 4 // the connection began and was cut off
)

// connectStdin is where the connect command reads what it sends; a test replaces it.
var connectStdin io.Reader = os.Stdin

func cmdConnect(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tcpraid connect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data", defaultDataDir, "data directory of the node")
	peer := fs.String("peer", "", "name of the peer the connection goes through")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: tcpraid connect -peer PEER HOST:PORT [flags]")
		fmt.Fprintln(stderr, "       tcpraid connect -peer PEER HOST PORT [flags]")
		fs.PrintDefaults()
	}
	var rest []string
	for {
		switch err := fs.Parse(args); {
		case errors.Is(err, flag.ErrHelp):
			return 0
		case err != nil:
			return connectUsage
		}
		if fs.NArg() == 0 {
			break
		}
		rest = append(rest, fs.Arg(0))
		args = fs.Args()[1:]
	}
	usage := func(msg string) int {
		fmt.Fprintf(stderr, "tcpraid connect: %s\n", msg)
		fs.Usage()
		return connectUsage
	}
	var host, portText string
	switch len(rest) {
	case 1:
		var err error
		if host, portText, err = net.SplitHostPort(rest[0]); err != nil {
			return usage(fmt.Sprintf("%q is not HOST:PORT. An IPv6 address goes in brackets, or give the host and the port as two arguments.", rest[0]))
		}
	case 2:
		host, portText = rest[0], rest[1]
	default:
		return usage("name the destination, as HOST:PORT or as HOST PORT")
	}
	port, err := strconv.Atoi(portText)
	switch {
	case err != nil || port < 1 || port > 65535:
		return usage(fmt.Sprintf("the port %q is not a number from 1 to 65535", portText))
	case host == "":
		return usage("the destination has no host")
	case *peer == "":
		return usage("-peer is missing. Name the peer the connection goes through. \"tcpraid peers\" lists them.")
	case *dataDir == "":
		return usage("-data is empty. Name the data directory.")
	}
	code, err := connect(*dataDir, api.ConnectRequest{Peer: *peer, Host: host, Port: port}, stdin, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "tcpraid connect: %v\n", err)
	}
	return code
}

// connect asks the node of dataDir for the connection req describes and copies between
// it and in and out until it ends. It returns the exit status and, unless that is
// connectDone, why.
func connect(dataDir string, req api.ConnectRequest, in io.Reader, out io.Writer) (int, error) {
	a, err := openAPI(dataDir, 0)
	if err != nil {
		return connectNoNode, fmt.Errorf("%w (%v) (-data %s)", errNoNode, err, dataDir)
	}
	defer a.close()
	tcp, err := (&net.Dialer{Timeout: 10 * time.Second}).Dial("tcp", a.addr)
	if err != nil {
		return connectNoNode, fmt.Errorf("%w (%v) (-data %s)", errNoNode, err, dataDir)
	}
	defer tcp.Close()
	c := tcp
	if a.tls != nil {
		// the same check as every command: the certificate the node recorded, before
		// anything is sent
		tc := tls.Client(tcp, a.tls)
		tc.SetDeadline(time.Now().Add(10 * time.Second))
		if err := tc.Handshake(); err != nil {
			return connectNoNode, fmt.Errorf("%w (%v) (-data %s)", errNoNode, err, dataDir)
		}
		tc.SetDeadline(time.Time{})
		c = tc
	}
	body, err := json.Marshal(req)
	if err != nil {
		return connectNoNode, err
	}
	hr, err := http.NewRequest(http.MethodPost, a.base+api.PathConnect, bytes.NewReader(body))
	if err != nil {
		return connectNoNode, err
	}
	hr.Header.Set("Authorization", "Bearer "+a.token)
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Connection", "Upgrade")
	hr.Header.Set("Upgrade", api.ConnectUpgrade)
	c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := hr.Write(c); err != nil {
		return connectNoNode, fmt.Errorf("%w (%v) (-data %s)", errNoNode, err, dataDir)
	}
	c.SetWriteDeadline(time.Time{})
	// The node answers when the connection to the destination begins, or when it is
	// clear that none will: it bounds that time itself.
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, hr)
	if err != nil {
		return connectNoNode, fmt.Errorf("%w (%s did not answer the request: %v) (-data %s)", errNoNode, a.addr, err, dataDir)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Upgrade") != api.ConnectUpgrade {
		answer, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		var e struct {
			Error string `json:"error"`
		}
		switch json.Unmarshal(answer, &e); {
		case e.Error == "":
			return connectNoNode, fmt.Errorf("%w (%s answered %s) (-data %s)", errNoNode, a.addr, resp.Status, dataDir)
		case resp.StatusCode == http.StatusNotFound && e.Error == "no such path":
			return connectNoNode, errors.New("the node on this host does not carry connections for this command. It runs a version before 0.9.0. Update the node.")
		case resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusForbidden:
			return connectRefused, errors.New(e.Error)
		}
		return connectNoNode, errors.New(e.Error)
	}
	// From here on a close of this end is a reset, also the one the system does when
	// this process is killed: the node then ends the session as cut off, and the
	// destination does not take what it got for all there was. An end of the input
	// is sent as a half-close, which this does not change.
	if tc, ok := tcp.(*net.TCPConn); ok {
		tc.SetLinger(0)
	}
	sent := make(chan error, 1)
	go func() {
		_, err := io.Copy(c, in)
		if err == nil {
			if cw, ok := c.(interface{ CloseWrite() error }); ok {
				err = cw.CloseWrite()
			}
		}
		sent <- err
	}()
	// read and written by hand, so that it is known which of the two failed
	buf := make([]byte, 64<<10)
	for {
		n, rerr := br.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return connectCut, fmt.Errorf("the standard output could not be written, and the connection was given up: %v", werr)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return connectCut, fmt.Errorf("the connection was cut off: %v", rerr)
		}
	}
	// The destination has ended its side, and everything it sent was written out. The
	// output is closed, so that whoever reads it sees that end. The other direction
	// goes on: a destination may end its side first and still read. The command ends
	// when its input has ended as well and was passed on, and until then a close of
	// this end stays a reset, so that the destination never takes a part of the input
	// for all of it.
	if oc, ok := out.(io.Closer); ok {
		oc.Close()
	}
	if err := <-sent; err != nil {
		return connectCut, fmt.Errorf("the connection was cut off while sending: %v", err)
	}
	// an ordinary close: the stream ended as it should in both directions
	if tc, ok := tcp.(*net.TCPConn); ok {
		tc.SetLinger(-1)
	}
	return connectDone, nil
}
