package main

import (
	"bytes"
	"context"
	"crypto/tls"
	json "encoding/json/v2"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	"github.com/MarioGK/t3-agent37/internal/peers"
)

func writeWorkspaceFixture(t *testing.T) workspacePlaybook {
	t.Helper()
	playbook := workspacePlaybook{
		Path:     "~/Projects/Example Workspace",
		RootRepo: "example-org/example-workspace",
		Repositories: []workspaceRepository{
			{Repo: "example-org/example-app"},
			{Repo: "example-org/example-tools", Ref: "main", Directory: "tools"},
		},
	}
	filename, err := workstationConfigPath(workspacePlaybookFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(playbook)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return playbook
}

func TestWorkspacePreviewAndOptOut(t *testing.T) {
	t.Setenv("AGENT37_API_KEY", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	playbook := writeWorkspaceFixture(t)
	var output bytes.Buffer
	args := []string{"up", "--dry-run", "--template", "agent37-base"}
	if err := run(t.Context(), args, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	var preview struct {
		Workspace workspacePlaybook `json:"workspace"`
	}
	if err := json.Unmarshal(output.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Workspace.Path != playbook.Path || preview.Workspace.RootRepo != playbook.RootRepo ||
		!slices.Equal(preview.Workspace.Repositories, playbook.Repositories) {
		t.Fatalf("unexpected workspace preview: %s", output.String())
	}

	opts, err := parse(append(args, "--workspace=false"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := opts.loadWorkspacePlaybook(nil); err != nil {
		t.Fatal(err)
	}
	payload := previewCreatePayload(opts)
	metadata := payload["metadata"].(map[string]string)
	if payload["workspace"] != nil || metadata[workspaceDisabledKey] != "true" {
		t.Fatalf("workspace opt-out was not saved: %v", payload)
	}
	for _, tc := range []struct {
		command []string
		want    bool
	}{
		{[]string{"up", "agent-1"}, false},
		{[]string{"up", "agent-1", "--workspace=true"}, true},
		{[]string{"start", "agent-1"}, false},
	} {
		resume, err := parse(tc.command, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		err = resume.loadWorkspacePlaybook(metadata)
		if err != nil || (resume.playbook != nil) != tc.want {
			t.Errorf("%v with saved opt-out, want workspace %t, got %v, %v", tc.command, tc.want, resume.playbook, err)
		}
	}
}

func TestWorkspacePlaybookIsOptionalAndValidatedBeforeProvisioning(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("AGENT37_API_KEY", "")
	args := []string{"up", "--dry-run", "--template", "agent37-base"}
	var output bytes.Buffer
	if err := run(t.Context(), args, &output, io.Discard); err != nil || strings.Contains(output.String(), `"workspace"`) {
		t.Fatalf("default launch without a playbook: %v, %s", err, output.String())
	}
	if err := run(t.Context(), append(args, "--workspace=true"), io.Discard, io.Discard); !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), workspacePlaybookFile) {
		t.Fatalf("explicit workspace without a file returned %v", err)
	}
	writeWorkspaceFixture(t)
	filename, err := workstationConfigPath(workspacePlaybookFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{
		`{"path":"~/../outside","repositories":[{"repo":"example-org/app"}]}`,
		`{"path":"~/Projects","repositories":[{"repo":"https://github.com/example-org/app"}]}`,
		`{"path":"~/Projects","repositories":[{"repo":"example-org/app"},{"repo":"another-org/app"}]}`,
	} {
		if err := os.WriteFile(filename, []byte(entry), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := run(t.Context(), args, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "invalid workspace playbook") {
			t.Errorf("playbook %s returned %v", entry, err)
		}
	}
	if err := run(t.Context(), append(args, "--workspace=false"), io.Discard, io.Discard); err != nil {
		t.Fatalf("workspace opt-out read the invalid file: %v", err)
	}
}

func TestBatchWorkspaceFailureCanResumeByName(t *testing.T) {
	t.Setenv("T3_AGENT37_PEERS_FILE", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("AGENT37_API_KEY", "fixture")
	t.Setenv("T3_AGENT37_GITHUB_TOKEN", "fixture-workspace-token")
	playbook := writeWorkspaceFixture(t)
	const firstID, secondID = "aaaaaaaaaa", "bbbbbbbbbb"
	type fixtureSandbox struct {
		instance       agent37.Instance
		lastSpec       launchSpec
		upCalls        int
		workspaceReady bool
		joined         bool
	}
	sandboxes := map[string]*fixtureSandbox{}
	var mu sync.Mutex
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		write := func(value any) {
			w.Header().Set("Content-Type", "application/json")
			data, err := json.Marshal(value)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(data)
		}
		execResult := func(value any) {
			data, _ := json.Marshal(value)
			write(map[string]any{"exit_code": 0, "stdout": string(data)})
		}
		switch {
		case r.URL.Path == "/v1/instances" && r.Method == http.MethodGet:
			active := make([]agent37.Instance, 0, len(sandboxes))
			for _, sandbox := range sandboxes {
				active = append(active, sandbox.instance)
			}
			write(map[string]any{"data": active})
		case r.URL.Path == "/v1/instances" && r.Method == http.MethodPost:
			var instance agent37.Instance
			data, err := io.ReadAll(r.Body)
			if err != nil || json.Unmarshal(data, &instance) != nil {
				t.Error("invalid create payload")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if bytes.Contains(data, []byte("example-org/")) {
				t.Error("the instance create request included the local repository list")
			}
			if !requiresWorkspaceSetup(instance) {
				t.Error("the instance was discoverable before the workspace finished")
			}
			instance.ID, instance.Status = firstID, "running"
			if instance.Name == "agent-2" {
				instance.ID = secondID
			}
			if sandboxes[instance.ID] != nil {
				t.Error("retry created a second sandbox")
			}
			sandboxes[instance.ID] = &fixtureSandbox{instance: instance}
			write(instance)
		case r.URL.Path == "/.well-known/t3/environment":
			if !strings.HasSuffix(r.Host, ".ts.net") {
				t.Error("workspace setup waited for the unused protected T3 route")
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			id, _, _ := strings.Cut(strings.TrimPrefix(r.Host, "a37-agent-"), ".")
			_, id, _ = strings.Cut(id, "-")
			write(map[string]string{"environmentId": id})
		case r.Method == http.MethodPut && r.URL.Path == "/v1/files/content":
			id, _, _ := strings.Cut(r.Host, ".")
			sandbox := sandboxes[id]
			if sandbox == nil || !strings.HasSuffix(r.URL.Query().Get("path"), "/spec.json") {
				t.Error("unexpected launch spec upload")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			data, _ := io.ReadAll(r.Body)
			var spec launchSpec
			if err := json.Unmarshal(data, &spec); err != nil {
				t.Errorf("invalid launch spec JSON: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			sandbox.lastSpec = spec
			write(map[string]any{})
		default:
			id, action, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v1/instances/"), "/")
			sandbox := sandboxes[id]
			if sandbox == nil {
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if r.Method == http.MethodGet && action == "" {
				write(sandbox.instance)
				return
			}
			var request struct {
				Command string `json:"command"`
			}
			data, _ := io.ReadAll(r.Body)
			if action != "exec" || json.Unmarshal(data, &request) != nil {
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			name := sandbox.instance.Name
			origin := "https://a37-" + name + "-" + id + ".fixture.ts.net"
			switch {
			case strings.Contains(request.Command, "gzip.decompress"):
				execResult(nil)
			case strings.Contains(request.Command, " up launch-"):
				spec := sandbox.lastSpec
				if spec.RequireWorkspaceReady && !sandbox.workspaceReady {
					execResult(setupProof{WorkspacePending: true})
					return
				}
				sandbox.upCalls++
				sandbox.joined = true
				proof := setupProof{
					EnvironmentID: id, Label: name, Port: 3773, ProviderReady: true, PeerProxy: true,
					TailnetURL: origin, TailnetHostname: "a37-" + name + "-" + id, TailnetID: id,
					GitHub: spec.GitHub != nil && spec.GitHub.Token != "",
				}
				if spec.Workspace != nil {
					if spec.Workspace.Path != playbook.Path || spec.Workspace.RootRepo != playbook.RootRepo {
						t.Errorf("unexpected workspace spec: %+v", spec.Workspace)
					}
					if id == firstID && sandbox.upCalls == 1 {
						proof.WorkspaceErrors = []workspaceCloneFailure{{
							Status: "clone_failed", Repo: "example-org/example-tools", Folder: "tools",
						}}
					} else {
						sandbox.workspaceReady = true
						proof.WorkspaceReady = true
					}
				}
				execResult(proof)
			case strings.Contains(request.Command, " inspect "):
				if !strings.Contains(request.Command, shellQuote(workspaceReadyFile)) {
					t.Error("discovery bypassed the workspace readiness check")
				}
				if !sandbox.workspaceReady {
					execResult(sandboxPeer{WorkspacePending: true})
					return
				}
				proof := sandboxPeer{Offline: !sandbox.joined, PeerProxy: true}
				if sandbox.joined {
					proof.EnvironmentID, proof.URL = id, origin
				}
				execResult(proof)
			case strings.Contains(request.Command, " env list --json"):
				execResult([]any{})
			default:
				t.Errorf("unexpected guest command: %s", request.Command)
				w.WriteHeader(http.StatusBadRequest)
			}
		}
	}))
	t.Cleanup(server.Close)
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.Proxy = nil
	dialer := &tls.Dialer{Config: transport.TLSClientConfig}
	transport.DialTLSContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, server.Listener.Addr().String())
	}
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = transport

	opts, err := parse([]string{"up", "2", "--no-desktop", "--template", "agent37-base", "--provider", "codex"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := opts.loadWorkspacePlaybook(nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var output, diagnostic bytes.Buffer
	state, err := statePath()
	if err != nil {
		t.Fatal(err)
	}
	progress := startLaunchProgress(ctx, &diagnostic, opts)
	err = runBatch(ctx, agent37.New("fixture"), func(context.Context) (*peers.Enrollment, error) { return nil, nil }, opts, state, &output, progress)
	if err == nil || !strings.Contains(err.Error(), "could not clone example-org/example-tools") ||
		!strings.Contains(diagnostic.String(), "agent-2 Workspace ready in T3 Code") ||
		strings.Contains(output.String(), "[3/3]") {
		t.Fatalf("unexpected batch result: %v\nstdout: %s\nstderr: %s", err, output.String(), diagnostic.String())
	}
	var batchResult struct {
		MeshPending bool `json:"mesh_pending"`
	}
	if err := json.Unmarshal(output.Bytes(), &batchResult); err != nil || batchResult.MeshPending {
		t.Fatalf("the incomplete workspace interfered with peer sync: %v, %s", err, output.String())
	}
	output.Reset()
	diagnostic.Reset()
	if err := run(ctx, []string{"connect", "agent-1", "--pair"}, &output, &diagnostic); err == nil ||
		!strings.Contains(err.Error(), "finish workspace setup with t3-agent37 up agent-1") || output.Len() != 0 {
		t.Fatalf("manual pairing bypassed cloning: %v, %s", err, output.String())
	}
	if err := run(ctx, []string{"up", "agent-1"}, &output, &diagnostic); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), firstID) ||
		!strings.Contains(diagnostic.String(), "agent-1 Workspace ready in T3 Code") {
		t.Fatalf("retry did not finish the same sandbox: %s\n%s", output.String(), diagnostic.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if !sandboxes[firstID].workspaceReady || !sandboxes[secondID].workspaceReady {
		t.Fatal("both sandboxes must have verified workspace registrations after retry")
	}
}
