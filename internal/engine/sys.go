package engine

// Operating system helpers (Linux).

import (
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

func rssMB() int {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmRSS:") {
			if f := strings.Fields(l); len(f) >= 2 {
				kb, _ := strconv.Atoi(f[1])
				return kb / 1024
			}
		}
	}
	return 0
}

// resetClose closes a LAN-side connection with RST: a FIN would read as a clean end.
func resetClose(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetLinger(0)
	}
	c.Close()
}

// OriginalDst returns the address a connection was going to before a redirect rule of
// the host sent it here (SO_ORIGINAL_DST). c must be a TCP connection.
func OriginalDst(c net.Conn) (net.IP, int, error) { return origDst(c) }

func origDst(c net.Conn) (net.IP, int, error) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return nil, 0, errors.New("not tcp")
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return nil, 0, err
	}
	var ip net.IP
	var port int
	var serr error
	rc.Control(func(fd uintptr) {
		// IPv4: SO_ORIGINAL_DST returns a sockaddr_in (read through the 20-byte Mreq struct)
		if m, e := syscall.GetsockoptIPv6Mreq(int(fd), syscall.IPPROTO_IP, 80); e == nil {
			port = int(m.Multiaddr[2])<<8 | int(m.Multiaddr[3])
			ip = net.IPv4(m.Multiaddr[4], m.Multiaddr[5], m.Multiaddr[6], m.Multiaddr[7])
			return
		}
		// IPv6: IP6T_SO_ORIGINAL_DST (80 at SOL_IPV6) returns a sockaddr_in6 (28 bytes)
		var sa [28]byte
		l := uint32(len(sa))
		if _, _, e := syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, syscall.IPPROTO_IPV6, 80,
			uintptr(unsafe.Pointer(&sa[0])), uintptr(unsafe.Pointer(&l)), 0); e != 0 {
			serr = e
			return
		}
		port = int(sa[2])<<8 | int(sa[3])
		ip = net.IP(append([]byte(nil), sa[8:24]...))
	})
	return ip, port, serr
}

func keepAlive(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(10 * time.Second)
	}
}
