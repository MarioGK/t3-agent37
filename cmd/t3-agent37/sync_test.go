package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	"github.com/MarioGK/t3-agent37/internal/peers"
)

func TestSyncRequiresConfirmedStaleSandbox(t *testing.T) {
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	link := peers.SandboxLink{Peer: "dev", Name: "terra-2", SandboxID: "ab12cd34ef", Status: "unreachable"}
	for _, tc := range []struct {
		name       string
		apiStatus  int
		instance   string
		linkStatus string
		wantStale  bool
		wantError  bool
		record     bool
	}{
		{name: "stopped", instance: "stopped", wantStale: true},
		{name: "deleted", instance: "deleted", wantStale: true},
		{name: "sleeping", instance: "sleeping"},
		{name: "running during network outage", instance: "running"},
		{name: "missing from the current workspace", apiStatus: http.StatusNotFound},
		{name: "confirmed local deletion", apiStatus: http.StatusNotFound, record: true, wantStale: true},
		{name: "reachable despite missing from current workspace", apiStatus: http.StatusNotFound, linkStatus: "reachable"},
		{name: "unauthorized", apiStatus: http.StatusUnauthorized, wantError: true},
		{name: "service outage", apiStatus: http.StatusServiceUnavailable, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			if tc.record {
				if err := recordDeletedSandbox(t.Context(), state, downReceipt{ID: link.SandboxID, Name: link.Name, Status: "deleted"}); err != nil {
					t.Fatal(err)
				}
			}
			http.DefaultTransport = downTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/instances/ab12cd34ef" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				if tc.apiStatus != 0 {
					return &http.Response{StatusCode: tc.apiStatus, Body: io.NopCloser(strings.NewReader("{}"))}, nil
				}
				row := fmt.Sprintf(`{"id":"ab12cd34ef","status":%q,"metadata":{"managed_by":"t3-agent37","environment":"terra-2","tailnet":"tailscale"}}`, tc.instance)
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(row))}, nil
			})
			candidate := link
			if tc.linkStatus != "" {
				candidate.Status = tc.linkStatus
			}
			stale, err := staleSandboxLink(t.Context(), agent37.New("fixture"), candidate, state)
			if stale != tc.wantStale || (err != nil) != tc.wantError {
				t.Fatalf("stale = %t, error = %v", stale, err)
			}
		})
	}
}

func TestCleanupUsesCloudOwnerCLIWithoutDesktopPairing(t *testing.T) {
	peerFile := filepath.Join(t.TempDir(), "invalid-peers.json")
	if err := os.WriteFile(peerFile, []byte(`broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("T3_AGENT37_PEERS_FILE", peerFile)
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	var unlinked bool
	http.DefaultTransport = downTestTransport(func(r *http.Request) (*http.Response, error) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/instances":
			return downResponse(200, `{"data":[{"id":"cccccccccc","status":"running","metadata":{"managed_by":"t3-agent37","environment":"survivor","tailnet":"tailscale","desktop_confirmation":"required"}}]}`), nil
		case "POST /v1/instances/cccccccccc/exec":
			body, _ := io.ReadAll(r.Body)
			var stdout string
			switch {
			case strings.Contains(string(body), " unlink "):
				unlinked = true
			case strings.Contains(string(body), " list --json"):
				stdout = `[{"environmentId":"gone-env","urls":["https://a37-gone-aaaaaaaaaa.example.ts.net"]}]`
			default:
				stdout = `{"url":"https://a37-survivor-cccccccccc.example.ts.net","peerProxy":false,"offline":true,"desktopConfirmed":false}`
			}
			return downResponse(200, fmt.Sprintf(`{"exit_code":0,"stdout":%q}`, stdout)), nil
		default:
			return nil, fmt.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	if err := unlinkSandbox(t.Context(), agent37.New("fixture"), "gone", "aaaaaaaaaa", io.Discard); err == nil || !unlinked {
		t.Fatalf("cloud cleanup did not continue after the fleet file failed: %t, %v", unlinked, err)
	}
}

func TestSyncPreviewDoesNotRunGuestExec(t *testing.T) {
	override := filepath.Join(t.TempDir(), "peers.json")
	if err := os.WriteFile(override, []byte(`[]`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("T3_AGENT37_PEERS_FILE", override)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = downTestTransport(func(r *http.Request) (*http.Response, error) {
		t.Errorf("preview sent unexpected request %s %s", r.Method, r.URL.Path)
		return nil, fmt.Errorf("unexpected request")
	})
	if err := syncSandboxLinks(t.Context(), agent37.New("fixture"), true, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}
