package engine

import (
	"errors"
	"syscall"
)

// wsaeconnaborted is WSAECONNABORTED, which Windows also reports for a connection that
// was cut off under a write or a read.
const wsaeconnaborted = syscall.Errno(10053)

// isReset says whether err is a connection that the other end reset. Windows reports
// its own error numbers for it, and words them in the language of the system.
func isReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.WSAECONNRESET) || errors.Is(err, wsaeconnaborted)
}
