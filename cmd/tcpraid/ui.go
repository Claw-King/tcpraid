package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strings"

	"github.com/Claw-King/tcpraid/internal/node"
)

// uiPasswordEnv is the environment variable that sets the password of the web
// interface, for hosts where a variable is easier to set than a file (containers).
const uiPasswordEnv = "TCPRAID_UI_PASSWORD"

// uiPassword returns the hash of the password of the web interface of the node whose
// data directory is dir.
//
// The password is the content of file, if one is named; else the value of the
// environment variable; else the one whose hash the data directory holds. A password
// given the first two ways replaces the stored hash. When there is none of the three, a
// random password is made, its hash is stored, and the password itself is logged, this
// once.
func uiPassword(dir, file string) (hash string, err error) {
	given, from := os.Getenv(uiPasswordEnv), "the environment variable "+uiPasswordEnv
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("-ui-password-file: %w", err)
		}
		given, from = strings.TrimRight(string(b), "\r\n"), "-ui-password-file "+file
		if given == "" {
			return "", fmt.Errorf("-ui-password-file %s: the file holds no password", file)
		}
	}
	stored, err := node.ReadUIPassword(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	switch {
	case given != "" && stored != "" && node.CheckPassword(stored, given):
		return stored, nil
	case given != "":
		if hash, err = node.HashPassword(given); err != nil {
			return "", err
		}
		log.Printf("web interface: the password is the one from %s", from)
	case stored != "":
		return stored, nil
	default:
		password, err := node.NewPassword()
		if err != nil {
			return "", err
		}
		if hash, err = node.HashPassword(password); err != nil {
			return "", err
		}
		// the one place a password is logged: nobody has it yet, and it is not stored
		log.Printf("web interface: no password was set, so one was made. It is shown this once: %s", password)
		log.Printf("web interface: to set your own, start the node with -ui-password-file FILE or the environment variable %s", uiPasswordEnv)
	}
	if err := node.WriteUIPassword(dir, hash); err != nil {
		return "", err
	}
	return hash, nil
}
