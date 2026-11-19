package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTerraScaleDryRunWithNoCredentials(t *testing.T) {
	t.Setenv("AGENT37_API_KEY", "")
	t.Setenv("T3_AGENT37_CLIPROXY_KEY", "fixture-secret")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var output bytes.Buffer
	err := run(context.Background(), []string{"create", "dev", "--template", "terrascale-agent37@3", "--dry-run"}, &output, &output)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(output.Bytes(), []byte("fixture-secret")) {
		t.Fatal("dry-run printed a credential")
	}
	var payload struct {
		Metadata    map[string]string
		Resources   map[string]int
		Type        string
		Budget      map[string]int
		AutoSleep   bool `json:"auto_sleep"`
		IdleTimeout int  `json:"idle_timeout_seconds"`
	}
	if err := json.Unmarshal(output.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Metadata["provider"] != "cliproxy" || payload.Metadata["tailnet"] != "tailscale" ||
		payload.Type != "default" || payload.Resources["cpu"] != 4 || payload.Resources["memory"] != 8 ||
		!payload.AutoSleep || payload.IdleTimeout != 900 || payload.Budget != nil {
		t.Fatalf("unexpected TerraScale provisioning defaults: %+v", payload)
	}
}

func TestFlagsCanFollowName(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	opts, err := parse([]string{"create", "dev", "--template", "terrascale-agent37@3", "--tailscale", "--yes"}, &bytes.Buffer{})
	if err != nil || opts.name != "dev" || opts.generatedName || !opts.tailscale || opts.provider != "cliproxy" || !opts.yes {
		t.Fatalf("unexpected options: %+v, %v", opts, err)
	}
	opts, err = parse([]string{"connect", "dev", "--pair"}, &bytes.Buffer{})
	if err != nil || !opts.pair {
		t.Fatalf("unexpected connect options: %+v, %v", opts, err)
	}
}

func TestLsUsesListCommand(t *testing.T) {
	var output bytes.Buffer
	opts, err := parse([]string{"ls"}, &output)
	if err != nil || opts.action != "list" {
		t.Fatalf("ls = %+v, %v", opts, err)
	}
	if _, err := parse([]string{"ls", "dev"}, &output); err == nil {
		t.Fatal("ls accepted a sandbox name")
	}
}

func TestCreateWithoutName(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	state := filepath.Join(t.TempDir(), "new-state")
	t.Setenv("XDG_STATE_HOME", state)
	var output bytes.Buffer
	if err := run(context.Background(), []string{"create", "--template", "terrascale-agent37@3", "--dry-run"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Name     string
		Metadata map[string]string
	}
	if err := json.Unmarshal(output.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Name != "agent-1" || payload.Metadata["environment"] != payload.Name {
		t.Fatalf("invalid generated sandbox name or metadata: %+v", payload)
	}
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run wrote state or could not check it: %v", err)
	}
	if _, err := parse([]string{"create", "", "--dry-run"}, &bytes.Buffer{}); err == nil {
		t.Fatal("create accepted an empty explicit name")
	}
}

func TestProvisioningRequiresTailnetBeforeCreatingState(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "new-state"))
	t.Setenv("T3_AGENT37_TAILSCALE_CLIENT_ID", "")
	t.Setenv("T3_AGENT37_TAILSCALE_CLIENT_SECRET", "")
	var output bytes.Buffer
	_, err := parse([]string{"up", "--template", "terrascale-agent37@3", "--no-tailscale"}, &output)
	if err == nil || !strings.Contains(err.Error(), "Tailscale is required") {
		t.Fatalf("up accepted a loopback fallback: %v", err)
	}
	err = run(context.Background(), []string{"up", "--template", "terrascale-agent37@3"}, &output, &output)
	if err == nil || !strings.Contains(err.Error(), "Tailscale enrollment requires") {
		t.Fatalf("up accepted missing enrollment credentials: %v", err)
	}
	if _, err := os.Stat(os.Getenv("XDG_STATE_HOME")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("up reserved state before it found the credential: %v", err)
	}
	if _, err := pairLink(context.Background(), nil, "aaaaaaaaaa", "http://127.0.0.1:43738"); err == nil {
		t.Fatal("pairing accepted a loopback origin")
	}
}

func TestUpWithoutNameCreatesAndNamedUpStarts(t *testing.T) {
	t.Setenv("AGENT37_API_KEY", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var output bytes.Buffer
	opts, err := parse([]string{"up", "--template", "terrascale-agent37@3"}, &output)
	if err != nil || opts.action != "create" || !opts.generatedName || !opts.yes || opts.dryRun {
		t.Fatalf("bare up = %+v, %v", opts, err)
	}
	opts, err = parse([]string{"up", "dev"}, &output)
	if err != nil || opts.action != "start" || opts.name != "dev" || opts.generatedName {
		t.Fatalf("named up = %+v, %v", opts, err)
	}
	if _, err := parse([]string{"up", "dev", "--dry-run"}, &output); err == nil {
		t.Fatal("named up accepted a provisioning flag")
	}
	if err := run(context.Background(), []string{"up", "--dry-run", "--template", "terrascale-agent37@3", "--tailscale"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Name     string
		Template string
		Metadata map[string]string
	}
	if err := json.Unmarshal(output.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Name != "agent-1" || payload.Template != "terrascale-agent37@3" ||
		payload.Metadata["provider"] != "cliproxy" || payload.Metadata["tailnet"] != "tailscale" {
		t.Fatalf("unexpected up preview: %+v", payload)
	}
}

func TestDeleteRequiresExplicitConfirmation(t *testing.T) {
	for _, incomplete := range []string{"--yes", "--delete"} {
		if _, err := parse([]string{"down", "dev", incomplete}, &bytes.Buffer{}); err == nil {
			t.Fatalf("down accepted %s without its companion flag", incomplete)
		}
	}
	opts, err := parse([]string{"down", "dev", "--delete", "--yes"}, &bytes.Buffer{})
	if err != nil || opts.action != "stop" || !opts.deleteSandbox {
		t.Fatalf("unexpected delete options: %+v, %v", opts, err)
	}
}
