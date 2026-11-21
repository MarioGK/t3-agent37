package main

import (
	"cmp"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	"github.com/MarioGK/t3-agent37/internal/peers"
)

// The Agent37 guest already has Python. The workstation executable carries
// these guest programs and delivers them over the protected exec API.
//
//go:embed guest/remote.py
var guestRemote []byte

//go:embed guest/codex_proxy.py
var guestCodexProxy []byte

//go:embed guest/claude_proxy.py
var guestClaudeProxy []byte

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
	EnvironmentID    string `json:"environmentId"`
	Label            string `json:"label"`
	LabelSaved       bool   `json:"labelSaved"`
	Port             int    `json:"port"`
	ProviderReady    bool   `json:"providerReady"`
	PeerProxy        bool   `json:"peerProxy"`
	DesktopConfirmed bool   `json:"desktopConfirmed"`
	TailnetURL       string `json:"-"`
	RequestedName    string `json:"-"`
	preparedPairing  desktopPairing
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func setup(ctx context.Context, client *agent37.Client, instance agent37.Instance, peerLinks bool) (setupProof, error) {
	install, configure, name, err := setupCommands(instance, peerLinks)
	if err != nil {
		return setupProof{}, err
	}
	return configureGuestT3(ctx, client, instance.ID, name, install+" && "+configure)
}

func setupCommands(instance agent37.Instance, peerLinks bool) (install, configure, name string, err error) {
	provider := displayProvider(instance)
	if _, allowed := providers[provider]; !allowed {
		return "", "", "", errors.New("sandbox metadata names an unsupported T3 provider")
	}
	install = "python3 -c " + shellQuote(installGuest) + " remote.py " + base64.StdEncoding.EncodeToString(guestRemote)
	if provider == "cliproxy" {
		install += " codex_proxy.py " + base64.StdEncoding.EncodeToString(guestCodexProxy) +
			" claude_proxy.py " + base64.StdEncoding.EncodeToString(guestClaudeProxy)
	}
	if instance.Metadata["tailnet"] == "tailscale" {
		install += " tailnet.py " + base64.StdEncoding.EncodeToString(guestTailnet)
	}
	name = cmp.Or(instance.Name, instance.Metadata["environment"])
	configure = remote + " setup " + provider + " " + shellQuote(name)
	if peerLinks {
		configure += " --peer-links"
	}
	return install, configure, name, nil
}

func configureGuestT3(ctx context.Context, client *agent37.Client, id, name, command string) (setupProof, error) {
	proof, err := client.ExecJSON[setupProof](ctx, id, command)
	if err != nil {
		return proof, err
	}
	proof.RequestedName = name
	if proof.EnvironmentID == "" {
		return proof, errors.New("sandbox setup did not return a T3 environment ID")
	}
	_, _, err = client.WaitForT3(ctx, id, proof.EnvironmentID, proof.Port)
	return proof, err
}

type tailIdentity struct {
	ID                 string `json:"id"`
	Hostname           string `json:"hostname"`
	EnrollmentRequired bool   `json:"enrollmentRequired"`
}

func joinTailnet(ctx context.Context, client *agent37.Client, enrollment *peers.Enrollment, id, name string) error {
	if err := joinTailnetOnce(ctx, client, enrollment, id, name); err != nil {
		return fmt.Errorf("could not enroll %s in Tailscale: %w", name, err)
	}
	return nil
}

func joinTailnetOnce(ctx context.Context, client *agent37.Client, enrollment *peers.Enrollment, id, name string) error {
	if err := agent37.ValidateID(id); err != nil {
		return err
	}
	if !namePattern.MatchString(name) {
		return errors.New("invalid Tailscale sandbox name")
	}
	hostname := "a37-" + name + "-" + id
	command := remote + " tailnet-join " + hostname
	proof, err := client.ExecJSON[tailIdentity](ctx, id, command)
	if err != nil {
		return err
	}
	confirm := func(identity tailIdentity) error {
		if identity.EnrollmentRequired || identity.ID == "" || identity.Hostname != hostname {
			return errors.New("Tailscale did not confirm the sandbox identity")
		}
		return nil
	}
	if !proof.EnrollmentRequired {
		return confirm(proof)
	}
	if enrollment == nil {
		enrollment, err = verifyTailscaleEnrollment(ctx)
		if err != nil {
			return err
		}
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	filename := "tailscale-enrollment-" + hex.EncodeToString(nonce[:])
	return enrollment.UseKey(ctx, func(key string) error {
		err := uploadGuestFile(ctx, func() error { return client.UploadEnrollmentKey(ctx, id, filename, key) })
		if err == nil {
			var joined tailIdentity
			joined, err = client.ExecJSON[tailIdentity](ctx, id, command+" "+filename)
			if err == nil {
				err = confirm(joined)
			}
		}
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_, _ = client.Exec(cleanup, id, "unlink \"$HOME/.local/share/t3-agent37/"+filename+"\"")
		}
		return err
	})
}

type sandboxBootstrap struct {
	peerLinks, mayRestart, githubAccess, skipWorkspace bool
	prepareDesktop                                     bool
	playbook                                           *workspacePlaybook
}

func bootstrap(ctx context.Context, client *agent37.Client, enrollment *peers.Enrollment, instance agent37.Instance, name string, settings sandboxBootstrap, diagnostic io.Writer) (setupProof, error) {
	if settings.playbook == nil && !settings.skipWorkspace {
		if err := requireWorkspaceReady(ctx, client, instance, name, false); err != nil {
			return setupProof{}, err
		}
	}
	stageStart := time.Now()
	var proof setupProof
	var err error
	type publication struct {
		url string
		err error
	}
	var earlyPublication chan publication
	if settings.playbook != nil && !settings.mayRestart && instance.Metadata["tailnet"] == "tailscale" {
		install, configure, requestedName, err := setupCommands(instance, settings.peerLinks)
		if err != nil {
			return proof, err
		}
		if _, err := client.Exec(ctx, instance.ID, install); err != nil {
			return proof, err
		}
		published := make(chan publication, 1)
		earlyPublication = published
		prepare, cancel := context.WithCancel(ctx)
		defer func() {
			cancel()
			if earlyPublication != nil {
				<-earlyPublication
			}
		}()
		go func() {
			url, err := publishTailnet(prepare, client, enrollment, instance.ID, name, diagnostic)
			published <- publication{url, err}
		}()
		proof, err = configureGuestT3(ctx, client, instance.ID, requestedName, configure)
		if err != nil {
			return proof, err
		}
	} else {
		proof, err = setup(ctx, client, instance, settings.peerLinks)
	}
	if err != nil {
		return proof, err
	}
	if settings.peerLinks && !proof.PeerProxy && settings.mayRestart {
		if _, err := client.Lifecycle(ctx, instance.ID, "restart"); err != nil {
			return proof, err
		}
		proof, err = setup(ctx, client, instance, settings.peerLinks)
		if err != nil {
			return proof, err
		}
	}
	launchTiming(diagnostic, name, "Guest T3 startup", stageStart)
	if proof.RequestedName != "" && proof.Label != proof.RequestedName {
		if proof.LabelSaved {
			notice(diagnostic, info, "T3 will show %s after the sandbox restarts.", proof.RequestedName)
		} else {
			notice(diagnostic, warning, "The sandbox could not set the T3 display name to %s.", proof.RequestedName)
		}
	}
	// The GitHub handoff waits for protected gateway health. Tailscale uses its
	// own userspace daemon and leaves GitHub DNS and routes alone.
	var workspaceErr error
	var prepareWorkspace func()
	if settings.githubAccess || settings.playbook != nil || settings.skipWorkspace {
		prepareWorkspace = func() {
			if err := ctx.Err(); err != nil {
				workspaceErr = err
				return
			}
			if settings.githubAccess {
				if err := configureGitHubAccess(ctx, client, instance.ID, name, diagnostic); err != nil && ctx.Err() == nil {
					notice(diagnostic, warning, "%v", err)
				}
			}
			if settings.playbook != nil {
				playbook := *settings.playbook
				workspaceErr = playbook.clone(ctx, client, instance.ID, name, diagnostic)
				if workspaceErr != nil {
					return
				}
				var installTools func()
				var toolErr error
				if playbook.terraScale() {
					installTools = func() {
						toolErr = playbook.installTerraScaleTools(ctx, client, instance.ID, name, diagnostic)
					}
				}
				projectErr := finishSandboxSetup(ctx, installTools, func() error {
					return playbook.register(ctx, client, instance.ID, name, diagnostic)
				})
				workspaceErr = errors.Join(projectErr, toolErr)
				if workspaceErr != nil {
					return
				}
			}
			if settings.playbook != nil || settings.skipWorkspace {
				workspaceErr = requireWorkspaceReady(ctx, client, instance, name, true)
				if workspaceErr == nil && settings.playbook != nil {
					notice(diagnostic, success, "%s Workspace ready in T3 Code", name)
				}
			}
		}
	}
	err = finishSandboxSetup(ctx, prepareWorkspace, func() error {
		if instance.Metadata["tailnet"] != "tailscale" {
			return nil
		}
		var url string
		if earlyPublication != nil {
			result := <-earlyPublication
			earlyPublication = nil
			if result.err != nil {
				return result.err
			}
			url = result.url
		} else {
			var err error
			url, err = publishTailnet(ctx, client, enrollment, instance.ID, name, diagnostic)
			if err != nil {
				return err
			}
		}
		verifyStarted := time.Now()
		var preparePairing func()
		if settings.prepareDesktop {
			// Hold the one-time URL in memory until HTTPS and the workspace are ready.
			preparePairing = func() {
				preparation, cancel := context.WithTimeout(ctx, 8*time.Second)
				defer cancel()
				pairing, err := issueDesktopPairing(preparation, client, instance.ID, url, desktopPairingTimeout+earlyPairingMaxAge)
				if err == nil {
					proof.preparedPairing = pairing
				}
			}
		}
		return finishSandboxSetup(ctx, preparePairing, func() error {
			var err error
			proof.TailnetURL, err = peers.VerifyTailnet(ctx, name, proof.EnvironmentID, url)
			if err == nil {
				launchTiming(diagnostic, name, "Tailscale HTTPS verification", verifyStarted)
			}
			return err
		})
	})
	return proof, errors.Join(err, workspaceErr)
}

func publishTailnet(ctx context.Context, client *agent37.Client, enrollment *peers.Enrollment, id, name string, diagnostic io.Writer) (string, error) {
	started := time.Now()
	if err := joinTailnet(ctx, client, enrollment, id, name); err != nil {
		return "", err
	}
	launchTiming(diagnostic, name, "Tailscale enrollment", started)
	started = time.Now()
	private, err := client.ExecJSON[struct{ URL string }](ctx, id, remote+" tailnet-serve-peer")
	if err == nil {
		launchTiming(diagnostic, name, "Tailscale Serve", started)
	}
	return private.URL, err
}

func finishSandboxSetup(ctx context.Context, background func(), foreground func() error) error {
	if background == nil {
		return foreground()
	}
	var group sync.WaitGroup
	group.Go(background)
	err := foreground()
	group.Wait()
	return cmp.Or(ctx.Err(), err)
}

func wake(ctx context.Context, client *agent37.Client, instance agent37.Instance) (agent37.Instance, error) {
	update, err := client.Lifecycle(ctx, instance.ID, "start")
	if update.Status != "" {
		instance.Status = update.Status
	}
	return instance, err
}

func leaveSandboxTailnet(ctx context.Context, client *agent37.Client, id string) error {
	check := `if test -f "$HOME/.local/share/t3-agent37/tailnet.py"; then ` + remote + " tailnet-leave; fi"
	_, err := client.Exec(ctx, id, check)
	return err
}

func deleteSandbox(ctx context.Context, client *agent37.Client, instance agent37.Instance, name string) (bool, error) {
	if instance.Status == "deleting" {
		return false, fmt.Errorf("Agent37 is still deleting %s. Retry to check its status", name)
	}
	tailnetPending := instance.Metadata["tailnet"] == "tailscale" && instance.Status != "stopped"
	if tailnetPending && !instance.PastDue && (instance.Status == "running" || instance.Status == "sleeping") {
		// Guest access is best effort. Agent37 DELETE also works for failed
		// instances and cold sleepers that cannot be stopped.
		logout, cancel := context.WithTimeout(ctx, 25*time.Second)
		tailnetPending = leaveSandboxTailnet(logout, client, instance.ID) != nil
		cancel()
	}
	if err := ctx.Err(); err != nil {
		return tailnetPending, err
	}
	return tailnetPending, client.Delete(ctx, instance.ID)
}

func stopSandbox(ctx context.Context, client *agent37.Client, instance agent37.Instance, name string) (agent37.Instance, error) {
	if instance.Status == "stopped" {
		return instance, nil
	}
	stopCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	tailnet := instance.Metadata["tailnet"] == "tailscale"
	loggedOut := !tailnet || instance.PastDue
	for {
		switch instance.Status {
		case "stopped":
			return instance, nil
		case "running", "sleeping":
			if !loggedOut {
				err := leaveSandboxTailnet(stopCtx, client, instance.ID)
				if err != nil && !agent37RetryAfterCheckpoint(err) {
					return instance, fmt.Errorf("Tailscale logout failed for %s. Retry stop: %w", name, err)
				}
				loggedOut = err == nil
			}
			if loggedOut {
				update, err := client.Lifecycle(stopCtx, instance.ID, "stop")
				if err == nil && update.ID == instance.ID && update.Status == "stopped" {
					instance.Status = "stopped"
					return instance, nil
				}
				if err != nil && !agent37RetryAfterCheckpoint(err) {
					return instance, err
				}
			}
		case "provisioning", "starting", "stopping", "restarting", "updating", "waking":
			// Recheck while Agent37 completes the in-flight operation.
		default:
			return instance, fmt.Errorf("Agent37 cannot stop %s in state %q", name, instance.Status)
		}
		select {
		case <-stopCtx.Done():
			return instance, stopCtx.Err()
		case <-time.After(time.Second):
		}
		current, err := readDownTarget(stopCtx, client, downReceipt{ID: instance.ID, Name: name})
		if err != nil {
			return instance, err
		}
		instance = current
	}
}

func sleepSandbox(ctx context.Context, client *agent37.Client, instance agent37.Instance, name string) (agent37.Instance, error) {
	switch instance.Status {
	case "sleeping":
		return instance, nil
	case "running":
	default:
		return instance, fmt.Errorf("%s is %s. Start the sandbox before putting it to sleep", name, instance.Status)
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	for {
		update, err := client.Lifecycle(ctx, instance.ID, "sleep")
		if err == nil {
			if update.ID != instance.ID || update.Status != "sleeping" {
				return instance, fmt.Errorf("Agent37 did not confirm that %s is sleeping", name)
			}
			instance.Status = update.Status
			return instance, nil
		}
		if !agent37RetryAfterCheckpoint(err) {
			return instance, err
		}
		select {
		case <-ctx.Done():
			return instance, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func agent37RetryAfterCheckpoint(err error) bool {
	apiError, ok := errors.AsType[*agent37.Error](err)
	return ok && apiError.Status == http.StatusConflict && apiError.Code == "try_again"
}

func startSandbox(ctx context.Context, client *agent37.Client, instance agent37.Instance, name string, settings sandboxBootstrap, diagnostic io.Writer) (agent37.Instance, setupProof, error) {
	wasStopped := instance.Status == "stopped"
	var enrollment *peers.Enrollment
	if wasStopped && instance.Metadata["tailnet"] == "tailscale" {
		var err error
		enrollment, err = verifyTailscaleEnrollment(ctx)
		if err != nil {
			return instance, setupProof{}, err
		}
	}
	if instance.Status != "running" {
		var err error
		instance, err = wake(ctx, client, instance)
		if err != nil {
			return instance, setupProof{}, err
		}
	}
	if instance.Metadata["tailnet"] == "tailscale" {
		settings.mayRestart = wasStopped
		proof, err := bootstrap(ctx, client, enrollment, instance, name, settings, diagnostic)
		if err != nil {
			return instance, proof, err
		}
		instance, err = client.Get(ctx, instance.ID)
		return instance, proof, err
	}
	return instance, setupProof{}, nil
}

func prepareCreated(ctx context.Context, client *agent37.Client, enrollment *peers.Enrollment, instance agent37.Instance, opts options, diagnostic io.Writer) (setupProof, error) {
	provider := displayProvider(instance)
	if instance.Template != opts.template || provider != opts.provider || (instance.Metadata["tailnet"] == "tailscale") != opts.tailscale {
		return setupProof{}, errors.New("this name already uses a different template, provider, or Tailscale setting")
	}
	if provider == "opencode" {
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
	proof, err := bootstrap(ctx, client, enrollment, instance, opts.name, sandboxBootstrap{
		peerLinks: true, mayRestart: wasStopped, githubAccess: opts.githubAccess,
		prepareDesktop: opts.up && !opts.noDesktop, playbook: opts.playbook,
	}, diagnostic)
	if err == nil && !proof.ProviderReady {
		notice(diagnostic, warning, "%s needs sign-in. Run t3-agent37 terminal %s.", providerLabel(provider), opts.name)
	}
	if err == nil && proof.TailnetURL == "" {
		err = errors.New("Agent37 did not provide the sandbox's Tailscale address")
	}
	return proof, err
}

func configureGitHubAccess(ctx context.Context, client *agent37.Client, id, name string, diagnostic io.Writer) error {
	access, err := inheritGitHub(ctx, client, id)
	if err != nil {
		return fmt.Errorf("could not set up GitHub for %s. Retry with t3-agent37 up %s --github: %w", name, name, err)
	}
	if access.GitHub {
		notice(diagnostic, success, "GitHub CLI and HTTPS Git are ready on %s.", name)
	} else {
		notice(diagnostic, warning, "No GitHub login found. Sign in with gh, then run t3-agent37 up %s --github.", name)
	}
	return nil
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
		instance, _, err = startSandbox(ctx, client, instance, name, sandboxBootstrap{}, diagnostic)
		if err != nil {
			return err
		}
	}
	if !start || instance.Metadata["tailnet"] != "tailscale" || provider != previous {
		// Pass the requested provider to bootstrap before updating cloud metadata.
		override := instance
		override.Metadata = map[string]string{"provider": provider, "tailnet": instance.Metadata["tailnet"]}
		if _, err := setup(ctx, client, override, false); err != nil {
			return err
		}
	}
	if provider == "cliproxy" {
		notice(output, success, "CLIProxy model catalog refreshed on %s.", name)
		return nil
	}
	if err := client.SetProvider(ctx, instance, provider); err != nil {
		return err
	}
	notice(output, success, "Managed OpenCode is ready on %s with agent37/default.", name)
	return nil
}

func pairLink(ctx context.Context, client *agent37.Client, id, origin, label string, ttl time.Duration) (string, error) {
	base, err := url.Parse(origin)
	if err != nil {
		return "", err
	}
	if base.Scheme != "https" || !strings.HasSuffix(base.Hostname(), ".ts.net") {
		return "", errors.New("T3 pairing requires a private Tailscale HTTPS address")
	}
	command := remote + " pair " + shellQuote(origin) + " " + shellQuote(label) + fmt.Sprintf(" %ds", int(ttl/time.Second))
	reply, err := client.ExecJSON[struct{ PairURL string }](ctx, id, command)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(reply.PairURL)
	if err != nil || parsed.Scheme != base.Scheme || parsed.Host != base.Host ||
		parsed.User != nil || parsed.Path != "/pair" || parsed.RawQuery != "" {
		return "", errors.New("T3 returned an unexpected pairing address")
	}
	if token, found := strings.CutPrefix(parsed.Fragment, "token="); !found || token == "" {
		return "", errors.New("T3 pairing URL does not contain a token")
	}
	return reply.PairURL, nil
}
