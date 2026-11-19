package agent37

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const smokeT3Version = "0.1.11-nightly.20261118.4903"

func TestNativeT3RelaySmoke(t *testing.T) {
	if os.Getenv("T3_AGENT37_NATIVE_SMOKE") != "1" {
		t.Skip("set T3_AGENT37_NATIVE_SMOKE=1 to run the native T3 relay smoke test")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	root := t.TempDir()
	binary := nativeSmokeBinary(t, ctx, root)
	base := filepath.Join(root, "data")
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	// T3 detects installed harnesses at startup. Keep their discovery inside
	// the fixture even when a real Codex CLI is installed on the test host.
	if err := os.Symlink(truePath, filepath.Join(bin, "codex")); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "HOME="+root, "T3CODE_HOME="+base, "CODEX_HOME="+filepath.Join(root, ".codex"),
		"XDG_CONFIG_HOME="+filepath.Join(root, ".config"), "XDG_DATA_HOME="+filepath.Join(root, ".local", "share"),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	client := &http.Client{Timeout: 5 * time.Second}

	start := func() (string, Descriptor, func()) {
		t.Helper()
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
		origin := "http://" + listener.Addr().String()
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		serveCtx, stop := context.WithCancel(ctx)
		serve := exec.CommandContext(serveCtx, binary, "serve", "--host", "127.0.0.1", "--port", port,
			"--base-dir", base, "--mode", "web", "--no-browser")
		serve.Env = env
		serve.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		serve.Cancel = func() error {
			err := syscall.Kill(-serve.Process.Pid, syscall.SIGKILL)
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		if err := serve.Start(); err != nil {
			stop()
			t.Fatalf("start pinned T3: %v", err)
		}
		done := make(chan struct{})
		go func() {
			_ = serve.Wait()
			close(done)
		}()
		cleanup := func() {
			stop()
			<-done
		}
		t.Cleanup(cleanup)

		readyCtx, readyCancel := context.WithTimeout(ctx, 20*time.Second)
		defer readyCancel()
		ticker := time.NewTicker(150 * time.Millisecond)
		defer ticker.Stop()
		for {
			var info Descriptor
			err := nativeSmokeJSON(readyCtx, client, http.MethodGet, origin+"/.well-known/t3/environment", nil, "", &info)
			if err == nil {
				if info.EnvironmentID == "" || strings.TrimPrefix(info.ServerVersion, "v") != smokeT3Version {
					t.Fatalf("pinned T3 reported unexpected environment identity or version %q", info.ServerVersion)
				}
				return origin, info, cleanup
			}
			select {
			case <-done:
				t.Fatal("pinned T3 exited during startup")
			case <-readyCtx.Done():
				t.Fatalf("pinned T3 did not become ready: %v", err)
			case <-ticker.C:
			}
		}
	}

	origin, upstream, stop := start()
	relay := httptest.NewUnstartedServer(nil)
	defer relay.Close()
	handler, err := RelayHandler(origin, "fixture-only", relay.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	relay.Config.Handler = handler
	relay.Start()
	var downstream Descriptor
	if err = nativeSmokeJSON(ctx, client, http.MethodGet, relay.URL+"/.well-known/t3/environment", nil, "", &downstream); err != nil {
		t.Fatalf("discover T3 through relay: %v", err)
	}
	if upstream.EnvironmentID != downstream.EnvironmentID {
		t.Fatal("T3 environment identity changed through relay")
	}

	pairCtx, pairCancel := context.WithTimeout(ctx, 15*time.Second)
	defer pairCancel()
	pair := exec.CommandContext(pairCtx, binary, "auth", "pairing", "create", "--base-dir", base, "--ttl", "5m", "--label", "smoke", "--json")
	pair.Env = env
	output, err := pair.Output()
	if err != nil {
		t.Fatalf("create disposable T3 pairing credential: %v", err)
	}
	var pairing struct {
		Credential string `json:"credential"`
	}
	if err = json.Unmarshal(output, &pairing); err != nil || pairing.Credential == "" {
		t.Fatal("T3 pairing response did not contain a credential")
	}
	form := url.Values{
		"grant_type":           {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":        {pairing.Credential},
		"subject_token_type":   {"urn:t3:params:oauth:token-type:environment-bootstrap"},
		"requested_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
	}
	var token struct {
		Access string `json:"access_token"`
	}
	if err = nativeSmokeJSON(ctx, client, http.MethodPost, relay.URL+"/oauth/token", form, "", &token); err != nil || token.Access == "" {
		t.Fatalf("exchange T3 pairing credential through relay: %v", err)
	}
	var auth struct {
		Ticket string `json:"ticket"`
	}
	if err = nativeSmokeJSON(ctx, client, http.MethodPost, relay.URL+"/api/auth/websocket-ticket/rpc", nil, token.Access, &auth); err != nil || auth.Ticket == "" {
		t.Fatalf("mint native T3 WebSocket ticket through relay: %v", err)
	}

	query := url.Values{"orchestrationProtocol": {"2"}, "wsTicket": {auth.Ticket}}
	endpoint := "ws" + strings.TrimPrefix(relay.URL, "http") + "/ws?" + query.Encode()
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, response, err := dialer.DialContext(ctx, endpoint, http.Header{"Origin": {"t3code://app"}})
	if err != nil {
		if response != nil {
			response.Body.Close()
			t.Fatalf("upgrade native T3 WebSocket through relay: HTTP %d", response.StatusCode)
		}
		t.Fatal("could not upgrade native T3 WebSocket through relay")
	}
	defer conn.Close()
	connectionDeadline := time.Now().Add(5 * time.Second)
	if err := conn.SetWriteDeadline(connectionDeadline); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(connectionDeadline); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(map[string]string{"_tag": "Ping"}); err != nil {
		t.Fatalf("send native T3 Ping: %v", err)
	}
	var pong struct {
		Tag string `json:"_tag"`
	}
	if err = conn.ReadJSON(&pong); err != nil || pong.Tag != "Pong" {
		t.Fatalf("read native T3 Pong through relay: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	stop()
	_, restarted, _ := start()
	if upstream.EnvironmentID != restarted.EnvironmentID {
		t.Fatal("T3 changed its environment identity after restart")
	}
}

func nativeSmokeJSON(ctx context.Context, client *http.Client, method, endpoint string, form url.Values, bearer string, result any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	request.Header.Set("Origin", "t3code://app")
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(result); err != nil {
		return errors.New("invalid T3 JSON response")
	}
	return nil
}

func nativeSmokeBinary(t *testing.T, ctx context.Context, root string) string {
	t.Helper()
	if filename := os.Getenv("T3_AGENT37_T3_BINARY"); filename != "" {
		absolute, err := filepath.Abs(filename)
		if err != nil {
			t.Fatal(err)
		}
		return absolute
	}
	var arch, checksum string
	switch runtime.GOARCH {
	case "amd64":
		arch, checksum = "x64", "d7450450c9f71c6daac7fb1ec0d5b22fd7e2365f9e124c730fd5d3dc3e3e1d2a"
	case "arm64":
		arch, checksum = "arm64", "3b35b94c96098bdd4282536c7f8ca18447593b8c00af6cf7bfcc2e16be155ceb"
	default:
		t.Skipf("no pinned Linux T3 binary for %s", runtime.GOARCH)
	}
	baseURL := "https://github.com/pingdotgg/t3code/releases/download/v" + smokeT3Version + "/"
	name := "t3-" + smokeT3Version + "-linux-" + arch + ".tar.gz"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+name, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 80 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("could not download pinned T3 runtime")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("download pinned T3 runtime: HTTP %d", response.StatusCode)
	}
	archive, err := os.Create(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	const archiveLimit = 512 << 20
	size, copyErr := io.Copy(io.MultiWriter(archive, hash), io.LimitReader(response.Body, archiveLimit+1))
	closeErr := archive.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		t.Fatalf("save pinned T3 runtime: %v", err)
	}
	if size > archiveLimit || hex.EncodeToString(hash.Sum(nil)) != checksum {
		t.Fatal("pinned T3 runtime archive failed size or checksum verification")
	}
	runtimeDir := filepath.Join(root, "runtime")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := exec.CommandContext(ctx, "tar", "-xzf", archive.Name(), "--strip-components=1", "-C", runtimeDir).Run(); err != nil {
		t.Fatalf("extract pinned T3 runtime: %v", err)
	}
	return filepath.Join(runtimeDir, "t3")
}
