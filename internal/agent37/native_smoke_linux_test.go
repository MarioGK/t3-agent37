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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const smokeT3Version = "0.1.12-nightly.20261123.5138"

func TestNativeT3Smoke(t *testing.T) {
	if os.Getenv("T3_AGENT37_NATIVE_SMOKE") != "1" {
		t.Skip("set T3_AGENT37_NATIVE_SMOKE=1 to test the native T3 server")
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
	if err = nativeSmokeJSON(ctx, client, http.MethodPost, origin+"/oauth/token", form, "", &token); err != nil || token.Access == "" {
		t.Fatalf("exchange T3 pairing credential: %v", err)
	}
	checkPairingSession := func(connected bool) {
		t.Helper()
		command := exec.CommandContext(ctx, binary, "auth", "session", "list", "--base-dir", base, "--json")
		command.Env = env
		listing, err := command.Output()
		if err != nil {
			t.Fatal("could not inspect the authenticated client session")
		}
		type sessionEntry struct {
			Subject         string                 `json:"subject"`
			LastConnectedAt string                 `json:"lastConnectedAt"`
			Client          struct{ Label string } `json:"client"`
		}
		var sessions []sessionEntry
		if err := json.Unmarshal(listing, &sessions); err != nil {
			t.Fatal("T3 returned an invalid session list")
		}
		if !slices.ContainsFunc(sessions, func(session sessionEntry) bool {
			return session.Subject == "one-time-token" && session.Client.Label == "smoke" && (session.LastConnectedAt != "") == connected
		}) {
			t.Fatalf("expected a redeemed pairing session with connected=%t", connected)
		}
	}
	checkPairingSession(false)
	var auth struct {
		Ticket string `json:"ticket"`
	}
	if err = nativeSmokeJSON(ctx, client, http.MethodPost, origin+"/api/auth/websocket-ticket/rpc", nil, token.Access, &auth); err != nil || auth.Ticket == "" {
		t.Fatalf("mint native T3 WebSocket ticket: %v", err)
	}

	query := url.Values{"orchestrationProtocol": {"2"}, "wsTicket": {auth.Ticket}}
	endpoint := "ws" + strings.TrimPrefix(origin, "http") + "/ws?" + query.Encode()
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, response, err := dialer.DialContext(ctx, endpoint, http.Header{"Origin": {"t3code://app"}})
	if err != nil {
		if response != nil {
			response.Body.Close()
			t.Fatalf("upgrade native T3 WebSocket: HTTP %d", response.StatusCode)
		}
		t.Fatal("could not upgrade native T3 WebSocket")
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
		t.Fatalf("read native T3 Pong: %v", err)
	}
	checkPairingSession(true)
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
		arch, checksum = "x64", "77974bc1679da7e078370985fd4ed23ac479c5beec7a90b33668c9d6e89cf441"
	case "arm64":
		arch, checksum = "arm64", "28fccd13573672160fd9d5496932ed3ff2f49f834cf2a99deb13b6d85b4866b8"
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
