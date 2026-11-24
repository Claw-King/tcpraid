//go:build !windows

package node

// renameIsBusy says whether err, from a rename, means that another process has the
// target open and the rename may work a moment later. Only Windows refuses a rename
// for that.
func renameIsBusy(error) bool { return false }
