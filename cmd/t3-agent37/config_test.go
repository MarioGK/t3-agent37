package main

import (
	"bytes"
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

func TestSandboxSizeDefaultsAndOneOffOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("AGENT37_API_KEY", "")
	ctx := t.Context()
	var output bytes.Buffer
	assertSize := func(want sandboxSize, args ...string) {
		t.Helper()
		output.Reset()
		if err := run(ctx, args, &output, io.Discard); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		var got sandboxSize
		if err := json.Unmarshal(output.Bytes(), &got); err != nil || got != want {
			t.Fatalf("%v: got %s, want %v: %v", args, &output, want, err)
		}
	}
	assertSize(sandboxSize{4, 8}, "config", "--show-size")
	assertSize(sandboxSize{4, 16}, "config", "--ram", "16")
	assertSize(sandboxSize{4, 16}, "config", "--cpu", "4")
	assertSize(sandboxSize{4, 16}, "config", "--show-size")
	file, err := readWorkstationConfig()
	if err != nil || file.values.sandboxSize() != (sandboxSize{4, 16}) {
		t.Fatalf("resource selection did not persist: %+v, %v", file.values.Resources, err)
	}
	for _, tc := range []struct {
		args  []string
		want  sandboxSize
		count int
	}{
		{[]string{"up", "--dry-run"}, sandboxSize{4, 16}, 1},
		{[]string{"up", "--dry-run", "--cpu", "2"}, sandboxSize{2, 4}, 1},
		{[]string{"up", "10", "--dry-run", "--ram", "32"}, sandboxSize{4, 32}, 10},
		{[]string{"create", "example", "--dry-run", "--ram", "4"}, sandboxSize{2, 4}, 1},
	} {
		args := append(tc.args, "--template", "terrascale-agent37@5")
		output.Reset()
		if err := run(ctx, args, &output, io.Discard); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		data := output.Bytes()
		if tc.count == 1 {
			data = append(append([]byte{'['}, data...), ']')
		}
		var plans []struct{ Resources sandboxSize }
		if err := json.Unmarshal(data, &plans); err != nil || len(plans) != tc.count {
			t.Fatalf("%v: invalid plan count in %s: %v", args, data, err)
		}
		for _, plan := range plans {
			if plan.Resources != tc.want {
				t.Fatalf("%v: planned %v, want %v", args, plan.Resources, tc.want)
			}
		}
	}
	assertSize(sandboxSize{4, 16}, "config", "--show-size")
	for _, args := range [][]string{
		{"config", "--ram", "12"}, {"config", "--cpu", "2", "--ram", "8"}, {"config", "--ram", "0"},
		{"config", "--show-size", "--cpu", "4"}, {"up", "--dry-run", "--cpu", "8", "--template", "terrascale-agent37@5"},
		{"up", "existing", "--ram", "32"},
	} {
		if err := run(ctx, args, io.Discard, io.Discard); err == nil {
			t.Fatalf("accepted unsupported resource request: %v", args)
		}
	}
	assertSize(sandboxSize{4, 16}, "config", "--show-size")
}
