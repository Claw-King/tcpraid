package node

import (
	"errors"
	"syscall"
)

// renameIsBusy says whether err, from a rename, means that another process has the
// target open and the rename may work a moment later: Windows refuses to replace a
// file that is open without the right to delete it, with a sharing violation or with
// "access is denied".
func renameIsBusy(err error) bool {
	return errors.Is(err, errSharingViolation) || errors.Is(err, syscall.ERROR_ACCESS_DENIED)
}
