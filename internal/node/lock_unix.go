//go:build !windows

package node

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockDir takes the exclusive lock on the data directory without waiting for it. The
// lock belongs to the returned file and ends when the file is closed or the process
// ends, so a node that crashed leaves no stale lock.
func lockDir(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, LockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("the data directory %s is in use by another tcpraid", dir)
		}
		return nil, fmt.Errorf("lock %s: %w. The data directory must be on a file system that supports file locks.", f.Name(), err)
	}
	return f, nil
}
