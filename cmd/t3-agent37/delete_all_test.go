package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestDeleteAllPreviewAndRetryOnlyManagedIDs(t *testing.T) {
	t.Setenv("AGENT37_API_KEY", "fixture")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("T3_AGENT37_PEERS_FILE", "")
	const (
		a       = `{"id":"aaaaaaaaaa","status":"stopped","metadata":{"managed_by":"t3-agent37","environment":"alpha","tailnet":"tailscale"}}`
		b       = `{"id":"bbbbbbbbbb","status":"stopped","metadata":{"managed_by":"t3-agent37","environment":"beta","tailnet":"tailscale"}}`
		c       = `{"id":"cccccccccc","status":"running","metadata":{"managed_by":"t3-agent37","environment":"gamma"}}`
		foreign = `{"id":"dddddddddd","name":"other","status":"running"}`
	)
	rows := map[string]string{"aaaaaaaaaa": a, "bbbbbbbbbb": b, "cccccccccc": c}
	attempts := map[string]int{}
	var lists int
	var mu sync.Mutex
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = downTestTransport(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		id := strings.TrimPrefix(r.URL.Path, "/v1/instances/")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/instances":
			lists++
			data := []string{foreign}
			for _, row := range rows {
				data = append(data, row)
			}
			return downResponse(200, `{"data":[`+strings.Join(data, ",")+`]}`), nil
		case r.Method == http.MethodGet:
			if row, ok := rows[id]; ok {
				return downResponse(200, row), nil
			}
			return downResponse(404, `{}`), nil
		case r.Method == http.MethodDelete && id != "dddddddddd":
			attempts[id]++
			delete(rows, id)
			if id == "cccccccccc" {
				return nil, io.ErrUnexpectedEOF
			}
			return downResponse(204, ""), nil
		default:
			return nil, fmt.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	var output, diagnostic bytes.Buffer
	call := func(args ...string) ([]map[string]any, error) {
		output.Reset()
		err := run(t.Context(), args, &output, &diagnostic)
		var result []map[string]any
		if decodeErr := json.Unmarshal(output.Bytes(), &result); decodeErr != nil {
			t.Fatalf("invalid output %s: %v (command error %v)", &output, decodeErr, err)
		}
		return result, err
	}
	plan, err := call("delete", "--all", "--dry-run")
	if err != nil || len(plan) != 3 || len(attempts) != 0 || plan[0]["action"] != "delete" {
		t.Fatalf("preview = %v, %v, attempts %v", plan, err, attempts)
	}
	state, _ := statePath()
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("preview created state: %v", err)
	}
	result, err := call("del", "--all")
	if err == nil || len(result) != 3 || result[2]["error"] == nil || len(attempts) != 3 || lists != 3 {
		t.Fatalf("initial deletion = %v, %v, attempts %v, list count %d", result, err, attempts, lists)
	}
	plan, err = call("delete", "--all", "--dry-run")
	if err != nil || len(plan) != 1 || plan[0]["action"] != "cleanup" || attempts["cccccccccc"] != 1 {
		t.Fatalf("retry preview = %v, %v, attempts %v", plan, err, attempts)
	}
	result, err = call("delete", "--all")
	if err != nil || len(result) != 1 || result[0]["id"] != "cccccccccc" || result[0]["status"] != "deleted" || attempts["cccccccccc"] != 1 {
		t.Fatalf("retry = %v, %v, attempts %v", result, err, attempts)
	}
	result, err = call("delete", "--all")
	if err != nil || len(result) != 0 || strings.Contains(diagnostic.String(), "\x1b") {
		t.Fatalf("empty retry or diagnostics = %v, %v, %q", result, err, &diagnostic)
	}
	if _, err := parse([]string{"delete", "--all", "alpha"}, io.Discard); err == nil {
		t.Fatal("accepted a named bulk deletion")
	}
}
