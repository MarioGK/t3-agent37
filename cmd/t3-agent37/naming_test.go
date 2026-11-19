package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/MarioGK/t3-agent37/internal/agent37"
)

func TestConfiguredNamePrefix(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	directory := filepath.Join(base, owner)
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(directory, "config.json")
	if err := os.WriteFile(config, []byte(`{"name_prefix":"terra-dev"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := configuredNamePrefix(); err != nil || got != "terra-dev" {
		t.Fatalf("configured prefix = %q, %v", got, err)
	}
	if err := os.WriteFile(config, []byte(`{"name_prefix":"Bad prefix"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := configuredNamePrefix(); err == nil {
		t.Fatal("invalid prefix was accepted")
	}
}

func TestNameCounterContinuesAfterReservation(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state space #")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	instances := []agent37.Instance{
		{Name: "terra-2"},
		{Name: "different", Metadata: map[string]string{"environment": "terra-8"}},
		{Name: "another-prefix-40"},
	}
	if got, err := reserveSandboxName(ctx, state, "terra", instances); err != nil || got != "terra-9" {
		t.Fatalf("first reservation = %q, %v", got, err)
	}
	for range 2 {
		if got, err := previewSandboxName(ctx, state, "terra"); err != nil || got != "terra-10" {
			t.Fatalf("preview = %q, %v", got, err)
		}
	}
	if got, err := reserveSandboxName(ctx, state, "terra", nil); err != nil || got != "terra-10" {
		t.Fatalf("next reservation with no existing sandboxes = %q, %v", got, err)
	}
	if got, err := reserveSandboxName(ctx, state, "other", nil); err != nil || got != "other-1" {
		t.Fatalf("new prefix reservation = %q, %v", got, err)
	}
}

func TestNameCounterReservesUniqueConcurrentNames(t *testing.T) {
	state := t.TempDir()
	const count = 12
	results := make(chan string, count)
	for range count {
		go func() {
			name, err := reserveSandboxName(context.Background(), state, "agent", nil)
			if err != nil {
				t.Error(err)
			}
			results <- name
		}()
	}
	got := make(map[string]bool, count)
	for range count {
		got[<-results] = true
	}
	for number := 1; number <= count; number++ {
		if name := fmt.Sprintf("agent-%d", number); !got[name] {
			t.Errorf("missing reservation %s", name)
		}
	}
}
