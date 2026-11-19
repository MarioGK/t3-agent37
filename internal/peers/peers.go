// Package peers pairs a sandbox and its private T3 peers with short-lived native grants.
package peers

import (
	"bytes"
	"cmp"
	"context"
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
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	accessRequired        = "approval-required"
	descriptorPath        = "/.well-known/t3/environment"
	jsonLimit             = 4 << 20
	orchestrationProtocol = "2"
)

var sshAlias = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
var environmentID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:-]{0,127}$`)

// Peer describes a trusted workstation SSH alias and the same T3 server's private API.
type Peer struct {
	Host          string `json:"ssh"`
	URL           string `json:"url"`
	EnvironmentID string `json:"environmentId"`
}

// Configured reads the peer file or returns an empty list if it has not been configured.
func Configured() ([]Peer, error) {
	override, overridden := os.LookupEnv("T3_AGENT37_PEERS_FILE")
	if overridden && override == "" {
		return nil, nil
	}
	path := override
	switch {
	case !overridden:
		base := os.Getenv("XDG_CONFIG_HOME")
		var err error
		if base == "" && runtime.GOOS == "windows" {
			base, err = os.UserConfigDir()
		} else if base == "" {
			var home string
			home, err = os.UserHomeDir()
			base = filepath.Join(home, ".config")
		}
		if err != nil {
			return nil, errors.New("could not locate the T3 peer configuration")
		}
		path = filepath.Join(base, "t3-agent37", "peers.json")
	case strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`):
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, errors.New("could not locate the T3 peer configuration")
		}
		path = filepath.Join(home, path[2:])
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		if !overridden {
			return nil, nil
		}
		return nil, errors.New("the configured T3 peers file does not exist")
	}
	if err != nil {
		return nil, errors.New("could not read the configured T3 peers file")
	}
	defer file.Close()

	contents, err := io.ReadAll(io.LimitReader(file, jsonLimit+1))
	if err != nil || len(contents) > jsonLimit {
		return nil, errors.New("could not read the configured T3 peers file")
	}
	var list []Peer
	if json.Unmarshal(contents, &list) != nil || list == nil {
		return nil, errors.New("invalid T3 peer configuration")
	}
	seen := make(map[string]bool, len(list))
	for i := range list {
		peer := &list[i]
		if !sshAlias.MatchString(peer.Host) || !environmentID.MatchString(peer.EnvironmentID) || seen[peer.EnvironmentID] {
			return nil, errors.New("invalid T3 peer configuration")
		}
		peer.URL, err = privateOrigin(peer.URL)
		if err != nil {
			return nil, err
		}
		seen[peer.EnvironmentID] = true
	}
	return list, nil
}

func privateOrigin(raw string) (string, error) {
	invalid := errors.New("T3 peers need a private Tailscale HTTPS origin")
	origin, err := url.Parse(raw)
	if err != nil || origin.Scheme != "https" || origin.User != nil ||
		!strings.HasSuffix(strings.ToLower(origin.Hostname()), ".ts.net") ||
		origin.RawQuery != "" || origin.ForceQuery || strings.Contains(raw, "#") ||
		origin.RawPath != "" || (origin.Path != "" && origin.Path != "/") || strings.HasSuffix(origin.Host, ":") {
		return "", invalid
	}
	if port := origin.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number <= 0 || number > 65535 {
			return "", invalid
		}
	}
	return strings.TrimSuffix(raw, "/"), nil
}

func issueOverSSH(ctx context.Context, peer Peer) (string, error) {
	// The SSH stream is captured and the owner session is never passed in argv.
	const script = `set -eu
for binary in "$HOME/.t3/bin/t3" "$HOME/.local/bin/t3" "$HOME/.local/share/mise/shims/t3"; do
    if test -x "$binary"; then
        exec "$binary" auth session issue --json --ttl 5m --label t3-agent37-peer-sync \
            --scope access:read --scope access:write --scope orchestration:read --scope orchestration:operate
    fi
done
exit 127
`
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=5", peer.Host, "sh", "-s")
	command.Stdin = strings.NewReader(script)
	output, err := command.Output()
	var response struct {
		Token string `json:"token"`
	}
	if err != nil || json.Unmarshal(output, &response) != nil || response.Token == "" {
		return "", fmt.Errorf("could not authorize T3 peer administration over SSH on %s", peer.Host)
	}
	return response.Token, nil
}

type peerAPI struct {
	client        *http.Client
	name          string
	origin        string
	environmentID string
	headers       http.Header
}

func newPeerAPI(client *http.Client, name, origin, id, agent37Key string) peerAPI {
	headers := make(http.Header)
	headers.Set("X-T3-Orchestration-Protocol", orchestrationProtocol)
	if agent37Key != "" {
		headers.Set("X-Agent37-Key", agent37Key)
	}
	return peerAPI{client: client, name: name, origin: origin, environmentID: id, headers: headers}
}

func (p peerAPI) request(ctx context.Context, method, route, token string, payload, result any) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("could not encode %s request for %s", route, p.name)
		}
		body = bytes.NewReader(data)
	}
	// Keep request and response errors out of diagnostics because they may contain credentials.
	failure := fmt.Errorf("could not read %s from %s", route, p.name)
	req, err := http.NewRequestWithContext(ctx, method, p.origin+route, body)
	if err != nil {
		return failure
	}
	req.Header = p.headers.Clone()
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := p.client.Do(req)
	if err != nil {
		return failure
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%s returned HTTP %d for %s", p.name, response.StatusCode, route)
	}
	if err := readJSON(response.Body, result); err != nil {
		return failure
	}
	return nil
}

func readJSON(body io.Reader, result any) error {
	data, err := io.ReadAll(io.LimitReader(body, jsonLimit+1))
	if err != nil {
		return err
	}
	if len(data) > jsonLimit {
		return errors.New("response exceeds the size limit")
	}
	return json.Unmarshal(data, result)
}

func (p peerAPI) verify(ctx context.Context, attempts int, requirePeerLinks bool) error {
	type descriptor struct {
		EnvironmentID string `json:"environmentId"`
		Capabilities  struct {
			PeerLinks            bool `json:"peerLinks"`
			LinkFence            bool `json:"linkFence"`
			MCPModeLimitHeader   bool `json:"mcpModeLimitHeader"`
			PeerLinkPairingCodes bool `json:"peerLinkPairingCodes"`
		} `json:"capabilities"`
	}
	var requestErr error
	for attempt := range max(attempts, 1) {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
		var row descriptor
		requestErr = p.request(ctx, http.MethodGet, descriptorPath, "", nil, &row)
		if requestErr != nil {
			continue
		}
		if row.EnvironmentID != p.environmentID {
			return fmt.Errorf("T3 environment identity changed for %s", p.name)
		}
		capabilities := row.Capabilities
		if requirePeerLinks && (!capabilities.PeerLinks || !capabilities.LinkFence || !capabilities.MCPModeLimitHeader || !capabilities.PeerLinkPairingCodes) {
			return fmt.Errorf("%s does not support scoped T3 peer links", p.name)
		}
		return nil
	}
	return requestErr
}

// VerifyTailnet checks that the private address serves the expected sandbox.
func VerifyTailnet(ctx context.Context, name, environmentID, address string) (string, error) {
	origin, err := privateOrigin(address)
	if err != nil {
		return "", err
	}
	client := newHTTPClient(5 * time.Second)
	defer client.CloseIdleConnections()
	target := newPeerAPI(client, name, origin, environmentID, "")
	timeout := fmt.Errorf("%s joined Tailscale, but its private HTTPS endpoint did not become ready within one minute", name)
	ready, cancel := context.WithTimeoutCause(ctx, time.Minute, timeout)
	defer cancel()
	if err := target.verify(ready, 60, false); err != nil {
		return "", cmp.Or(context.Cause(ready), err)
	}
	return origin, nil
}

func (p peerAPI) rpc(ctx context.Context, token, method string, payload, result any) error {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	var ticket struct {
		Ticket string `json:"ticket"`
	}
	if err := p.request(ctx, http.MethodPost, "/api/auth/websocket-ticket/rpc", token, nil, &ticket); err != nil {
		return err
	}

	// Upgrade failures can contain a one-time ticket, so never return the raw error.
	failure := fmt.Sprintf("%s did not complete T3 RPC %s", p.name, method)
	wsURL, err := url.Parse(p.origin + "/ws")
	if err != nil || ticket.Ticket == "" {
		return errors.New(failure)
	}
	wsURL.Scheme = "wss"
	query := wsURL.Query()
	query.Set("wsTicket", ticket.Ticket)
	query.Set("orchestrationProtocol", orchestrationProtocol)
	wsURL.RawQuery = query.Encode()

	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	if transport, ok := p.client.Transport.(*http.Transport); ok {
		dialer.NetDialContext = transport.DialContext
		dialer.TLSClientConfig = transport.TLSClientConfig
	}
	ws, response, err := dialer.DialContext(ctx, wsURL.String(), p.headers)
	if err != nil {
		if response != nil {
			response.Body.Close()
		}
		return errors.New(failure)
	}
	defer ws.Close()
	stop := context.AfterFunc(ctx, func() { _ = ws.Close() })
	defer stop()
	ws.SetReadLimit(jsonLimit)
	deadline, _ := ctx.Deadline()
	_ = ws.SetReadDeadline(deadline)
	_ = ws.SetWriteDeadline(deadline)

	// Every call has a dedicated connection and always uses request ID 1.
	if ws.WriteJSON(map[string]any{
		"_tag": "Request", "id": "1", "tag": method, "payload": payload, "headers": []any{},
	}) != nil {
		return errors.New(failure)
	}
	for {
		var message struct {
			Tag       string          `json:"_tag"`
			RequestID json.RawMessage `json:"requestId"`
			Exit      struct {
				Tag   string          `json:"_tag"`
				Value json.RawMessage `json:"value"`
				Cause []struct {
					Error struct {
						Reason string `json:"reason"`
					} `json:"error"`
				} `json:"cause"`
			} `json:"exit"`
		}
		if ws.ReadJSON(&message) != nil {
			return errors.New(failure)
		}
		if message.Tag == "Ping" {
			if ws.WriteJSON(map[string]string{"_tag": "Pong"}) != nil {
				return errors.New(failure)
			}
			continue
		}
		if message.Tag != "Exit" || (string(message.RequestID) != `"1"` && string(message.RequestID) != "1") {
			continue
		}
		if message.Exit.Tag == "Success" {
			if json.Unmarshal(message.Exit.Value, result) == nil {
				return nil
			}
			return errors.New(failure)
		}
		for _, cause := range message.Exit.Cause {
			switch reason := cause.Error.Reason; reason {
			case "disabled", "unreachable", "incompatible", "self", "pairing_rejected", "storage":
				return fmt.Errorf("%s (%s)", failure, strings.ReplaceAll(reason, "_", " "))
			}
		}
		return errors.New(failure)
	}
}

type link struct {
	EnvironmentID string   `json:"environmentId"`
	Status        string   `json:"status"`
	Access        string   `json:"access"`
	URLs          []string `json:"urls"`
}

func ensureLink(ctx context.Context, source peerAPI, sourceToken string, target peerAPI, targetToken, targetURL string) error {
	var listed struct {
		Links []link `json:"links"`
	}
	if err := source.rpc(ctx, sourceToken, "peerLinks.list", struct{}{}, &listed); err != nil {
		return err
	}
	access := accessRequired
	index := slices.IndexFunc(listed.Links, func(previous link) bool {
		return previous.EnvironmentID == target.environmentID
	})
	if index >= 0 {
		previous := listed.Links[index]
		if previous.Status == "reachable" {
			return nil
		}
		if previous.Status != "expired" || !slices.Contains(previous.URLs, targetURL) {
			return fmt.Errorf("review the existing T3 link from %s to %s", source.name, target.name)
		}
		access = previous.Access
	}
	if access != accessRequired && access != "read-only" {
		return fmt.Errorf("renew the existing link from %s to %s in T3", source.name, target.name)
	}
	var grant struct {
		Purpose    string `json:"purpose"`
		Access     string `json:"access"`
		Credential string `json:"credential"`
	}
	if err := target.request(ctx, http.MethodPost, "/api/auth/pairing-token", targetToken, map[string]string{
		"label": "t3-agent37 " + source.name, "purpose": "peer-link", "access": access,
	}, &grant); err != nil {
		return err
	}
	if grant.Purpose != "peer-link" || grant.Access != access || grant.Credential == "" {
		return fmt.Errorf("%s did not issue a scoped peer pairing code", target.name)
	}
	var linked link
	if err := source.rpc(ctx, sourceToken, "peerLinks.link", map[string]string{
		"url": targetURL, "pairingCode": grant.Credential, "access": access,
		"expectedEnvironmentId": target.environmentID,
	}, &linked); err != nil {
		return err
	}
	if linked.EnvironmentID != target.environmentID || linked.Access != access {
		return fmt.Errorf("T3 did not verify the link from %s to %s", source.name, target.name)
	}
	return nil
}

// Sync verifies both sandbox endpoints and sets up directed links to and from each peer.
func Sync(ctx context.Context, configured []Peer, name, sandboxID, privateURL, protectedURL, agent37Key, cloudToken string) error {
	privateURL, err := privateOrigin(privateURL)
	if err != nil {
		return err
	}
	client := newHTTPClient(35 * time.Second)
	defer client.CloseIdleConnections()

	cloud := newPeerAPI(client, name, protectedURL, sandboxID, agent37Key)
	private := newPeerAPI(client, name, privateURL, sandboxID, "")
	if err := cloud.verify(ctx, 1, true); err != nil {
		return err
	}
	if err := private.verify(ctx, 6, true); err != nil {
		return err
	}
	var failures []string
	for _, peer := range configured {
		target := newPeerAPI(client, peer.Host, peer.URL, peer.EnvironmentID, "")
		if err := syncPeer(ctx, cloud, cloudToken, target, peer, privateURL); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %s", peer.Host, err))
		}
	}
	if len(failures) != 0 {
		return fmt.Errorf("T3 peer links incomplete: %s", strings.Join(failures, "; "))
	}
	return nil
}

func syncPeer(ctx context.Context, cloud peerAPI, cloudToken string, target peerAPI, peer Peer, privateURL string) error {
	if err := target.verify(ctx, 1, true); err != nil {
		return err
	}
	token, err := issueOverSSH(ctx, peer)
	if err != nil {
		return err
	}
	if err := ensureLink(ctx, cloud, cloudToken, target, token, peer.URL); err != nil {
		return err
	}
	return ensureLink(ctx, target, token, cloud, cloudToken, privateURL)
}

func newHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = dialer.DialContext

	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
