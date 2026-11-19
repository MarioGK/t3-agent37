package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"time"

	"github.com/gofrs/flock"
)

const defaultNamePrefix = "agent"

var prefixPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,31}[a-z0-9])?$`)

type workstationConfig struct {
	NamePrefix string `json:"name_prefix"`
	Template   string `json:"template,omitempty"`
	Provider   string `json:"provider,omitempty"`
	Tailscale  *bool  `json:"tailscale,omitempty"`
}

func validateNamePrefix(prefix string) error {
	if !prefixPattern.MatchString(prefix) {
		return errors.New("use 1 to 33 lowercase letters, digits, or hyphens and start and end with a letter or digit")
	}
	return nil
}

func loadWorkstationConfig() (workstationConfig, error) {
	file, err := readWorkstationConfig()
	if err == nil {
		if err = validateNamePrefix(file.values.NamePrefix); err != nil {
			err = fmt.Errorf("invalid name_prefix in %s: %w", file.path, err)
		}
	}
	return file.values, err
}

type configFile struct {
	path     string
	original []byte
	values   workstationConfig
}

func readWorkstationConfig() (configFile, error) {
	file := configFile{values: workstationConfig{NamePrefix: defaultNamePrefix}}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		var err error
		if runtime.GOOS == "windows" {
			base, err = os.UserConfigDir()
		} else {
			var home string
			home, err = os.UserHomeDir()
			base = filepath.Join(home, ".config")
		}
		if err != nil {
			return file, err
		}
	}
	file.path = filepath.Join(base, owner, "config.json")
	if target, err := filepath.EvalSymlinks(file.path); err == nil {
		file.path = target
	} else if !errors.Is(err, os.ErrNotExist) {
		return file, err
	}
	data, err := os.ReadFile(file.path)
	if errors.Is(err, os.ErrNotExist) {
		return file, nil
	}
	if err != nil {
		return file, err
	}
	if err := json.Unmarshal(data, &file.values); err != nil {
		return file, fmt.Errorf("invalid config %s: %w", file.path, err)
	}
	file.original = data
	return file, nil
}

func (file configFile) save(ctx context.Context) error {
	dir := filepath.Dir(file.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lock := flock.New(file.path + ".lock")
	locked, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		return err
	}
	if !locked {
		return errors.New("could not lock the configuration file")
	}
	defer lock.Unlock()
	current, err := os.ReadFile(file.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !bytes.Equal(current, file.original) {
		return errors.New("the config changed while the editor was open. Run config again")
	}

	var existing map[string]json.RawMessage
	if current != nil {
		if err := json.Unmarshal(current, &existing); err != nil {
			return err
		}
	}
	encoded, err := json.Marshal(file.values)
	if err != nil {
		return err
	}
	fields := make(map[string]json.RawMessage)
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return err
	}
	for _, key := range []string{"name_prefix", "template", "provider", "tailscale"} {
		delete(existing, key)
	}
	maps.Copy(fields, existing)

	temp, err := os.CreateTemp(dir, ".t3-agent37-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	encoder := json.NewEncoder(temp)
	encoder.SetIndent("", "  ")
	writeErr := encoder.Encode(fields)
	closeErr := temp.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	return os.Rename(temp.Name(), file.path)
}
