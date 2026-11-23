//go:build !linux

package engine

// Operating system helpers for the systems that are not Linux.

import (
	"errors"
	"net"
)

// What this system lacks: see sys_linux.go. The callers refuse the settings that need
// these before anything starts.
const (
	RSSSupported         = false
	TransparentSupported = false
	MarkSupported        = false
)

// rssMB has no value here: the resident memory is not read on this system.
func rssMB() int { return 0 }

func origDst(net.Conn) (net.IP, int, error) {
	return nil, 0, errors.New("the original destination of a redirected connection can only be read on Linux")
}
