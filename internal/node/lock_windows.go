package node

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// errSharingViolation is ERROR_SHARING_VIOLATION: another process has the file open.
const errSharingViolation = syscall.Errno(32)

// lockDir takes the exclusive lock on the data directory without waiting for it: it
// opens the lock file for writing and shares it for reading only, so no second node can
// open it the same way while this one holds it (others may still read the file). The
// lock belongs to the returned file and ends when the file is closed or the process
// ends, so a node that crashed leaves no stale lock.
func lockDir(dir string) (*os.File, error) {
	path := filepath.Join(dir, LockFile)
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, syscall.FILE_SHARE_READ, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, errSharingViolation) {
			return nil, fmt.Errorf("the data directory %s is in use by another tcpraid", dir)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return os.NewFile(uintptr(h), path), nil
}
