package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MarioGK/t3-agent37/internal/agent37"
)

type downTestTransport func(*http.Request) (*http.Response, error)

func (f downTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestBareDownPinsNewestManagedInstance(t *testing.T) {
	t.Setenv("AGENT37_API_KEY", "fixture-only")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const older = `{"id":"aaaaaaaaaa","name":"z-older","status":"running","created":100,"metadata":{"managed_by":"t3-agent37","environment":"z-older"}}`
	const newer = `{"id":"bbbbbbbbbb","name":"a-newer","status":"running","created":200,"metadata":{"managed_by":"t3-agent37","environment":"a-newer"}}`
	const unmanaged = `{"id":"cccccccccc","status":"running","created":300}`
	const deleted = `{"id":"dddddddddd","status":"deleted","created":400,"metadata":{"managed_by":"t3-agent37","environment":"old-deleted"}}`
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	calls := []string{}
	http.DefaultTransport = downTestTransport(func(r *http.Request) (*http.Response, error) {
		call := r.Method + " " + r.URL.Path
		calls = append(calls, call)
		body := ""
		status := http.StatusOK
		switch call {
		case "GET /v1/instances":
			body = `{"data":[` + strings.Join([]string{newer, unmanaged, older, deleted}, ",") + `]}`
		case "GET /v1/instances/bbbbbbbbbb":
			body = newer
		case "POST /v1/instances/bbbbbbbbbb/stop":
			body = `{"id":"bbbbbbbbbb","status":"stopped"}`
		case "DELETE /v1/instances/bbbbbbbbbb":
			status = http.StatusNoContent
		default:
			return nil, fmt.Errorf("unexpected request %s", call)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	ctx := context.Background()
	var output bytes.Buffer
	var diagnostic bytes.Buffer
	if err := run(ctx, []string{"down", "--dry-run"}, &output, &diagnostic); err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(output.Bytes(), &row); err != nil || row["id"] != "bbbbbbbbbb" || row["name"] != "a-newer" ||
		row["action"] != "delete" || len(calls) != 1 {
		t.Fatalf("wrong preview or side effects: %v, %v, %v", row, calls, err)
	}
	output.Reset()
	if err := run(ctx, []string{"down"}, &output, &diagnostic); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(output.Bytes(), &row); err != nil || row["id"] != "bbbbbbbbbb" || row["status"] != "deleted" || len(calls) != 5 {
		t.Fatalf("wrong deletion: %v, %v, %v", row, calls, err)
	}
}

func TestLatestManagedRefusesAmbiguousCreationTime(t *testing.T) {
	instance := agent37.Instance{
		ID: "aaaaaaaaaa", Created: 100, Status: "stopped",
		Metadata: map[string]string{"managed_by": owner, "environment": "example"},
	}
	other := instance
	other.ID = "bbbbbbbbbb"
	if _, err := latestManaged([]agent37.Instance{instance, other}); err == nil {
		t.Fatal("accepted two newest sandboxes with the same timestamp")
	}
	other.Created = 0
	if _, err := latestManaged([]agent37.Instance{instance, other}); err == nil {
		t.Fatal("accepted a managed sandbox with no timestamp")
	}
}
