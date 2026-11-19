package peers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/client/tailscale/v2"
)

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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		strings.Contains(err.Error(), "fixture-secret") {
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
