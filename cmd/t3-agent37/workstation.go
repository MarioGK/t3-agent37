package main

import (
	"os"
	"path/filepath"
	"runtime"
)

func statePath() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" && runtime.GOOS == "windows" {
		base = os.Getenv("LOCALAPPDATA")
	}
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, owner), nil
}
