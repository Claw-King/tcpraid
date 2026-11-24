//go:build !windows

package engine

import (
	"errors"
	"syscall"
)

// isReset says whether err is a connection that the other end reset.
func isReset(err error) bool { return errors.Is(err, syscall.ECONNRESET) }
