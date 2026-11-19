package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/MarioGK/t3-agent37/internal/peers"
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
	values, err := runtimeCredentials(agent37KeyName)
	if err != nil {
		return "", err
	}
	if key = values[agent37KeyName]; key == "" {
		return "", errors.New("run t3-agent37 setup, set AGENT37_API_KEY, or save a key with agent37 ssh setup")
	}
	return key, nil
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
	data, err := readCredentialFile(path, "Agent37 credentials")
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
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

func runtimeCredentials(names ...string) (map[string]string, error) {
	values := make(map[string]string, len(names))
	var saved map[string]string
	for _, name := range names {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			if saved == nil {
				file, err := readRuntimeConfig()
				if err != nil {
					return nil, err
				}
				saved = file.values
			}
			value = strings.TrimSpace(saved[name])
		}
		values[name] = value
	}
	return values, nil
}

type runtimeConfig struct {
	path     string
	original []byte
	values   map[string]string
}

func (file runtimeConfig) value(name string) string {
	return cmp.Or(strings.TrimSpace(os.Getenv(name)), strings.TrimSpace(file.values[name]))
}

func readRuntimeConfig() (runtimeConfig, error) {
	file := runtimeConfig{values: make(map[string]string)}
	path, err := workstationConfigPath("runtime.json")
	if err != nil {
		return file, err
	}
	file.path = path
	data, err := readCredentialFile(path, "runtime credentials")
	if errors.Is(err, os.ErrNotExist) {
		return file, nil
	}
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

func readCredentialFile(path, name string) ([]byte, error) {
	// Reject named pipes before opening the path.
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s at %s must be a regular file", name, path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	// Check the open file as the path may have changed since the first check.
	info, err = file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s at %s must be a regular file", name, path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("restrict access to %s at %s with chmod 600", name, path)
	}
	return io.ReadAll(file)
}

func tailscaleCredentials() (string, string, error) {
	values, err := runtimeCredentials(tailClientID, tailSecret)
	if err != nil {
		return "", "", err
	}
	clientID, clientSecret := values[tailClientID], values[tailSecret]
	if clientID == "" || clientSecret == "" {
		return "", "", fmt.Errorf("Tailscale enrollment requires %s and %s. Run t3-agent37 setup", tailClientID, tailSecret)
	}
	return clientID, clientSecret, nil
}

func workstationTailscaleTags() ([]string, error) {
	config, err := readWorkstationConfig()
	if err != nil {
		return nil, err
	}
	tags, err := peers.ParseEnrollmentTags(config.values.TailscaleTags)
	if err != nil {
		return nil, fmt.Errorf("invalid Tailscale tags. Run t3-agent37 setup: %w", err)
	}
	return tags, nil
}

func verifyTailscaleEnrollment(ctx context.Context) (*peers.Enrollment, error) {
	id, secret, err := tailscaleCredentials()
	if err != nil {
		return nil, err
	}
	tags, err := workstationTailscaleTags()
	if err != nil {
		return nil, err
	}
	return peers.VerifyEnrollment(ctx, id, secret, tags)
}
