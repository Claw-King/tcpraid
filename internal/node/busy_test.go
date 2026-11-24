package node

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var errBusy = errors.New("the process cannot access the file because it is being used by another process")

// failing returns a stand-in for os.ReadFile that fails n times with errBusy before it
// reads, and a counter of its calls.
func failing(n int) (func(string) ([]byte, error), *int) {
	calls := 0
	return func(path string) ([]byte, error) {
		calls++
		if calls <= n {
			return nil, errBusy
		}
		return os.ReadFile(path)
	}, &calls
}

// A file that cannot be read for a moment is read again: the address and the token of
// a node that is just being started are found by a command that asks at that moment.
func TestReadsTryABusyFileAgain(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAPIAddr(dir, "127.0.0.1:9850"); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("ab", tokenBytes)
	if err := os.WriteFile(filepath.Join(dir, TokenFile), []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	defer func() { readFile = os.ReadFile }()

	var calls *int
	readFile, calls = failing(3)
	if got, err := ReadAPIAddr(dir); err != nil || got != "127.0.0.1:9850" || *calls != 4 {
		t.Errorf("the address after three failures: %q, %v, %d reads", got, err, *calls)
	}
	readFile, calls = failing(3)
	if got, err := ReadToken(dir); err != nil || got != token || *calls != 4 {
		t.Errorf("the token after three failures: %v, %d reads", err, *calls)
	}

	// a file that stays busy: the error comes after about a second, not at once and
	// not much later
	readFile, calls = failing(1 << 30)
	began := time.Now()
	_, err := ReadAPIAddr(dir)
	took := time.Since(began)
	if !errors.Is(err, errBusy) || *calls != busyTries {
		t.Errorf("a file that stays busy: %v after %d reads, want the error after %d", err, *calls, busyTries)
	}
	if took < 500*time.Millisecond || took > 10*time.Second {
		t.Errorf("a file that stays busy was given up after %v, want about a second", took)
	}
}

// A file that is not there is not waited for: that is the answer.
func TestReadsDoNotWaitForAMissingFile(t *testing.T) {
	defer func() { readFile = os.ReadFile }()
	calls := 0
	readFile = func(path string) ([]byte, error) {
		calls++
		return os.ReadFile(path)
	}
	dir := t.TempDir()
	if _, err := ReadAPIAddr(dir); !errors.Is(err, fs.ErrNotExist) || calls != 1 {
		t.Errorf("no address file: %v after %d reads, want not-exist after one", err, calls)
	}
	calls = 0
	if _, err := ReadToken(dir); !errors.Is(err, fs.ErrNotExist) || calls != 1 {
		t.Errorf("no token file: %v after %d reads, want not-exist after one", err, calls)
	}
}

// Putting a file in place is tried again where the system says the target is busy, and
// only there.
func TestRenameTriesABusyTargetAgain(t *testing.T) {
	defer func() { renameFile, busyRename = os.Rename, renameIsBusy }()
	dir := t.TempDir()
	path := filepath.Join(dir, AddrFile)

	calls := 0
	renameFile = func(from, to string) error {
		calls++
		if calls <= 2 {
			return errBusy
		}
		return os.Rename(from, to)
	}
	busyRename = func(err error) bool { return errors.Is(err, errBusy) }
	if err := WriteAPIAddr(dir, "127.0.0.1:1"); err != nil || calls != 3 {
		t.Fatalf("a target that is busy twice: %v after %d renames", err, calls)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "127.0.0.1:1" {
		t.Errorf("the file after the rename that worked: %q, %v", b, err)
	}

	// an error that is not "busy" is reported at once
	calls = 0
	busyRename = func(error) bool { return false }
	if err := WriteAPIAddr(dir, "127.0.0.1:2"); !errors.Is(err, errBusy) || calls != 1 {
		t.Errorf("an error that is not busy: %v after %d renames, want it after one", err, calls)
	}
	// and nothing is left behind next to the file
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Errorf("the directory after a rename that failed holds %d entries, %v", len(entries), err)
	}
	if renameIsBusy(errBusy) && renameIsBusy(fs.ErrPermission) {
		t.Error("renameIsBusy takes every error for a busy target")
	}
}
