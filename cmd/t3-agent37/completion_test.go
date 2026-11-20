package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestCompleteCommandsAndFlags(t *testing.T) {
	root := completionCandidates(t, "")
	for _, command := range commands {
		if !slices.Contains(root, command.name) {
			t.Errorf("root completion is missing %s", command.name)
		}
	}
	for _, tc := range []struct {
		args    []string
		present string
		absent  string
	}{
		{args: []string{"setup", "--repair-tailscale", "--"}, present: "--dry-run", absent: "--verify"},
		{args: []string{"setup", "--check", "--"}, present: "--no-browser", absent: "--dry-run"},
		{args: []string{"up", "--"}, present: "--no-desktop", absent: "--yes"},
		{args: []string{"up", "ts-1", "--"}, present: "--help", absent: "--provider"},
		{args: []string{"up", "--provider", ""}, present: "cliproxy"},
		{args: []string{"connect", "ts-1", "--"}, present: "--pair"},
		{args: []string{"sync", "--"}, present: "--dry-run", absent: "--no-desktop"},
		{args: []string{"completion", ""}, present: "fish"},
	} {
		got := completionCandidates(t, tc.args...)
		if !slices.Contains(got, tc.present) || slices.Contains(got, tc.absent) {
			t.Errorf("completing %v returned %v", tc.args, got)
		}
	}
}

func TestCompleteSandboxNamesAndTemplateRevisions(t *testing.T) {
	t.Setenv("AGENT37_API_KEY", "fixture-only")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	http.DefaultTransport = downTestTransport(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/instances":
			body = `{"data":[
				{"status":"running","metadata":{"managed_by":"t3-agent37","environment":"agent-1"}},
				{"status":"stopped","metadata":{"managed_by":"t3-agent37","environment":"agent-2"}},
				{"status":"deleted","metadata":{"managed_by":"t3-agent37","environment":"agent-3"}},
				{"status":"running","metadata":{"environment":"agent-4"}}
			]}`
		case "GET /v1/templates":
			body = `{"data":[{"name":"terra-agent","scope":"workspace","revision":4},{"name":"agent37-base","scope":"system","revision":null}]}`
		case "GET /v1/templates/terra-agent":
			body = `{"name":"terra-agent","scope":"workspace","revision":4,"revisions":[{"revision":4},{"revision":2}]}`
		default:
			t.Errorf("completion made unexpected request %s %s", r.Method, r.URL.Path)
			return nil, fmt.Errorf("unexpected request")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	for _, tc := range []struct{ args, want []string }{
		{args: []string{"delete", "agent-"}, want: []string{"agent-1", "agent-2"}},
		{args: []string{"create", "agent-"}, want: []string{"agent-3"}},
		{args: []string{"up", "--no-desktop", ""}},
		{args: []string{"down", "agent-1", ""}},
		{args: []string{"up", "--template", ""}, want: []string{"terra-agent", "terra-agent@4"}},
		{args: []string{"up", "--template", "terra-agent@"}, want: []string{"terra-agent@2", "terra-agent@4"}},
	} {
		if got := completionCandidates(t, tc.args...); !slices.Equal(got, tc.want) {
			t.Errorf("completing %v returned %v, want %v", tc.args, got, tc.want)
		}
	}
}

func completionCandidates(t *testing.T, args ...string) []string {
	t.Helper()
	var output bytes.Buffer
	if err := run(t.Context(), append([]string{"__completeNoDesc"}, args...), &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	candidates, ok := strings.CutSuffix(output.String(), ":4\n")
	if !ok {
		t.Fatalf("unexpected completion output %q", output.String())
	}
	return strings.Fields(candidates)
}
