package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAgent37APIKeyUsesOfficialConfigAndRespectsEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("AGENT37_API_KEY", "")
	if _, err := agent37APIKey(); err == nil {
		t.Fatal("accepted a missing credential")
	}
	directory := filepath.Join(home, ".config", "agent37")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "config.json"), []byte(`{"apiKey":"saved-fixture"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if key, err := agent37APIKey(); err != nil || key != "saved-fixture" {
		t.Fatalf("could not use saved credential: %v", err)
	}
	t.Setenv("AGENT37_API_KEY", "environment-fixture")
	if key, err := agent37APIKey(); err != nil || key != "environment-fixture" {
		t.Fatalf("could not use environment credential: %v", err)
	}
}
