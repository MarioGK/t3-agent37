package main

import (
	"bytes"
	json "encoding/json/v2"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/MarioGK/t3-agent37/internal/agent37"
)

type githubRoundTripFunc func(*http.Request) (*http.Response, error)

func (transport githubRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestLaunchSpecSendsCredentialsOnlyInPrivateUpload(t *testing.T) {
	const fixtureToken = "fixture-gh-access-value"
	const fixtureTailKey = "tskey-auth-fixture-secret"
	t.Setenv("T3_AGENT37_GITHUB_TOKEN", fixtureToken)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	handoff, err := readGitHubHandoff(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	actions := []string{"stage", "upload-retry", "upload", "up"}
	stagePattern := regexp.MustCompile(`launch-[a-f0-9]{32}`)
	var stage string
	var count int
	http.DefaultTransport = githubRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal("could not read fixture request")
		}
		if count == len(actions) {
			t.Fatal("unexpected launch request")
		}
		action := actions[count]
		count++
		if request.Header.Get("X-Agent37-Key") != "fixture-workspace-key" ||
			request.Header.Get("Authorization") != "" ||
			strings.Contains(request.URL.String(), fixtureToken) ||
			strings.Contains(request.URL.String(), fixtureTailKey) {
			t.Fatal("unexpected request authentication or credential disclosure")
		}
		response := `{"exit_code":0,"stdout":"{\"environmentId\":\"env-1\",\"port\":3773,\"github\":true,\"gitIdentity\":false}"}`
		status := http.StatusOK
		switch action {
		case "upload-retry", "upload":
			var spec launchSpec
			if err := json.Unmarshal(body, &spec); err != nil ||
				spec.GitHub == nil || spec.GitHub.Token != fixtureToken ||
				spec.Tailscale == nil || spec.Tailscale.AuthKey != fixtureTailKey ||
				request.URL.Host != "ab12cd34ef.agent37.app" || request.Method != http.MethodPut ||
				request.URL.Query().Get("path") != "~/.local/share/t3-agent37/"+stage+"/spec.json" ||
				request.URL.Query().Get("overwrite") != "false" {
				t.Fatal("launch spec upload did not use the prepared private file")
			}
			response = ""
			if action == "upload-retry" {
				status = http.StatusBadGateway
				response = `{"error":"container_unreachable"}`
			}
		default:
			var payload struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(body, &payload); err != nil ||
				strings.Contains(payload.Command, fixtureToken) ||
				strings.Contains(payload.Command, fixtureTailKey) {
				t.Fatal("exec command must carry no credentials")
			}
			found := stagePattern.FindString(payload.Command)
			if found == "" || (stage != "" && found != stage) {
				t.Fatalf("exec command %q did not reference stage %q", payload.Command, stage)
			}
			stage = found
			if action == "stage" {
				response = `{"exit_code":0,"stdout":""}`
			}
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
	})

	proof, err := runLaunchSpec[setupProof](t.Context(), agent37.New("fixture-workspace-key"), "ab12cd34ef", "up", launchSpec{
		Provider:  "codex",
		Name:      "ts-1",
		GitHub:    &handoff,
		Tailscale: &tailscaleSpec{Hostname: "a37-ts-1-ab12cd34ef", AuthKey: fixtureTailKey},
	})
	if err != nil || !proof.GitHub || proof.GitIdentity || count != len(actions) {
		t.Fatalf("launch result=%+v count=%d err=%v", proof, count, err)
	}
}

func TestCLIProxyCatalogPreflightFiltersMediaAndValidatesDefault(t *testing.T) {
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = githubRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.String() != "https://proxy.example.com/v1/models" ||
			request.Header.Get("Authorization") != "Bearer secret-key" {
			t.Fatalf("unexpected CLIProxy request %s %s", request.Method, request.URL)
		}
		body := `{"data":[{"id":"glm-5.3"},{"id":"gpt-6.2-sol"},{"id":"gpt-image-1"},{"id":"grok-tts-1"}]}`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(body))}, nil
	})
	catalog, err := fetchCLIProxyCatalog(t.Context(), map[string]string{
		proxyURL: "https://proxy.example.com",
		proxyKey: "secret-key",
	})
	if err != nil || !catalog.HasSol || len(catalog.Models) != 2 {
		t.Fatalf("catalog = %+v, err = %v", catalog, err)
	}
	if _, err := fetchCLIProxyCatalog(t.Context(), map[string]string{
		proxyURL:   "https://proxy.example.com",
		proxyKey:   "secret-key",
		proxyModel: "missing-model",
	}); err == nil || !strings.Contains(err.Error(), "absent from the catalog") {
		t.Fatalf("expected missing model error, got %v", err)
	}
}

func TestGitHubHandoffIgnoresMissingWorkstationTools(t *testing.T) {
	t.Setenv("T3_AGENT37_GITHUB_TOKEN", "")
	t.Setenv("PATH", t.TempDir())
	handoff, err := readGitHubHandoff(t.Context())
	if err != nil || handoff != (githubHandoff{}) {
		t.Fatalf("missing workstation tools handoff=%+v err=%v", handoff, err)
	}
}

func TestWorkstationGitHubRejectsInvalidOverride(t *testing.T) {
	t.Setenv("T3_AGENT37_GITHUB_TOKEN", "fixture-sentinel\nnot-a-token")
	if _, err := workstationGitHubToken(t.Context()); err == nil || strings.Contains(err.Error(), "fixture-sentinel") {
		t.Fatal("invalid token was accepted or included in an error")
	}
}

func TestGitHubLoginMissingDistinguishesToolFailures(t *testing.T) {
	if !githubLoginMissing(&exec.ExitError{Stderr: []byte("no oauth token found for github.com\n")}) {
		t.Fatal("missing login was treated as a tool failure")
	}
	if githubLoginMissing(&exec.ExitError{Stderr: []byte("mise ERROR configuration is untrusted\n")}) {
		t.Fatal("tool failure was treated as a missing login")
	}
}
