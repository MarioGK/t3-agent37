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
	"strings"
	"testing"

	"github.com/MarioGK/t3-agent37/internal/agent37"
)

type downTestTransport func(*http.Request) (*http.Response, error)

func (f downTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestDeletionCommandsSelectManagedTargets(t *testing.T) {
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
		case "POST /v1/instances/aaaaaaaaaa/stop":
			body = `{"id":"aaaaaaaaaa","status":"stopped"}`
		case "DELETE /v1/instances/aaaaaaaaaa":
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
	output.Reset()
	if err := run(ctx, []string{"delete", "z-older", "--dry-run"}, &output, &diagnostic); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(output.Bytes(), &row); err != nil || row["id"] != "aaaaaaaaaa" || row["action"] != "delete" || len(calls) != 6 {
		t.Fatalf("wrong named preview: %v, %v, %v", row, calls, err)
	}
	output.Reset()
	if err := run(ctx, []string{"delete", "z-older"}, &output, &diagnostic); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(output.Bytes(), &row); err != nil || row["id"] != "aaaaaaaaaa" || row["status"] != "deleted" || len(calls) != 9 {
		t.Fatalf("wrong named deletion: %v, %v, %v", row, calls, err)
	}
}

func TestBareDeleteUsesPickerInATerminal(t *testing.T) {
	opts, err := parse([]string{"delete"}, io.Discard)
	if err != nil || !opts.pickDelete || !opts.deleteSandbox {
		t.Fatalf("bare delete = %+v, %v", opts, err)
	}
	err = run(context.Background(), []string{"delete"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "requires a terminal") {
		t.Fatalf("bare delete with redirected input/output = %v", err)
	}
	if _, err := parse([]string{"delete", "two", "names"}, io.Discard); err == nil {
		t.Fatal("delete accepted multiple names")
	}
}

func TestDownRetriesExactTargetAfterLostDeleteResponse(t *testing.T) {
	t.Setenv("AGENT37_API_KEY", "fixture-only")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const older = `{"id":"aaaaaaaaaa","status":"stopped","created":100,"metadata":{"managed_by":"t3-agent37","environment":"ts-1"}}`
	const target = `{"id":"bbbbbbbbbb","status":"stopped","created":200,"metadata":{"managed_by":"t3-agent37","environment":"ts-2"}}`
	const replacement = `{"id":"cccccccccc","status":"stopped","created":300,"metadata":{"managed_by":"t3-agent37","environment":"ts-2"}}`
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	var calls []string
	var deletedTarget, createdReplacement bool
	http.DefaultTransport = downTestTransport(func(r *http.Request) (*http.Response, error) {
		call := r.Method + " " + r.URL.Path
		calls = append(calls, call)
		switch call {
		case "GET /v1/instances":
			rows := []string{older}
			if !deletedTarget {
				rows = append(rows, target)
			}
			if createdReplacement {
				rows = append(rows, replacement)
			}
			return downResponse(http.StatusOK, `{"data":[`+strings.Join(rows, ",")+`]}`), nil
		case "GET /v1/instances/bbbbbbbbbb":
			if deletedTarget {
				return downResponse(http.StatusNotFound, `{}`), nil
			}
			return downResponse(http.StatusOK, target), nil
		case "DELETE /v1/instances/bbbbbbbbbb":
			deletedTarget = true
			return nil, io.ErrUnexpectedEOF
		case "GET /v1/instances/cccccccccc":
			return downResponse(http.StatusOK, replacement), nil
		case "DELETE /v1/instances/cccccccccc":
			createdReplacement = false
			return downResponse(http.StatusNoContent, ""), nil
		default:
			return nil, fmt.Errorf("unexpected request %s", call)
		}
	})
	var output bytes.Buffer
	if err := run(t.Context(), []string{"down"}, &output, io.Discard); err == nil || !deletedTarget {
		t.Fatalf("expected a lost response after delete, got %v", err)
	}
	createdReplacement = true
	calls = nil
	if row := runDownTest(t, "down"); row["id"] != "bbbbbbbbbb" || row["status"] != "deleted" ||
		!slices.Equal(calls, []string{"GET /v1/instances/bbbbbbbbbb"}) {
		t.Fatalf("retry selected a different target: %v, %v", row, calls)
	}
	if row := runDownTest(t, "down"); row["id"] != "cccccccccc" || row["status"] != "deleted" {
		t.Fatalf("newer sandbox was not selected: %v", row)
	}
	calls = nil
	if row := runDownTest(t, "down", "--dry-run"); row["id"] != "cccccccccc" || row["action"] != "none" ||
		!slices.Equal(calls, []string{"GET /v1/instances"}) {
		t.Fatalf("preview selected an older sandbox: %v, %v", row, calls)
	}
	if row := runDownTest(t, "delete", "ts-2"); row["name"] != "ts-2" || row["status"] != "absent" {
		t.Fatalf("named deletion was not idempotent: %v", row)
	}
}

func TestDownSucceedsAndRetriesPendingPeerCleanup(t *testing.T) {
	t.Setenv("AGENT37_API_KEY", "fixture-only")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	peerFile := filepath.Join(t.TempDir(), "peers.json")
	t.Setenv("T3_AGENT37_PEERS_FILE", peerFile)
	const target = `{"id":"bbbbbbbbbb","status":"stopped","created":200,"metadata":{"managed_by":"t3-agent37","environment":"ts-1","tailnet":"tailscale"}}`
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	var deletes int
	http.DefaultTransport = downTestTransport(func(r *http.Request) (*http.Response, error) {
		call := r.Method + " " + r.URL.Path
		switch call {
		case "GET /v1/instances":
			if deletes == 0 {
				return downResponse(http.StatusOK, `{"data":[`+target+`]}`), nil
			}
			return downResponse(http.StatusOK, `{"data":[]}`), nil
		case "GET /v1/instances/bbbbbbbbbb":
			return downResponse(http.StatusOK, target), nil
		case "DELETE /v1/instances/bbbbbbbbbb":
			deletes++
			return downResponse(http.StatusNoContent, ""), nil
		default:
			return nil, fmt.Errorf("unexpected request %s", call)
		}
	})
	var output, diagnostic bytes.Buffer
	if err := run(t.Context(), []string{"down"}, &output, &diagnostic); err != nil || deletes != 1 ||
		!strings.Contains(output.String(), `"status":"deleted"`) || !strings.Contains(diagnostic.String(), "T3 cleanup pending for ts-1") {
		t.Fatalf("deletion did not report the peer problem separately: %v, %d, %s, %s", err, deletes, &output, &diagnostic)
	}
	if err := os.WriteFile(peerFile, []byte(`[]`), 0o600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	diagnostic.Reset()
	if err := run(t.Context(), []string{"down"}, &output, &diagnostic); err != nil || deletes != 1 || diagnostic.Len() != 0 {
		t.Fatalf("retry did not finish cleanup without deleting again: %v, %d, %s", err, deletes, &diagnostic)
	}
}

func downResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
}

func runDownTest(t *testing.T, args ...string) map[string]any {
	t.Helper()
	var output bytes.Buffer
	if err := run(t.Context(), args, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(output.Bytes(), &row); err != nil {
		t.Fatalf("invalid lifecycle output %q: %v", &output, err)
	}
	return row
}

func TestDelAlias(t *testing.T) {
	for _, tail := range [][]string{nil, {"--dry-run"}, {"ts-1"}, {"ts-1", "--dry-run"}} {
		want, err := parse(append([]string{"delete"}, tail...), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		got, err := parse(append([]string{"del"}, tail...), io.Discard)
		if err != nil || got != want {
			t.Fatalf("del %v = %+v, %v, want %+v", tail, got, err, want)
		}
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
