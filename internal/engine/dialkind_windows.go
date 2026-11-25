package engine

import (
	"errors"
	"syscall"
)

// Windows reports these with its own error numbers, which the names of package syscall
// for the Unix numbers do not match.
const (
	wsaetimedout    = syscall.Errno(10060)
	wsaeconnrefused = syscall.Errno(10061)
	wsaenetunreach  = syscall.Errno(10051)
	wsaehostunreach = syscall.Errno(10065)
)

func isRefused(err error) bool {
	return errors.Is(err, wsaeconnrefused) || errors.Is(err, syscall.ECONNREFUSED)
}

func isUnreachable(err error) bool {
	return errors.Is(err, wsaehostunreach) || errors.Is(err, wsaenetunreach) ||
		errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH)
}

func isTimedOut(err error) bool {
	return errors.Is(err, wsaetimedout) || errors.Is(err, syscall.ETIMEDOUT)
}
