//go:build !windows

package pair

import (
	"errors"
	"syscall"
)

// IsReset says whether err is a connection that the other end reset.
func IsReset(err error) bool { return errors.Is(err, syscall.ECONNRESET) }
