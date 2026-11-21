package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
		case "DELETE /v1/instances/bbbbbbbbbb":
			status = http.StatusNoContent
		case "GET /v1/instances/aaaaaaaaaa":
			body = older
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
	if err := json.Unmarshal(output.Bytes(), &row); err != nil || row["id"] != "bbbbbbbbbb" || row["status"] != "deleted" || len(calls) != 4 {
		t.Fatalf("wrong deletion: %v, %v, %v", row, calls, err)
	}
	output.Reset()
	if err := run(ctx, []string{"down", "z-older", "--dry-run"}, &output, &diagnostic); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(output.Bytes(), &row); err != nil || row["id"] != "aaaaaaaaaa" || row["action"] != "delete" || len(calls) != 5 {
		t.Fatalf("wrong named preview: %v, %v, %v", row, calls, err)
	}
	output.Reset()
	if err := run(ctx, []string{"down", "z-older"}, &output, &diagnostic); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(output.Bytes(), &row); err != nil || row["id"] != "aaaaaaaaaa" || row["status"] != "deleted" || len(calls) != 8 {
		t.Fatalf("wrong named deletion: %v, %v, %v", row, calls, err)
	}
}

func TestDownPickerRequiresATerminal(t *testing.T) {
	opts, err := parse([]string{"down", "--pick"}, io.Discard)
	if err != nil || opts.action != "down" || !opts.pickDelete {
		t.Fatalf("down --pick = %+v, %v", opts, err)
	}
	err = run(context.Background(), []string{"down", "--pick"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "requires a terminal") {
		t.Fatalf("down --pick with redirected input/output = %v", err)
	}
	if _, err := parse([]string{"down", "two", "names"}, io.Discard); err == nil {
		t.Fatal("down accepted multiple names")
	}
	for _, args := range [][]string{{"down", "ts-1", "--pick"}, {"down", "--all", "--pick"}} {
		if _, err := parse(args, io.Discard); err == nil {
			t.Errorf("accepted conflicting targets %v", args)
		}
	}
}

func TestSleepUsesProviderHibernateWithoutTouchingT3OrTailnet(t *testing.T) {
	t.Setenv("AGENT37_API_KEY", "fixture-only")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const old = `{"id":"aaaaaaaaaa","status":"running","created":100,"metadata":{"managed_by":"t3-agent37","environment":"ts-1"}}`
	const newest = `{"id":"bbbbbbbbbb","status":%q,"created":200,"metadata":{"managed_by":"t3-agent37","environment":"ts-2","tailnet":"tailscale"}}`
	const unmanaged = `{"id":"cccccccccc","status":"running","created":300}`
	status := "running"
	checkpointing := true
	var calls []string
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	http.DefaultTransport = downTestTransport(func(r *http.Request) (*http.Response, error) {
		call := r.Method + " " + r.URL.Path
		calls = append(calls, call)
		switch call {
		case "GET /v1/instances":
			body := `{"data":[` + strings.Join([]string{old, unmanaged, fmt.Sprintf(newest, status)}, ",") + `]}`
			return downResponse(http.StatusOK, body), nil
		case "GET /v1/instances/bbbbbbbbbb":
			return downResponse(http.StatusOK, fmt.Sprintf(newest, status)), nil
		case "POST /v1/instances/bbbbbbbbbb/sleep":
			if checkpointing {
				checkpointing = false
				return downResponse(http.StatusConflict, `{"error":"try_again"}`), nil
			}
			status = "sleeping"
			return downResponse(http.StatusOK, `{"id":"bbbbbbbbbb","status":"sleeping"}`), nil
		default:
			return nil, fmt.Errorf("unexpected request %s", call)
		}
	})
	preview := runDownTest(t, "sleep", "ts-2", "--dry-run")
	if preview["action"] != "sleep" || preview["id"] != "bbbbbbbbbb" || len(calls) != 1 {
		t.Fatalf("sleep preview = %v, requests = %v", preview, calls)
	}
	calls = nil
	result := runDownTest(t, "sleep")
	want := []string{
		"GET /v1/instances", "GET /v1/instances/bbbbbbbbbb",
		"POST /v1/instances/bbbbbbbbbb/sleep", "POST /v1/instances/bbbbbbbbbb/sleep",
	}
	if result["status"] != "sleeping" || result["name"] != "ts-2" || !slices.Equal(calls, want) {
		t.Fatalf("sleep = %v, requests = %v", result, calls)
	}
	calls = nil
	result = runDownTest(t, "sleep", "ts-2")
	if result["status"] != "sleeping" || !slices.Equal(calls, want[:2]) {
		t.Fatalf("sleeping sandbox = %v, requests = %v", result, calls)
	}
	status = "stopped"
	calls = nil
	if err := run(t.Context(), []string{"sleep", "ts-2"}, io.Discard, io.Discard); err == nil || !slices.Equal(calls, want[:2]) {
		t.Fatalf("stopped sandbox = %v, requests = %v", err, calls)
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
	if row := runDownTest(t, "down", "--dry-run"); row["id"] != "bbbbbbbbbb" || row["action"] != "none" ||
		!slices.Equal(calls, []string{"GET /v1/instances/bbbbbbbbbb"}) {
		t.Fatalf("preview retargeted or advertised an already applied operation: %v, %v", row, calls)
	}
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
	if row := runDownTest(t, "down", "ts-2"); row["name"] != "ts-2" || row["status"] != "absent" {
		t.Fatalf("named deletion was not idempotent: %v", row)
	}
}

func TestDeleteRetriesAcrossEntrypoints(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		first     []string
		retry     []string
		loseReply bool
	}{
		{"same name after lost reply", []string{"down", "con"}, []string{"down", "con"}, true},
		{"bare then named after lost reply", []string{"down"}, []string{"down", "--delete", "--yes", "con"}, true},
		{"named then bare after lost result", []string{"down", "con"}, []string{"down"}, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Setenv("AGENT37_API_KEY", "fixture-only")
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			state, _ := statePath()
			if err := os.MkdirAll(state, 0o700); err != nil {
				t.Fatal(err)
			}
			marker := sandboxStatePath(state, "con", ".pending")
			if err := os.WriteFile(marker, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			const original = `{"id":"aaaaaaaaaa","name":"my cloud label","status":"failed","created":100,"metadata":{"managed_by":"t3-agent37","environment":"con"}}`
			const replacement = `{"id":"bbbbbbbbbb","status":"running","created":200,"metadata":{"managed_by":"t3-agent37","environment":"con"}}`
			transport := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = transport })
			var deleted bool
			http.DefaultTransport = downTestTransport(func(r *http.Request) (*http.Response, error) {
				switch call := r.Method + " " + r.URL.Path; call {
				case "GET /v1/instances":
					if deleted {
						return downResponse(200, `{"data":[`+replacement+`]}`), nil
					}
					return downResponse(200, `{"data":[`+original+`]}`), nil
				case "GET /v1/instances/aaaaaaaaaa":
					if deleted {
						return downResponse(404, `{}`), nil
					}
					return downResponse(200, original), nil
				case "DELETE /v1/instances/aaaaaaaaaa":
					if deleted {
						return nil, errors.New("duplicate deletion")
					}
					deleted = true
					if scenario.loseReply {
						return nil, io.ErrUnexpectedEOF
					}
					return downResponse(200, `{"id":"aaaaaaaaaa","deleted":true}`), nil
				default:
					t.Errorf("unexpected request %s", call)
					return nil, errors.New("unexpected request")
				}
			})
			if err := run(t.Context(), scenario.first, brokenOutput{}, io.Discard); err == nil || !deleted {
				t.Fatalf("expected ambiguous result after delete, got %v", err)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("old create marker remains: %v", err)
			}
			if err := os.WriteFile(marker, []byte("replacement"), 0o600); err != nil {
				t.Fatal(err)
			}
			if row := runDownTest(t, scenario.retry...); row["id"] != "aaaaaaaaaa" || row["name"] != "con" || row["cloud_name"] != "my cloud label" {
				t.Fatalf("retry changed the original target: %v", row)
			}
			if content, err := os.ReadFile(marker); err != nil || string(content) != "replacement" {
				t.Fatalf("retry changed the replacement marker: %q, %v", content, err)
			}
		})
	}
}

type brokenOutput struct{}

func (brokenOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestDownDoesNotRetargetOnKeyChange(t *testing.T) {
	t.Setenv("AGENT37_API_KEY", "new-key")
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "test[legacy]"))
	state, _ := statePath()
	legacyPath := filepath.Join(state, "last-down-"+keyScope(agent37.New("old-key"))+".json")
	if err := saveConfigJSON(t.Context(), legacyPath, nil, downReceipt{ID: "aaaaaaaaaa", Name: "ts-2", Status: "deleted", Created: 200}); err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = transport })
	http.DefaultTransport = downTestTransport(func(r *http.Request) (*http.Response, error) {
		t.Errorf("changed-key command accessed API: %s %s", r.Method, r.URL.Path)
		return nil, errors.New("unexpected request")
	})
	for _, args := range [][]string{{"down"}, {"down", "--delete", "--yes"}, {"down", "--dry-run"}} {
		if err := run(t.Context(), args, io.Discard, io.Discard); !errors.Is(err, errDownKeyChanged) {
			t.Fatalf("%v: expected explicit target requirement, got %v", args, err)
		}
	}
}

func TestDownPreviewOfEmptyWorkspace(t *testing.T) {
	t.Setenv("AGENT37_API_KEY", "fixture-only")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	transport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = transport })
	http.DefaultTransport = downTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/instances" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		return downResponse(200, `{"data":[]}`), nil
	})
	if row := runDownTest(t, "down", "--dry-run"); row["status"] != "absent" || row["action"] != "none" {
		t.Fatalf("wrong empty preview: %v", row)
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
	const newer = `{"id":"cccccccccc","status":"running","created":300,"metadata":{"managed_by":"t3-agent37","environment":"ts-2"}}`
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
			return downResponse(http.StatusOK, `{"data":[`+newer+`]}`), nil
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
		!strings.Contains(output.String(), `"status":"deleted"`) || !strings.Contains(output.String(), `"cleanup_pending":true`) ||
		!strings.Contains(diagnostic.String(), "T3 cleanup pending for ts-1") {
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

func TestLatestDownReceiptRefusesASecondTimestampTie(t *testing.T) {
	transport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = transport })
	http.DefaultTransport = downTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/instances" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		const rows = `{"data":[{"id":"aaaaaaaaaa","status":"running","created":200,"metadata":{"managed_by":"t3-agent37","environment":"original"}},
		{"id":"bbbbbbbbbb","status":"running","created":200,"metadata":{"managed_by":"t3-agent37","environment":"new"}}]}`
		return downResponse(200, rows), nil
	})
	_, err := selectDownTarget(t.Context(), agent37.New("fixture"), options{latest: true}, downReceipt{ID: "aaaaaaaaaa", Created: 200})
	if err == nil || !strings.Contains(err.Error(), "same Agent37 creation time") {
		t.Fatalf("expected explicit choice for a timestamp tie, got %v", err)
	}
}

func TestStopRetriesCheckpointInProgress(t *testing.T) {
	transport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = transport })
	var calls []string
	http.DefaultTransport = downTestTransport(func(r *http.Request) (*http.Response, error) {
		call := r.Method + " " + r.URL.Path
		calls = append(calls, call)
		switch call {
		case "POST /v1/instances/aaaaaaaaaa/exec":
			if len(calls) == 1 {
				return downResponse(409, `{"error":{"code":"try_again"}}`), nil
			}
			return downResponse(200, `{"exit_code":0}`), nil
		case "GET /v1/instances/aaaaaaaaaa":
			return downResponse(200, `{"id":"aaaaaaaaaa","status":"sleeping","metadata":{"managed_by":"t3-agent37","environment":"test","tailnet":"tailscale"}}`), nil
		case "POST /v1/instances/aaaaaaaaaa/stop":
			return downResponse(200, `{"id":"aaaaaaaaaa","status":"stopped"}`), nil
		default:
			return nil, fmt.Errorf("unexpected request %s", call)
		}
	})
	instance, err := stopSandbox(t.Context(), agent37.New("fixture"), agent37.Instance{ID: "aaaaaaaaaa", Status: "sleeping",
		Metadata: map[string]string{"tailnet": "tailscale"}}, "test")
	if err != nil || instance.Status != "stopped" || len(calls) != 4 {
		t.Fatalf("checkpoint retry failed: %s, %v, %v", instance.Status, calls, err)
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

func TestRetiredDeletionCommandsAreRejected(t *testing.T) {
	for _, command := range []string{"delete", "del"} {
		for _, args := range [][]string{{command}, {command, "ts-1"}, {command, "--all"}} {
			if _, err := parse(args, io.Discard); err == nil {
				t.Errorf("accepted retired command %v", args)
			}
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
