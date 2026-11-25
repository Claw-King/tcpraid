package main

// The connect command: one connection to a destination at a peer, on the standard input
// and output of the command. It is made for programs that run a command to reach a
// server, and it goes through the node on this host, by its API (api/connect.go).

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"

	"github.com/Claw-King/tcpraid/client"
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
// connectDone, why. The connection is the one of package client (Dial), which keeps
// the rule that a stream that was cut off is never closed like a complete one.
func connect(dataDir string, req api.ConnectRequest, in io.Reader, out io.Writer) (int, error) {
	conn, err := client.New(dataDir).Dial(context.Background(), req.Peer, net.JoinHostPort(req.Host, strconv.Itoa(req.Port)))
	var refused *client.RefusedError
	switch {
	case errors.As(err, &refused):
		return connectRefused, err
	case err != nil:
		return connectNoNode, err
	}
	// Until the input has ended and was passed on, the close of the connection is a
	// reset, also the one the system does when this process is killed.
	defer conn.Close()
	sent := make(chan error, 1)
	go func() {
		_, err := io.Copy(conn, in)
		if err == nil {
			err = conn.CloseWrite()
		}
		sent <- err
	}()
	// read and written by hand, so that it is known which of the two failed
	buf := make([]byte, 64<<10)
	for {
		n, rerr := conn.Read(buf)
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
	// when its input has ended as well and was passed on (CloseWrite), and only then
	// is the close of the connection an ordinary one.
	if oc, ok := out.(io.Closer); ok {
		oc.Close()
	}
	if err := <-sent; err != nil {
		return connectCut, fmt.Errorf("the connection was cut off while sending: %v", err)
	}
	return connectDone, nil
}
