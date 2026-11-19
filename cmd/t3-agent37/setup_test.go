package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestSetupCheckRecognizesCredentialsSavedBySetup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("T3_AGENT37_PEERS_FILE", "")
	for _, name := range []string{agent37KeyName, tailClientID, tailSecret, proxyURL, proxyKey} {
		t.Setenv(name, "")
	}
	ctx := context.Background()
	var output bytes.Buffer
	check := func() error {
		output.Reset()
		return run(ctx, []string{"setup", "--check"}, &output, io.Discard)
	}
	if err := check(); err == nil ||
		!strings.Contains(output.String(), "Needs attention Tailscale OAuth client") ||
		!strings.Contains(output.String(), "Needs attention Agent37 template") {
		t.Fatalf("setup --check on fresh workstation = %v\n%s", err, &output)
	}

	config, err := readWorkstationConfig()
	if err != nil {
		t.Fatal(err)
	}
	config.values.Template = "terrascale-agent37@3"
	if err := config.save(ctx); err != nil {
		t.Fatal(err)
	}
	file, err := readRuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	file.values[agent37KeyName] = "fixture-agent37-key"
	file.values[tailClientID] = "fixture-tail-id"
	file.values[tailSecret] = "fixture-tail-secret"
	file.values[proxyKey] = "fixture-proxy-key"
	if err := saveConfigJSON(ctx, file.path, file.original, file.values); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file.path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("runtime credentials have permissions %v", info.Mode().Perm())
	}
	if key, err := agent37APIKey(); err != nil || key != file.values[agent37KeyName] {
		t.Fatalf("could not load setup key: %v", err)
	}
	t.Setenv(proxyURL, "http://example.com")
	if err := check(); err == nil || !strings.Contains(output.String(), "Needs attention CLIProxy HTTPS base URL") {
		t.Fatalf("setup --check with invalid CLIProxy URL = %v\n%s", err, &output)
	}
	t.Setenv(proxyURL, "https://models.example.com/v1")
	if err := check(); err != nil || !strings.Contains(output.String(), "Run t3-agent37 setup --verify") ||
		strings.Contains(output.String(), "fixture-") {
		t.Fatalf("setup --check with complete settings = %v\n%s", err, &output)
	}
}

func TestSetupVerifyReportsMissingEnrollmentWithoutTerminal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("PATH", "")
	t.Setenv(tailClientID, "")
	t.Setenv(tailSecret, "")
	t.Setenv("T3_AGENT37_PEERS_FILE", "")
	var output bytes.Buffer
	err := run(context.Background(), []string{"setup", "--verify"}, &output, io.Discard)
	if err == nil || !strings.Contains(output.String(), "Needs attention Tailscale OAuth client") ||
		!strings.Contains(output.String(), "Local Tailscale status unavailable") {
		t.Fatalf("setup --verify without credentials or local client = %v\n%s", err, &output)
	}
}
