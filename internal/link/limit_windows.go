package link

// openFileLimit: Windows has no per-process limit of open files of this kind to ask
// for, so the listener keeps its default bound.
func openFileLimit() (uint64, bool) { return 0, false }
