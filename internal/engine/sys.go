package engine

// Operating system helpers that are the same on every system. What differs is in
// sys_linux.go and sys_other.go.

import (
	"net"
	"strconv"
	"time"
)

// resetClose closes a LAN-side connection with RST: a FIN would read as a clean end.
func resetClose(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetLinger(0)
	}
	c.Close()
}

// ArmReset makes every close of the LAN-side connection c a reset from now on, the
// close that the kernel does when the process dies included (it is killed, it runs out
// of memory): a FIN there would read as the clean end of a stream that was cut off.
// Call it as soon as the connection exists. A session that ends cleanly takes it back
// (cleanClose); a half-close (CloseWrite) is not changed by it.
func ArmReset(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetLinger(0)
	}
}

// cleanClose closes a LAN-side connection the ordinary way: what the socket still holds
// is sent, then a FIN. Only for a stream that ended completely in both directions.
func cleanClose(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetLinger(-1)
	}
	c.Close()
}

// OriginalDst returns the address a connection was going to before a redirect rule of
// the host sent it here (SO_ORIGINAL_DST). c must be a TCP connection. Only Linux has
// it (TransparentSupported); elsewhere it returns an error.
func OriginalDst(c net.Conn) (net.IP, int, error) { return origDst(c) }

func keepAlive(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(10 * time.Second)
	}
}

// rssText is the resident memory for the stats line: "12MB", or "n/a" on a system
// where it is not read.
func rssText(mb int) string {
	if !RSSSupported {
		return "n/a"
	}
	return strconv.Itoa(mb) + "MB"
}
