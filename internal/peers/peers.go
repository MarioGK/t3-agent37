// Package peers pairs a sandbox and its private T3 peers with short-lived native grants.
package peers

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
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
var unixUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)
var errGuestAgentUnavailable = errors.New("Incus guest agent unavailable")
var errPeerUnreachable = errors.New("unreachable")

// Peer describes a trusted workstation SSH alias and the same T3 server's private API.
type Peer struct {
	Host          string      `json:"ssh"`
	URL           string      `json:"url"`
	EnvironmentID string      `json:"environmentId"`
	Incus         *IncusOwner `json:"incus,omitempty"`
}

// IncusOwner is an optional owner-command route through a trusted SSH host.
type IncusOwner struct {
	Host     string `json:"ssh"`
	Instance string `json:"instance"`
	User     string `json:"user"`
	UID      uint32 `json:"uid"`
	GID      uint32 `json:"gid"`
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
		if relay := peer.Incus; relay != nil &&
			(!sshAlias.MatchString(relay.Host) || !sshAlias.MatchString(relay.Instance) ||
				!unixUser.MatchString(relay.User) || relay.UID == 0 || relay.GID == 0) {
			return nil, fmt.Errorf("invalid Incus owner route for T3 peer %s", peer.Host)
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

const ownerCLIScript = `set -eu
base=${T3CODE_HOME:-$HOME/.t3}
export MISE_AUTO_INSTALL=0
pid=$(systemctl --user show --value -p MainPID "t3code-$(id -un).service" 2>/dev/null || :)
native=
case "$pid" in
    ''|0|*[!0-9]*) ;;
    *) native=$(readlink "/proc/$pid/exe" 2>/dev/null || :) ;;
esac
cached=
for path in "$base"/runtime/versions/*/t3; do
    if test -x "$path"; then cached=$path; fi
done
for binary in "$native" "$cached" "$HOME/.t3/bin/t3" "$HOME/.local/bin/t3" "$HOME/.local/share/mise/shims/t3"; do
    if test -x "$binary"; then
        exec "$binary" "$@" --base-dir "$base"
    fi
done
exit 127
`

// Only pass fixed arguments or validated shell-safe identifiers to the SSH command.
func runOwnerCLI(ctx context.Context, peer Peer, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	host := peer.Host
	remote := append([]string{"sh", "-s", "--"}, args...)
	if relay := peer.Incus; relay != nil {
		host = relay.Host
		remote = append([]string{"incus", "exec", relay.Instance,
			"--user", strconv.FormatUint(uint64(relay.UID), 10), "--group", strconv.FormatUint(uint64(relay.GID), 10),
			"--env", "HOME=/home/" + relay.User, "--env", "PATH=/run/current-system/sw/bin:/usr/local/bin:/usr/bin:/bin",
			"--"}, remote...)
	}
	ssh := []string{"-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=5", host}
	command := exec.CommandContext(ctx, "ssh", append(ssh, remote...)...)
	command.Stdin = strings.NewReader(ownerCLIScript)
	output, err := command.Output()
	return output, ownerCommandError(peer, err)
}

func ownerCommandError(peer Peer, err error) error {
	if err == nil {
		return nil
	}
	// SSH output can include credential-bearing CLI diagnostics. Only expose
	// the fixed Incus transport error, with names from the trusted peer file.
	if exit, ok := errors.AsType[*exec.ExitError](err); ok && peer.Incus != nil {
		for line := range strings.SplitSeq(string(exit.Stderr), "\n") {
			if strings.TrimSpace(line) == "Error: Failed to connect to the agent" {
				return fmt.Errorf("%w for %s via %s", errGuestAgentUnavailable, peer.Incus.Instance, peer.Incus.Host)
			}
		}
	}
	return errors.New("SSH owner command failed")
}

func issueOverSSH(ctx context.Context, peer Peer) (string, error) {
	output, err := runOwnerCLI(ctx, peer, "auth", "session", "issue", "--json", "--ttl", "5m", "--label", "t3-agent37-peer-sync",
		"--scope", "access:read", "--scope", "access:write", "--scope", "orchestration:read", "--scope", "orchestration:operate")
	var response struct {
		Token string `json:"token"`
	}
	if err != nil {
		return "", fmt.Errorf("could not authorize T3 peer administration on %s: %w", peer.Host, err)
	}
	if json.Unmarshal(output, &response) != nil || response.Token == "" {
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

func newPeerAPI(client *http.Client, name, origin, id string) peerAPI {
	headers := make(http.Header)
	headers.Set("X-T3-Orchestration-Protocol", orchestrationProtocol)
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
			delay := time.Second
			if !requirePeerLinks && attempt <= 8 {
				delay = 250 * time.Millisecond
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
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
	target := newPeerAPI(client, name, origin, environmentID)
	// Accept the endpoint as soon as it works, with enough time for a fresh
	// node to finish certificate issuance after a slow first handshake.
	timeout := fmt.Errorf("%s joined Tailscale, but its private HTTPS endpoint did not become ready within two minutes", name)
	ready, cancel := context.WithTimeoutCause(ctx, 2*time.Minute, timeout)
	defer cancel()
	if err := target.verify(ready, 240, false); err != nil {
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
	tlsConfig := &tls.Config{}
	if transport, ok := p.client.Transport.(*http.Transport); ok {
		dialer.NetDialContext = transport.DialContext
		if transport.TLSClientConfig != nil {
			tlsConfig = transport.TLSClientConfig.Clone()
		}
	}
	// Gorilla writes an HTTP/1.1 Upgrade. Tailscale Serve also advertises
	// HTTP/2, which the ticket request's HTTP transport may have negotiated.
	tlsConfig.NextProtos = []string{"http/1.1"}
	dialer.TLSClientConfig = tlsConfig
	headers := p.headers.Clone()
	headers.Set("Origin", p.origin)
	ws, response, err := dialer.DialContext(ctx, wsURL.String(), headers)
	if err != nil {
		if response != nil {
			response.Body.Close()
			return fmt.Errorf("%s (WebSocket HTTP %d)", failure, response.StatusCode)
		}
		return fmt.Errorf("%s (WebSocket connection failed)", failure)
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
		return fmt.Errorf("%s (WebSocket send failed)", failure)
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
						Tag    string `json:"_tag"`
						Reason string `json:"reason"`
					} `json:"error"`
				} `json:"cause"`
			} `json:"exit"`
		}
		if err := ws.ReadJSON(&message); err != nil {
			if closeError, ok := errors.AsType[*websocket.CloseError](err); ok {
				return fmt.Errorf("%s (WebSocket closed with code %d)", failure, closeError.Code)
			}
			if ctx.Err() != nil {
				return fmt.Errorf("%s (timed out)", failure)
			}
			return fmt.Errorf("%s (WebSocket response failed)", failure)
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
			return fmt.Errorf("%s (invalid result)", failure)
		}
		for _, cause := range message.Exit.Cause {
			if cause.Error.Tag == "EnvironmentAuthorizationError" {
				return fmt.Errorf("%s (session scope denied)", failure)
			}
			switch reason := cause.Error.Reason; reason {
			case "unreachable":
				return fmt.Errorf("%s (%w)", failure, errPeerUnreachable)
			case "disabled", "incompatible", "self", "pairing_rejected", "storage":
				return fmt.Errorf("%s (%s)", failure, strings.ReplaceAll(reason, "_", " "))
			}
		}
		return fmt.Errorf("%s (T3 rejected the request)", failure)
	}
}

type link struct {
	EnvironmentID string   `json:"environmentId"`
	Status        string   `json:"status"`
	Access        string   `json:"access"`
	URLs          []string `json:"urls"`
}

func listPeerLinks(ctx context.Context, source peerAPI, token string) ([]link, error) {
	var listed struct {
		Links []link `json:"links"`
	}
	if err := source.rpc(ctx, token, "peerLinks.list", struct{}{}, &listed); err != nil {
		return nil, err
	}
	return listed.Links, nil
}

func ensureListedLink(ctx context.Context, source peerAPI, sourceToken string, target peerAPI, targetToken string, previous link) error {
	access := accessRequired
	if previous.EnvironmentID != "" {
		if previous.Status == "reachable" {
			return nil
		}
		if len(previous.URLs) != 1 || !strings.EqualFold(strings.TrimSuffix(previous.URLs[0], "/"), target.origin) {
			return errors.New("review the existing address in T3")
		}
		if previous.Status == "unreachable" {
			return fmt.Errorf("the existing T3 link is temporarily %w", errPeerUnreachable)
		}
		if previous.Status != "expired" {
			return errors.New("review the existing link in T3")
		}
		access = previous.Access
	}
	if access != accessRequired && access != "read-only" {
		return errors.New("renew the existing access grant in T3")
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
		"url": target.origin, "pairingCode": grant.Credential, "access": access,
		"expectedEnvironmentId": target.environmentID,
	}, &linked); err != nil {
		return err
	}
	if linked.EnvironmentID != target.environmentID || linked.Access != access {
		return errors.New("T3 did not verify the link identity and access")
	}
	return nil
}

func newHTTPClient(timeout time.Duration) *http.Client {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{ForceAttemptHTTP2: true}
	}
	transport := base.Clone()
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
