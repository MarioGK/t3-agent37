package main

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MarioGK/t3-agent37/internal/agent37"
)

func TestBatchMeshReusesProofAndDiscoversOnlyExistingNodes(t *testing.T) {
	newInstance := func(id, name string, confirmationRequired bool) agent37.Instance {
		metadata := map[string]string{"managed_by": owner, "environment": name, "tailnet": "tailscale"}
		if confirmationRequired {
			metadata[desktopConfirmationKey] = "required"
		}
		return agent37.Instance{ID: id, Name: name, Status: "running", Metadata: metadata}
	}
	ready := newInstance("aaaaaaaaaa", "ready", true)
	headless := newInstance("bbbbbbbbbb", "headless", false)
	pending := newInstance("cccccccccc", "pending", true)
	failed := newInstance("dddddddddd", "failed", false)
	wrongOrigin := newInstance("eeeeeeeeee", "wrong-origin", false)
	missingProxy := newInstance("ffffffffff", "missing-proxy", false)
	existing := newInstance("gggggggggg", "existing", true)
	inventory := []agent37.Instance{ready, headless, pending, failed, wrongOrigin, missingProxy, existing}

	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	var calls atomic.Int32
	http.DefaultTransport = namingTestTransport(func(request *http.Request) string {
		calls.Add(1)
		if request.Method != http.MethodPost || request.URL.Path != "/v1/instances/gggggggggg/exec" {
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		return `{"exit_code":0,"stdout":"{\"environmentId\":\"existing-env\",\"url\":\"https://a37-existing-gggggggggg.example.ts.net\",\"peerProxy\":true,\"desktopConfirmed\":true}"}`
	})

	results := []batchSandbox{
		{name: ready.Name, instance: ready, proof: setupProof{EnvironmentID: "ready-env", TailnetURL: "https://a37-ready-aaaaaaaaaa.example.ts.net", PeerProxy: true, DesktopConfirmed: true}},
		{name: headless.Name, instance: headless, proof: setupProof{EnvironmentID: "headless-env", TailnetURL: "https://a37-headless-bbbbbbbbbb.example.ts.net", PeerProxy: true}},
		{name: pending.Name, instance: pending, proof: setupProof{EnvironmentID: "pending-env", TailnetURL: "https://a37-pending-cccccccccc.example.ts.net", PeerProxy: true}},
		{name: failed.Name, instance: failed, err: errors.New("setup failed")},
		{name: wrongOrigin.Name, instance: wrongOrigin, proof: setupProof{EnvironmentID: "wrong-origin-env", TailnetURL: "https://foreign.example.ts.net", PeerProxy: true}},
		{name: missingProxy.Name, instance: missingProxy, proof: setupProof{EnvironmentID: "missing-proxy-env", TailnetURL: "https://a37-missing-proxy-ffffffffff.example.ts.net"}},
	}

	nodes, err := batchMeshNodes(t.Context(), agent37.New("fixture-key"), inventory, results)
	if err == nil || !strings.Contains(err.Error(), wrongOrigin.Name) || !strings.Contains(err.Error(), missingProxy.Name) {
		t.Fatalf("expected rejected proofs to be reported, got %v", err)
	}
	var names, cleanupOnly []string
	for _, node := range nodes {
		if node.CleanupOnly {
			cleanupOnly = append(cleanupOnly, node.Name)
		} else {
			names = append(names, node.Name)
		}
	}
	if want := []string{"ready", "headless", "existing"}; !slices.Equal(names, want) {
		t.Fatalf("mesh nodes = %v, want %v", names, want)
	}
	if want := []string{"pending", "missing-proxy"}; !slices.Equal(cleanupOnly, want) {
		t.Fatalf("cleanup nodes = %v, want %v", cleanupOnly, want)
	}
	if calls.Load() != 1 {
		t.Fatalf("discovery calls = %d, want 1", calls.Load())
	}
	if inventory[0].ID != ready.ID || inventory[len(inventory)-1].ID != existing.ID {
		t.Fatal("batch mesh preparation modified the inventory snapshot")
	}
}
