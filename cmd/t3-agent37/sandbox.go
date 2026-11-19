package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	"github.com/MarioGK/t3-agent37/internal/peers"
)

// The Agent37 guest already has Python. The workstation executable carries
// these guest programs and delivers them over the protected exec API.
//
//go:embed guest/remote.py
var guestRemote []byte

//go:embed guest/tailnet.py
var guestTailnet []byte

const remote = `python3 "$HOME/.local/share/t3-agent37/remote.py"`

const installGuest = `import base64, pathlib, sys, os, tempfile
path = pathlib.Path.home() / '.local/share/t3-agent37'
path.mkdir(mode=0o700, parents=True, exist_ok=True)
for filename, body in zip(sys.argv[1::2], sys.argv[2::2]):
    with tempfile.NamedTemporaryFile(dir=path, delete=False) as stage:
        stage.write(base64.b64decode(body))
    os.replace(stage.name, path / filename)`

type setupProof struct {
	EnvironmentID string `json:"environmentId"`
	Port          int    `json:"port"`
	ProviderReady bool   `json:"providerReady"`
	PeerProxy     bool   `json:"peerProxy"`
}

func execJSON[T any](ctx context.Context, client *agent37.Client, id, command string) (T, error) {
	var result T
	output, err := client.Exec(ctx, id, command)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return result, errors.New("sandbox returned invalid setup data")
	}
	return result, nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func waitForT3(ctx context.Context, client *agent37.Client, id, expected string, port int) (agent37.Descriptor, int, error) {
	deadline, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	return client.WaitForT3(deadline, id, expected, port)
}

func setup(ctx context.Context, client *agent37.Client, instance agent37.Instance, configured []peers.Peer) (setupProof, error) {
	provider := displayProvider(instance)
	if _, allowed := templates[provider]; !allowed {
		return setupProof{}, errors.New("sandbox metadata names an unsupported T3 provider")
	}
	command := "python3 -c " + shellQuote(installGuest) + " remote.py " + base64.StdEncoding.EncodeToString(guestRemote)
	if instance.Metadata["tailnet"] == "tailscale" {
		command += " tailnet.py " + base64.StdEncoding.EncodeToString(guestTailnet)
	}
	command += " && " + remote + " setup " + provider
	if len(configured) != 0 {
		command += " --peer-links"
	}
	proof, err := execJSON[setupProof](ctx, client, instance.ID, command)
	if err != nil {
		return proof, err
	}
	if proof.EnvironmentID == "" {
		return proof, errors.New("sandbox setup did not return a T3 environment ID")
	}
	_, _, err = waitForT3(ctx, client, instance.ID, proof.EnvironmentID, proof.Port)
	return proof, err
}

type tailIdentity struct {
	ID                 string `json:"id"`
	Hostname           string `json:"hostname"`
	EnrollmentRequired bool   `json:"enrollmentRequired"`
}

func joinTailnet(ctx context.Context, client *agent37.Client, id, name string) error {
	if err := joinTailnetOnce(ctx, client, id, name); err != nil {
		return fmt.Errorf("could not enroll %s in Tailscale: %w", name, err)
	}
	return nil
}

func joinTailnetOnce(ctx context.Context, client *agent37.Client, id, name string) error {
	if err := agent37.ValidateID(id); err != nil {
		return err
	}
	if !namePattern.MatchString(name) {
		return errors.New("invalid Tailscale sandbox name")
	}
	hostname := "a37-" + name + "-" + id
	command := remote + " tailnet-join " + hostname
	proof, err := execJSON[tailIdentity](ctx, client, id, command)
	if err != nil {
		return err
	}
	if proof.EnrollmentRequired {
		clientID, clientSecret, err := peers.TailscaleCredentials()
		if err != nil {
			return err
		}
		key, err := peers.EnrollmentKey(ctx, clientID, clientSecret)
		if err != nil {
			return err
		}
		var nonce [16]byte
		if _, err = rand.Read(nonce[:]); err != nil {
			return err
		}
		filename := "tailscale-enrollment-" + hex.EncodeToString(nonce[:])
		if err = client.UploadEnrollmentKey(ctx, id, filename, key); err == nil {
			proof, err = execJSON[tailIdentity](ctx, client, id, command+" "+filename)
		}
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_, _ = client.Exec(cleanup, id, "unlink \"$HOME/.local/share/t3-agent37/"+filename+"\"")
			return err
		}
	}
	if proof.ID == "" || proof.Hostname != hostname {
		return errors.New("Tailscale did not confirm the sandbox identity")
	}
	return nil
}

func linkFleet(ctx context.Context, client *agent37.Client, id, name string, configured []peers.Peer, proof setupProof, diagnostic io.Writer) error {
	if !proof.PeerProxy {
		return fmt.Errorf("%s needs a fresh boot for T3 peer routing; run down and up when idle", name)
	}
	private, err := execJSON[struct{ URL string }](ctx, client, id, remote+" tailnet-serve-peer")
	if err != nil {
		return err
	}
	admin, err := execJSON[struct{ Token string }](ctx, client, id, remote+" peer-admin")
	if err != nil {
		return err
	}
	protected, err := agent37.ProtectedOrigin(id, proof.Port)
	if err != nil {
		return err
	}
	if err := peers.Sync(ctx, configured, name, proof.EnvironmentID, private.URL, protected, client.Key, admin.Token); err != nil {
		return err
	}
	hosts := make([]string, 0, len(configured))
	for _, peer := range configured {
		hosts = append(hosts, peer.Host)
	}
	fmt.Fprintf(diagnostic, "Linked %s with %s.\n", name, strings.Join(hosts, ", "))
	return nil
}

func bootstrap(ctx context.Context, client *agent37.Client, instance agent37.Instance, name string, configured []peers.Peer, mayRestart bool, diagnostic io.Writer) (setupProof, error) {
	proof, err := setup(ctx, client, instance, configured)
	if err != nil {
		return proof, err
	}
	if len(configured) != 0 && !proof.PeerProxy && mayRestart {
		if _, err := client.Lifecycle(ctx, instance.ID, "restart"); err != nil {
			return proof, err
		}
		proof, err = setup(ctx, client, instance, configured)
		if err != nil {
			return proof, err
		}
	}
	if instance.Metadata["tailnet"] == "tailscale" {
		if err := joinTailnet(ctx, client, instance.ID, name); err != nil {
			return proof, err
		}
	}
	if len(configured) != 0 {
		return proof, linkFleet(ctx, client, instance.ID, name, configured, proof, diagnostic)
	}
	return proof, nil
}

func wake(ctx context.Context, client *agent37.Client, instance agent37.Instance) (agent37.Instance, error) {
	if instance.Status == "stopped" && instance.Metadata["tailnet"] == "tailscale" {
		if _, _, err := peers.TailscaleCredentials(); err != nil {
			return instance, err
		}
	}
	update, err := client.Lifecycle(ctx, instance.ID, "start")
	if update.Status != "" {
		instance.Status = update.Status
	}
	return instance, err
}

func lifecycle(ctx context.Context, client *agent37.Client, instance agent37.Instance, name, action string, configured []peers.Peer, diagnostic io.Writer) (agent37.Instance, error) {
	tailnet := instance.Metadata["tailnet"] == "tailscale"
	if action == "stop" {
		if instance.Status == "stopped" {
			return instance, nil
		}
		if tailnet {
			check := `if test -f "$HOME/.local/share/t3-agent37/tailnet.py"; then ` + remote + " tailnet-leave; fi"
			if _, err := client.Exec(ctx, instance.ID, check); err != nil {
				return instance, fmt.Errorf("Tailscale logout failed for %s; retry down: %w", name, err)
			}
		}
		update, err := client.Lifecycle(ctx, instance.ID, "stop")
		if update.Status != "" {
			instance.Status = update.Status
		}
		return instance, err
	}
	wasStopped := instance.Status == "stopped"
	if instance.Status != "running" {
		var err error
		instance, err = wake(ctx, client, instance)
		if err != nil {
			return instance, err
		}
	}
	if tailnet {
		if _, err := bootstrap(ctx, client, instance, name, configured, wasStopped, diagnostic); err != nil {
			return instance, err
		}
		return client.Get(ctx, instance.ID)
	}
	return instance, nil
}

func prepareCreated(ctx context.Context, client *agent37.Client, instance agent37.Instance, name string, configured []peers.Peer, diagnostic io.Writer) (setupProof, error) {
	if displayProvider(instance) == "opencode" {
		if err := client.RequireManagedBudget(ctx, instance.ID, false); err != nil {
			return setupProof{}, err
		}
	}
	wasStopped := instance.Status == "stopped"
	if wasStopped || instance.Status == "sleeping" {
		var err error
		instance, err = wake(ctx, client, instance)
		if err != nil {
			return setupProof{}, err
		}
	}
	return bootstrap(ctx, client, instance, name, configured, wasStopped, diagnostic)
}

func prepare(ctx context.Context, client *agent37.Client, instance agent37.Instance, name string, output, diagnostic io.Writer) error {
	previous := displayProvider(instance)
	provider := "opencode"
	if previous == "cliproxy" {
		provider = previous
	} else if err := client.RequireManagedBudget(ctx, instance.ID, previous != provider); err != nil {
		return err
	}
	start := instance.Status == "stopped" || instance.Status == "sleeping"
	if start {
		var err error
		instance, err = lifecycle(ctx, client, instance, name, "start", nil, diagnostic)
		if err != nil {
			return err
		}
	}
	if !start || instance.Metadata["tailnet"] != "tailscale" || provider != previous {
		// Pass the requested provider to bootstrap before updating cloud metadata.
		override := instance
		override.Metadata = map[string]string{"provider": provider, "tailnet": instance.Metadata["tailnet"]}
		if _, err := setup(ctx, client, override, nil); err != nil {
			return err
		}
	}
	if provider == "cliproxy" {
		fmt.Fprintf(output, "CLIProxy model catalog refreshed on %s.\n", name)
		return nil
	}
	if err := client.SetProvider(ctx, instance, provider); err != nil {
		return err
	}
	fmt.Fprintf(output, "Managed OpenCode is ready on %s (agent37/default).\n", name)
	return nil
}

func pairLink(ctx context.Context, client *agent37.Client, id string, port int) (string, error) {
	reply, err := execJSON[struct{ PairURL string }](ctx, client, id, remote+" pair "+strconv.Itoa(port))
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(reply.PairURL)
	if err != nil || parsed.Scheme != "http" || parsed.Host != fmt.Sprintf("127.0.0.1:%d", port) ||
		parsed.User != nil || parsed.Path != "/pair" || parsed.RawQuery != "" {
		return "", errors.New("T3 returned an unexpected pairing address")
	}
	if token, found := strings.CutPrefix(parsed.Fragment, "token="); !found || token == "" {
		return "", errors.New("T3 pairing URL does not contain a token")
	}
	return reply.PairURL, nil
}
