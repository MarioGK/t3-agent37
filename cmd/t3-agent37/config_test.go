package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveDefaultsAndApplyProvisioningOverrides(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	ctx := t.Context()
	file, err := readWorkstationConfig()
	if err != nil || file.values.NamePrefix != "agent" {
		t.Fatalf("read default config: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(file.path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file.path, []byte(`{"extra":{"keep":true},"tailscale":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err = readWorkstationConfig()
	if err != nil {
		t.Fatal(err)
	}
	file.values.NamePrefix = "terra"
	file.values.Template = "terrascale-agent37@3"
	file.values.Provider = "opencode"
	file.values.Tailscale = nil
	if err := file.save(ctx); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(file.path)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	if object["extra"] == nil || object["tailscale"] != nil {
		t.Fatalf("saving lost an existing setting or failed to clear the Tailscale override: %s", data)
	}
	opts, err := parse([]string{"create", "dev", "--dry-run"}, io.Discard)
	if err != nil || opts.template != "terrascale-agent37@3" || opts.provider != "opencode" || !opts.tailscale {
		t.Fatalf("named provisioning defaults = %+v, %v", opts, err)
	}
	opts, err = parse([]string{"up", "--template", "agent37-codex", "--provider", "codex"}, io.Discard)
	if err != nil || opts.namePrefix != "terra" || opts.template != "agent37-codex" || opts.provider != "codex" || !opts.tailscale {
		t.Fatalf("explicit provisioning overrides = %+v, %v", opts, err)
	}
	if err := file.save(ctx); err == nil || !strings.Contains(err.Error(), "config changed") {
		t.Fatalf("stale editor was allowed to overwrite the current config: %v", err)
	}
}

func TestConfigChecksForTerminalBeforeCredentials(t *testing.T) {
	t.Setenv("AGENT37_API_KEY", "")
	err := run(t.Context(), []string{"config"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "requires a terminal") {
		t.Fatalf("config with redirected output = %v", err)
	}
}
