package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestRuntimeCredentialsUseEnvironmentAndSavedValues(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(tailClientID, " environment-client ")
	t.Setenv(tailSecret, " ")
	path, err := workstationConfigPath("runtime.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"T3_AGENT37_TAILSCALE_CLIENT_ID":"saved-client","T3_AGENT37_TAILSCALE_CLIENT_SECRET":" saved-secret "}`), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := runtimeCredentials(tailClientID, tailSecret)
	if err != nil || values[tailClientID] != "environment-client" || values[tailSecret] != "saved-secret" {
		t.Fatalf("could not combine environment and saved credentials: %v", err)
	}

	const malformed = `{"T3_AGENT37_TAILSCALE_CLIENT_SECRET":"fixture-private"} trailing`
	if err := os.WriteFile(path, []byte(malformed), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeCredentials(tailClientID, tailSecret); err == nil || strings.Contains(err.Error(), "fixture-private") {
		t.Fatalf("malformed credential file error was missing or printed a credential: %v", err)
	}
	t.Setenv(tailSecret, "environment-secret")
	values, err = runtimeCredentials(tailClientID, tailSecret)
	if err != nil || values[tailSecret] != "environment-secret" {
		t.Fatalf("could not use environment credentials without loading the file: %v", err)
	}
}

func TestReadCredentialFileRequiresOwnerOnlyRegularFile(t *testing.T) {
	directory := t.TempDir()
	if _, err := readCredentialFile(directory, "test credentials"); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("reading a directory as a credential file = %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	path := filepath.Join(directory, "credential.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := readCredentialFile(path, "test credentials"); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("reading a credential file with group access = %v", err)
	}
}
