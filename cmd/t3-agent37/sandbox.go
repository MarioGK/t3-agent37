package main

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	"github.com/MarioGK/t3-agent37/internal/peers"
	"github.com/MarioGK/t3-agent37/templates/terrascale"
)

const (
	startupCmd = `python3 -B "$(test -f "$HOME/.local/share/t3-agent37/startup.py" && printf '%s' "$HOME/.local/share/t3-agent37/startup.py" || printf '%s' /opt/t3-agent37/startup.py)"`

	installStartup = `import base64, gzip, hashlib, os, pathlib, sys, tempfile
os.umask(0o077)
root = pathlib.Path.home() / '.local/share/t3-agent37'
root.mkdir(mode=0o700, parents=True, exist_ok=True)
want, stage, body = sys.argv[1], sys.argv[2], sys.argv[3]
baked, user = pathlib.Path('/opt/t3-agent37/startup.py'), root / 'startup.py'
same = lambda p: not p.is_symlink() and p.is_file() and hashlib.sha256(p.read_bytes()).hexdigest() == want
if same(baked):
    user.unlink(missing_ok=True)
elif not same(user):
    with tempfile.NamedTemporaryFile(dir=root, delete=False) as tmp:
        tmp_path = pathlib.Path(tmp.name)
        try:
            tmp.write(gzip.decompress(base64.b64decode(body)))
            os.chmod(tmp_path, 0o700)
            tmp.flush()
            tmp_path.replace(user)
        finally:
            tmp_path.unlink(missing_ok=True)
if stage:
    (root / stage).mkdir(mode=0o700)`

	defaultCLIProxyModel = "glm-5.3"
	solCLIProxyModel     = "gpt-6.2-sol"
)

var (
	cliproxyModelPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/()-]{0,159}$`)
	cliproxyMediaPrefix    = []string{"gpt-image-", "grok-imagine-", "grok-tts", "grok-voice-"}
	cliproxyFallbackModels = []string{
		defaultCLIProxyModel,
		solCLIProxyModel,
		"claude-sonnet-5-5",
		"claude-opus-5-5",
		"gpt-6.1-sol",
		"gemini-4-neon-xhigh",
	}

	startupAsset = sync.OnceValues(func() (hash string, encoded string) {
		sum := sha256.Sum256(terrascale.StartupScript)
		var buf bytes.Buffer
		w, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		_, _ = w.Write(terrascale.StartupScript)
		_ = w.Close()
		return hex.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString(buf.Bytes())
	})
)

type cliproxyCatalog struct {
	Models       []string
	DefaultModel string
	HasSol       bool
}

func fetchCLIProxyCatalog(ctx context.Context, env map[string]string) (cliproxyCatalog, error) {
	base := strings.TrimRight(strings.TrimSpace(env[proxyURL]), "/")
	key := strings.TrimSpace(env[proxyKey])
	explicitModel := strings.TrimSpace(env[proxyModel])
	if !validCLIProxyURL(base) || key == "" {
		return cliproxyCatalog{}, errors.New("CLIProxy needs an HTTPS base URL and client key")
	}
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return cliproxyCatalog{}, errors.New("CLIProxy model catalog is unavailable")
	}
	request.Header.Set("Authorization", "Bearer "+key)
	client := &http.Client{
		Transport:     http.DefaultTransport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Do(request)
	if err != nil {
		return cliproxyCatalog{}, cmp.Or(ctx.Err(), errors.New("CLIProxy model catalog is unavailable"))
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return cliproxyCatalog{}, fmt.Errorf("CLIProxy model catalog returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.UnmarshalRead(io.LimitReader(response.Body, 4<<20), &payload); err != nil || payload.Data == nil {
		return cliproxyCatalog{}, errors.New("CLIProxy model catalog has an unexpected format")
	}
	models := make([]string, 0, len(payload.Data))
	seen := make(map[string]struct{}, len(payload.Data))
	for _, entry := range payload.Data {
		id := entry.ID
		if !cliproxyModelPattern.MatchString(id) || slices.ContainsFunc(cliproxyMediaPrefix, func(p string) bool { return strings.HasPrefix(id, p) }) {
			continue
		}
		if _, dup := seen[id]; !dup {
			seen[id] = struct{}{}
			models = append(models, id)
		}
	}
	if len(models) == 0 {
		return cliproxyCatalog{}, errors.New("CLIProxy catalog returned no usable models")
	}
	selected := explicitModel
	if selected != "" {
		if _, ok := seen[selected]; !ok {
			return cliproxyCatalog{}, fmt.Errorf("CLIProxy default model %q is absent from the catalog", selected)
		}
	} else {
		for _, candidate := range cliproxyFallbackModels {
			if _, ok := seen[candidate]; ok {
				selected = candidate
				break
			}
		}
		if selected == "" {
			selected = models[0]
		}
	}
	_, hasSol := seen[solCLIProxyModel]
	return cliproxyCatalog{Models: models, DefaultModel: selected, HasSol: hasSol}, nil
}

type launchSpec struct {
	Provider              string             `json:"provider,omitempty"`
	Name                  string             `json:"name,omitempty"`
	PeerLinks             bool               `json:"peerLinks,omitzero"`
	RequireWorkspaceReady bool               `json:"requireWorkspaceReady,omitzero"`
	MarkWorkspaceReady    bool               `json:"markWorkspaceReady,omitzero"`
	CLIProxyModels        []string           `json:"cliproxyModels,omitempty"`
	Tailscale             *tailscaleSpec     `json:"tailscale,omitempty"`
	GitHub                *githubHandoff     `json:"github,omitempty"`
	Workspace             *workspacePlaybook `json:"workspace,omitempty"`
	Pairing               *pairingSpec       `json:"pairing,omitempty"`
	IssueOwner            bool               `json:"issueOwner,omitzero"`
}

type tailscaleSpec struct {
	Hostname string `json:"hostname"`
	AuthKey  string `json:"authKey,omitempty"`
}

type pairingSpec struct {
	Label string `json:"label"`
	TTL   string `json:"ttl"`
}

type workspaceCloneFailure struct {
	Status string `json:"status"`
	Repo   string `json:"repo"`
	Folder string `json:"folder"`
}

type setupProof struct {
	EnvironmentID      string                  `json:"environmentId"`
	Label              string                  `json:"label"`
	LabelSaved         bool                    `json:"labelSaved"`
	Port               int                     `json:"port"`
	ProviderReady      bool                    `json:"providerReady"`
	PeerProxy          bool                    `json:"peerProxy"`
	DesktopConfirmed   bool                    `json:"desktopConfirmed"`
	WorkspacePending   bool                    `json:"workspacePending"`
	TailnetURL         string                  `json:"tailnetUrl"`
	TailnetHostname    string                  `json:"tailnetHostname"`
	TailnetID          string                  `json:"tailnetId"`
	EnrollmentRequired bool                    `json:"enrollmentRequired"`
	TailnetError       string                  `json:"tailnetError"`
	GitHub             bool                    `json:"github"`
	GitIdentity        bool                    `json:"gitIdentity"`
	GitHubError        string                  `json:"githubError"`
	WorkspaceReady     bool                    `json:"workspaceReady"`
	SeededWorktrees    int                     `json:"seededWorktrees"`
	WorkspaceErrors    []workspaceCloneFailure `json:"workspaceErrors"`
	ManifestError      string                  `json:"manifestError"`
	ToolsStage         string                  `json:"toolsStage"`
	ProjectsError      string                  `json:"projectsError"`
	PairingURL         string                  `json:"pairingUrl"`
	OwnerToken         string                  `json:"ownerToken"`
	Timings            map[string]float64      `json:"timings"`
	RequestedName      string                  `json:"-"`
	preparedPairing    desktopPairing
	preparedOwner      *peers.PreparedOwnerSession
}

type tailIdentity struct {
	ID                 string `json:"id"`
	Hostname           string `json:"hostname"`
	URL                string `json:"url"`
	EnrollmentRequired bool   `json:"enrollmentRequired"`
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func stageGuestStartup(ctx context.Context, client *agent37.Client, id, stage string) error {
	hash, encoded := startupAsset()
	cmd := "python3 -c " + shellQuote(installStartup) + " " + hash + " " + shellQuote(stage) + " " + encoded
	_, err := client.Exec(ctx, id, cmd)
	return err
}

func runLaunchSpec[T any](ctx context.Context, client *agent37.Client, id, action string, spec launchSpec) (T, error) {
	var zero T
	payload, err := json.Marshal(spec)
	if err != nil {
		return zero, errors.New("could not encode sandbox launch request")
	}
	defer clear(payload)

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return zero, err
	}
	stage := "launch-" + hex.EncodeToString(nonce[:])
	if err := stageGuestStartup(ctx, client, id, stage); err != nil {
		return zero, err
	}
	consumed := false
	defer func() {
		if !consumed {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_, _ = client.Exec(cleanup, id, startupCmd+" cleanup "+stage)
		}
	}()
	if err := uploadGuestFile(ctx, func() error { return client.UploadLaunchSpec(ctx, id, stage, payload) }); err != nil {
		return zero, err
	}
	result, err := client.ExecJSON[T](ctx, id, startupCmd+" "+action+" "+stage)
	if err == nil {
		consumed = true
	}
	return result, err
}

func setup(ctx context.Context, client *agent37.Client, instance agent37.Instance, peerLinks bool) (setupProof, error) {
	provider := displayProvider(instance)
	if _, allowed := providers[provider]; !allowed {
		return setupProof{}, errors.New("sandbox metadata names an unsupported T3 provider")
	}
	name := cmp.Or(instance.Name, instance.Metadata["environment"])
	proof, err := runLaunchSpec[setupProof](ctx, client, instance.ID, "up", launchSpec{
		Provider: provider, Name: name, PeerLinks: peerLinks,
	})
	if err != nil {
		return proof, err
	}
	proof.RequestedName = name
	if proof.EnvironmentID == "" {
		return proof, errors.New("sandbox setup did not return a T3 environment ID")
	}
	if _, err := agent37.ProtectedOrigin(instance.ID, proof.Port); err != nil {
		return proof, err
	}
	_, _, err = client.WaitForT3(ctx, instance.ID, proof.EnvironmentID, proof.Port)
	return proof, err
}

func enrollTailnet(ctx context.Context, client *agent37.Client, enrollment *peers.Enrollment, id, hostname string) (string, error) {
	var err error
	if enrollment == nil {
		enrollment, err = verifyTailscaleEnrollment(ctx)
		if err != nil {
			return "", err
		}
	}
	var identity tailIdentity
	err = enrollment.UseKey(ctx, func(key string) error {
		var runErr error
		identity, runErr = runLaunchSpec[tailIdentity](ctx, client, id, "tailnet-publish", launchSpec{
			Tailscale: &tailscaleSpec{Hostname: hostname, AuthKey: key},
		})
		if runErr != nil {
			return runErr
		}
		if identity.EnrollmentRequired || identity.ID == "" || identity.Hostname != hostname || identity.URL == "" {
			return errors.New("Tailscale did not confirm the sandbox identity")
		}
		return nil
	})
	return identity.URL, err
}

type sandboxBootstrap struct {
	peerLinks, mayRestart, githubAccess, skipWorkspace bool
	prepareDesktop, fresh                              bool
	playbook                                           *workspacePlaybook
	cliproxyModels                                     []string
	preparedGitHub                                     func() (githubHandoff, error)
}

func bootstrap(ctx context.Context, client *agent37.Client, enrollment *peers.Enrollment, instance agent37.Instance, name string, settings sandboxBootstrap, diagnostic io.Writer) (setupProof, error) {
	provider := displayProvider(instance)
	if _, allowed := providers[provider]; !allowed {
		return setupProof{}, errors.New("sandbox metadata names an unsupported T3 provider")
	}
	tailnet := instance.Metadata["tailnet"] == "tailscale"
	var hostname string
	if tailnet {
		if err := agent37.ValidateID(instance.ID); err != nil {
			return setupProof{}, err
		}
		if !namePattern.MatchString(name) {
			return setupProof{}, errors.New("invalid Tailscale sandbox name")
		}
		hostname = "a37-" + name + "-" + instance.ID
	}

	spec := launchSpec{
		Provider:              provider,
		Name:                  cmp.Or(instance.Name, instance.Metadata["environment"], name),
		PeerLinks:             settings.peerLinks,
		RequireWorkspaceReady: settings.playbook == nil && !settings.skipWorkspace && requiresWorkspaceSetup(instance),
		MarkWorkspaceReady:    settings.skipWorkspace && requiresWorkspaceSetup(instance),
		Workspace:             settings.playbook,
		IssueOwner:            settings.peerLinks && tailnet,
	}
	if provider == "cliproxy" && len(settings.cliproxyModels) != 0 {
		spec.CLIProxyModels = settings.cliproxyModels
	}
	if tailnet {
		spec.Tailscale = &tailscaleSpec{Hostname: hostname}
	}

	var pairingLabel string
	if settings.prepareDesktop && tailnet {
		var nonce [16]byte
		_, _ = rand.Read(nonce[:])
		pairingLabel = "t3-agent37-desktop-" + hex.EncodeToString(nonce[:])
		ttl := fmt.Sprintf("%ds", int((desktopPairingTimeout+earlyPairingMaxAge)/time.Second))
		spec.Pairing = &pairingSpec{Label: pairingLabel, TTL: ttl}
	}

	if settings.githubAccess {
		readGH := settings.preparedGitHub
		if readGH == nil {
			readGH = func() (githubHandoff, error) { return readGitHubHandoff(ctx) }
		}
		handoff, err := readGH()
		if err != nil {
			if ctx.Err() == nil {
				notice(diagnostic, warning, "could not set up GitHub for %s. Retry with t3-agent37 up %s --github: %v", name, name, err)
			}
		} else if handoff == (githubHandoff{}) {
			notice(diagnostic, warning, "No GitHub login found. Sign in with gh, then run t3-agent37 up %s --github.", name)
		} else {
			spec.GitHub = &handoff
		}
	}

	started := time.Now()
	var proof setupProof
	var err error
	if tailnet && settings.fresh && enrollment != nil {
		err = enrollment.UseKey(ctx, func(key string) error {
			spec.Tailscale.AuthKey = key
			var runErr error
			proof, runErr = runLaunchSpec[setupProof](ctx, client, instance.ID, "up", spec)
			spec.Tailscale.AuthKey = ""
			if runErr != nil {
				return runErr
			}
			if proof.TailnetURL == "" {
				return cmp.Or(tailnetProofError(proof), errors.New("Tailscale did not confirm the sandbox identity"))
			}
			return nil
		})
		// Keep a partial proof when Tailscale failed after T3/workspace ran,
		// while preserving the key revocation error if enrollment failed.
		if err != nil && proof.EnvironmentID == "" {
			return proof, err
		}
	} else {
		proof, err = runLaunchSpec[setupProof](ctx, client, instance.ID, "up", spec)
	}
	if err != nil && proof.EnvironmentID == "" {
		return proof, err
	}
	if proof.WorkspacePending {
		return setupProof{}, fmt.Errorf("finish workspace setup with t3-agent37 up %s before linking T3", name)
	}
	proof.RequestedName = spec.Name
	if proof.EnvironmentID == "" {
		return proof, errors.New("sandbox setup did not return a T3 environment ID")
	}
	if _, originErr := agent37.ProtectedOrigin(instance.ID, proof.Port); originErr != nil {
		return proof, originErr
	}

	if settings.peerLinks && !proof.PeerProxy && settings.mayRestart {
		prev := proof
		if _, restartErr := client.Lifecycle(ctx, instance.ID, "restart"); restartErr != nil {
			return proof, restartErr
		}
		retry := spec
		retry.GitHub, retry.Workspace, retry.MarkWorkspaceReady = nil, nil, false
		if workspaceProofError(settings.playbook, name, prev) != nil {
			retry.Pairing = nil
		}
		if retry.Tailscale != nil {
			retry.Tailscale = &tailscaleSpec{Hostname: hostname}
		}
		proof, err = runLaunchSpec[setupProof](ctx, client, instance.ID, "up", retry)
		if err != nil {
			return proof, err
		}
		proof.RequestedName = spec.Name
		proof.GitHub, proof.GitIdentity, proof.GitHubError = prev.GitHub, prev.GitIdentity, prev.GitHubError
		proof.WorkspaceReady, proof.SeededWorktrees = prev.WorkspaceReady, prev.SeededWorktrees
		proof.WorkspaceErrors, proof.ManifestError = prev.WorkspaceErrors, prev.ManifestError
		proof.ToolsStage, proof.ProjectsError = prev.ToolsStage, prev.ProjectsError
	}

	if tailnet && proof.EnrollmentRequired {
		url, enrollErr := enrollTailnet(ctx, client, enrollment, instance.ID, hostname)
		if enrollErr != nil {
			return proof, fmt.Errorf("could not publish %s on Tailscale: %w", name, enrollErr)
		}
		proof.TailnetURL = url
		proof.TailnetHostname = hostname
	} else if tailnet && proof.TailnetURL == "" {
		return proof, fmt.Errorf("could not publish %s on Tailscale: %w", name, cmp.Or(err, tailnetProofError(proof), errors.New("Tailscale did not confirm the sandbox identity")))
	}
	launchTiming(diagnostic, name, "Guest startup", started)
	reportGuestTimings(diagnostic, name, proof)

	if proof.RequestedName != "" && proof.Label != proof.RequestedName {
		if proof.LabelSaved {
			notice(diagnostic, info, "T3 will show %s after the sandbox restarts.", proof.RequestedName)
		} else {
			notice(diagnostic, warning, "The sandbox could not set the T3 display name to %s.", proof.RequestedName)
		}
	}
	if spec.GitHub != nil {
		if proof.GitHubError != "" {
			notice(diagnostic, warning, "could not set up GitHub for %s. Retry with t3-agent37 up %s --github: %s", name, name, proof.GitHubError)
		} else if proof.GitHub {
			notice(diagnostic, success, "GitHub CLI and HTTPS Git are ready on %s.", name)
		} else {
			notice(diagnostic, warning, "No GitHub login found. Sign in with gh, then run t3-agent37 up %s --github.", name)
		}
	}

	workspaceErr := workspaceProofError(settings.playbook, name, proof)
	if workspaceErr == nil && settings.playbook != nil {
		notice(diagnostic, success, "%s Workspace ready in T3 Code", name)
	}

	var verifyErr error
	if tailnet {
		verifyStarted := time.Now()
		var report func(string, time.Duration)
		if os.Getenv("T3_AGENT37_TIMINGS") == "1" {
			report = func(stage string, elapsed time.Duration) {
				notice(diagnostic, info, "%s · %s · %.3fs", name, stage, elapsed.Seconds())
			}
		}
		proof.TailnetURL, verifyErr = peers.VerifyTailnet(ctx, name, proof.EnvironmentID, proof.TailnetURL, report)
		if verifyErr == nil {
			launchTiming(diagnostic, name, "Tailscale HTTPS verification", verifyStarted)
		} else if ctx.Err() == nil {
			verifyErr = enrichTailnetVerifyError(ctx, client, instance.ID, verifyErr)
		}
	} else {
		_, _, verifyErr = client.WaitForT3(ctx, instance.ID, proof.EnvironmentID, proof.Port)
	}

	if verifyErr == nil && workspaceErr == nil && proof.PairingURL != "" && pairingLabel != "" {
		if validatePairLink(proof.TailnetURL, proof.PairingURL) == nil {
			proof.preparedPairing = desktopPairing{url: proof.PairingURL, label: pairingLabel, issuedAt: time.Now()}
		}
	}
	if proof.OwnerToken != "" && proof.PeerProxy {
		proof.preparedOwner = peers.ReadyOwnerSession(proof.OwnerToken)
	}
	return proof, errors.Join(verifyErr, workspaceErr)
}

func tailnetProofError(proof setupProof) error {
	if proof.TailnetError != "" {
		return errors.New(proof.TailnetError)
	}
	return nil
}

func enrichTailnetVerifyError(ctx context.Context, client *agent37.Client, id string, verifyErr error) error {
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	status, err := client.ExecJSON[struct {
		RateLimited bool   `json:"rateLimited"`
		RetryAfter  string `json:"retryAfter"`
	}](probe, id, startupCmd+" tailnet-cert-error")
	if err != nil || !status.RateLimited {
		return verifyErr
	}
	if status.RetryAfter != "" {
		return fmt.Errorf("Tailscale Let's Encrypt certificate rate limit reached (50 certs / 7 days for this tailnet; retry after %s). Use sleep / up <name> to reuse existing sandboxes: %w", status.RetryAfter, verifyErr)
	}
	return fmt.Errorf("Tailscale Let's Encrypt certificate rate limit reached (50 certs / 7 days for this tailnet). Use sleep / up <name> to reuse existing sandboxes: %w", verifyErr)
}

func reportGuestTimings(diagnostic io.Writer, name string, proof setupProof) {
	if os.Getenv("T3_AGENT37_TIMINGS") != "1" {
		return
	}
	for _, stage := range []string{"t3", "tailnet", "workspace", "projects", "tools"} {
		if secs, ok := proof.Timings[stage]; ok {
			notice(diagnostic, info, "%s · guest %s · %.3fs", name, stage, secs)
		}
	}
	if proof.SeededWorktrees > 0 {
		notice(diagnostic, info, "%s Reused %d baked worktrees", name, proof.SeededWorktrees)
	}
}

func wake(ctx context.Context, client *agent37.Client, instance agent37.Instance) (agent37.Instance, error) {
	update, err := client.Lifecycle(ctx, instance.ID, "start")
	if update.Status != "" {
		instance.Status = update.Status
	}
	return instance, err
}

func leaveSandboxTailnet(ctx context.Context, client *agent37.Client, id string) error {
	check := `if test -f "$HOME/.local/share/t3-agent37/startup.py"; then python3 -B "$HOME/.local/share/t3-agent37/startup.py" tailnet-leave; ` +
		`elif test -f /opt/t3-agent37/startup.py; then python3 -B /opt/t3-agent37/startup.py tailnet-leave; ` +
		`elif test -f "$HOME/.local/share/t3-agent37/tailnet.py"; then python3 "$HOME/.local/share/t3-agent37/remote.py" tailnet-leave; fi`
	_, err := client.Exec(ctx, id, check)
	return err
}

func deleteSandbox(ctx context.Context, client *agent37.Client, instance agent37.Instance, name string) (bool, error) {
	if instance.Status == "deleting" {
		return false, fmt.Errorf("Agent37 is still deleting %s. Retry to check its status", name)
	}
	tailnetPending := instance.Metadata["tailnet"] == "tailscale" && instance.Status != "stopped"
	if tailnetPending && !instance.PastDue && (instance.Status == "running" || instance.Status == "sleeping") {
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
	loggedOut := instance.Metadata["tailnet"] != "tailscale" || instance.PastDue
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
	fresh := enrollment != nil && opts.generatedName && !wasStopped && instance.Status != "sleeping"
	if wasStopped || instance.Status == "sleeping" {
		var err error
		instance, err = wake(ctx, client, instance)
		if err != nil {
			return setupProof{}, err
		}
	}
	proof, err := bootstrap(ctx, client, enrollment, instance, opts.name, sandboxBootstrap{
		peerLinks: true, mayRestart: wasStopped, githubAccess: opts.githubAccess,
		prepareDesktop: opts.up && !opts.noDesktop, fresh: fresh, playbook: opts.playbook,
		cliproxyModels: opts.cliproxyModels, preparedGitHub: opts.preparedGitHub,
	}, diagnostic)
	if err == nil && !proof.ProviderReady {
		notice(diagnostic, warning, "%s needs sign-in. Run t3-agent37 terminal %s.", providerLabel(provider), opts.name)
	}
	if err == nil && proof.TailnetURL == "" {
		err = errors.New("Agent37 did not provide the sandbox's Tailscale address")
	}
	return proof, err
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

func validatePairLink(origin, pairURL string) error {
	base, err := url.Parse(origin)
	if err != nil {
		return err
	}
	if base.Scheme != "https" || !strings.HasSuffix(base.Hostname(), ".ts.net") {
		return errors.New("T3 pairing requires a private Tailscale HTTPS address")
	}
	parsed, err := url.Parse(pairURL)
	if err != nil || parsed.Scheme != base.Scheme || parsed.Host != base.Host ||
		parsed.User != nil || parsed.Path != "/pair" || parsed.RawQuery != "" {
		return errors.New("T3 returned an unexpected pairing address")
	}
	if token, found := strings.CutPrefix(parsed.Fragment, "token="); !found || token == "" {
		return errors.New("T3 pairing URL does not contain a token")
	}
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
	command := startupCmd + " pair " + shellQuote(origin) + " " + shellQuote(label) + fmt.Sprintf(" %ds", int(ttl/time.Second))
	reply, err := client.ExecJSON[struct {
		PairURL string `json:"pairUrl"`
	}](ctx, id, command)
	if err != nil {
		return "", err
	}
	if err := validatePairLink(origin, reply.PairURL); err != nil {
		return "", err
	}
	return reply.PairURL, nil
}
