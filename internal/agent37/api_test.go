package agent37

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type testTransport func(*http.Request) (int, string)

func (transport testTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	status, body := transport(request)
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestAgent37RoutesAndAuthentication(t *testing.T) {
	const id = "ab12cd34ef"
	const enrollment = "tailscale-enrollment-0123456789abcdef0123456789abcdef"
	client := New("private-fixture-key")
	client.http.Transport = testTransport(func(request *http.Request) (int, string) {
		switch request.URL.Host {
		case "api.agent37.com":
			if request.Header.Get("Authorization") != "Bearer private-fixture-key" ||
				request.Header.Get("X-Agent37-Key") != "" {
				t.Error("incorrect control-plane authentication")
			}
		case "sandbox.agent37.app", "ab12cd34ef.agent37.app":
			if request.Header.Get("X-Agent37-Key") != "private-fixture-key" ||
				request.Header.Get("Authorization") != "" {
				t.Error("incorrect sandbox authentication")
			}
		default:
			t.Errorf("unexpected request host %s", request.URL.Host)
		}
		switch request.Method + " " + request.URL.Path {
		case "GET /v1/instances":
			return 200, `{"data":[{"id":"ab12cd34ef","name":"pilot","metadata":{"managed_by":"t3-agent37","custom":{"priority":2},"nullable":null}}]}`
		case "PATCH /v1/instances/" + id:
			var body struct {
				Metadata map[string]json.RawMessage `json:"metadata"`
			}
			if json.NewDecoder(request.Body).Decode(&body) != nil ||
				string(body.Metadata["provider"]) != `"cliproxy"` ||
				string(body.Metadata["custom"]) != `{"priority":2}` ||
				string(body.Metadata["nullable"]) != "null" {
				t.Error("metadata update did not preserve other values")
			}
			return 200, `{}`
		case "POST /v1/instances/" + id + "/exec":
			if request.Header.Get("Content-Type") != "application/json" {
				t.Error("exec payload is not JSON")
			}
			return 200, `{"exit_code":0,"stdout":"ready","truncated":false}`
		case "PUT /v1/files/content":
			body, err := io.ReadAll(request.Body)
			if err != nil || string(body) != "fixture-enroll" ||
				request.Header.Get("Content-Type") != "application/octet-stream" ||
				request.URL.Query().Get("path") != "~/.local/share/t3-agent37/"+enrollment ||
				request.URL.Query().Get("overwrite") != "false" {
				t.Error("enrollment key upload changed")
			}
			return 204, ""
		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
			return 404, `{}`
		}
	})
	ctx := context.Background()
	instances, err := client.List(ctx)
	if err != nil || len(instances) != 1 || instances[0].Metadata["managed_by"] != "t3-agent37" {
		t.Fatalf("could not list managed instance: %v", err)
	}
	if err := client.SetProvider(ctx, instances[0], "cliproxy"); err != nil {
		t.Fatal(err)
	}
	if stdout, err := client.Exec(ctx, id, "verify"); err != nil || stdout != "ready" {
		t.Fatalf("exec returned %q, %v", stdout, err)
	}
	if err := client.UploadEnrollmentKey(ctx, id, enrollment, "fixture-enroll"); err != nil {
		t.Fatal(err)
	}
	if err := client.UploadEnrollmentKey(ctx, id, "../unsafe", "fixture-enroll"); err == nil {
		t.Fatal("unsafe enrollment filename accepted")
	}
}

func TestAgent37ErrorsHideResponseDetails(t *testing.T) {
	client := New("fixture-key")
	client.http.Transport = testTransport(func(*http.Request) (int, string) {
		return 403, `{"error":{"code":"forbidden","message":"internal credential"}}`
	})
	_, err := client.Get(context.Background(), "ab12cd34ef")
	var apiError *Error
	if !errors.As(err, &apiError) || apiError.Status != 403 || apiError.Code != "forbidden" ||
		strings.Contains(err.Error(), "internal credential") {
		t.Fatalf("unexpected API error: %v", err)
	}
}

func TestWaitForT3ProbesAlternatePortAndChecksIdentity(t *testing.T) {
	client := New("fixture-key")
	client.http.Transport = testTransport(func(request *http.Request) (int, string) {
		if request.Header.Get("X-Agent37-Key") != "fixture-key" || request.URL.Path != "/.well-known/t3/environment" {
			t.Error("unexpected T3 discovery request")
		}
		if strings.Contains(request.URL.Host, "-3773") {
			return 404, `{}`
		}
		return 200, `{"environmentId":"env-id","serverVersion":"0.1.11","capabilities":{"peerLinks":true}}`
	})
	descriptor, port, err := client.WaitForT3(context.Background(), "ab12cd34ef", "env-id", 0)
	if err != nil || port != 4373 || descriptor.EnvironmentID != "env-id" || descriptor.Capabilities["peerLinks"] != true {
		t.Fatalf("expected the alternate port and pinned environment, got %#v, %d, %v", descriptor, port, err)
	}
	if _, _, err := client.WaitForT3(context.Background(), "ab12cd34ef", "other-id", 4373); err == nil {
		t.Fatal("mismatched T3 environment accepted")
	}
}
