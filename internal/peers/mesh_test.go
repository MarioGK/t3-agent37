package peers

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	json "encoding/json/v2"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
)

func TestMeshReconcilesEveryDirectionAndPreservesAccess(t *testing.T) {
	mesh := newMeshFixture(t, "alpha", "beta", "gamma")
	mesh.links["alpha"]["beta"] = link{EnvironmentID: "beta", Status: "reachable", Access: "read-only"}
	mesh.links["beta"]["gamma"] = link{
		EnvironmentID: "gamma", Status: "expired", Access: "read-only", URLs: []string{mesh.nodes[2].URL + "/"},
	}
	for range 2 {
		count, err := reconcileMesh(t.Context(), mesh.client, mesh.nodes)
		if err != nil || count != 6 {
			t.Fatalf("reconcileMesh: count %d, error %v", count, err)
		}
	}
	if mesh.grants != 5 || mesh.lists != 6 || mesh.issues != 6 {
		t.Fatalf("expected only missing or expired pair grants and one list/session per source; grants %d, lists %d, issues %d", mesh.grants, mesh.lists, mesh.issues)
	}
	if mesh.links["alpha"]["beta"].Access != "read-only" || mesh.links["beta"]["gamma"].Access != "read-only" ||
		mesh.links["gamma"]["beta"].Access != accessRequired {
		t.Fatalf("unexpected link permissions: %v", mesh.links)
	}
}

func TestMeshKeepsReconcilingAfterVerificationAndLinkFailures(t *testing.T) {
	mesh := newMeshFixture(t, "alpha", "beta", "gamma", "invalid")
	mesh.nodes[3].EnvironmentID = "unexpected"
	mesh.rejectSource, mesh.rejectTarget = "beta", "alpha"
	count, err := reconcileMesh(t.Context(), mesh.client, mesh.nodes)
	if count != 5 || err == nil || !strings.Contains(err.Error(), "beta to alpha") ||
		!strings.Contains(err.Error(), "identity changed for invalid") {
		t.Fatalf("reconcileMesh: count %d, error %v", count, err)
	}
	if mesh.issues != 3 || mesh.links["alpha"]["beta"].Status != "reachable" {
		t.Fatalf("wrong-identity node was authorized or reversing a failed link was skipped: issues %d, links %v", mesh.issues, mesh.links)
	}
}

func TestMeshReconcilesTenNodesAndRetriesTemporaryReachability(t *testing.T) {
	names := make([]string, 10)
	for i := range names {
		names[i] = fmt.Sprintf("sandbox-%d", i+1)
	}
	mesh := newMeshFixture(t, names...)
	mesh.transientSource, mesh.transientTarget = names[0], names[1]
	count, err := reconcileMesh(t.Context(), mesh.client, mesh.nodes)
	if count != 90 || err != nil || mesh.grants != 91 || mesh.lists != 11 {
		t.Fatalf("ten-node mesh: %d links, %d grants, %d lists, %v", count, mesh.grants, mesh.lists, err)
	}
}

func TestSyncMeshReadsOnceAndReplacesAStaleAddress(t *testing.T) {
	mesh := newMeshFixture(t, "alpha", "beta", "gamma")
	mesh.links["alpha"]["beta"] = link{
		EnvironmentID: "beta", Status: "reachable", Access: "read-only",
		URLs: []string{"https://a37-gone-aaaaaaaaaa.test.ts.net"},
	}
	mesh.links["alpha"]["gamma"] = link{EnvironmentID: "gamma", Status: "reachable", Access: "read-only"}
	mesh.nodes[0].ListLinksJSON = func(context.Context) ([]byte, error) {
		t.Error("healthy peer should reuse the verified RPC list")
		return nil, nil
	}
	count, removed, err := syncMesh(t.Context(), mesh.client, mesh.nodes,
		func(_ context.Context, entry SandboxLink, remove func() error) (bool, error) {
			if entry.SandboxID != "aaaaaaaaaa" || entry.EnvironmentID != "beta" {
				t.Errorf("unexpected stale link: %+v", entry)
				return false, nil
			}
			err := remove()
			return err == nil, err
		})
	if err != nil || count != 6 || len(removed) != 1 || mesh.lists != 3 || mesh.issues != 3 {
		t.Fatalf("sync: %d links, %d inventories, %d sessions, removed %v, error %v", count, mesh.lists, mesh.issues, removed, err)
	}
	if len(mesh.unlinked) != 1 || mesh.unlinked[0] != "beta" || mesh.links["alpha"]["beta"].Access != accessRequired ||
		mesh.links["alpha"]["gamma"].Access != "read-only" || mesh.grants != 5 {
		t.Fatalf("sync failed to replace the stale address or preserve existing access: %+v", mesh.links["alpha"])
	}
}

func TestSyncMeshFallsBackToOwnerCLIWhenHttpsCannotBeVerified(t *testing.T) {
	mesh := newMeshFixture(t, "alpha", "beta", "offline")
	mesh.nodes[2].EnvironmentID = "expected-other-id"
	mesh.nodes[2].ListLinksJSON = func(context.Context) ([]byte, error) {
		return []byte(`[{"environmentId":"gone","urls":["https://a37-gone-aaaaaaaaaa.test.ts.net"]}]`), nil
	}
	mesh.nodes[2].Unlink = func(_ context.Context, id string) error {
		if id != "gone" {
			return fmt.Errorf("unexpected stale target %q", id)
		}
		return nil
	}
	count, removed, err := syncMesh(t.Context(), mesh.client, mesh.nodes,
		func(_ context.Context, _ SandboxLink, remove func() error) (bool, error) {
			err := remove()
			return err == nil, err
		})
	if err == nil || !strings.Contains(err.Error(), "identity changed for offline") || count != 2 ||
		len(removed) != 1 || removed[0].Peer != "offline" || mesh.issues != 2 || mesh.lists != 2 {
		t.Fatalf("fallback: %d links, %d lists, %d sessions, removed %v, error %v", count, mesh.lists, mesh.issues, removed, err)
	}
}

func TestCloudPruneDryRunUsesReadOnlyInventory(t *testing.T) {
	mesh := newMeshFixture(t, "alpha")
	node := mesh.nodes[0]
	node.ListLinksJSON = func(context.Context) ([]byte, error) {
		return json.Marshal([]link{
			{EnvironmentID: "candidate", Status: "unreachable", URLs: []string{"https://a37-demo-1-ab12cd34ef.test.ts.net"}},
			{EnvironmentID: "foreign", Status: "unreachable", URLs: []string{"https://a37-demo-2-de12ab34cf.other.ts.net"}},
		})
	}
	record := func(_ context.Context, candidate SandboxLink, remove func() error) (bool, error) {
		if candidate.EnvironmentID != "candidate" || candidate.SandboxID != "ab12cd34ef" || candidate.Peer != "alpha" {
			t.Fatalf("unexpected cloud link: %+v", candidate)
		}
		return true, remove()
	}
	for _, dryRun := range []bool{true, false} {
		pruned, err := pruneMeshNode(t.Context(), mesh.client, node, dryRun, record)
		if err != nil || len(pruned) != 1 {
			t.Fatalf("cloud prune (dry run %t): %v, %v", dryRun, pruned, err)
		}
		if dryRun && (mesh.issues != 0 || len(mesh.unlinked) != 0) {
			t.Fatalf("dry run authorized/mutated T3: %d sessions, unlinked %v", mesh.issues, mesh.unlinked)
		}
	}
	if mesh.issues != 1 || len(mesh.unlinked) != 1 || mesh.unlinked[0] != "candidate" {
		t.Fatalf("unexpected owner sessions or unlink operations: %d, %v", mesh.issues, mesh.unlinked)
	}
}

type meshFixture struct {
	client *http.Client
	nodes  []MeshNode

	mu                               sync.Mutex
	rejectSource, rejectTarget       string
	transientSource, transientTarget string
	grants, issues, lists            int
	unlinked                         []string
	links                            map[string]map[string]link
}

func newMeshFixture(t *testing.T, names ...string) *meshFixture {
	t.Helper()
	fixture := &meshFixture{links: make(map[string]map[string]link)}
	roots := x509.NewCertPool()
	addresses := make(map[string]string, len(names))
	for _, name := range names {
		fixture.links[name] = make(map[string]link)
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fixture.serve(t, name, w, r)
		}))
		t.Cleanup(server.Close)
		roots.AddCert(server.Certificate())
		host := name + ".test.ts.net"
		addresses[host+":443"] = server.Listener.Addr().String()
		fixture.nodes = append(fixture.nodes, MeshNode{
			Name: name, URL: "https://" + host, EnvironmentID: name,
			IssueToken: func(context.Context) (string, error) {
				fixture.mu.Lock()
				defer fixture.mu.Unlock()
				fixture.issues++
				return "fixture-owner-" + name, nil
			},
		})
	}
	dialer := &net.Dialer{}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "example.com"},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addresses[address])
		},
	}
	fixture.client = &http.Client{Transport: transport}
	t.Cleanup(fixture.client.CloseIdleConnections)
	return fixture
}

func (mesh *meshFixture) serve(t *testing.T, name string, w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case descriptorPath:
		if r.Header.Get("Authorization") != "" {
			t.Error("descriptor request carries a token before verifying identity")
		}
		_, _ = w.Write([]byte(`{"environmentId":"` + name + `","capabilities":{"peerLinks":true,"linkFence":true,"mcpModeLimitHeader":true,"peerLinkPairingCodes":true}}`))
	case "/api/auth/websocket-ticket/rpc", "/api/auth/pairing-token":
		if r.Header.Get("Authorization") != "Bearer fixture-owner-"+name {
			t.Error("wrong owner session for fixture endpoint")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/auth/websocket-ticket/rpc" {
			_, _ = w.Write([]byte(`{"ticket":"fixture-ticket"}`))
			return
		}
		var grant map[string]string
		if err := readJSON(r.Body, &grant); err != nil || grant["purpose"] != "peer-link" {
			t.Errorf("unexpected peer grant request: %v, %v", grant, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mesh.mu.Lock()
		mesh.grants++
		mesh.mu.Unlock()
		body, _ := json.Marshal(map[string]string{"purpose": "peer-link", "credential": "fixture-pair-" + name, "access": grant["access"]})
		_, _ = w.Write(body)
	case "/ws":
		upgrader := websocket.Upgrader{}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer ws.Close()
		var request struct {
			Tag     string            `json:"tag"`
			ID      string            `json:"id"`
			Payload map[string]string `json:"payload"`
		}
		if err := ws.ReadJSON(&request); err != nil {
			t.Error(err)
			return
		}
		mesh.mu.Lock()
		defer mesh.mu.Unlock()
		switch request.Tag {
		case "peerLinks.list":
			mesh.lists++
			entries := make([]link, 0, len(mesh.links[name]))
			for _, link := range mesh.links[name] {
				entries = append(entries, link)
			}
			_ = ws.WriteJSON(reply(request.ID, map[string]any{"links": entries}))
		case "peerLinks.link":
			target := request.Payload["expectedEnvironmentId"]
			if request.Payload["pairingCode"] != "fixture-pair-"+target || request.Payload["url"] != "https://"+target+".test.ts.net" {
				t.Error("link used the wrong one-time code or address")
			}
			if name == mesh.rejectSource && target == mesh.rejectTarget {
				_ = ws.WriteJSON(map[string]any{"_tag": "Exit", "requestId": request.ID, "exit": map[string]string{"_tag": "Failure"}})
				return
			}
			if name == mesh.transientSource && target == mesh.transientTarget {
				mesh.transientSource, mesh.transientTarget = "", ""
				_ = ws.WriteJSON(map[string]any{"_tag": "Exit", "requestId": request.ID, "exit": map[string]any{
					"_tag": "Failure", "cause": []any{map[string]any{"error": map[string]string{"reason": "unreachable"}}},
				}})
				return
			}
			linked := link{EnvironmentID: target, Status: "reachable", Access: request.Payload["access"]}
			mesh.links[name][target] = linked
			_ = ws.WriteJSON(reply(request.ID, linked))
		case "peerLinks.unlink":
			id := request.Payload["environmentId"]
			delete(mesh.links[name], id)
			mesh.unlinked = append(mesh.unlinked, id)
			_ = ws.WriteJSON(reply(request.ID, map[string]bool{"removed": true}))
		default:
			t.Errorf("unexpected RPC %s", request.Tag)
		}
	default:
		http.NotFound(w, r)
	}
}
