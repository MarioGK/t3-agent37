package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func agent37APIKey() (string, error) {
	if key := strings.TrimSpace(os.Getenv("AGENT37_API_KEY")); key != "" {
		return key, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	// Match the path and schema written by the official Agent37 CLI.
	path := filepath.Join(home, ".config", "agent37", "config.json")
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", errors.New("set AGENT37_API_KEY or save a key with agent37 ssh setup")
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("Agent37 credentials at %s must be a regular file", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("restrict access to Agent37 credentials at %s with chmod 600", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var config struct {
		APIKey string `json:"apiKey"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return "", fmt.Errorf("invalid Agent37 credentials file at %s", path)
	}
	key := strings.TrimSpace(config.APIKey)
	if key == "" {
		return "", fmt.Errorf("no API key in Agent37 credentials at %s", path)
	}
	return key, nil
}
