package peers

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"tailscale.com/client/tailscale/v2"
)

func TestRepairEnrollmentTagsUsesPolicyRevision(t *testing.T) {
	const initial = `{
		// Preserve these comments, owners, and grants.
		"tagOwners": {"tag:dev": ["group:dev"]},
		"grants": [{"src": ["tag:dev"], "dst": ["tag:dev"], "ip": ["tcp:443"]}],
	}`
	var validations, saves atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v2/tailnet/-/acl":
			w.Header().Set("ETag", `"revision"`)
			fmt.Fprint(w, initial)
		case "POST /api/v2/tailnet/-/acl/validate", "POST /api/v2/tailnet/-/acl":
			if r.URL.Path == "/api/v2/tailnet/-/acl" {
				saves.Add(1)
				if r.Header.Get("If-Match") != `"revision"` {
					t.Error("the policy update omitted its read revision")
				}
			} else {
				validations.Add(1)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil || !strings.Contains(string(body), `"tag:agent37": []`) ||
				!strings.Contains(string(body), `"tag:dev": ["group:dev"]`) ||
				!strings.Contains(string(body), `"ip": ["tcp:443"]`) ||
				!strings.Contains(string(body), "// Preserve these comments") {
				t.Error("the updated policy failed to preserve existing data or add the new tag")
			}
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &tailscale.Client{BaseURL: base, HTTP: server.Client()}
	tags := []string{"tag:dev", DefaultEnrollmentTag}
	added, err := ensureEnrollmentTags(t.Context(), client.PolicyFile(), tags, true)
	if err != nil || !slices.Equal(added, []string{DefaultEnrollmentTag}) || validations.Load() != 1 || saves.Load() != 0 {
		t.Fatalf("dry-run changed the policy or returned the wrong tags: %v, %v", added, err)
	}
	added, err = ensureEnrollmentTags(t.Context(), client.PolicyFile(), tags, false)
	if err != nil || !slices.Equal(added, []string{DefaultEnrollmentTag}) || validations.Load() != 2 || saves.Load() != 1 {
		t.Fatalf("repair failed to save the expected change: %v, %v", added, err)
	}
}

func TestAddEnrollmentTagsLeavesExistingPolicyAlone(t *testing.T) {
	const raw = `{"tagOwners": {"tag:Agent37": ["group:admin"]}, "futureSetting": 1}`
	updated, added, err := addEnrollmentTags(raw, []string{DefaultEnrollmentTag})
	if err != nil || len(added) != 0 || updated != raw {
		t.Fatalf("repair altered an existing definition: %v", err)
	}
}
