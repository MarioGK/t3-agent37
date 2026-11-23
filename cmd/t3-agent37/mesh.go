package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	"github.com/MarioGK/t3-agent37/internal/peers"
)

// Fallbacks keep mesh inspection and cleanup working against running sandboxes
// provisioned before startup.py existed.
const legacyInspectGuestPeer = `import json, sys
from pathlib import Path
root = Path.home() / ".local/share/t3-agent37"
if len(sys.argv) > 1 and not (root / sys.argv[1]).is_file():
    print(json.dumps({"workspacePending": True}))
    sys.exit(0)
sys.path.insert(0, str(root))
import remote, tailnet
status = tailnet.status() or {}
peer = tailnet.identity(status)
try:
    server = remote.descriptor()
except RuntimeError:
    server = None
dns = ((status.get("Self") or {}).get("DNSName") or "").rstrip(".")
environment = (server or {}).get("environmentId", "")
print(json.dumps({"environmentId": environment,
                  "url": "https://" + dns if dns else "",
                  "offline": peer is None,
                  "peerProxy": (remote.ROOT / "peer-proxy-active").is_file(),
                  "desktopConfirmed": bool(environment) and getattr(remote, "desktop_confirmed", lambda _: False)(environment)}))`

const legacyGuestEnvironment = `import os, subprocess, sys
from pathlib import Path
sys.path.insert(0, str(Path.home() / ".local/share/t3-agent37"))
import remote
env = dict(os.environ, NODE_USE_ENV_PROXY="1", HTTPS_PROXY="http://127.0.0.1:1055",
           NO_PROXY="127.0.0.1,localhost,::1")
raise SystemExit(subprocess.run([str(remote.T3), "environment", *sys.argv[1:],
                                 "--base-dir", str(remote.DATA)], stderr=subprocess.DEVNULL,
                                 env=env).returncode)`

func startupOrLegacy(startupArgs, legacyCmd string) string {
	return `if test -f "$HOME/.local/share/t3-agent37/startup.py"; then python3 -B "$HOME/.local/share/t3-agent37/startup.py" ` + startupArgs +
		`; elif test -f /opt/t3-agent37/startup.py; then python3 -B /opt/t3-agent37/startup.py ` + startupArgs +
		`; else ` + legacyCmd + `; fi`
}

type sandboxPeer struct {
	EnvironmentID    string `json:"environmentId"`
	URL              string `json:"url"`
	PeerProxy        bool   `json:"peerProxy"`
	DesktopConfirmed bool   `json:"desktopConfirmed"`
	Offline          bool   `json:"offline"`
	WorkspacePending bool   `json:"workspacePending,omitzero"`
}

func meshNode(client *agent37.Client, id, name string, proof sandboxPeer) (peers.MeshNode, error) {
	origin, err := url.Parse(proof.URL)
	if err != nil || origin.Scheme != "https" || origin.Port() != "" || origin.User != nil ||
		(origin.Path != "" && origin.Path != "/") || origin.RawQuery != "" || origin.Fragment != "" {
		return peers.MeshNode{}, errors.New("sandbox did not report a valid Tailscale HTTPS address")
	}
	host, suffix, _ := strings.Cut(origin.Hostname(), ".")
	if host != "a37-"+name+"-"+id || !strings.HasSuffix(suffix, ".ts.net") {
		return peers.MeshNode{}, errors.New("sandbox did not report its assigned Tailscale and T3 identities")
	}
	return peers.MeshNode{
		Name: name, URL: proof.URL, EnvironmentID: proof.EnvironmentID,
		IssueToken: func(ctx context.Context) (string, error) { return issueSandboxOwner(ctx, client, id) },
		ListLinksJSON: func(ctx context.Context) ([]byte, error) {
			cmd := startupOrLegacy("env list --json", "python3 -B -c "+shellQuote(legacyGuestEnvironment)+" list --json")
			value, err := client.Exec(ctx, id, cmd)
			return []byte(value), err
		},
		Unlink: func(ctx context.Context, environmentID string) error {
			quoted := shellQuote(environmentID)
			cmd := startupOrLegacy("env unlink "+quoted, "python3 -B -c "+shellQuote(legacyGuestEnvironment)+" unlink "+quoted)
			_, err := client.Exec(ctx, id, cmd)
			return err
		},
	}, nil
}

func issueSandboxOwner(ctx context.Context, client *agent37.Client, id string) (string, error) {
	cmd := startupOrLegacy("peer-admin", `python3 "$HOME/.local/share/t3-agent37/remote.py" peer-admin`)
	admin, err := client.ExecJSON[struct {
		Token string `json:"token"`
	}](ctx, id, cmd)
	if err != nil {
		return "", err
	}
	if admin.Token == "" {
		return "", errors.New("sandbox did not issue a T3 owner session")
	}
	return admin.Token, nil
}

func discoverSandboxPeer(ctx context.Context, client *agent37.Client, instance agent37.Instance, allowWake, forCleanup bool) (sandboxPeer, error) {
	probe, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	woke := instance.Status == "sleeping" && allowWake
	if woke {
		if _, err := wake(probe, client, instance); err != nil {
			return sandboxPeer{}, err
		}
	}
	startupArgs := "inspect"
	legacyCmd := "python3 -B -c " + shellQuote(legacyInspectGuestPeer)
	if requiresWorkspaceSetup(instance) {
		marker := shellQuote(workspaceReadyFile)
		startupArgs += " " + marker
		legacyCmd += " " + marker
	}
	command := startupOrLegacy(startupArgs, legacyCmd)
	for {
		running, err := client.ExecJSON[sandboxPeer](probe, instance.ID, command)
		ready := running.EnvironmentID != "" && !running.Offline
		if err == nil && (running.WorkspacePending || ready || forCleanup && running.URL != "") {
			return running, nil
		}
		if !woke {
			if err != nil {
				return sandboxPeer{}, err
			}
			return sandboxPeer{}, errors.New("sandbox T3 on Tailscale is unavailable")
		}
		select {
		case <-probe.Done():
			return sandboxPeer{}, errors.New("sandbox T3 on Tailscale did not become ready after waking")
		case <-time.After(time.Second):
		}
	}
}

func cloudMeshNodes(ctx context.Context, client *agent37.Client, instances []agent37.Instance, skipID string, allowWake bool) ([]peers.MeshNode, error) {
	return discoverCloudNodes(ctx, client, instances, skipID, allowWake, false)
}

func discoverCloudNodes(ctx context.Context, client *agent37.Client, instances []agent37.Instance, skipID string, allowWake, forCleanup bool) ([]peers.MeshNode, error) {
	var group sync.WaitGroup
	limit := make(chan struct{}, 6)
	nodes := make([]peers.MeshNode, len(instances))
	failures := make([]error, len(instances))
	for i, instance := range instances {
		if instance.ID == skipID || !isManaged(instance) || instance.Metadata["tailnet"] != "tailscale" {
			continue
		}
		if instance.Status != "running" && (!allowWake || instance.Status != "sleeping") {
			if forCleanup {
				failures[i] = fmt.Errorf("cannot check T3 links on %s while it is %s", instance.Metadata["environment"], instance.Status)
			}
			continue
		}
		group.Go(func() {
			select {
			case limit <- struct{}{}:
				defer func() { <-limit }()
			case <-ctx.Done():
				failures[i] = ctx.Err()
				return
			}
			name := instance.Metadata["environment"]
			if agent37.ValidateID(instance.ID) != nil || !namePattern.MatchString(name) {
				failures[i] = errors.New("a managed sandbox has an invalid identity")
				return
			}
			proof, err := discoverSandboxPeer(ctx, client, instance, allowWake, forCleanup)
			if err == nil {
				if proof.WorkspacePending {
					return
				}
				pendingConfirmation := requiresDesktopConfirmation(instance) && !proof.DesktopConfirmed
				if pendingConfirmation && !forCleanup {
					return
				}
				nodes[i], err = meshNode(client, instance.ID, name, proof)
				nodes[i].CleanupOnly = pendingConfirmation || !proof.PeerProxy || proof.Offline || proof.EnvironmentID == ""
				if !proof.PeerProxy && !forCleanup {
					nodes[i] = peers.MeshNode{}
					err = errors.New("sandbox needs a fresh boot for T3 peer routing")
				}
				if err == nil && forCleanup && allowWake && !pendingConfirmation && !proof.PeerProxy {
					err = errors.New("sandbox needs a fresh boot for T3 peer routing")
				}
			}
			if err != nil {
				failures[i] = fmt.Errorf("%s: %w", name, err)
			}
		})
	}
	group.Wait()
	var ready []peers.MeshNode
	for _, node := range nodes {
		if node.URL != "" {
			ready = append(ready, node)
		}
	}
	return ready, errors.Join(failures...)
}

func linkStartedSandbox(ctx context.Context, client *agent37.Client, instance agent37.Instance, name string, configured []peers.Peer, proof setupProof, diagnostic io.Writer) bool {
	started := time.Now()
	if requiresDesktopConfirmation(instance) && !proof.DesktopConfirmed {
		notice(diagnostic, warning, "T3 linking awaits desktop confirmation. Run t3-agent37 link %s.", name)
		return false
	}
	if !proof.PeerProxy || proof.EnvironmentID == "" {
		notice(diagnostic, warning, "T3 mesh pending for %s. Sandbox needs a fresh boot for T3 peer routing.", name)
		return false
	}
	node, err := meshNode(client, instance.ID, name, sandboxPeer{
		EnvironmentID: proof.EnvironmentID, URL: proof.TailnetURL, PeerProxy: proof.PeerProxy,
	})
	if err != nil {
		notice(diagnostic, warning, "T3 mesh pending for %s. %v", name, err)
		return false
	}
	node.PreparedOwner = proof.preparedOwner
	meshCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	instances, inventoryErr := client.List(meshCtx)
	others, discoveryErr := cloudMeshNodes(meshCtx, client, instances, instance.ID, true)
	count, linkErr := peers.ReconcileMesh(meshCtx, configured, append([]peers.MeshNode{node}, others...))
	if err := errors.Join(inventoryErr, discoveryErr, linkErr, meshCtx.Err()); err != nil {
		notice(diagnostic, warning, "T3 mesh incomplete (%d links verified). Run t3-agent37 sync. %v", count, err)
		return false
	}
	reportMesh(count, diagnostic)
	launchTiming(diagnostic, name, "Peer linking", started)
	return true
}

func reportMesh(count int, diagnostic io.Writer) {
	if count != 0 {
		notice(diagnostic, success, "Verified %d T3 environment links.", count)
	}
}
