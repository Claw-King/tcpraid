package engine

import (
	"context"
	"net"
	"syscall"
	"time"
)

// DialGroup: subs the client keeps alive on one path in one role.
type DialGroup struct {
	Path uint8
	Name string
	Ctrl bool
	// Repair: re-sent chunks only, new data only while no data sub is up (a clean side path
	// that fills holes fast and takes over when the data paths are down)
	Repair bool
	Want   int
	Dial   func() (net.Conn, error)
}

func markControl(mark int) func(_, _ string, rc syscall.RawConn) error {
	if mark == 0 {
		return nil
	}
	return func(_, _ string, rc syscall.RawConn) error {
		var serr error
		if err := rc.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_MARK, mark)
		}); err != nil {
			return err
		}
		return serr
	}
}

// NewDial returns a function that opens one TCP connection to addr: directly, or through
// the SOCKS5 proxy given as socks5://[user:pass@]host:port (empty = direct). A non-zero
// mark sets SO_MARK on the socket (needs CAP_NET_ADMIN).
func NewDial(addr, proxy string, mark int) (func() (net.Conn, error), error) {
	d := &net.Dialer{Timeout: 10 * time.Second, Control: markControl(mark)}
	px, err := parseSocks(proxy)
	if err != nil {
		return nil, err
	}
	if px != nil {
		return func() (net.Conn, error) { return px.dial(d, addr) }, nil
	}
	return func() (net.Conn, error) { return d.Dial("tcp", addr) }, nil
}

// DialContext opens one TCP connection to addr like the function NewDial returns,
// directly or through the SOCKS5 proxy, and gives up when ctx is done.
func DialContext(ctx context.Context, addr, proxy string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 10 * time.Second}
	px, err := parseSocks(proxy)
	if err != nil {
		return nil, err
	}
	if px != nil {
		return px.dialContext(ctx, d, addr)
	}
	return d.DialContext(ctx, "tcp", addr)
}
