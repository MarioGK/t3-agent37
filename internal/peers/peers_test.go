package peers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

func TestConfiguredPeersValidatesPrivateOrigins(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("T3_AGENT37_PEERS_FILE", "")
	if err := os.Unsetenv("T3_AGENT37_PEERS_FILE"); err != nil {
		t.Fatal(err)
	}
	if list, err := Configured(); err != nil || len(list) != 0 {
		t.Fatalf("empty configuration: %v, %v", list, err)
	}
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "t3-agent37", "peers.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`[{"ssh":"beta","url":"https://beta.test.ts.net:18456/","environmentId":"beta-id"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	list, err := Configured()
	expected := Peer{Host: "beta", URL: "https://beta.test.ts.net:18456", EnvironmentID: "beta-id"}
	if err != nil || len(list) != 1 || list[0] != expected {
		t.Fatalf("configured peers: %v, %v", list, err)
	}
	for _, origin := range []string{
		"http://beta.test.ts.net", "https://example.net", "https://user@beta.test.ts.net",
		"https://127.0.0.1:43738",
		"https://beta.test.ts.net:65536", "https://beta.test.ts.net/#", "https://beta.test.ts.net//",
	} {
		if _, err := privateOrigin(origin); err == nil {
			t.Errorf("accepted invalid origin %q", origin)
		}
	}
	t.Setenv("T3_AGENT37_PEERS_FILE", "")
	if list, err := Configured(); err != nil || len(list) != 0 {
		t.Fatalf("disabled configuration: %v, %v", list, err)
	}
}

func TestDirectTailnetDiscoveryChecksIdentityWithoutPeerCapability(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != descriptorPath || r.Header.Get("X-Agent37-Key") != "" ||
			r.Header.Get("X-T3-Orchestration-Protocol") != "2" {
			t.Error("direct discovery sent the wrong request")
		}
		fmt.Fprint(w, `{"environmentId":"sandbox-proof","capabilities":{}}`)
	}))
	defer server.Close()
	target := newPeerAPI(server.Client(), "sandbox", server.URL, "sandbox-proof")
	if err := target.verify(t.Context(), 1, false); err != nil {
		t.Fatal(err)
	}
	if err := target.verify(t.Context(), 1, true); err == nil {
		t.Fatal("peer discovery accepted a server without scoped peer links")
	}
	target.environmentID = "different-sandbox"
	if err := target.verify(t.Context(), 1, false); err == nil {
		t.Fatal("direct discovery accepted the wrong environment")
	}
}

func TestT3PeersLinkWithNativeScopedGrantAndReuseReachableLink(t *testing.T) {
	upgrader := websocket.Upgrader{}
	var targetURL atomic.Value
	var linked atomic.Bool
	cloud := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Agent37-Key") != "" {
			t.Error("private sandbox request received an Agent37 key")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case descriptorPath:
			fmt.Fprint(w, `{"environmentId":"cloud-id","capabilities":{"peerLinks":true,"linkFence":true,"mcpModeLimitHeader":true,"peerLinkPairingCodes":true,"serverInstallation":{"kind":"standalone"}}}`)
		case "/api/auth/websocket-ticket/rpc":
			if r.ProtoMajor != 2 || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer fixture-owner-token" {
				t.Error("wrong ticket authentication")
			}
			fmt.Fprint(w, `{"ticket":"fixture-ticket"}`)
		case "/ws":
			if r.URL.Query().Get("orchestrationProtocol") != "2" || r.URL.Query().Get("wsTicket") != "fixture-ticket" ||
				r.Header.Get("Authorization") != "" || r.Header.Get("Origin") != "https://"+r.Host {
				t.Error("wrong websocket authentication")
			}
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
			switch request.Tag {
			case "peerLinks.list":
				if linked.Load() {
					_ = ws.WriteJSON(reply(request.ID, map[string]any{"links": []link{{EnvironmentID: "beta-id", Status: "reachable", Access: accessRequired}}}))
				} else {
					_ = ws.WriteJSON(reply(request.ID, map[string]any{"links": []link{}}))
				}
			case "peerLinks.link":
				if request.Payload["url"] != targetURL.Load() || request.Payload["expectedEnvironmentId"] != "beta-id" ||
					request.Payload["access"] != accessRequired || request.Payload["pairingCode"] != "fixture-one-time" {
					t.Error("wrong scoped peer link payload")
				}
				linked.Store(true)
				_ = ws.WriteJSON(reply(request.ID, link{EnvironmentID: "beta-id", Access: accessRequired}))
			default:
				t.Errorf("unexpected RPC %q", request.Tag)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	cloud.EnableHTTP2 = true
	cloud.StartTLS()
	defer cloud.Close()

	var grants atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Agent37-Key") != "" {
			t.Error("target received the Agent37 key")
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/auth/pairing-token" || r.Header.Get("Authorization") != "Bearer fixture-peer-token" {
			t.Error("wrong grant authentication")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["label"] != "t3-agent37 sandbox" ||
			body["purpose"] != "peer-link" || body["access"] != accessRequired {
			t.Error("wrong grant payload")
		}
		grants.Add(1)
		fmt.Fprint(w, `{"credential":"fixture-one-time","purpose":"peer-link","access":"approval-required"}`)
	}))
	defer target.Close()
	targetURL.Store(target.URL)

	ctx := t.Context()
	sourceAPI := newPeerAPI(cloud.Client(), "sandbox", cloud.URL, "cloud-id")
	targetAPI := newPeerAPI(target.Client(), "beta", target.URL, "beta-id")
	if err := sourceAPI.verify(ctx, 1, true); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := ensureLink(ctx, sourceAPI, "fixture-owner-token", targetAPI, "fixture-peer-token", target.URL); err != nil {
			t.Fatal(err)
		}
	}
	if count := grants.Load(); count != 1 {
		t.Errorf("minted %d pairing codes across two attempts; expected 1", count)
	}
}

func reply(id string, value any) any {
	return map[string]any{"_tag": "Exit", "requestId": id, "exit": map[string]any{"_tag": "Success", "value": value}}
}
