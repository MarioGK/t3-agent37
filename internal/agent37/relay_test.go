package agent37

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestRelayAdvertisesBearerPairingForItsLoopbackOrigin(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "identity" {
			t.Error("the relay did not request an uncompressed descriptor")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"origin"`)
		_, _ = io.WriteString(w, `{"environmentId":"example","capabilities":{"accessTokenRenewal":true,"linkedEnvironments":true}}`)
	}))
	defer upstream.Close()
	relay := httptest.NewUnstartedServer(nil)
	handler, err := RelayHandler(upstream.URL, "fixture-key", relay.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	relay.Config.Handler = handler
	relay.Start()
	defer relay.Close()
	response, err := relay.Client().Get(relay.URL + "/.well-known/t3/environment")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var descriptor Descriptor
	if err := json.NewDecoder(response.Body).Decode(&descriptor); err != nil {
		t.Fatal(err)
	}
	if descriptor.EnvironmentID != "example" || descriptor.Capabilities["accessTokenRenewal"] != false ||
		descriptor.Capabilities["linkedEnvironments"] != true || response.Header.Get("ETag") != "" {
		t.Fatal("relay discovery advertised the wrong pairing capabilities")
	}
}

func TestRelayTransportNegotiatesHTTP1WithTLS(t *testing.T) {
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()

	transport := relayTransport()
	defer transport.CloseIdleConnections()
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	transport.TLSClientConfig = &tls.Config{RootCAs: roots}
	response, err := (&http.Client{Transport: transport}).Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent || response.ProtoMajor != 1 {
		t.Fatalf("unexpected upstream response: %s %s", response.Proto, response.Status)
	}
}

func TestRelayForwardsHTTPAndWebSocket(t *testing.T) {
	const agentKey = "relay-agent-key"
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Agent37-Key") != agentKey ||
			r.Header.Get("Authorization") != "Bearer native-t3" || r.URL.Query().Get("ticket") != "+" {
			t.Error("WebSocket credentials or query were changed")
			http.Error(w, "invalid handshake", http.StatusUnauthorized)
			return
		}
		upgrader := websocket.Upgrader{
			Subprotocols: []string{"fixture"},
			CheckOrigin:  func(r *http.Request) bool { return r.Header.Get("Origin") == "t3code://app" },
		}
		headers := http.Header{"X-Agent37-Key": {"fixture response marker"}}
		conn, err := upgrader.Upgrade(w, r, headers)
		if err != nil {
			t.Error("could not upgrade fixture WebSocket")
			return
		}
		defer conn.Close()
		for {
			kind, content, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if conn.WriteMessage(kind, content) != nil {
				return
			}
		}
	})
	mux.HandleFunc("/check/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("X-Agent37-Key") != agentKey ||
			r.Header.Get("Authorization") != "Bearer native-t3" {
			t.Error("HTTP method or authorization was changed")
		}
		if r.Header.Get("Forwarded") != "" || r.Header.Get("X-Forwarded-Host") != "" ||
			r.Header.Get("X-Forwarded-Custom") != "" || r.Header.Get("X-Hop-Marker") != "" {
			t.Error("client forwarding or hop headers reached upstream")
		}
		if r.URL.EscapedPath() != "/check/a%2Fb" || r.URL.RawQuery != "value=%2B" {
			t.Errorf("raw request URL was not preserved: %s", r.URL.RequestURI())
		}
		data, err := io.ReadAll(r.Body)
		if err != nil || string(data) != `{"ok":true}` {
			t.Error("request body changed")
		}
		w.Header().Set("X-Agent37-Key", agentKey)
		w.Header().Set("Forwarded", "private")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("relay ok"))
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()
	downstream := httptest.NewUnstartedServer(nil)
	var err error
	downstream.Config.Handler, err = RelayHandler(upstream.URL, agentKey, downstream.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	downstream.Start()
	defer downstream.Close()

	req, err := http.NewRequest(http.MethodPost, downstream.URL+"/check/a%2Fb?value=%2B", strings.NewReader(`{"ok":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer native-t3")
	req.Header.Set("X-Agent37-Key", "client-supplied")
	req.Header.Set("Forwarded", "host=outside.example")
	req.Header.Set("X-Forwarded-Host", "outside.example")
	req.Header.Set("X-Forwarded-Custom", "outside.example")
	req.Header.Set("Connection", "X-Hop-Marker")
	req.Header.Set("X-Hop-Marker", "client-supplied")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || res.StatusCode != http.StatusCreated || string(data) != "relay ok" {
		t.Fatal("relay changed the upstream response")
	}
	if res.Header.Get("X-Agent37-Key") != "" || res.Header.Get("Forwarded") != "" {
		t.Fatal("private response headers crossed the relay")
	}

	wsURL := "ws" + strings.TrimPrefix(downstream.URL, "http") + "/ws?ticket=%2B"
	dialer := websocket.Dialer{Subprotocols: []string{"fixture"}}
	conn, wsResponse, err := dialer.Dial(wsURL, http.Header{
		"Authorization": {"Bearer native-t3"}, "Origin": {"t3code://app"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if conn.Subprotocol() != "fixture" || wsResponse.Header.Get("X-Agent37-Key") != "" {
		t.Fatal("WebSocket subprotocol or private headers were mishandled")
	}
	for _, kind := range []int{websocket.TextMessage, websocket.BinaryMessage} {
		if err := conn.WriteMessage(kind, []byte("payload")); err != nil {
			t.Fatal(err)
		}
		gotKind, payload, err := conn.ReadMessage()
		if err != nil || gotKind != kind || string(payload) != "payload" {
			t.Fatal("WebSocket message was not preserved")
		}
	}
}

func TestRelayRejectsForeignHostAndOrigin(t *testing.T) {
	handler, err := RelayHandler("http://127.0.0.1:1", "unused-fixture-key", "127.0.0.1:43737")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		url     string
		origins []string
	}{
		{name: "foreign host", url: "http://example.invalid/"},
		{name: "foreign origin", url: "http://127.0.0.1:43737/", origins: []string{"https://example.invalid"}},
		{name: "multiple origins", url: "http://127.0.0.1:43737/", origins: []string{"t3code://app", "http://127.0.0.1:43737"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, tc.url, nil)
			for _, origin := range tc.origins {
				request.Header.Add("Origin", origin)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status %d, want forbidden", response.Code)
			}
		})
	}
}
