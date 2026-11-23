// Command t3-agent37-bench benchmarks end-to-end Agent37 sandbox provisioning,
// T3 Code connectivity, and TerraScale workspace readiness.
package main

import (
	"bytes"
	"cmp"
	"context"
	json "encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	"github.com/MarioGK/t3-agent37/internal/peers"
)

type benchConfig struct {
	Bin              string
	Compare          string
	Runs             int
	Count            int
	Parallel         int
	Template         string
	Desktop          bool
	Keep             bool
	VerifyTerraScale bool
	E2E              bool
	Target           time.Duration
	JSONOutput       bool
	OutFile          string
}

type sandboxRef struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	TailnetURL    string `json:"t3_url"`
	EnvironmentID string `json:"environment_id"`
	Desktop       string `json:"desktop,omitempty"`
}

type readinessReport struct {
	Sandbox             string   `json:"sandbox"`
	InstanceID          string   `json:"instanceId"`
	TailnetURL          string   `json:"tailnetUrl"`
	EnvironmentID       string   `json:"environmentId"`
	Label               string   `json:"label,omitempty"`
	ServerVersion       string   `json:"serverVersion,omitempty"`
	HTTPSReachable      bool     `json:"httpsReachable"`
	PeerLinksCapable    bool     `json:"peerLinksCapable"`
	DesktopConfirmed    bool     `json:"desktopConfirmed"`
	ConnectedPeers      []string `json:"connectedPeers,omitempty"`
	MissingPeers        []string `json:"missingPeers,omitempty"`
	ReversePeers        []string `json:"reversePeers,omitempty"`
	MissingReversePeers []string `json:"missingReversePeers,omitempty"`
	RegisteredProjects  int      `json:"registeredProjects"`
	MissingProjects     []string `json:"missingProjects,omitempty"`
	MissingTools        []string `json:"missingTools,omitempty"`
	SkillsLinked        bool     `json:"skillsLinked"`
	OrchCurrent         bool     `json:"orchCurrent"`
	ProvidersReady      bool           `json:"providersReady"`
	GitHubReady         bool           `json:"githubReady"`
	E2ESteps            []e2eStep      `json:"e2eSteps,omitempty"`
	Errors              []string       `json:"errors,omitempty"`
	VerifySeconds       float64        `json:"verifySeconds"`
}

type e2eStep struct {
	Name    string  `json:"name"`
	Passed  bool    `json:"passed"`
	Seconds float64 `json:"seconds"`
	Detail  string  `json:"detail,omitempty"`
}

func (r readinessReport) ready(requireDesktop, verifyTerraScale bool) bool {
	if !r.HTTPSReachable || !r.PeerLinksCapable || len(r.MissingPeers) > 0 || len(r.MissingReversePeers) > 0 || len(r.Errors) > 0 {
		return false
	}
	for _, step := range r.E2ESteps {
		if !step.Passed {
			return false
		}
	}
	if requireDesktop && !r.DesktopConfirmed {
		return false
	}
	if verifyTerraScale {
		if len(r.MissingProjects) > 0 || len(r.MissingTools) > 0 || !r.SkillsLinked || !r.OrchCurrent || !r.ProvidersReady || !r.GitHubReady {
			return false
		}
	}
	return true
}

type runResult struct {
	Label       string               `json:"label"`
	Run         int                  `json:"run"`
	StartedAt   time.Time            `json:"startedAt"`
	UpSeconds   float64              `json:"upSeconds"`
	DownSeconds float64              `json:"downSeconds,omitempty"`
	PassedSLA   bool                 `json:"passedSla"`
	Ready       bool                 `json:"ready"`
	Sandboxes   []sandboxRef         `json:"sandboxes,omitempty"`
	Stages      map[string]float64   `json:"stages,omitempty"`
	PerSandbox  map[string]map[string]float64 `json:"perSandboxStages,omitempty"`
	SeededTrees int                  `json:"seededWorktrees,omitempty"`
	Readiness   []readinessReport    `json:"readiness,omitempty"`
	Error       string               `json:"error,omitempty"`
	Diagnostics string               `json:"diagnostics,omitempty"`
}

type benchReport struct {
	TargetSeconds float64     `json:"targetSeconds"`
	Count         int         `json:"count"`
	Runs          int         `json:"runs"`
	Desktop       bool        `json:"desktop"`
	Template      string      `json:"template,omitempty"`
	Results       []runResult `json:"results"`
}

// Guest readiness check script run once per sandbox after `up` finishes.
const guestReadinessProbe = `import json, os, pathlib, shutil, subprocess, sys, tomllib
from urllib.request import ProxyHandler, Request, build_opener

home = pathlib.Path.home().resolve()
root = home / ".local/share/t3-agent37"
data = root / "data"
port = 4373 if os.environ.get("AGENT37_GATEWAY_PORT") == "3773" else 3773
startup = root / "startup.py" if (root / "startup.py").is_file() else pathlib.Path("/opt/t3-agent37/startup.py")
use_startup = startup.is_file()

def run(cmd, cwd=None, timeout=15):
    return subprocess.run(cmd, cwd=cwd, capture_output=True, text=True, timeout=timeout)

out = {"errors": [], "connectedPeers": [], "missingTools": [], "registeredProjects": 0,
       "missingProjects": [], "skillsLinked": False, "orchCurrent": False,
       "providersReady": False, "githubReady": False, "desktopConfirmed": False}

# 1. Inspect T3 + desktop confirmation + peer links.
if use_startup:
    try:
        info = json.loads(run(["python3", "-B", str(startup), "inspect"]).stdout)
        out["desktopConfirmed"] = bool(info.get("desktopConfirmed"))
    except Exception as e:
        out["errors"].append(f"inspect: {e}")
    try:
        res = run(["python3", "-B", str(startup), "env", "list", "--json"], timeout=20)
        if res.returncode == 0 and res.stdout.strip():
            links = json.loads(res.stdout)
            out["connectedPeers"] = [l.get("environmentId", "") for l in links if l.get("status") == "reachable"]
            out["peerLinks"] = [{"id": l.get("environmentId", ""), "label": l.get("label", ""), "status": l.get("status", "")} for l in links]
        else:
            out["errors"].append(f"env list failed (exit {res.returncode})")
    except Exception as e:
        out["errors"].append(f"env list: {e}")
else:
    try:
        dc = (root / "desktop-confirmed").read_text().strip()
        out["desktopConfirmed"] = bool(dc)
    except Exception:
        pass
    try:
        sys.path.insert(0, str(root))
        import remote
        env = dict(os.environ, NODE_USE_ENV_PROXY="1", HTTPS_PROXY="http://127.0.0.1:1055", NO_PROXY="127.0.0.1,localhost,::1")
        res = subprocess.run([str(remote.T3), "environment", "list", "--json", "--base-dir", str(remote.DATA)], capture_output=True, text=True, timeout=20, env=env)
        if res.returncode == 0 and res.stdout.strip():
            links = json.loads(res.stdout)
            out["connectedPeers"] = [l.get("environmentId", "") for l in links if l.get("status") == "reachable"]
            out["peerLinks"] = [{"id": l.get("environmentId", ""), "label": l.get("label", ""), "status": l.get("status", "")} for l in links]
    except Exception as e:
        out["errors"].append(f"legacy env list: {e}")

# 2. Check registered T3 projects via local API.
t3 = pathlib.Path("/usr/local/bin/t3")
if not t3.is_file():
    for cand in sorted((root / "versions").glob("*/t3")):
        t3 = cand
try:
    issued = json.loads(run([str(t3), "auth", "session", "issue", "--base-dir", str(data),
                             "--scope", "orchestration:read", "--ttl", "1m", "--label", "t3-agent37-bench", "--json"]).stdout)
    try:
        req = Request(f"http://127.0.0.1:{port}/api/projects", headers={"Authorization": f"Bearer {issued['token']}"})
        with build_opener(ProxyHandler({})).open(req, timeout=5) as resp:
            projects = [p for p in json.load(resp).get("projects", []) if not p.get("deletedAt") and not p.get("archivedAt")]
        out["registeredProjects"] = len(projects)
        registered_keys = {(p.get("repositoryIdentity") or {}).get("canonicalKey", "").lower() for p in projects}
    finally:
        run([str(t3), "auth", "session", "revoke", "--base-dir", str(data), issued["sessionId"]])
except Exception as e:
    registered_keys = set()
    out["errors"].append(f"projects api: {e}")

# 3. Verify TerraScale workspace, tools, providers, and GitHub auth.
ws_arg = sys.argv[1] if len(sys.argv) > 1 else ""
if ws_arg:
    ws = (home / ws_arg.removeprefix("~/")).resolve()
    expected = {"github.com/terrascale/workspace"}
    manifest = ws / "repos.txt"
    if manifest.is_file():
        for entry in manifest.read_text().split():
            expected.add(f"github.com/terrascale/{entry.lower()}")
    out["missingProjects"] = sorted(k for k in expected if k not in registered_keys)

    tools = ["git", "gh", "dotnet", "go", "mise", "node", "npm", "python3", "rustc", "codex", "claude", "opencode", "ts", "agy", "ts-orch"]
    search_path = os.pathsep.join([str(root / "bin"), str(home / ".local/bin"), os.environ.get("PATH", "")])
    out["missingTools"] = [t for t in tools if shutil.which(t, path=search_path) is None]

    # Check skills and helper symlinks
    skill_dirs = [home / d for d in (".gemini/config/skills", ".agents/skills", ".claude/skills", ".codex/skills", ".config/opencode/skills")]
    out["skillsLinked"] = all(d.is_dir() and any(d.iterdir()) for d in skill_dirs)

    # Check ts-orch matches ts-orchestrator HEAD
    orch_repo = ws / "ts-orchestrator"
    orch_bin = home / ".local/bin/ts-orch" if (home / ".local/bin/ts-orch").is_file() else pathlib.Path("/usr/local/bin/ts-orch")
    try:
        head = run(["git", "-C", str(orch_repo), "rev-parse", "HEAD"]).stdout.strip()
        info_txt = run(["go", "version", "-m", str(orch_bin)]).stdout
        out["orchCurrent"] = (f"vcs.revision={head}" in info_txt and "vcs.modified=false" in info_txt)
    except Exception:
        out["orchCurrent"] = False

    # Check providers (OpenCode, Codex, Claude, T3 settings)
    try:
        cfg_home = pathlib.Path(os.environ.get("XDG_CONFIG_HOME") or home / ".config")
        oc = json.loads((cfg_home / "opencode/opencode.json").read_text())
        codex_cfg = tomllib.loads((home / ".codex/config.toml").read_text())
        t3_settings = json.loads((data / "userdata/settings.json").read_text())
        out["providersReady"] = (
            bool(oc.get("provider", {}).get("cliproxy", {}).get("models"))
            and codex_cfg.get("model_provider") == "t3_agent37_cliproxy"
            and (home / ".local/bin/claude").exists()
            and bool(t3_settings.get("linkedEnvironmentsEnabled"))
        )
    except Exception:
        out["providersReady"] = False

    gh_path = shutil.which("gh", path=search_path)
    out["githubReady"] = bool(gh_path and run([gh_path, "auth", "token", "--hostname", "github.com"]).returncode == 0)

print(json.dumps(out))
`

var (
	singleReadyRe = regexp.MustCompile(`Cloud T3 ready \(([a-z0-9]{10}), ([^)]+)\)\.`)
	tailnetURLRe  = regexp.MustCompile(`T3 Code on Tailscale (https://\S+)`)
	timingLineRe  = regexp.MustCompile(`([a-z0-9][a-z0-9-]*)\s+·\s+(.+?)\s+·\s+([0-9]+\.[0-9]+)s`)
	seededLineRe  = regexp.MustCompile(`Reused (\d+) baked worktrees`)
	sandboxNameRe = regexp.MustCompile(`^https://a37-([a-z0-9-]+)-[a-z0-9]{10}\.`)
)

func main() {
	cfg := parseFlags()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := runBench(ctx, cfg, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "t3-agent37-bench: %v\n", err)
		os.Exit(1)
	}
}

func parseFlags() benchConfig {
	var cfg benchConfig
	flag.StringVar(&cfg.Bin, "bin", "", "Path to t3-agent37 binary (default: builds ./cmd/t3-agent37)")
	flag.StringVar(&cfg.Compare, "compare", "", "Optional second t3-agent37 binary for A/B comparison")
	flag.IntVar(&cfg.Runs, "runs", 1, "Number of benchmark iterations per binary")
	flag.IntVar(&cfg.Count, "count", 1, "Sandboxes per run (1 = single up, >1 = batch up <count>)")
	flag.IntVar(&cfg.Parallel, "parallel", 0, "Optional --parallel value for batch runs")
	flag.StringVar(&cfg.Template, "template", "", "Optional --template override")
	flag.BoolVar(&cfg.Desktop, "desktop", false, "Run with T3 Code desktop pairing instead of --no-desktop")
	flag.BoolVar(&cfg.Keep, "keep", false, "Keep created sandboxes instead of deleting them after verification")
	flag.BoolVar(&cfg.VerifyTerraScale, "verify-terrascale", true, "Verify TerraScale workspace, tools, projects, and T3 peer links")
	flag.BoolVar(&cfg.E2E, "e2e", false, "Run live LLM harness turns, GitHub PR dry-run, and TerraScale Go/.NET builds inside the sandbox")
	flag.DurationVar(&cfg.Target, "target", 2*time.Minute, "Target wall-clock SLA budget for sandbox readiness")
	flag.BoolVar(&cfg.JSONOutput, "json", false, "Print JSON report to stdout")
	flag.StringVar(&cfg.OutFile, "out", "", "Optional path to write JSON benchmark report")
	flag.Parse()
	return cfg
}

func runBench(ctx context.Context, cfg benchConfig, out, diag io.Writer) error {
	if cfg.Runs < 1 || cfg.Count < 1 {
		return errors.New("--runs and --count must be >= 1")
	}
	binPath := cfg.Bin
	var tempDir string
	if binPath == "" {
		var err error
		tempDir, err = os.MkdirTemp("", "t3-agent37-bench-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tempDir)
		binPath = filepath.Join(tempDir, "t3-agent37")
		fmt.Fprintf(diag, "Building ./cmd/t3-agent37…\n")
		build := exec.CommandContext(ctx, "go", "build", "-o", binPath, "./cmd/t3-agent37")
		build.Stderr = diag
		if err := build.Run(); err != nil {
			return fmt.Errorf("go build ./cmd/t3-agent37 failed: %w", err)
		}
	}

	type targetBin struct {
		label string
		path  string
	}
	targets := []targetBin{{label: binLabel(binPath, cfg.Bin == ""), path: binPath}}
	if cfg.Compare != "" {
		targets = append(targets, targetBin{label: binLabel(cfg.Compare, false), path: cfg.Compare})
	}

	apiKey, err := loadAgent37Key()
	if err != nil {
		return err
	}
	client := agent37.New(apiKey)
	configuredPeers, err := peers.Configured()
	if err != nil {
		return err
	}
	workspacePath := loadWorkspacePath()

	report := benchReport{
		TargetSeconds: cfg.Target.Seconds(),
		Count:         cfg.Count,
		Runs:          cfg.Runs,
		Desktop:       cfg.Desktop,
		Template:      cfg.Template,
	}

	var failures []error
	for _, target := range targets {
		for runNum := 1; runNum <= cfg.Runs; runNum++ {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintf(diag, "\n==> [%s] Run %d/%d (count=%d, desktop=%v, target=%s)\n",
				target.label, runNum, cfg.Runs, cfg.Count, cfg.Desktop, cfg.Target)
			res := executeSingleRun(ctx, cfg, target.label, target.path, runNum, client, configuredPeers, workspacePath, diag)
			report.Results = append(report.Results, res)
			if res.Error != "" {
				failures = append(failures, fmt.Errorf("%s run %d: %s", target.label, runNum, res.Error))
			} else if !res.Ready {
				failures = append(failures, fmt.Errorf("%s run %d: sandbox failed readiness verification", target.label, runNum))
			} else if !res.PassedSLA {
				failures = append(failures, fmt.Errorf("%s run %d: took %.2fs (budget %.2fs)", target.label, runNum, res.UpSeconds, cfg.Target.Seconds()))
			}
		}
	}

	if cfg.OutFile != "" {
		data, err := json.Marshal(report)
		if err != nil {
			return err
		}
		if err := os.WriteFile(cfg.OutFile, append(data, '\n'), 0o600); err != nil {
			return err
		}
	}
	if cfg.JSONOutput {
		data, err := json.Marshal(report)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, string(data))
		return errors.Join(err, errors.Join(failures...))
	}
	printSummary(out, report)
	return errors.Join(failures...)
}

func binLabel(path string, builtLocal bool) string {
	if builtLocal {
		return "working-tree"
	}
	out, err := exec.Command(path, "--version").Output()
	if err == nil {
		if ver := strings.TrimSpace(string(out)); ver != "" {
			return filepath.Base(path) + "@" + ver
		}
	}
	return filepath.Base(path)
}

func executeSingleRun(ctx context.Context, cfg benchConfig, label, binPath string, runNum int, client *agent37.Client, configuredPeers []peers.Peer, workspacePath string, diag io.Writer) runResult {
	res := runResult{
		Label:     label,
		Run:       runNum,
		StartedAt: time.Now(),
	}
	args := []string{"up"}
	if cfg.Count > 1 {
		args = append(args, strconv.Itoa(cfg.Count))
		if cfg.Parallel > 0 {
			args = append(args, "--parallel", strconv.Itoa(cfg.Parallel))
		}
	}
	if !cfg.Desktop {
		args = append(args, "--no-desktop")
	}
	if cfg.Template != "" {
		args = append(args, "--template", cfg.Template)
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, binPath, args...)
	cmd.Env = append(os.Environ(), "T3_AGENT37_TIMINGS=1")
	cmd.Stdout = &stdout
	cmd.Stderr = io.MultiWriter(&stderr, diag)

	upStart := time.Now()
	upErr := cmd.Run()
	res.UpSeconds = round3(time.Since(upStart).Seconds())
	res.PassedSLA = time.Since(upStart) <= cfg.Target
	res.Diagnostics = stderr.String()
	res.Stages, res.PerSandbox, res.SeededTrees = parseTimings(stderr.String())
	res.Sandboxes = parseUpOutput(cfg.Count, stdout.String())

	// Ensure cleanup runs even if verification fails or context is interrupted.
	if !cfg.Keep {
		defer func() {
			cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer cancel()
			targets := sandboxNamesForCleanup(cleanCtx, client, res.Sandboxes, res.StartedAt)
			if len(targets) == 0 {
				return
			}
			downStart := time.Now()
			for _, name := range targets {
				fmt.Fprintf(diag, "  Cleaning up %s…\n", name)
				down := exec.CommandContext(cleanCtx, binPath, "down", name)
				down.Stderr = diag
				_ = down.Run()
			}
			res.DownSeconds = round3(time.Since(downStart).Seconds())
		}()
	}

	if upErr != nil {
		res.Error = fmt.Sprintf("up failed: %v", upErr)
		return res
	}
	if len(res.Sandboxes) != cfg.Count {
		res.Error = fmt.Sprintf("expected %d sandbox(es) in output, got %d", cfg.Count, len(res.Sandboxes))
		return res
	}

	allReady := true
	for _, sb := range res.Sandboxes {
		rd := verifySandboxReadiness(ctx, client, sb, configuredPeers, workspacePath, cfg.VerifyTerraScale, cfg.E2E, diag)
		res.Readiness = append(res.Readiness, rd)
		if !rd.ready(cfg.Desktop, cfg.VerifyTerraScale) {
			allReady = false
		}
	}
	res.Ready = allReady
	return res
}

func verifySandboxReadiness(ctx context.Context, client *agent37.Client, sb sandboxRef, configuredPeers []peers.Peer, workspacePath string, verifyTerraScale, runE2E bool, diag io.Writer) readinessReport {
	started := time.Now()
	rd := readinessReport{
		Sandbox:       sb.Name,
		InstanceID:    sb.ID,
		TailnetURL:    sb.TailnetURL,
		EnvironmentID: sb.EnvironmentID,
	}
	defer func() { rd.VerifySeconds = round3(time.Since(started).Seconds()) }()

	// 1. Check T3 Code HTTPS descriptor directly over Tailscale.
	httpClient := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(sb.TailnetURL, "/")+"/.well-known/t3/environment", nil)
	if err != nil {
		rd.Errors = append(rd.Errors, "invalid tailnet URL")
	} else if resp, err := httpClient.Do(req); err != nil {
		rd.Errors = append(rd.Errors, fmt.Sprintf("tailnet HTTPS unreachable: %v", err))
	} else {
		defer resp.Body.Close()
		var desc struct {
			EnvironmentID string `json:"environmentId"`
			Label         string `json:"label"`
			ServerVersion string `json:"serverVersion"`
			Capabilities  struct {
				PeerLinks bool `json:"peerLinks"`
			} `json:"capabilities"`
		}
		if resp.StatusCode == http.StatusOK && json.UnmarshalRead(resp.Body, &desc) == nil {
			rd.HTTPSReachable = desc.EnvironmentID == sb.EnvironmentID
			rd.Label = desc.Label
			rd.ServerVersion = desc.ServerVersion
			rd.PeerLinksCapable = desc.Capabilities.PeerLinks
			if !rd.HTTPSReachable {
				rd.Errors = append(rd.Errors, fmt.Sprintf("environmentId mismatch (%s != %s)", desc.EnvironmentID, sb.EnvironmentID))
			}
		} else {
			rd.Errors = append(rd.Errors, fmt.Sprintf("descriptor returned HTTP %d", resp.StatusCode))
		}
	}

	// 2. Run single in-guest readiness probe.
	wsArg := ""
	if verifyTerraScale {
		wsArg = cmp.Or(workspacePath, "~/Projects/TerraScale")
	}
	cmd := "python3 -B -c " + shellQuote(guestReadinessProbe) + " " + shellQuote(wsArg)
	var probe struct {
		Errors             []string `json:"errors"`
		ConnectedPeers     []string `json:"connectedPeers"`
		RegisteredProjects int      `json:"registeredProjects"`
		MissingProjects    []string `json:"missingProjects"`
		MissingTools       []string `json:"missingTools"`
		SkillsLinked       bool     `json:"skillsLinked"`
		OrchCurrent        bool     `json:"orchCurrent"`
		ProvidersReady     bool     `json:"providersReady"`
		GitHubReady        bool     `json:"githubReady"`
		DesktopConfirmed   bool     `json:"desktopConfirmed"`
	}
	probe, err = client.ExecJSON[struct {
		Errors             []string `json:"errors"`
		ConnectedPeers     []string `json:"connectedPeers"`
		RegisteredProjects int      `json:"registeredProjects"`
		MissingProjects    []string `json:"missingProjects"`
		MissingTools       []string `json:"missingTools"`
		SkillsLinked       bool     `json:"skillsLinked"`
		OrchCurrent        bool     `json:"orchCurrent"`
		ProvidersReady     bool     `json:"providersReady"`
		GitHubReady        bool     `json:"githubReady"`
		DesktopConfirmed   bool     `json:"desktopConfirmed"`
	}](ctx, sb.ID, cmd)
	if err != nil {
		rd.Errors = append(rd.Errors, fmt.Sprintf("guest probe failed: %v", err))
		return rd
	}
	rd.Errors = append(rd.Errors, probe.Errors...)
	rd.ConnectedPeers = probe.ConnectedPeers
	rd.RegisteredProjects = probe.RegisteredProjects
	rd.MissingProjects = probe.MissingProjects
	rd.MissingTools = probe.MissingTools
	rd.SkillsLinked = probe.SkillsLinked
	rd.OrchCurrent = probe.OrchCurrent
	rd.ProvidersReady = probe.ProvidersReady
	rd.GitHubReady = probe.GitHubReady
	rd.DesktopConfirmed = probe.DesktopConfirmed

	for _, peer := range configuredPeers {
		if !slices.Contains(probe.ConnectedPeers, peer.EnvironmentID) {
			rd.MissingPeers = append(rd.MissingPeers, peer.Host)
		}
	}

	// 3. Verify configured peers see this sandbox linked back in reverse.
	if len(configuredPeers) > 0 {
		var mu sync.Mutex
		seenReverse := make(map[string]bool, len(configuredPeers))
		_, peerErr := peers.PruneSandboxLinks(ctx, configuredPeers, true, func(_ context.Context, link peers.SandboxLink, _ func() error) (bool, error) {
			if link.EnvironmentID == sb.EnvironmentID && link.SandboxID == sb.ID {
				mu.Lock()
				seenReverse[link.Peer] = true
				mu.Unlock()
			}
			return false, nil
		})
		if peerErr != nil {
			rd.Errors = append(rd.Errors, fmt.Sprintf("reverse peer check: %v", peerErr))
		}
		for _, peer := range configuredPeers {
			if seenReverse[peer.Host] {
				rd.ReversePeers = append(rd.ReversePeers, peer.Host)
			} else {
				rd.MissingReversePeers = append(rd.MissingReversePeers, peer.Host)
			}
		}
	}
	if runE2E {
		ws := cmp.Or(workspacePath, "~/Projects/TerraScale")
		rd.E2ESteps = runGuestE2E(ctx, client, sb.ID, ws, diag)
	}
	return rd
}

func runGuestE2E(ctx context.Context, client *agent37.Client, id, workspacePath string, diag io.Writer) []e2eStep {
	wsRel := strings.TrimPrefix(workspacePath, "~/")
	checks := []struct {
		name    string
		timeout time.Duration
		cmd     string
	}{
		{"opencode turn", 45 * time.Second, `timeout 35s opencode run "Reply with exactly: OK_OPENCODE"`},
		{"codex turn", 45 * time.Second, `timeout 35s codex exec --skip-git-repo-check "Reply with exactly: OK_CODEX"`},
		{"claude turn", 45 * time.Second, `timeout 35s claude -p "Reply with exactly: OK_CLAUDE"`},
		{"agy models", 25 * time.Second, `timeout 20s agy models | head -n 5`},
		{"gh pr & private repo", 25 * time.Second, `gh auth status --hostname github.com >/dev/null && gh pr list -R TerraScale/terrascale --limit 1 >/dev/null && gh repo view TerraScale/ts-management --json name >/dev/null`},
		{"git branch+commit+push-dry", 30 * time.Second, fmt.Sprintf(`cd "$HOME/%s/terrascale" && git fetch --quiet origin main && git checkout -q -b e2e-dryrun-check && git commit --allow-empty -q -m "e2e check" && git push --dry-run origin e2e-dryrun-check && git checkout -q - && git branch -D -q e2e-dryrun-check`, wsRel)},
		{"ts-orchestrator go build", 60 * time.Second, fmt.Sprintf(`cd "$HOME/%s/ts-orchestrator" && CGO_ENABLED=0 go build -trimpath -o /dev/null ./cmd/terrascale-orchestrator`, wsRel)},
		{"terrascale dotnet build", 90 * time.Second, fmt.Sprintf(`cd "$HOME/%s/terrascale" && dotnet build src/TerraScale.Logging/TerraScale.Logging.csproj -v minimal`, wsRel)},
	}
	steps := make([]e2eStep, 0, len(checks))
	for _, c := range checks {
		stepCtx, cancel := context.WithTimeout(ctx, c.timeout)
		started := time.Now()
		out, err := client.Exec(stepCtx, id, `export PATH="$HOME/.local/share/t3-agent37/bin:$HOME/.local/bin:$PATH"; `+c.cmd)
		dt := round3(time.Since(started).Seconds())
		cancel()
		step := e2eStep{Name: c.name, Passed: err == nil, Seconds: dt}
		if err != nil {
			step.Detail = err.Error()
		} else if trimmed := strings.TrimSpace(strings.ReplaceAll(out, "\n", " ")); trimmed != "" {
			if len(trimmed) > 100 {
				trimmed = trimmed[:100]
			}
			step.Detail = trimmed
		}
		fmt.Fprintf(diag, "  E2E %-28s pass=%-5v (%.2fs)\n", step.Name, step.Passed, step.Seconds)
		steps = append(steps, step)
	}
	return steps
}

func parseTimings(diagnostics string) (map[string]float64, map[string]map[string]float64, int) {
	flat := make(map[string]float64)
	perSandbox := make(map[string]map[string]float64)
	seeded := 0
	for line := range strings.SplitSeq(diagnostics, "\n") {
		if m := seededLineRe.FindStringSubmatch(line); len(m) == 2 {
			if n, err := strconv.Atoi(m[1]); err == nil {
				seeded += n
			}
		}
		m := timingLineRe.FindStringSubmatch(line)
		if len(m) != 4 {
			continue
		}
		target, stage := strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
		secs, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			continue
		}
		if perSandbox[target] == nil {
			perSandbox[target] = make(map[string]float64)
		}
		perSandbox[target][stage] = secs
		if existing, ok := flat[stage]; !ok || secs > existing {
			flat[stage] = secs
		}
	}
	return flat, perSandbox, seeded
}

func parseUpOutput(count int, stdout string) []sandboxRef {
	if count > 1 {
		var batch struct {
			Sandboxes []sandboxRef `json:"sandboxes"`
		}
		if json.Unmarshal([]byte(stdout), &batch) == nil && len(batch.Sandboxes) > 0 {
			return batch.Sandboxes
		}
		return nil
	}
	var sb sandboxRef
	if m := singleReadyRe.FindStringSubmatch(stdout); len(m) == 3 {
		sb.ID, sb.EnvironmentID = m[1], m[2]
	}
	if m := tailnetURLRe.FindStringSubmatch(stdout); len(m) == 2 {
		sb.TailnetURL = m[1]
		if nm := sandboxNameRe.FindStringSubmatch(sb.TailnetURL); len(nm) == 2 {
			sb.Name = nm[1]
		}
	}
	if sb.ID != "" && sb.TailnetURL != "" {
		return []sandboxRef{sb}
	}
	return nil
}

func sandboxNamesForCleanup(ctx context.Context, client *agent37.Client, parsed []sandboxRef, startedAt time.Time) []string {
	var names []string
	for _, sb := range parsed {
		if sb.Name != "" {
			names = append(names, sb.Name)
		}
	}
	if len(names) > 0 {
		return names
	}
	// Fallback if `up` failed mid-way: find any managed sandbox created after startedAt.
	instances, err := client.List(ctx)
	if err != nil {
		return nil
	}
	cutoff := startedAt.Add(-5 * time.Second).Unix()
	for _, inst := range instances {
		if inst.Status != "deleted" && inst.Metadata["managed_by"] == "t3-agent37" && inst.Created >= cutoff {
			if name := cmp.Or(inst.Metadata["environment"], inst.Name); name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

func printSummary(out io.Writer, report benchReport) {
	fmt.Fprintf(out, "\n=== t3-agent37 Benchmark Summary (target <= %.0fs, count=%d, desktop=%v) ===\n\n",
		report.TargetSeconds, report.Count, report.Desktop)
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "BINARY\tRUN\tWALL\tSLA\tREADY\tCREATE\tGUEST\tHTTPS\tMESH\tPROJECTS\tPEERS\tSEEDED")
	for _, r := range report.Results {
		sla := "FAIL"
		if r.PassedSLA && r.Error == "" {
			sla = "PASS"
		}
		ready := "FAIL"
		if r.Ready && r.Error == "" {
			ready = "PASS"
		}
		projects, peersStr := "-", "-"
		if len(r.Readiness) > 0 {
			rd := r.Readiness[0]
			projects = fmt.Sprintf("%d (miss %d)", rd.RegisteredProjects, len(rd.MissingProjects))
			peersStr = fmt.Sprintf("%d ok (miss %d)", len(rd.ConnectedPeers), len(rd.MissingPeers))
		}
		mesh := cmp.Or(r.Stages["Peer linking"], r.Stages["Peer sync"])
		fmt.Fprintf(tw, "%s\t%d\t%.2fs\t%s\t%s\t%.2fs\t%.2fs\t%.2fs\t%.2fs\t%s\t%s\t%d\n",
			r.Label, r.Run, r.UpSeconds, sla, ready,
			r.Stages["Agent37 creation"], r.Stages["Guest startup"], r.Stages["Tailscale HTTPS verification"], mesh,
			projects, peersStr, r.SeededTrees)
	}
	_ = tw.Flush()

	// Print aggregate stats when running multiple iterations.
	if len(report.Results) > 1 {
		var passing []float64
		for _, r := range report.Results {
			if r.Error == "" && r.Ready {
				passing = append(passing, r.UpSeconds)
			}
		}
		if len(passing) > 0 {
			slices.Sort(passing)
			median := passing[len(passing)/2]
			fmt.Fprintf(out, "\nAggregate (%d/%d passed): min=%.2fs median=%.2fs max=%.2fs (target <= %.0fs)\n",
				len(passing), len(report.Results), passing[0], median, passing[len(passing)-1], report.TargetSeconds)
		}
	}

	// Print detailed stage breakdown for each run.
	stageOrder := []string{
		"Preflight", "Agent37 creation", "Guest startup",
		"guest t3", "guest tailnet", "guest workspace", "guest projects", "guest tools",
		"MagicDNS resolved", "Tailscale TCP connected", "Trusted TLS connected",
		"Tailscale HTTPS verification", "Desktop pairing and approval", "Peer linking", "Peer sync", "Command elapsed",
	}
	for _, r := range report.Results {
		fmt.Fprintf(out, "\n[%s run %d] Stage Breakdown:\n", r.Label, r.Run)
		st := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		for _, k := range stageOrder {
			if v, ok := r.Stages[k]; ok {
				fmt.Fprintf(st, "  %s\t%.3fs\n", k, v)
			}
		}
		_ = st.Flush()
		for _, rd := range r.Readiness {
			fmt.Fprintf(out, "  Readiness (%s): label=%q version=%s https=%v peerLinks=%v desktop=%v projects=%d skills=%v orch=%v providers=%v gh=%v peers=%d/%d\n",
				rd.Sandbox, rd.Label, rd.ServerVersion, rd.HTTPSReachable, rd.PeerLinksCapable, rd.DesktopConfirmed, rd.RegisteredProjects,
				rd.SkillsLinked, rd.OrchCurrent, rd.ProvidersReady, rd.GitHubReady, len(rd.ConnectedPeers), len(rd.ReversePeers))
			if len(rd.MissingPeers) > 0 {
				fmt.Fprintf(out, "    MISSING PEERS (sandbox -> peer): %s\n", strings.Join(rd.MissingPeers, ", "))
			}
			if len(rd.MissingReversePeers) > 0 {
				fmt.Fprintf(out, "    MISSING REVERSE PEERS (peer -> sandbox): %s\n", strings.Join(rd.MissingReversePeers, ", "))
			}
			if len(rd.MissingProjects) > 0 {
				fmt.Fprintf(out, "    MISSING PROJECTS: %s\n", strings.Join(rd.MissingProjects, ", "))
			}
			if len(rd.MissingTools) > 0 {
				fmt.Fprintf(out, "    MISSING TOOLS: %s\n", strings.Join(rd.MissingTools, ", "))
			}
			for _, e := range rd.Errors {
				fmt.Fprintf(out, "    ERROR: %s\n", e)
			}
			for _, step := range rd.E2ESteps {
				status := "PASS"
				if !step.Passed {
					status = "FAIL"
				}
				fmt.Fprintf(out, "    E2E %-4s %-28s %.2fs\n", status, step.Name, step.Seconds)
			}
		}
		if r.Error != "" {
			fmt.Fprintf(out, "  RUN ERROR: %s\n", r.Error)
		}
	}
}

func loadAgent37Key() (string, error) {
	if key := strings.TrimSpace(os.Getenv("AGENT37_API_KEY")); key != "" {
		return key, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	xdg := cmp.Or(os.Getenv("XDG_CONFIG_HOME"), filepath.Join(home, ".config"))
	for _, p := range []string{
		filepath.Join(home, ".config", "agent37", "config.json"),
		filepath.Join(xdg, "t3-agent37", "runtime.json"),
	} {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var raw map[string]any
		if json.Unmarshal(data, &raw) == nil {
			for _, field := range []string{"apiKey", "AGENT37_API_KEY", "api_key"} {
				if val, ok := raw[field].(string); ok && strings.TrimSpace(val) != "" {
					return strings.TrimSpace(val), nil
				}
			}
		}
	}
	return "", errors.New("AGENT37_API_KEY not found in env or workstation config")
}

func loadWorkspacePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	p := cmp.Or(os.Getenv("T3_AGENT37_WORKSPACE_FILE"), filepath.Join(cmp.Or(os.Getenv("XDG_CONFIG_HOME"), filepath.Join(home, ".config")), "t3-agent37", "workspace.json"))
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	var ws struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(data, &ws) == nil {
		return ws.Path
	}
	return ""
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func round3(v float64) float64 {
	return float64(int(v*1000+0.5)) / 1000
}
