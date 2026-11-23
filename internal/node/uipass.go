package node

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The password of the web interface. The data directory holds only its hash:
//
//	pbkdf2-sha256$ITERATIONS$SALT$KEY
//
// with salt and key in base64 (PBKDF2 with HMAC-SHA256, a random salt of 16 bytes, a
// key of 32 bytes).
const (
	// UIPasswordFile is the name of the file with the hash, in the data directory.
	UIPasswordFile = "ui.password"

	passwordScheme  = "pbkdf2-sha256"
	passwordSaltLen = 16
	passwordKeyLen  = 32
	// maxPasswordIterations bounds the work a stored hash can ask for.
	maxPasswordIterations = 10_000_000
)

// PasswordIterations is the PBKDF2 iteration count of a new hash.
var PasswordIterations = 600_000

// HashPassword returns the hash of a password, in the form stored in the data directory.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("the password is empty")
	}
	salt := make([]byte, passwordSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, PasswordIterations, passwordKeyLen)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("%s$%d$%s$%s", passwordScheme, PasswordIterations, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// CheckPassword reports whether password is the one that hash was made from. A hash
// that is not of the form HashPassword returns matches nothing.
func CheckPassword(hash, password string) bool {
	iter, salt, key, err := parsePasswordHash(hash)
	if err != nil || password == "" {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(key))
	return err == nil && subtle.ConstantTimeCompare(got, key) == 1
}

func parsePasswordHash(hash string) (iter int, salt, key []byte, err error) {
	bad := errors.New("not a password hash of this program")
	f := strings.Split(hash, "$")
	if len(f) != 4 || f[0] != passwordScheme {
		return 0, nil, nil, bad
	}
	if iter, err = strconv.Atoi(f[1]); err != nil || iter < 1 || iter > maxPasswordIterations {
		return 0, nil, nil, bad
	}
	enc := base64.RawStdEncoding
	if salt, err = enc.DecodeString(f[2]); err != nil || len(salt) != passwordSaltLen {
		return 0, nil, nil, bad
	}
	if key, err = enc.DecodeString(f[3]); err != nil || len(key) != passwordKeyLen {
		return 0, nil, nil, bad
	}
	return iter, salt, key, nil
}

// ReadUIPassword returns the password hash stored in the data directory dir. A missing
// file is reported with an error that matches fs.ErrNotExist; a file that does not hold
// a hash is an error and is left as it is.
func ReadUIPassword(dir string) (string, error) {
	path := filepath.Join(dir, UIPasswordFile)
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	hash := strings.TrimSpace(string(b))
	if _, _, _, err := parsePasswordHash(hash); err != nil {
		return "", fmt.Errorf("%s: %w. The file is left as it is. Remove it to have a new password made.", path, err)
	}
	return hash, nil
}

// WriteUIPassword stores a password hash in the data directory dir, in place of the one
// that is there.
func WriteUIPassword(dir, hash string) error {
	if _, _, _, err := parsePasswordHash(hash); err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, UIPasswordFile), []byte(hash+"\n"), 0o600)
}

// NewPassword returns a random password of 16 letters and digits (80 bits).
func NewPassword() (string, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.EncodeToString(b)), nil
}
