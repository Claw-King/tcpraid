package node

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPasswordHash(t *testing.T) {
	defer func(n int) { PasswordIterations = n }(PasswordIterations)
	PasswordIterations = 1000
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h, "correct horse") || !strings.HasPrefix(h, "pbkdf2-sha256$1000$") {
		t.Fatalf("hash %q", h)
	}
	if !CheckPassword(h, "correct horse") {
		t.Error("the right password does not match its hash")
	}
	for _, wrong := range []string{"", "correct hors", "correct horse ", "Correct horse"} {
		if CheckPassword(h, wrong) {
			t.Errorf("%q matches the hash of another password", wrong)
		}
	}
	h2, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if h == h2 {
		t.Error("two hashes of one password are equal: no random salt")
	}
	if _, err := HashPassword(""); err == nil {
		t.Error("an empty password was hashed")
	}
	for _, bad := range []string{"", "x", "pbkdf2-sha256$0$AAAA$AAAA", "pbkdf2-sha256$99999999999$AAAA$AAAA", "md5$1$AAAA$AAAA", h + "$", strings.Replace(h, "$1000$", "$-5$", 1)} {
		if CheckPassword(bad, "correct horse") {
			t.Errorf("a password matches the malformed hash %q", bad)
		}
	}
}

func TestUIPasswordFile(t *testing.T) {
	defer func(n int) { PasswordIterations = n }(PasswordIterations)
	PasswordIterations = 1000
	dir := t.TempDir()
	if _, err := ReadUIPassword(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("no file: %v, want not exist", err)
	}
	h, _ := HashPassword("one")
	if err := WriteUIPassword(dir, h); err != nil {
		t.Fatal(err)
	}
	got, err := ReadUIPassword(dir)
	if err != nil || got != h {
		t.Fatalf("read %q, %v", got, err)
	}
	if st, err := os.Stat(filepath.Join(dir, UIPasswordFile)); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v, %v", st.Mode(), err)
	}
	h2, _ := HashPassword("two")
	if err := WriteUIPassword(dir, h2); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadUIPassword(dir); !CheckPassword(got, "two") || CheckPassword(got, "one") {
		t.Error("the second hash did not replace the first")
	}
	if err := WriteUIPassword(dir, "plain text"); err == nil {
		t.Error("something that is not a hash was stored")
	}
	// a file that holds something else is an error and stays
	path := filepath.Join(dir, UIPasswordFile)
	if err := os.WriteFile(path, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadUIPassword(dir); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("a file without a hash: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "hunter2\n" {
		t.Error("the file was changed")
	}
}

func TestNewPassword(t *testing.T) {
	a, err := NewPassword()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewPassword()
	if len(a) != 16 || a == b || strings.ToLower(a) != a {
		t.Fatalf("passwords %q %q", a, b)
	}
}

func TestSetName(t *testing.T) {
	dir := t.TempDir()
	n, err := Open(dir, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := n.SetName("beta"); err != nil {
		t.Fatal(err)
	}
	if err := n.SetName("bad\nname"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a name with a line break: %v", err)
	}
	if n.Name() != "beta" {
		t.Fatalf("name %q", n.Name())
	}
	n.Close()
	// the name is in the file: the next start has it
	n, err = Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	if n.Name() != "beta" {
		t.Fatalf("name after a restart %q", n.Name())
	}
}
