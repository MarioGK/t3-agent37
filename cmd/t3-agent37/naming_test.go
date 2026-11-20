package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	"github.com/gofrs/flock"
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
	if got, err := loadWorkstationConfig(); err != nil || got.NamePrefix != "terra-dev" {
		t.Fatalf("configured prefix = %q, %v", got.NamePrefix, err)
	}
	if err := os.WriteFile(config, []byte(`{"name_prefix":"Bad prefix"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadWorkstationConfig(); err == nil {
		t.Fatal("invalid prefix was accepted")
	}
}

func TestUpUsesTailnetDefaultsFromConfig(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	directory := filepath.Join(base, owner)
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"name_prefix":"terra","template":"terrascale-agent37@3","tailscale":true}`)
	if err := os.WriteFile(filepath.Join(directory, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	opts, err := parse([]string{"up"}, os.Stderr)
	if err != nil || opts.namePrefix != "terra" || opts.template != "terrascale-agent37@3" || opts.provider != "cliproxy" || !opts.tailscale {
		t.Fatalf("configured up = %+v, %v", opts, err)
	}
	opts, err = parse([]string{"up", "--template", "agent37-opencode"}, os.Stderr)
	if err != nil || opts.template != "agent37-opencode" || !opts.tailscale {
		t.Fatalf("up with explicit overrides = %+v, %v", opts, err)
	}
}

func TestNumberedNamesFillGaps(t *testing.T) {
	state := t.TempDir()
	// Old counter files and unlocked lifecycle locks must not reserve a name.
	for _, file := range []string{"names.sqlite3", "terra-1.lifecycle.lock"} {
		if err := os.WriteFile(filepath.Join(state, file), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	instances := []agent37.Instance{
		{Name: "terra-1", Status: "stopped"},
		{Name: "different", Metadata: map[string]string{"environment": "terra-2"}},
		{Name: "terra-4", Status: "running"},
		{Name: "terra-3", Status: "deleted"},
		{Name: "terra-03"},
	}
	for _, test := range []struct {
		label     string
		instances []agent37.Instance
		want      string
	}{
		{label: "empty workspace", want: "terra-1"},
		{label: "deleted third sandbox", instances: instances, want: "terra-3"},
		{label: "contiguous names", instances: append([]agent37.Instance{{Name: "terra-3"}}, instances...), want: "terra-5"},
	} {
		t.Run(test.label, func(t *testing.T) {
			got, lock, err := reserveSandboxName(t.Context(), state, "terra", test.instances)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if got != test.want {
				t.Errorf("reserved %q, want %q", got, test.want)
			}
		})
	}
}

func TestNumberedNamesReserveUniqueConcurrentNames(t *testing.T) {
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "agent-3.pending"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	const count = 12
	type reservation struct {
		name string
		lock *flock.Flock
		err  error
	}
	results := make(chan reservation, count)
	instances := []agent37.Instance{{Name: "agent-1"}}
	for range count {
		go func() {
			name, lock, err := reserveSandboxName(ctx, state, "agent", instances)
			results <- reservation{name: name, lock: lock, err: err}
		}()
	}
	got := make(map[string]bool, count)
	for range count {
		result := <-results
		if result.err != nil {
			t.Error(result.err)
			continue
		}
		t.Cleanup(func() {
			if err := result.lock.Close(); err != nil {
				t.Error(err)
			}
		})
		got[result.name] = true
	}
	expected := []string{"agent-2"}
	for i := range count - 1 {
		expected = append(expected, fmt.Sprintf("agent-%d", i+4))
	}
	for _, name := range expected {
		if !got[name] {
			t.Errorf("missing reservation %s", name)
		}
	}
}

type namingTestTransport func(*http.Request) string

func (fn namingTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(fn(request)))}, nil
}

func TestNumberedNamePreviewUsesLiveNames(t *testing.T) {
	t.Setenv(agent37KeyName, "naming-test")
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = namingTestTransport(func(request *http.Request) string {
		if request.Method != http.MethodGet || request.URL.Path != "/v1/instances" {
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		return `{"data":[{"name":"terra-1"},{"name":"terra-2","status":"stopped"},{"name":"terra-4"}]}`
	})
	state := filepath.Join(t.TempDir(), "state")
	if name, err := previewSandboxName(t.Context(), state, "terra"); err != nil || name != "terra-3" {
		t.Fatalf("preview = %q, %v", name, err)
	}
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preview wrote state: %v", err)
	}
}

func TestGeneratedCreateRechecksAReservedName(t *testing.T) {
	state := t.TempDir()
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = namingTestTransport(func(request *http.Request) string {
		if request.Method != http.MethodGet {
			t.Errorf("unexpected %s for occupied name", request.Method)
		}
		return `{"data":[{"name":"terra-1","status":"provisioning"}]}`
	})
	opts := options{name: "terra-1", generatedName: true}
	if _, err := getOrCreate(t.Context(), agent37.New("naming-test"), opts, state, io.Discard); !errors.Is(err, errSandboxNameTaken) {
		t.Fatalf("occupied name result: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, "terra-1.pending")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("collision left a pending marker: %v", err)
	}
}
