package main

import (
	"fmt"
	"io"
	"net/http"
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
	}{
		{name: "stopped", instance: "stopped", wantStale: true},
		{name: "deleted", instance: "deleted", wantStale: true},
		{name: "sleeping", instance: "sleeping"},
		{name: "running during network outage", instance: "running"},
		{name: "missing", apiStatus: http.StatusNotFound, wantStale: true},
		{name: "reachable despite missing from current workspace", apiStatus: http.StatusNotFound, linkStatus: "reachable"},
		{name: "unauthorized", apiStatus: http.StatusUnauthorized, wantError: true},
		{name: "service outage", apiStatus: http.StatusServiceUnavailable, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			stale, err := staleSandboxLink(t.Context(), agent37.New("fixture"), candidate)
			if stale != tc.wantStale || (err != nil) != tc.wantError {
				t.Fatalf("stale = %t, error = %v", stale, err)
			}
		})
	}
}
