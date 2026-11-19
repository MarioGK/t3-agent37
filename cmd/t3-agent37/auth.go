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

const (
	agent37KeyName = "AGENT37_API_KEY"
	tailClientID   = "T3_AGENT37_TAILSCALE_CLIENT_ID"
	tailSecret     = "T3_AGENT37_TAILSCALE_CLIENT_SECRET"
	proxyURL       = "T3_AGENT37_CLIPROXY_URL"
	proxyKey       = "T3_AGENT37_CLIPROXY_KEY"
	proxyModel     = "T3_AGENT37_CLIPROXY_MODEL"
)

func agent37APIKey() (string, error) {
	key, err := existingAgent37APIKey()
	if err != nil || key != "" {
		return key, err
	}
	key, err = runtimeCredential(agent37KeyName)
	if err == nil && key == "" {
		err = errors.New("run t3-agent37 setup, set AGENT37_API_KEY, or save a key with agent37 ssh setup")
	}
	return key, err
}

func existingAgent37APIKey() (string, error) {
	if key := strings.TrimSpace(os.Getenv(agent37KeyName)); key != "" {
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
		return "", nil
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

func runtimeCredential(name string) (string, error) {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value, nil
	}
	file, err := readRuntimeConfig()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(file.values[name]), nil
}

type runtimeConfig struct {
	path     string
	original []byte
	values   map[string]string
}

func (file runtimeConfig) value(name string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return strings.TrimSpace(file.values[name])
}

func readRuntimeConfig() (runtimeConfig, error) {
	file := runtimeConfig{values: make(map[string]string)}
	dir, err := workstationConfigDir()
	if err != nil {
		return file, err
	}
	file.path = filepath.Join(dir, "runtime.json")
	if target, err := filepath.EvalSymlinks(file.path); err == nil {
		file.path = target
	} else if !errors.Is(err, os.ErrNotExist) {
		return file, err
	}
	info, err := os.Stat(file.path)
	if errors.Is(err, os.ErrNotExist) {
		return file, nil
	}
	if err != nil {
		return file, err
	}
	if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return file, fmt.Errorf("runtime credentials at %s require an owner-only regular file", file.path)
	}
	data, err := os.ReadFile(file.path)
	if err != nil {
		return file, err
	}
	if err := json.Unmarshal(data, &file.values); err != nil {
		return file, fmt.Errorf("invalid runtime credentials file at %s", file.path)
	}
	if file.values == nil {
		file.values = make(map[string]string)
	}
	file.original = data
	return file, nil
}

func tailscaleCredentials() (string, string, error) {
	clientID, err := runtimeCredential(tailClientID)
	if err != nil {
		return "", "", err
	}
	clientSecret, err := runtimeCredential(tailSecret)
	if err != nil {
		return "", "", err
	}
	if clientID == "" || clientSecret == "" {
		return "", "", fmt.Errorf("Tailscale enrollment requires %s and %s. Run t3-agent37 setup", tailClientID, tailSecret)
	}
	return clientID, clientSecret, nil
}
