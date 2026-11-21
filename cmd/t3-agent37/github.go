package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/MarioGK/t3-agent37/internal/agent37"
)

//go:embed guest/github.py
var guestGitHub string

var githubTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

type githubAccessResult struct {
	GitHub      bool `json:"github"`
	GitIdentity bool `json:"gitIdentity"`
}

type githubHandoff struct {
	Token string `json:"token,omitempty"`
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

// inheritGitHub applies the local GitHub.com login and Git author defaults
// without placing credentials in the guest exec command or instance metadata.
func inheritGitHub(ctx context.Context, client *agent37.Client, id string) (githubAccessResult, error) {
	var result githubAccessResult
	token, err := workstationGitHubToken(ctx)
	if err != nil {
		return result, err
	}
	name, email := workstationGitIdentity(ctx)
	if token == "" && name == "" && email == "" {
		return result, ctx.Err()
	}
	payload, err := json.Marshal(githubHandoff{Token: token, Name: name, Email: email})
	if err != nil {
		return result, errors.New("could not encode GitHub access handoff")
	}
	defer clear(payload)

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return result, err
	}
	stage := "github-handoff-" + hex.EncodeToString(nonce[:])
	command := "python3 -c " + shellQuote(guestGitHub) + " " + stage
	applied := false
	defer func() {
		if !applied {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_, _ = client.Exec(cleanup, id, command+" cleanup")
		}
	}()

	if _, err := client.Exec(ctx, id, command+" prepare"); err != nil {
		return result, fmt.Errorf("could not prepare private GitHub handoff: %w", err)
	}
	if err := uploadGitHubHandoff(ctx, client, id, stage, payload); err != nil {
		return result, fmt.Errorf("could not deliver private GitHub handoff: %w", err)
	}
	result, err = client.ExecJSON[githubAccessResult](ctx, id, command+" apply")
	if err != nil {
		return result, fmt.Errorf("could not set up sandbox GitHub access: %w", err)
	}
	applied = true
	return result, nil
}

func uploadGitHubHandoff(ctx context.Context, client *agent37.Client, id, stage string, payload []byte) error {
	return uploadGuestFile(ctx, func() error { return client.UploadGitHubHandoff(ctx, id, stage, payload) })
}

func uploadGuestFile(ctx context.Context, upload func() error) error {
	for delay := time.Second; ; delay *= 2 {
		err := upload()
		apiError, ok := errors.AsType[*agent37.Error](err)
		if !ok || apiError.Status != http.StatusBadGateway || apiError.Code != "container_unreachable" || delay > 8*time.Second {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

func workstationGitHubToken(ctx context.Context) (string, error) {
	token := strings.TrimSpace(os.Getenv("T3_AGENT37_GITHUB_TOKEN"))
	if token == "" {
		lookup, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		gh := exec.CommandContext(lookup, "gh", "auth", "token", "--hostname", "github.com")
		gh.Env = slices.DeleteFunc(os.Environ(), func(entry string) bool {
			key, _, _ := strings.Cut(entry, "=")
			switch strings.ToUpper(key) {
			case "GH_TOKEN", "GITHUB_TOKEN", "GH_DEBUG", "DEBUG":
				return true
			}
			return false
		})
		gh.Env = append(gh.Env, "GH_PROMPT_DISABLED=1")
		stdout, err := gh.Output()
		defer clear(stdout)
		if lookup.Err() != nil {
			return "", errors.New("GitHub login lookup did not complete")
		}
		if githubLoginMissing(err) {
			return "", nil
		}
		if err != nil {
			return "", errors.New("GitHub CLI failed to read its login. Check gh auth status --hostname github.com")
		}
		token = strings.TrimSpace(string(stdout))
	}
	if token != "" && (len(token) > 4096 || !githubTokenPattern.MatchString(token)) {
		return "", errors.New("GitHub login returned an invalid token format")
	}
	return token, nil
}

func githubLoginMissing(err error) bool {
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	exit, ok := errors.AsType[*exec.ExitError](err)
	return ok && strings.TrimSpace(string(exit.Stderr)) == "no oauth token found for github.com"
}

func workstationGitIdentity(ctx context.Context) (name, email string) {
	lookup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	read := func(key string) string {
		data, err := exec.CommandContext(lookup, "git", "config", "--global", "--get", key).Output()
		value := strings.TrimSpace(string(data))
		if err != nil || len(value) > 512 || !utf8.ValidString(value) || strings.ContainsFunc(value, unicode.IsControl) {
			return ""
		}
		return value
	}
	return read("user.name"), read("user.email")
}
