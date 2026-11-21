package peers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"tailscale.com/client/tailscale/v2"
)

type oauthTestTransport func(*http.Request) (*http.Response, error)

func (f oauthTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestEnrollmentRetriesTemporaryOAuthFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var attempts []time.Time
		client := &http.Client{Transport: oauthTestTransport(func(r *http.Request) (*http.Response, error) {
			attempts = append(attempts, time.Now())
			if r.Method != http.MethodPost || r.URL.Path != "/api/v2/oauth/token" {
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
			switch len(attempts) {
			case 1:
				return nil, io.EOF
			case 2:
				return &http.Response{StatusCode: http.StatusServiceUnavailable,
					Header: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"2"}},
					Body:   io.NopCloser(strings.NewReader(`{"error":"temporarily_unavailable"}`))}, nil
			default:
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{"access_token":"fixture-token","token_type":"Bearer"}`))}, nil
			}
		})}
		enrollment, err := verifyEnrollment(t.Context(), client, &tailscaleAPI, "fixture-id", "fixture-secret", []string{DefaultEnrollmentTag})
		if err != nil || enrollment == nil || len(attempts) != 3 {
			t.Fatalf("expected recovery on the third request, got %d requests, err=%v", len(attempts), err)
		}
		if attempts[1].Sub(attempts[0]) < 500*time.Millisecond || attempts[2].Sub(attempts[1]) < 2*time.Second {
			t.Fatalf("retries ignored backoff or Retry-After: %v", attempts)
		}
	})
}

func TestEnrollmentBoundsRetriesAndStopsOnCancellation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		deadline time.Duration
		calls    int
	}{
		{"attempt limit", time.Minute, 3},
		{"deadline during backoff", 100 * time.Millisecond, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				client := &http.Client{Transport: oauthTestTransport(func(*http.Request) (*http.Response, error) {
					calls++
					return nil, io.ErrUnexpectedEOF
				})}
				ctx, cancel := context.WithTimeout(t.Context(), tc.deadline)
				defer cancel()
				_, err := verifyEnrollment(ctx, client, &tailscaleAPI, "fixture-id", "fixture-secret", []string{DefaultEnrollmentTag})
				if err == nil || calls != tc.calls || errors.Is(err, context.DeadlineExceeded) != (tc.calls == 1) {
					t.Fatalf("OAuth exchanged %d times, err=%v", calls, err)
				}
			})
		})
	}
}

func TestEnrollmentVerifiesScopeAndRevokesFailedAttempt(t *testing.T) {
	tags := []string{"tag:sandbox", "tag:terra"}
	var tokens, created, revoked atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/oauth/token":
			tokens.Add(1)
			if err := r.ParseForm(); err != nil || r.URL.RawQuery != "" ||
				r.PostForm.Get("client_id") != "fixture-id" || r.PostForm.Get("client_secret") != "fixture-secret" ||
				r.PostForm.Get("grant_type") != "client_credentials" ||
				r.PostForm.Get("scope") != "auth_keys" || r.PostForm.Get("tags") != "tag:sandbox tag:terra" {
				t.Error("invalid scoped OAuth token request")
			}
			fmt.Fprint(w, `{"access_token":"fixture-token","token_type":"Bearer","expires_in":3600}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/tailnet/-/keys":
			created.Add(1)
			var request tailscale.CreateKeyRequest
			if r.Header.Get("Authorization") != "Bearer fixture-token" || json.NewDecoder(r.Body).Decode(&request) != nil {
				t.Error("invalid auth key request")
			}
			device := request.Capabilities.Devices.Create
			if !device.Ephemeral || device.Reusable || !device.Preauthorized ||
				!slices.Equal(device.Tags, tags) || request.ExpirySeconds != 600 {
				t.Error("enrollment key is not limited to ephemeral use")
			}
			fmt.Fprint(w, `{"id":"fixture-key-id","key":"tskey-auth-fixture"}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v2/tailnet/-/keys/fixture-key-id":
			revoked.Add(1)
			if r.Header.Get("Authorization") != "Bearer fixture-token" {
				t.Error("key revocation did not reuse the scoped token")
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected Tailscale call %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := newHTTPClient(2 * time.Second)
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	enrollment, err := verifyEnrollment(ctx, client, base, "fixture-id", "fixture-secret", tags)
	if err != nil || tokens.Load() != 1 || created.Load() != 0 {
		t.Fatalf("preflight should request a token without creating a key: %v", err)
	}
	if err := enrollment.UseKey(ctx, func(key string) error {
		if key != "tskey-auth-fixture" {
			t.Error("incorrect key passed to guest delivery")
		}
		return nil
	}); err != nil || revoked.Load() != 0 {
		t.Fatalf("successful enrollment returned an error or revoked a consumed key: %v", err)
	}
	err = enrollment.UseKey(ctx, func(string) error {
		cancel()
		return context.Canceled
	})
	if !errors.Is(err, context.Canceled) || created.Load() != 2 || revoked.Load() != 1 || tokens.Load() != 1 {
		t.Fatalf("canceled enrollment should revoke its key with the original token: %v", err)
	}
}

func TestEnrollmentRedactsRejectedOAuthCredentials(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_scope","error_description":"fixture-secret"}`)
	}))
	defer server.Close()
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = verifyEnrollment(t.Context(), server.Client(), base, "fixture-id", "fixture-secret", []string{DefaultEnrollmentTag})
	if err == nil || !strings.Contains(err.Error(), "Auth Keys Write for tag:agent37") ||
		strings.Contains(err.Error(), "fixture-secret") || requests.Load() != 1 {
		t.Fatalf("unexpected scope error: %v", err)
	}
}

func TestEnrollmentExplainsDeniedTagsWithoutExposingResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"message":"requested tags [tag:agent37] are invalid or not permitted","detail":"fixture-secret"}`)
	}))
	defer server.Close()
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = verifyEnrollment(t.Context(), server.Client(), base, "fixture-id", "fixture-secret", []string{DefaultEnrollmentTag})
	if !errors.Is(err, ErrEnrollmentAuthorization) || !strings.Contains(err.Error(), "denied the requested tags tag:agent37") ||
		strings.Contains(err.Error(), "fixture-secret") {
		t.Fatalf("unexpected tag error: %v", err)
	}
}

func TestParseEnrollmentTags(t *testing.T) {
	for _, example := range []struct {
		input string
		want  []string
	}{
		{"", []string{DefaultEnrollmentTag}},
		{"tag:Sandbox, tag:terra, tag:sandbox", []string{"tag:sandbox", "tag:terra"}},
		{"tag:agent37,", nil},
		{"tag:agent37 tag:sandbox", nil},
		{"tag:1invalid", nil},
	} {
		tags, err := ParseEnrollmentTags(example.input)
		if (err != nil) != (example.want == nil) || !slices.Equal(tags, example.want) {
			t.Errorf("ParseEnrollmentTags(%q) = %v, %v", example.input, tags, err)
		}
	}
}

func TestRemovalUsesOnlyExactTaggedNode(t *testing.T) {
	for _, tc := range []struct {
		name, device string
		wantDelete   bool
		wantError    bool
	}{
		{"tagged target", `,{"name":"a37-ts-1-aaaaaaaaaa.dev.ts.net","hostname":"a37-ts-1-aaaaaaaaaa","nodeId":"nTarget","tags":["tag:agent37"]}`, true, false},
		{"tag mismatch", `,{"name":"a37-ts-1-aaaaaaaaaa.dev.ts.net","hostname":"a37-ts-1-aaaaaaaaaa","nodeId":"nWrong","tags":["tag:another"]}`, false, true},
		{"already gone", ``, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var deleted atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.Method + " " + r.URL.Path {
				case "POST /api/v2/oauth/token":
					if r.ParseForm() != nil || r.Form.Get("scope") != "devices:core" || r.Form.Get("tags") != "tag:agent37" {
						t.Error("unexpected device scope")
					}
					fmt.Fprint(w, `{"access_token":"fixture-token","token_type":"Bearer","expires_in":3600}`)
				case "GET /api/v2/tailnet/-/devices":
					fmt.Fprint(w, `{"devices":[{"name":"a37-ts-1-bbbbbbbbbb.dev.ts.net","hostname":"a37-ts-1-bbbbbbbbbb","nodeId":"nOther","tags":["tag:agent37"]}`+tc.device+`]}`)
				case "DELETE /api/v2/device/nTarget":
					deleted.Store(true)
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected Tailscale call %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			base, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			err = removeSandboxDevice(t.Context(), server.Client(), base, "fixture-id", "fixture-secret", []string{DefaultEnrollmentTag}, "a37-ts-1-aaaaaaaaaa")
			if deleted.Load() != tc.wantDelete || (err != nil) != tc.wantError {
				t.Fatalf("device removal = %t, %v", deleted.Load(), err)
			}
		})
	}
}
