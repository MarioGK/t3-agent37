package main

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/MarioGK/t3-agent37/internal/agent37"
)

type githubRoundTripFunc func(*http.Request) (*http.Response, error)

func (transport githubRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestGitHubHandoffSendsCredentialsOnlyInThePrivateUpload(t *testing.T) {
	const fixtureToken = "fixture-gh-access-value"
	t.Setenv("T3_AGENT37_GITHUB_TOKEN", fixtureToken)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	actions := []string{"prepare", "upload", "upload", "apply"}
	commandSuffix := regexp.MustCompile(` (github-handoff-[a-f0-9]{32}) (prepare|apply)$`)
	var stage string
	var count int
	http.DefaultTransport = githubRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal("could not read fixture request")
		}
		if count == len(actions) {
			t.Fatal("unexpected handoff request")
		}
		action := actions[count]
		count++
		if request.Header.Get("X-Agent37-Key") != "fixture-workspace-key" ||
			request.Header.Get("Authorization") != "" || strings.Contains(request.URL.String(), fixtureToken) {
			t.Fatal("unexpected handoff request authentication or disclosure")
		}
		response := `{"exit_code":0,"stdout":"{\"github\":true,\"gitIdentity\":false}"}`
		status := http.StatusOK
		if action == "upload" {
			var record githubHandoff
			if err := json.Unmarshal(body, &record); err != nil || record.Token != fixtureToken ||
				request.URL.Host != "ab12cd34ef.agent37.app" || request.Method != http.MethodPut ||
				request.URL.Query().Get("path") != "~/.local/share/t3-agent37/"+stage+"/access.json" ||
				request.URL.Query().Get("overwrite") != "false" {
				t.Fatal("handoff upload did not use the prepared private file")
			}
			response = ""
			if count == 2 {
				status = http.StatusBadGateway
				response = `{"error":"container_unreachable"}`
			}
		} else {
			var payload struct{ Command string }
			if err := json.Unmarshal(body, &payload); err != nil || strings.Contains(payload.Command, fixtureToken) {
				t.Fatal("handoff exec command must carry no token")
			}
			match := commandSuffix.FindStringSubmatch(payload.Command)
			if len(match) != 3 || match[2] != action || stage != "" && match[1] != stage {
				t.Fatal("handoff command did not use the expected action and private directory")
			}
			stage = match[1]
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
	})
	result, err := inheritGitHub(t.Context(), agent37.New("fixture-workspace-key"), "ab12cd34ef")
	if err != nil || !result.GitHub || result.GitIdentity || count != len(actions) {
		t.Fatalf("handoff result=%+v count=%d err=%v", result, count, err)
	}
}

func TestGitHubHandoffIgnoresMissingWorkstationTools(t *testing.T) {
	t.Setenv("T3_AGENT37_GITHUB_TOKEN", "")
	t.Setenv("PATH", t.TempDir())
	result, err := inheritGitHub(t.Context(), agent37.New("fixture-workspace-key"), "invalid-instance")
	if err != nil || result != (githubAccessResult{}) {
		t.Fatalf("missing workstation tools result=%+v err=%v", result, err)
	}
}

func TestWorkstationGitHubRejectsInvalidOverride(t *testing.T) {
	t.Setenv("T3_AGENT37_GITHUB_TOKEN", "fixture-sentinel\nnot-a-token")
	if _, err := workstationGitHubToken(t.Context()); err == nil || strings.Contains(err.Error(), "fixture-sentinel") {
		t.Fatal("invalid token was accepted or included in an error")
	}
}
