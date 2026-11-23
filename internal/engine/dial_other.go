//go:build !linux

package engine

import "syscall"

// markControl: no socket mark on this system. NewDial refuses a mark before it gets
// here, so there is nothing to set.
func markControl(int) func(_, _ string, rc syscall.RawConn) error { return nil }
