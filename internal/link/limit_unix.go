//go:build !windows

package link

import "syscall"

// openFileLimit returns how many files the process may have open.
func openFileLimit() (uint64, bool) {
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err != nil {
		return 0, false
	}
	return uint64(rl.Cur), true
}
