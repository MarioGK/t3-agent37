package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/MarioGK/t3-agent37/internal/agent37"
)

func TestMeshNodeRequiresAssignedPrivateOrigin(t *testing.T) {
	const url = "https://a37-ts-1-ab12cd34ef.example.ts.net"
	proof := sandboxPeer{URL: url, EnvironmentID: "t3-test", PeerProxy: true}
	node, err := meshNode(nil, "ab12cd34ef", "ts-1", proof)
	if err != nil || node.URL != url || node.EnvironmentID != proof.EnvironmentID {
		t.Fatalf("mesh node = %+v, %v", node, err)
	}
	for _, origin := range []string{
		"http://a37-ts-1-ab12cd34ef.example.ts.net",
		"https://a37-ts-1-other12345.example.ts.net",
		"https://a37-ts-1-ab12cd34ef.example.com",
		url + "/api",
		url + ":18456",
	} {
		proof.URL = origin
		if _, err := meshNode(nil, "ab12cd34ef", "ts-1", proof); err == nil {
			t.Errorf("accepted unassigned URL %s", origin)
		}
	}
}

func TestCloudMeshExcludesPendingDesktopPairing(t *testing.T) {
	instance := agent37.Instance{
		ID: "ab12cd34ef", Name: "ts-1", Status: "running",
		Metadata: map[string]string{"managed_by": owner, "environment": "ts-1", "tailnet": "tailscale", desktopConfirmationKey: "required"},
	}
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	for _, confirmed := range []bool{false, true} {
		http.DefaultTransport = namingTestTransport(func(request *http.Request) string {
			if request.Method != http.MethodPost || request.URL.Path != "/v1/instances/ab12cd34ef/exec" {
				t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
			}
			body, _ := json.Marshal(sandboxPeer{
				EnvironmentID: "t3-test", URL: "https://a37-ts-1-ab12cd34ef.example.ts.net", PeerProxy: true, DesktopConfirmed: confirmed,
			})
			response, _ := json.Marshal(map[string]any{"exit_code": 0, "stdout": string(body)})
			return string(response)
		})
		nodes, err := cloudMeshNodes(t.Context(), agent37.New("fixture-key"), []agent37.Instance{instance}, "", false)
		if err != nil || (len(nodes) == 1) != confirmed {
			t.Fatalf("confirmation=%t, nodes=%d, err=%v", confirmed, len(nodes), err)
		}
		// Batch up uses the full sync path, which can see an unpaired node for
		// cleanup while still excluding it from peer-link creation.
		nodes, err = discoverCloudNodes(t.Context(), agent37.New("fixture-key"), []agent37.Instance{instance}, "", false, true)
		if err != nil || len(nodes) != 1 || nodes[0].CleanupOnly == confirmed {
			t.Fatalf("sync ignored the pairing boundary: confirmation=%t, nodes=%+v, err=%v", confirmed, nodes, err)
		}
	}
}
