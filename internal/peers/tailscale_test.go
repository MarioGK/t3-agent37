package peers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEnrollmentKeyRequestsOnlyAnEphemeralAgent37Grant(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.RawQuery != "" {
			t.Error("OAuth credentials must travel in a POST body or header")
		}
		switch r.URL.Path {
		case "/oauth/token":
			if err := r.ParseForm(); err != nil || r.FormValue("client_id") != "fixture-id" ||
				r.FormValue("client_secret") != "fixture-secret" || r.FormValue("grant_type") != "client_credentials" {
				t.Error("invalid OAuth token request")
			}
			fmt.Fprint(w, `{"access_token":"fixture-token"}`)
		case "/tailnet/-/keys":
			var request struct {
				Capabilities struct {
					Devices struct {
						Create struct {
							Ephemeral     bool
							Reusable      bool
							Preauthorized bool
							Tags          []string
						}
					}
				}
				ExpirySeconds int
			}
			if r.Header.Get("Authorization") != "Bearer fixture-token" || json.NewDecoder(r.Body).Decode(&request) != nil {
				t.Error("invalid auth key request")
			}
			create := request.Capabilities.Devices.Create
			if !create.Ephemeral || create.Reusable || !create.Preauthorized || strings.Join(create.Tags, ",") != "tag:agent37" || request.ExpirySeconds != 600 {
				t.Error("enrollment key is not limited to ephemeral use")
			}
			fmt.Fprint(w, `{"key":"tskey-auth-fixture"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newHTTPClient(2 * time.Second)
	defer client.CloseIdleConnections()
	key, err := enrollmentKey(context.Background(), client, server.URL, "fixture-id", "fixture-secret")
	if err != nil || key != "tskey-auth-fixture" {
		t.Fatalf("enrollment key request failed: %v", err)
	}
}
