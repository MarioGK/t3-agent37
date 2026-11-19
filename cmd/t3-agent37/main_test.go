package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
)

func TestTerraScaleDryRunWithNoCredentials(t *testing.T) {
	t.Setenv("AGENT37_API_KEY", "")
	t.Setenv("T3_AGENT37_CLIPROXY_KEY", "fixture-secret")
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
	opts, err := parse([]string{"create", "dev", "--template", "terrascale-agent37@3", "--no-tailscale", "--yes"}, &bytes.Buffer{})
	if err != nil || opts.name != "dev" || opts.generatedName || opts.tailscale || opts.provider != "cliproxy" || !opts.yes {
		t.Fatalf("unexpected options: %+v, %v", opts, err)
	}
	opts, err = parse([]string{"connect", "--port", "49373", "dev", "--pair"}, &bytes.Buffer{})
	if err != nil || opts.port != 49373 || !opts.pair {
		t.Fatalf("unexpected connect options: %+v, %v", opts, err)
	}
}

func TestCreateWithoutName(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), []string{"create", "--dry-run"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Name     string
		Metadata map[string]string
	}
	if err := json.Unmarshal(output.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !namePattern.MatchString(payload.Name) || payload.Metadata["environment"] != payload.Name {
		t.Fatalf("invalid generated sandbox name or metadata: %+v", payload)
	}
	if _, err := parse([]string{"up"}, &bytes.Buffer{}); err == nil {
		t.Fatal("up accepted a missing name")
	}
	if _, err := parse([]string{"create", "", "--dry-run"}, &bytes.Buffer{}); err == nil {
		t.Fatal("create accepted an empty explicit name")
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

func TestRetainsExistingSQLiteRelayPorts(t *testing.T) {
	state := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(state, "ports.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE ports (instance TEXT PRIMARY KEY, port INTEGER UNIQUE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO ports VALUES (?, ?)", "ab12cd34ef", 45123); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if port, err := assignedPort(ctx, state, "ab12cd34ef", 0); err != nil || port != 45123 {
		t.Fatalf("upgraded relay returned %d, %v", port, err)
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	requested := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	if port, err := assignedPort(ctx, state, "bc23de45fg", requested); err != nil || port != requested {
		t.Fatalf("second relay returned %d, %v", port, err)
	}
	if _, err := assignedPort(ctx, state, "zz12cd34ef", requested); err == nil {
		t.Fatal("two sandboxes reserved the same relay port")
	}
	if err := releasePort(ctx, state, "bc23de45fg"); err != nil {
		t.Fatal(err)
	}
	if port, err := assignedPort(ctx, state, "zz12cd34ef", requested); err != nil || port != requested {
		t.Fatalf("released relay port returned %d, %v", port, err)
	}
}
