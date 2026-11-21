package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MarioGK/t3-agent37/internal/agent37"
)

func TestUpCountAndNamedStart(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, count := range []string{"1", "10"} {
		opts, err := parse([]string{"up", count, "--template", "terrascale-agent37@5", "--github=false"}, io.Discard)
		if err != nil || strconv.Itoa(opts.batchSize) != count || opts.action != "create" || !opts.generatedName || !opts.yes || opts.githubAccess {
			t.Fatalf("up %s was not parsed as a batch: %+v, %v", count, opts, err)
		}
	}
	for _, args := range [][]string{{"up", "0"}, {"up", "101"}, {"up", "--", "-2"}, {"up", "10", "extra"}} {
		if _, err := parse(args, io.Discard); err == nil {
			t.Fatalf("accepted invalid batch: %v", args)
		}
	}
	for _, args := range [][]string{{"up", "ts-10"}, {"start", "10"}} {
		opts, err := parse(args, io.Discard)
		if err != nil || opts.action != "start" || opts.batchSize != 0 || opts.name != args[1] {
			t.Fatalf("named start changed: %v, %+v, %v", args, opts, err)
		}
	}
}

func TestBatchPreviewFillsFreeSuffixes(t *testing.T) {
	t.Setenv("AGENT37_API_KEY", "fixture")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("T3_AGENT37_PEERS_FILE", "")
	state, err := statePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "agent-2.pending"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	var calls int
	http.DefaultTransport = downTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodGet || r.URL.Path != "/v1/instances" {
			return nil, fmt.Errorf("preview sent %s %s", r.Method, r.URL.Path)
		}
		return downResponse(200, `{"data":[{"name":"agent-1","status":"running"},{"name":"agent-4","status":"stopped"}]}`), nil
	})
	var output bytes.Buffer
	if err := run(t.Context(), []string{"up", "10", "--dry-run", "--template", "terrascale-agent37@5"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	var plans []struct {
		Name      string `json:"name"`
		Resources map[string]int
		AutoSleep bool `json:"auto_sleep"`
		Metadata  map[string]string
	}
	if err := json.Unmarshal(output.Bytes(), &plans); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, plan := range plans {
		names = append(names, plan.Name)
		if plan.Resources["cpu"] != 4 || plan.Resources["memory"] != 8 || !plan.AutoSleep || plan.Metadata[desktopConfirmationKey] != "required" {
			t.Fatalf("batch plan changed provisioning defaults: %+v", plan)
		}
	}
	want := []string{"agent-3", "agent-5", "agent-6", "agent-7", "agent-8", "agent-9", "agent-10", "agent-11", "agent-12", "agent-13"}
	if calls != 1 || !slices.Equal(names, want) {
		t.Fatalf("preview called Agent37 %d times and planned %v, want %v", calls, names, want)
	}
}

func TestBatchStartsTenCreatesInParallelAndReportsPartialResults(t *testing.T) {
	t.Setenv("T3_AGENT37_PEERS_FILE", "")
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	var creates atomic.Int32
	allStarted := make(chan struct{})
	http.DefaultTransport = downTestTransport(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/instances":
			return downResponse(200, `{"data":[{"name":"terra-1","status":"running"}]}`), nil
		case r.Method == http.MethodPost && r.URL.Path == "/v1/instances":
			var request struct {
				Name      string
				Metadata  map[string]string
				Resources sandboxSize
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				return nil, err
			}
			if request.Resources != (sandboxSize{4, 16}) {
				t.Errorf("create sent the wrong size: %v", request.Resources)
			}
			if creates.Add(1) == 10 {
				close(allStarted)
			}
			select {
			case <-allStarted:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			if request.Name == "terra-2" {
				return downResponse(409, `{"error":{"code":"instance_limit_reached"}}`), nil
			}
			_, number, _ := strings.Cut(request.Name, "-")
			suffix, err := strconv.Atoi(number)
			if err != nil {
				return nil, err
			}
			body := fmt.Sprintf(`{"id":"%010d","name":%q,"template":"fixture@1","status":"running","metadata":{"environment":%q,"managed_by":"t3-agent37","provider":"codex","tailnet":"tailscale"}}`, suffix, request.Name, request.Name)
			return downResponse(201, body), nil
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec"):
			return downResponse(200, `{"exit_code":1}`), nil
		default:
			return nil, fmt.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	state := t.TempDir()
	var output bytes.Buffer
	opts := options{action: "create", batchSize: 10, size: sandboxSize{4, 16}, generatedName: true, namePrefix: "terra", provider: "codex", template: "fixture@1", tailscale: true, noDesktop: true}
	progress := startLaunchProgress(io.Discard, opts)
	err := runBatch(ctx, agent37.New("fixture"), nil, opts, state, &output, progress)
	if err == nil || creates.Load() != 10 || !strings.Contains(err.Error(), "160 GB") || ctx.Err() != nil {
		t.Fatalf("batch status: %d create requests, command error %v, context error %v", creates.Load(), err, ctx.Err())
	}
	if !progress.preflight || progress.created != 9 || progress.failed != 10 || progress.synced {
		t.Fatalf("unexpected progress totals: created=%d failed=%d synced=%t", progress.created, progress.failed, progress.synced)
	}
	var result struct {
		Sandboxes []map[string]any
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || len(result.Sandboxes) != 10 {
		t.Fatalf("invalid partial results: %s, %v", &output, err)
	}
	for i, row := range result.Sandboxes {
		name := fmt.Sprintf("terra-%d", i+2)
		if row["name"] != name || row["error"] == nil || i > 0 && row["id"] == "" {
			t.Fatalf("batch lost sandbox identity for %s: %v", name, row)
		}
		if _, err := os.Stat(sandboxStatePath(state, name, ".pending")); !os.IsNotExist(err) {
			t.Fatalf("conclusive create outcome left a pending marker for %s: %v", name, err)
		}
	}
}

func TestBatchPairsReadySandboxesWhileProvisioningContinues(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var order []string
		results := provisionAndPairBatch(3, 2, func(i int) batchSandbox {
			time.Sleep([]time.Duration{3 * time.Second, time.Second, 500 * time.Millisecond}[i])
			return batchSandbox{name: fmt.Sprintf("ts-%d", i+1)}
		}, func(item *batchSandbox) {
			order = append(order, item.name)
			if len(order) == 1 && time.Since(start) != time.Second {
				t.Errorf("first pairing waited %v for provisioning", time.Since(start))
			}
			time.Sleep(2 * time.Second)
			item.proof.DesktopConfirmed = true
		})
		if !slices.Equal(order, []string{"ts-2", "ts-3", "ts-1"}) || time.Since(start) != 7*time.Second {
			t.Fatalf("pairing order %v after %v", order, time.Since(start))
		}
		for i, item := range results {
			if item.name != fmt.Sprintf("ts-%d", i+1) || !item.proof.DesktopConfirmed {
				t.Fatalf("lost input order or approval for result %d: %+v", i, item)
			}
		}
	})
}
