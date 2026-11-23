//go:build !linux

package main

import (
	"os"
	"path/filepath"
)

// defaultDataDir is where a node keeps its data when -data is not given: a directory
// "tcpraid" in the user's configuration directory (on macOS under ~/Library/Application
// Support, on Windows under %AppData%). Empty if the system names no such directory;
// -data must then be given.
var defaultDataDir = userDataDir()

func userDataDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "tcpraid")
}
