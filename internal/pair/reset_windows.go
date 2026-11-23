package pair

import (
	"errors"
	"syscall"
)

// wsaeconnaborted is WSAECONNABORTED, which Windows also reports for a connection that
// was cut off under a write or a read.
const wsaeconnaborted = syscall.Errno(10053)

// IsReset says whether err is a connection that the other end reset. Windows reports
// its own error numbers for it, which do not match syscall.ECONNRESET.
func IsReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.WSAECONNRESET) || errors.Is(err, wsaeconnaborted)
}
