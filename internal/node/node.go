// Package node holds what makes a tcpraid process a node: its data directory with the
// node's identity, its state file and the access token of its local API.
//
// Files in the data directory:
//
//	lock          locked (flock) by the node that runs on the directory; empty
//	identity.key  Ed25519 private key, PKCS#8, PEM (0600)
//	identity.crt  self-signed X.509 certificate for that key, PEM (0644)
//	state.json    node name, peers and rules (0600)
//	api.token     access token of the local API (0600)
//	api.addr      address the local API listens on, written at each start (0600)
//
// A node whose API is served over TLS keeps three more, described in apicert.go:
// api.key, api.crt and api.tls.
//
// Every file is written to a temporary file first and then put in place in one step, so
// a reader never sees half of one. The key and the token are put in place without
// replacing: creating them fails if they exist. (On a file system without hard links
// they are written under their own name instead; see createFile.)
package node

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// File names in the data directory.
const (
	LockFile  = "lock"
	KeyFile   = "identity.key"
	CertFile  = "identity.crt"
	StateFile = "state.json"
	TokenFile = "api.token"
	AddrFile  = "api.addr"
)

// tokenBytes is the length of the API token before hex encoding.
const tokenBytes = 32

// Node is an opened data directory. It holds the lock on the directory until Close, or
// until the process ends.
//
// The state (name, peers, pairing codes) is read and changed only through the methods in
// store.go: they take the Node's mutex, and every change is on disk before the method
// returns.
type Node struct {
	Dir      string
	Identity *Identity
	Token    string // access token of the local API

	lock *os.File

	mu        sync.Mutex
	state     *State
	statePath string
	// failed proofs per pairing code id since the node started; see RedeemPairingCode
	failures map[string]int
}

// Open locks the data directory dir, prepares it and loads it.
//
// The lock comes first: a directory another tcpraid runs on is an error, and nothing in
// it is read or written. Then what is missing is created -- the directory (0700), the
// identity, the API token, the state file -- and what exists is read and never replaced,
// except that the state file is rewritten when the node's name changes or when the file
// is of an older schema version.
//
// name is the node's name. An empty name keeps the one in the state file; without a
// state file it is the host name with a hyphen and four random characters after it
// (DefaultName), so that two machines with the same host name, such as two containers
// started from the same template, do not become two nodes of the same name.
//
// These are errors, and leave every file as it is: a name that CheckName refuses, whether
// it was given, is stored in the state file or is the host name; a key or token file
// that cannot be read or parsed; a state file that is not valid, has an unknown schema version or
// unknown fields; a certificate without its key or for another key; a state file with no
// identity file next to it (the identity is lost, and a new one would be another node to
// every peer).
func Open(dir, name string) (*Node, error) {
	if dir == "" {
		return nil, errors.New("no data directory is named")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lock, err := lockDir(dir)
	if err != nil {
		return nil, err
	}
	n, err := load(dir, name)
	if err != nil {
		lock.Close()
		return nil, err
	}
	n.lock = lock
	return n, nil
}

// Close releases the lock on the data directory.
func (n *Node) Close() error {
	return n.lock.Close()
}

// load does the work of Open in a locked directory.
func load(dir, name string) (*Node, error) {
	// The state file is read first: one that this program cannot use ends the start
	// before an identity or a token is created next to it.
	statePath := filepath.Join(dir, StateFile)
	st, old, err := loadState(statePath)
	fresh := errors.Is(err, fs.ErrNotExist)
	if err != nil && !fresh {
		return nil, err
	}
	if fresh {
		st = NewState("")
	}
	// where the name comes from, for the message when it is refused
	source := "-name"
	if name == "" {
		name, source = st.Node.Name, "the name stored in "+statePath
	}
	if name == "" {
		host, _ := os.Hostname()
		if host == "" {
			return nil, errors.New("the node has no name and this host has no host name to use. Set -name.")
		}
		name, source = host, "the host name"
		// a host name that cannot be a name is reported as it is, without the suffix
		if CheckName(host) == nil {
			if name, err = DefaultName(host); err != nil {
				return nil, err
			}
			source = "the host name with its suffix"
		}
	}
	if err := CheckName(name); err != nil {
		if source == "-name" {
			return nil, fmt.Errorf("-name %q: %w", name, err)
		}
		return nil, fmt.Errorf("%s, %q, cannot be the node's name: %w. Set -name.", source, name, err)
	}
	// a directory that has a state file has had an identity: none is made for it again
	id, err := loadOrCreateIdentity(dir, fresh)
	if err != nil {
		return nil, err
	}
	token, err := loadOrCreateToken(dir)
	if err != nil {
		return nil, err
	}
	// The state file is written last: a first start that is cut short leaves no state
	// file, and the next start completes it.
	if fresh || old || st.Node.Name != name {
		st.Node.Name = name
		if err := st.Save(statePath); err != nil {
			return nil, err
		}
	}
	return &Node{Dir: dir, Identity: id, Token: token, state: st, statePath: statePath, failures: map[string]int{}}, nil
}

// MaxNameLen is the longest node name, in bytes.
const MaxNameLen = 64

// cutName returns the longest start of name that has at most max bytes and does not end
// inside a character.
func cutName(name string, max int) string {
	for len(name) > max {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name
}

// nameSuffixLen is the number of random characters DefaultName adds.
const nameSuffixLen = 4

// DefaultName returns the name of a new node that was given none: host, a hyphen and
// four random characters out of the lower-case letters and the digits.
func DefaultName(host string) (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, nameSuffixLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		// 36 does not divide 256: the first four characters are a little more likely
		// than the rest, which does not matter for telling two nodes apart
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	// a long host name gives up its end to make room; what is left must not end in
	// a character that only joins two parts of a name
	if room := MaxNameLen - 1 - nameSuffixLen; len(host) > room {
		host = strings.TrimRight(cutName(host, room), "-._")
	}
	return host + "-" + string(b), nil
}

// CheckName reports whether name can be a node's name: 1 to MaxNameLen bytes of valid
// UTF-8, printable characters only, no white space at either end. The name is shown to
// people (in the status, in logs, to peers), so nothing in it may break a line, hide, or
// change how the text around it is displayed: control characters, line and paragraph
// separators, zero-width and directional formatting characters are all refused. The
// only white space allowed is the plain space, inside the name.
func CheckName(name string) error {
	switch {
	case name == "":
		return errors.New("the node name is empty")
	case len(name) > MaxNameLen:
		return fmt.Errorf("the node name is longer than %d bytes", MaxNameLen)
	case !utf8.ValidString(name):
		return errors.New("the node name is not valid UTF-8")
	}
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return errors.New("the node name contains a character that is not printable, such as a control, invisible or formatting character")
		}
	}
	if strings.TrimSpace(name) != name {
		return errors.New("the node name starts or ends with a space")
	}
	return nil
}

// Warnings reports what in the data directory other users of the host can get at: a
// directory that group or others can write to (whoever can write to it can replace the
// key and the token, whatever their own modes), and a key or token file that group or
// others can read. The node works all the same. A directory that others can only read,
// such as the usual 0755 of a volume, is not reported: the files in it are created 0600.
// On Windows nothing is reported: access there is not expressed by these mode bits.
func (n *Node) Warnings() []string {
	var ws []string
	if !modesMeanAccess {
		return ws
	}
	if fi, err := os.Stat(n.Dir); err == nil {
		if mode := fi.Mode().Perm(); mode&0o022 != 0 {
			ws = append(ws, fmt.Sprintf("%s is writable by group or others (mode %04o)", n.Dir, mode))
		}
	}
	for _, name := range []string{KeyFile, TokenFile, APIKeyFile} {
		path := filepath.Join(n.Dir, name)
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		if mode := fi.Mode().Perm(); mode&0o077 != 0 {
			ws = append(ws, fmt.Sprintf("%s is accessible by group or others (mode %04o)", path, mode))
		}
	}
	return ws
}

// loadOrCreateToken reads the API token, or creates it when the file does not exist.
func loadOrCreateToken(dir string) (string, error) {
	token, err := ReadToken(dir)
	if !errors.Is(err, fs.ErrNotExist) {
		return token, err
	}
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token = hex.EncodeToString(b)
	if err := createFile(filepath.Join(dir, TokenFile), []byte(token), 0o600); err != nil {
		return "", err
	}
	return token, nil
}

// ReadToken returns the API token stored in the data directory dir.
func ReadToken(dir string) (string, error) {
	path := filepath.Join(dir, TokenFile)
	b, err := readBusy(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(b))
	if raw, err := hex.DecodeString(token); err != nil || len(raw) != tokenBytes {
		return "", fmt.Errorf("%s: not a token (want %d hex digits). The file is left as it is.", path, 2*tokenBytes)
	}
	return token, nil
}

// WriteAPIAddr records the address the local API listens on, for commands that query
// the node.
func WriteAPIAddr(dir, addr string) error {
	return writeFile(filepath.Join(dir, AddrFile), []byte(addr), 0o600)
}

// ReadAPIAddr returns the address recorded by WriteAPIAddr.
func ReadAPIAddr(dir string) (string, error) {
	path := filepath.Join(dir, AddrFile)
	b, err := readBusy(path)
	if err != nil {
		return "", err
	}
	addr := strings.TrimSpace(string(b))
	if addr == "" {
		return "", fmt.Errorf("%s: empty", path)
	}
	return addr, nil
}

// A file that is busy is tried again busyTries times in all, busyPause apart: about a
// second.
const (
	busyTries = 11
	busyPause = 100 * time.Millisecond
)

// What readBusy and renameBusy call; variables so that a test can make them fail.
var (
	readFile   = os.ReadFile
	renameFile = os.Rename
	busyRename = renameIsBusy
)

// readBusy reads the file at path. A file that is not there is reported at once. Any
// other error is tried again for about a second, because it may pass: the node puts
// its files in place by renaming, and on Windows a file cannot be opened during the
// moment it is being renamed.
func readBusy(path string) ([]byte, error) {
	for try := 1; ; try++ {
		b, err := readFile(path)
		if err == nil || errors.Is(err, fs.ErrNotExist) || try == busyTries {
			return b, err
		}
		time.Sleep(busyPause)
	}
}

// renameBusy renames tmp to path. Where the system refuses because another process has
// path open at that moment (renameIsBusy: Windows only), it tries again for about a
// second.
func renameBusy(tmp, path string) error {
	for try := 1; ; try++ {
		err := renameFile(tmp, path)
		if err == nil || !busyRename(err) || try == busyTries {
			return err
		}
		time.Sleep(busyPause)
	}
}

// writeFile writes data to path with the given permissions, replacing a file that is
// there: it writes a temporary file in the same directory and renames it over path.
func writeFile(path string, data []byte, perm os.FileMode) error {
	return place(path, data, perm, renameBusy)
}

// createFile is writeFile for a file that must not exist: the temporary file is linked
// to path, which fails with an error matching fs.ErrExist if path is there, whoever put
// it there and however recently.
//
// Some file systems have no hard links. There the file is created under its own name
// with O_EXCL instead, which is as exclusive; a reader could then see it half written,
// but the only writer holds the lock on the directory, and a half-written key or token
// does not parse.
func createFile(path string, data []byte, perm os.FileMode) error {
	err := place(path, data, perm, os.Link)
	if err == nil || errors.Is(err, fs.ErrExist) {
		return err
	}
	var le *os.LinkError
	if !errors.As(err, &le) {
		return err // writing the temporary file failed, not linking it
	}
	return createDirect(path, data, perm)
}

// createDirect writes data to a new file at path, failing if path exists. A file it
// could not complete is removed.
func createDirect(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(perm)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// place writes data to a temporary file next to path and has into put it at path.
func place(path string, data []byte, perm os.FileMode, into func(tmp, path string) error) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// gone already after a rename; still there after a link or an error
	defer os.Remove(tmp)
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(perm)
	}
	if err == nil {
		// on disk before it gets its name: a crash must not leave an empty file under it
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = into(tmp, path)
	}
	if err != nil {
		return err
	}
	// The new name is on disk only once the directory is. A file system that cannot
	// sync a directory is not an error: the file itself is already synced.
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
