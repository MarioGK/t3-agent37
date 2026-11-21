package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	"github.com/MarioGK/t3-agent37/internal/peers"
	"github.com/gofrs/flock"
	flag "github.com/spf13/pflag"
)

const (
	owner       = "t3-agent37"
	sandboxIdle = 15 * time.Minute
)

var version = "dev"
var namePattern = regexp.MustCompile("^[a-z0-9][a-z0-9-]{0,42}$")
var providers = map[string]struct{}{
	"codex": {}, "claude": {}, "opencode": {}, "cliproxy": {},
}

type options struct {
	action, name, provider, template string
	namePrefix                       string
	targetID                         string
	batchSize, parallelism           int
	size                             sandboxSize
	cpu, ram                         *int
	showSize                         bool
	yes, dryRun, pair, tailscale     bool
	legacyDelete                     bool
	deleteAll                        bool
	checkSetup                       bool
	verifySetup                      bool
	repairSetup                      bool
	noSetupBrowser                   bool
	generatedName                    bool
	latest                           bool
	pickDelete                       bool
	noDesktop                        bool
	noTailscale                      bool
	githubAccess                     bool
	listJSON                         bool
}

func resolveOptions(opts options, flags *flag.FlagSet, args []string) (options, error) {
	if !flags.Changed("cpu") {
		opts.cpu = nil
	}
	if !flags.Changed("ram") {
		opts.ram = nil
	}
	if opts.action == "sync" {
		if len(args) != 0 {
			return opts, errors.New("sync does not take a sandbox name")
		}
		return opts, nil
	}
	if opts.action == "config" || opts.action == "setup" {
		if len(args) != 0 {
			return opts, fmt.Errorf("%s does not take a sandbox name", opts.action)
		}
		if opts.checkSetup && opts.verifySetup {
			return opts, errors.New("choose either --check or --verify")
		}
		if opts.repairSetup && (opts.checkSetup || opts.verifySetup) {
			return opts, errors.New("--repair-tailscale already verifies OAuth. Run it on its own")
		}
		if opts.dryRun && !opts.repairSetup {
			return opts, errors.New("--dry-run requires --repair-tailscale")
		}
		if opts.showSize && (opts.cpu != nil || opts.ram != nil) {
			return opts, errors.New("use --show-size on its own")
		}
		return opts, nil
	}
	if opts.action == "up" {
		if len(args) == 1 && batchCountArgument(args[0]) {
			count, err := strconv.Atoi(args[0])
			if err != nil || count < 1 || count > maxBatchSize {
				return opts, fmt.Errorf("up expects a sandbox count from 1 to %d", maxBatchSize)
			}
			opts.batchSize = count
		}
		if flags.Changed("parallel") && (opts.batchSize == 0 || opts.parallelism < 1 || opts.parallelism > maxBatchParallelism) {
			return opts, fmt.Errorf("--parallel requires up <count> and a value from 1 to %d", maxBatchParallelism)
		}
		if len(args) == 0 || opts.batchSize > 0 {
			opts.action = "create"
			opts.yes = !opts.dryRun
		} else {
			if !onlyStartFlags(flags) {
				return opts, errors.New("up accepts --github and --no-desktop when you pass a sandbox name")
			}
			opts.githubAccess = flags.Changed("github") && opts.githubAccess
			opts.action = "start"
		}
	}
	if opts.action == "list" {
		if len(args) != 0 {
			return opts, fmt.Errorf("%s does not take a sandbox name", flags.Name())
		}
		return opts, nil
	}
	if opts.action == "link" && len(args) == 0 {
		return opts, nil
	}
	if opts.action == "down" {
		if (flags.Changed("delete") || flags.Changed("yes")) && (!opts.legacyDelete || !opts.yes) {
			return opts, errors.New("the legacy --delete and --yes flags must be used together. Use stop or sleep to keep the disk")
		}
		if opts.deleteAll || opts.pickDelete {
			if len(args) != 0 || opts.deleteAll && opts.pickDelete {
				return opts, errors.New("choose one of a sandbox name, --all, or --pick")
			}
			return opts, nil
		}
	}
	switch {
	case (opts.action == "down" || opts.action == "sleep") && len(args) == 0:
		opts.latest = true
	case opts.action == "create" && (len(args) == 0 || opts.batchSize > 0):
		opts.generatedName = true
	case len(args) == 1 && namePattern.MatchString(args[0]):
		opts.name = args[0]
	default:
		return opts, errors.New("expected a name with 1 to 43 lowercase letters, digits, or hyphens")
	}
	if opts.action == "create" {
		if opts.yes == opts.dryRun {
			return opts, errors.New("choose exactly one of --yes and --dry-run")
		}
		config, err := loadWorkstationConfig()
		if err != nil {
			return opts, err
		}
		opts.size, err = selectSandboxSize(config.sandboxSize(), opts.cpu, opts.ram)
		if err != nil {
			return opts, err
		}
		opts.namePrefix = config.NamePrefix
		if !flags.Changed("github") {
			opts.githubAccess = config.GitHubAccess == nil || *config.GitHubAccess
		}
		if !flags.Changed("template") {
			opts.template = config.Template
		}
		if !flags.Changed("provider") {
			opts.provider = config.Provider
		}
		opts.provider = sandboxProvider(opts.provider, opts.template)
		_, supported := providers[opts.provider]
		if !supported {
			return opts, fmt.Errorf("unknown provider %q", opts.provider)
		}
		if opts.template == "" {
			return opts, errors.New("choose a Tailscale-ready template in t3-agent37 setup or with --template")
		}
		if opts.noTailscale || (flags.Changed("tailscale") && !opts.tailscale) ||
			(!flags.Changed("tailscale") && config.Tailscale != nil && !*config.Tailscale) {
			return opts, errors.New("Tailscale is required. Enable it in the workstation config")
		}
		opts.tailscale = true
	}
	return opts, nil
}

func onlyStartFlags(flags *flag.FlagSet) bool {
	allowed := 0
	for _, name := range []string{"github", "no-desktop"} {
		if flags.Changed(name) {
			allowed++
		}
	}
	return flags.NFlag() == allowed
}

func addCommandFlags(flags *flag.FlagSet, opts *options) {
	if opts.action == "up" {
		flags.IntVar(&opts.parallelism, "parallel", batchParallelism, fmt.Sprintf("Concurrent sandbox launches for up <count> (1–%d)", maxBatchParallelism))
	}
	if opts.action == "config" || opts.action == "create" || opts.action == "up" {
		opts.cpu = flags.Int("cpu", 0, "vCPUs for each new sandbox (2 or 4)")
		opts.ram = flags.Int("ram", 0, "RAM in GB for each new sandbox (4, 8, 16, or 32)")
	}
	if opts.action == "config" {
		flags.BoolVar(&opts.showSize, "show-size", false, "Print the default sandbox size")
	}
	if opts.action == "list" {
		flags.BoolVar(&opts.listJSON, "json", false, "Print JSON for scripts")
	}
	if opts.action == "setup" {
		flags.BoolVar(&opts.checkSetup, "check", false, "Check workstation configuration without prompts or network requests")
		flags.BoolVar(&opts.verifySetup, "verify", false, "Check local Tailscale status and verify OAuth access")
		flags.BoolVar(&opts.repairSetup, "repair-tailscale", false, "Use the OAuth client to define missing enrollment tags")
		flags.BoolVar(&opts.dryRun, "dry-run", false, "Preview Tailscale tag repair without saving the policy")
		flags.BoolVar(&opts.noSetupBrowser, "no-browser", false, "Keep setup in the terminal")
	}
	if opts.action == "create" {
		flags.BoolVar(&opts.yes, "yes", false, "Authorize paid provisioning")
	}
	if opts.action == "create" || opts.action == "up" || opts.action == "start" {
		flags.BoolVar(&opts.githubAccess, "github", opts.action != "start", "Pass GitHub CLI access and Git identity from this workstation")
	}
	if opts.action == "create" || opts.action == "up" {
		flags.StringVar(&opts.provider, "provider", "", "Provider: opencode, cliproxy, codex or claude")
		flags.StringVar(&opts.template, "template", "", "Agent37 template, optionally pinned by revision")
		flags.BoolVar(&opts.dryRun, "dry-run", false, "Preview the request without creating a sandbox")
		flags.BoolVar(&opts.tailscale, "tailscale", false, "Enroll the sandbox in Tailscale")
		flags.BoolVar(&opts.noTailscale, "no-tailscale", false, "Disable Tailscale enrollment")
		_ = flags.MarkHidden("no-tailscale")
	}
	if opts.action == "connect" {
		flags.BoolVar(&opts.pair, "pair", false, "Print a one-time T3 pairing link")
	}
	if opts.action == "up" || opts.action == "create" {
		flags.BoolVar(&opts.noDesktop, "no-desktop", false, "Skip the T3 Code desktop pairing request")
	}
	if opts.action == "link" {
		flags.BoolVar(&opts.dryRun, "dry-run", false, "List the sandboxes without waking or pairing them")
	}
	if opts.action == "down" {
		flags.BoolVar(&opts.legacyDelete, "delete", false, "Permanently delete the managed sandbox and its disk")
		flags.BoolVar(&opts.yes, "yes", false, "Confirm permanent deletion")
		_ = flags.MarkHidden("delete")
		_ = flags.MarkHidden("yes")
		flags.BoolVar(&opts.deleteAll, "all", false, "Permanently delete all managed sandboxes and their disks")
		flags.BoolVar(&opts.pickDelete, "pick", false, "Select a sandbox and confirm deletion in a menu")
		flags.BoolVar(&opts.dryRun, "dry-run", false, "Show which sandboxes and disks would be deleted")
	}
	if opts.action == "sync" {
		flags.BoolVar(&opts.dryRun, "dry-run", false, "Show stale T3 links without removing them")
	}
	if opts.action == "stop" {
		flags.BoolVar(&opts.dryRun, "dry-run", false, "Show which sandbox would be stopped")
	}
	if opts.action == "sleep" {
		flags.BoolVar(&opts.dryRun, "dry-run", false, "Show which sandbox would go to sleep")
	}
}

func sandboxProvider(provider, template string) string {
	if provider != "" {
		return provider
	}
	if terraTemplate(strings.TrimSpace(template)) {
		return "cliproxy"
	}
	return "opencode"
}

func terraTemplate(value string) bool {
	name, revision, pinned := strings.Cut(value, "@")
	if name != "terrascale-agent37" {
		return false
	}
	if !pinned || revision == "" {
		return true
	}
	number, err := strconv.ParseUint(revision, 10, 32)
	return err == nil && number >= 3
}

func createPayload(opts options, extraEnv map[string]string) map[string]any {
	metadata := map[string]string{"managed_by": owner, "environment": opts.name, "provider": opts.provider}
	if opts.generatedName && !opts.noDesktop {
		metadata[desktopConfirmationKey] = "required"
	}
	env := map[string]string{"AGENT37_MANAGED_PLUGIN_COMPOSIO_ENABLED": "false"}
	maps.Copy(env, extraEnv)
	payload := map[string]any{
		"name": opts.name, "template": opts.template, "metadata": metadata, "type": "default",
		"resources":    opts.size,
		"public_ports": []int{}, "auto_sleep": true, "idle_timeout_seconds": int(sandboxIdle / time.Second), "env": env,
	}
	if opts.provider == "opencode" {
		payload["budget"] = map[string]int{"credit_micros": agent37.ManagedCreditMicros}
	}
	if opts.tailscale {
		metadata["tailnet"] = "tailscale"
	}
	return payload
}

func cliproxyEnv() (map[string]string, error) {
	result, err := runtimeCredentials(proxyURL, proxyKey)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{proxyURL, proxyKey} {
		if result[name] == "" {
			return nil, fmt.Errorf("load %s for CLIProxy", name)
		}
	}
	if model := os.Getenv(proxyModel); model != "" {
		result[proxyModel] = model
	}
	return result, nil
}

func displayProvider(instance agent37.Instance) string {
	return cmp.Or(instance.Metadata["provider"], "codex")
}

func summary(instance agent37.Instance) map[string]any {
	name := cmp.Or(instance.Metadata["environment"], instance.Name)
	row := map[string]any{"id": instance.ID, "name": name, "status": instance.Status,
		"template": instance.Template, "provider": displayProvider(instance)}
	if instance.Name != "" && instance.Name != name {
		row["cloud_name"] = instance.Name
	}
	return row
}

func jsonOutput(output io.Writer, data any, pretty bool) error {
	encoder := json.NewEncoder(output)
	if pretty {
		encoder.SetIndent("", "  ")
	}
	return encoder.Encode(data)
}

func lookup(ctx context.Context, client *agent37.Client, name string) (agent37.Instance, error) {
	instances, err := client.List(ctx)
	if err != nil {
		return agent37.Instance{}, err
	}
	return namedManaged(instances, name)
}

func namedManaged(instances []agent37.Instance, name string) (agent37.Instance, error) {
	var found agent37.Instance
	for _, instance := range instances {
		if !isManaged(instance) || instance.Metadata["environment"] != name {
			continue
		}
		if found.ID != "" {
			return agent37.Instance{}, fmt.Errorf("more than one managed instance matches %s", name)
		}
		found = instance
	}
	return found, nil
}

func isManaged(instance agent37.Instance) bool {
	return instance.Status != "deleted" && instance.Metadata["managed_by"] == owner
}

func latestManaged(instances []agent37.Instance) (agent37.Instance, error) {
	var latest agent37.Instance
	tied := false
	for _, instance := range instances {
		if !isManaged(instance) {
			continue
		}
		if instance.Created <= 0 {
			return agent37.Instance{}, errors.New("Agent37 omitted a creation time. Pass a sandbox name explicitly")
		}
		if instance.Created > latest.Created {
			latest, tied = instance, false
		} else if instance.Created == latest.Created {
			tied = true
		}
	}
	if tied {
		return agent37.Instance{}, errors.New("the newest managed sandboxes share a creation time. Pass a sandbox name explicitly")
	}
	if latest.Created != 0 &&
		(agent37.ValidateID(latest.ID) != nil || !namePattern.MatchString(latest.Metadata["environment"])) {
		return agent37.Instance{}, errors.New("Agent37 returned an invalid identity for the newest managed sandbox")
	}
	return latest, nil
}

func getOrCreate(ctx context.Context, client *agent37.Client, opts options, state string, diagnostic io.Writer) (agent37.Instance, error) {
	marker := sandboxStatePath(state, opts.name, ".pending")
	if !opts.generatedName {
		existing, err := lookup(ctx, client, opts.name)
		if err != nil || existing.ID != "" {
			if existing.ID != "" {
				_ = os.Remove(marker)
			}
			return existing, err
		}
	}
	file, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		if opts.generatedName {
			return agent37.Instance{}, errSandboxNameTaken
		}
		return agent37.Instance{}, fmt.Errorf("a create operation for %s is pending. Retry to check its state", opts.name)
	}
	if err != nil {
		return agent37.Instance{}, err
	}
	_ = file.Close()
	keepMarker := false
	defer func() {
		if !keepMarker {
			_ = os.Remove(marker)
		}
	}()
	instances, err := client.List(ctx)
	if err != nil {
		return agent37.Instance{}, err
	}
	if opts.generatedName && slices.ContainsFunc(instances, func(instance agent37.Instance) bool {
		return instance.Status != "deleted" &&
			(instance.Name == opts.name || instance.Metadata["environment"] == opts.name)
	}) {
		return agent37.Instance{}, errSandboxNameTaken
	}
	existing, err := namedManaged(instances, opts.name)
	if err != nil || existing.ID != "" {
		return existing, err
	}
	var extra map[string]string
	if opts.provider == "cliproxy" {
		extra, err = cliproxyEnv()
		if err != nil {
			return existing, err
		}
	}
	keepMarker = true // The operation may have reached Agent37 even if the reply is lost.
	notice(diagnostic, info, "Creating %s on Agent37…", opts.name)
	instance, err := client.Create(ctx, createPayload(opts, extra))
	if err != nil {
		if response, ok := errors.AsType[*agent37.Error](err); ok {
			switch response.Status {
			case 400, 401, 402, 403, 404, 422, 429:
				keepMarker = false
			}
			if response.Code == "instance_limit_reached" || response.Code == "no_capacity" {
				keepMarker = false
			}
		}
		return instance, err
	}
	if err = agent37.ValidateID(instance.ID); err != nil {
		return instance, err
	}
	keepMarker = false
	return instance, nil
}

func operate(ctx context.Context, client *agent37.Client, enrollment *peers.Enrollment, opts options, state string, output, diagnostic io.Writer, progress *launchProgress) error {
	var instance agent37.Instance
	var configured []peers.Peer
	var launcher []string
	var err error
	if opts.action == "create" {
		if opts.generatedName && !opts.noDesktop {
			launcher, err = desktopLauncher(ctx)
			if err != nil {
				return fmt.Errorf("desktop confirmation is unavailable. Use --no-desktop for headless provisioning: %w", err)
			}
		}
		if opts.tailscale {
			configured, err = peers.Configured()
			if err != nil {
				return err
			}
		}
		progress.preflightDone("Creating " + opts.name)
		instance, err = getOrCreate(ctx, client, opts, state, diagnostic)
	} else if opts.targetID != "" {
		instance, err = client.Get(ctx, opts.targetID)
		if err == nil && (instance.ID != opts.targetID || !isManaged(instance) || instance.Metadata["environment"] != opts.name) {
			return errors.New("the selected sandbox changed. Choose a target again")
		}
	} else {
		instance, err = lookup(ctx, client, opts.name)
	}
	if err != nil {
		return err
	}
	if instance.ID == "" {
		return fmt.Errorf("no managed Agent37 environment named %s", opts.name)
	}
	if err := agent37.ValidateID(instance.ID); err != nil {
		return err
	}
	tailnet := instance.Metadata["tailnet"] == "tailscale"
	if !tailnet && (opts.action == "start" || opts.action == "connect") {
		return errors.New("this sandbox was created without Tailscale. Create a new sandbox with a Tailscale-ready template")
	}
	switch opts.action {
	case "start":
		configured, err = peers.Configured()
		if err != nil {
			return err
		}
		progress.preflightDone("Starting " + opts.name + " and restoring T3 Code")
		instance, proof, err := startSandbox(ctx, client, instance, opts.name, true, opts.githubAccess, diagnostic)
		if err != nil {
			return err
		}
		progress.provisioned(true)
		synced := false
		if proof.TailnetURL != "" {
			progress.setPhase("Syncing T3 environments")
			synced = linkStartedSandbox(ctx, client, instance, opts.name, configured, proof, diagnostic)
		}
		progress.meshFinished(synced)
		progress.close()
		return showLifecycleResult(output, "Sandbox started", instance, proof.TailnetURL)
	case "stop":
		instance, err = stopSandbox(ctx, client, instance, opts.name)
		if err != nil {
			return err
		}
		if tailnet {
			cleanupSandboxLinks(ctx, client, opts.name, instance.ID, diagnostic)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return showLifecycleResult(output, "Sandbox stopped", instance, "")
	case "sleep":
		instance, err = sleepSandbox(ctx, client, instance, opts.name)
		if err != nil {
			return err
		}
		return showLifecycleResult(output, "Sandbox sleeping", instance, "")
	case "terminal":
		link, err := client.Terminal(ctx, instance.ID)
		if err != nil {
			return err
		}
		if _, terminal := ttyOutput(output); terminal {
			return printDetails(output, "Agent37 terminal", detail{"Sandbox", opts.name}, detail{"Open", link})
		}
		_, err = fmt.Fprintln(output, link)
		return err
	case "prepare":
		return prepare(ctx, client, instance, opts.name, output, diagnostic)
	}
	_, interactiveOutput := ttyOutput(output)
	var proof setupProof
	if opts.action == "create" {
		progress.allocated()
		progress.setPhase("Setting up T3 Code and Tailscale")
		proof, err = prepareCreated(ctx, client, enrollment, instance, opts, diagnostic)
		if err != nil {
			return err
		}
		progress.provisioned(true)
		if launcher != nil {
			progress.setPhase("Waiting for T3 Code confirmation")
			err := handOffPairing(ctx, client, instance, opts.name, proof, launcher, diagnostic)
			progress.pairedDesktop(err == nil)
			if err != nil {
				return fmt.Errorf("%s is running. Run t3-agent37 link %s to finish pairing: %w", opts.name, opts.name, err)
			}
			proof.DesktopConfirmed = true
		}
		progress.setPhase("Syncing T3 environments")
		progress.meshFinished(linkStartedSandbox(ctx, client, instance, opts.name, configured, proof, diagnostic))
		progress.close()
		if !interactiveOutput {
			fmt.Fprintf(output, "Cloud T3 ready (%s, %s).\n", instance.ID, proof.EnvironmentID)
		}
	} else {
		if instance.Status == "stopped" {
			return errors.New("the sandbox is stopped. Run t3-agent37 up first")
		}
		proof, err = bootstrap(ctx, client, nil, instance, opts.name, false, false, false, diagnostic)
		if err != nil {
			return err
		}
	}
	if proof.TailnetURL == "" {
		return errors.New("Agent37 did not provide the sandbox's Tailscale address")
	}
	var outputErr error
	if interactiveOutput {
		if opts.action == "create" {
			instance.Status = "running"
			outputErr = printSandboxResult(output, "Sandbox ready", instance, proof.TailnetURL, detail{"Environment ID", proof.EnvironmentID})
		} else {
			outputErr = printDetails(output, "T3 Code connection", detail{"Sandbox", opts.name}, detail{"Tailscale", proof.TailnetURL})
		}
	} else {
		_, outputErr = fmt.Fprintln(output, "T3 Code on Tailscale", proof.TailnetURL)
	}
	if outputErr != nil {
		return outputErr
	}
	if opts.pair {
		pairing, err := pairLink(ctx, client, instance.ID, proof.TailnetURL, "t3-agent37")
		if err != nil {
			return err
		}
		if interactiveOutput {
			return printDetails(output, "T3 Code pairing · expires in 5 minutes", detail{"Open", pairing})
		}
		_, err = fmt.Fprintf(output, "Pair in T3 Code (expires in 5 minutes): %s\n", pairing)
		return err
	}
	return nil
}

func run(ctx context.Context, args []string, output, diagnostic io.Writer) error {
	root, err := newCLI(args, output, diagnostic, func(ctx context.Context, opts options) error {
		return runCommand(ctx, opts, output, diagnostic)
	})
	if err != nil {
		return err
	}
	return root.ExecuteContext(ctx)
}

func runCommand(ctx context.Context, opts options, output, diagnostic io.Writer) error {
	var err error
	if opts.action == "config" {
		if opts.showSize || opts.cpu != nil || opts.ram != nil {
			return configureSandboxSize(ctx, opts, output)
		}
		return editConfig(ctx, diagnostic)
	}
	if opts.action == "setup" {
		if opts.repairSetup {
			return repairTailscaleSetup(ctx, opts.dryRun, output)
		}
		return setupWorkstation(ctx, opts, output, diagnostic)
	}
	var state string
	if opts.generatedName {
		state, err = statePath()
		if err != nil {
			return err
		}
	}
	if opts.batchSize > 0 && opts.dryRun {
		return previewBatch(ctx, opts, state, output)
	}
	if opts.dryRun && opts.action == "create" {
		if opts.generatedName {
			opts.name, err = previewSandboxName(ctx, state, opts.namePrefix)
			if err != nil {
				return err
			}
		}
		if _, terminal := ttyOutput(output); terminal {
			return printDetails(output, "Preview · create sandbox",
				detail{"Sandbox", opts.name}, detail{"Template", opts.template}, detail{"Provider", providerLabel(opts.provider)},
				detail{"Resources", opts.size.String()},
				detail{"Auto-sleep", fmt.Sprintf("%d minutes", sandboxIdle/time.Minute)}, detail{"Network", "Private Tailscale"})
		}
		return jsonOutput(output, createPayload(opts, nil), true)
	}
	if opts.pickDelete && !terminalAvailable(diagnostic) {
		return errors.New("interactive deletion requires a terminal. Use down <name> to select a sandbox")
	}
	var progress *launchProgress
	if opts.action == "create" || opts.action == "start" {
		progress = startLaunchProgress(diagnostic, opts)
		defer progress.close()
		diagnostic = progress
	}
	key, err := agent37APIKey()
	if err != nil {
		return err
	}
	client := agent37.New(key)
	if opts.batchSize > 0 {
		return runBatch(ctx, client, verifyTailscaleEnrollment, opts, state, output, progress)
	}
	var enrollment *peers.Enrollment
	if opts.action == "create" {
		enrollment, err = verifyTailscaleEnrollment(ctx)
		if err != nil {
			return err
		}
		progress.setPhase("Preparing sandbox launch")
	}
	if opts.action == "sync" {
		return syncSandboxLinks(ctx, client, opts.dryRun, output, diagnostic)
	}
	if opts.action == "link" {
		return linkSandboxes(ctx, client, opts, output, diagnostic)
	}
	if opts.deleteAll {
		return runDeleteAll(ctx, client, opts.dryRun, output, diagnostic)
	}
	if opts.action == "down" && !opts.pickDelete {
		return runRecordedDown(ctx, client, opts, output, diagnostic)
	}
	if opts.action == "list" || opts.action == "sleep" || opts.dryRun || opts.pickDelete {
		instances, err := client.List(ctx)
		if err != nil {
			return err
		}
		if opts.action == "list" {
			return printSandboxList(output, instances, opts.listJSON)
		}
		var instance agent37.Instance
		switch {
		case opts.pickDelete:
			instance, err = pickSandboxToDelete(ctx, instances, opts.dryRun, diagnostic)
			if err == nil && instance.ID == "" {
				return nil
			}
		case opts.latest:
			instance, err = latestManaged(instances)
		default:
			instance, err = namedManaged(instances, opts.name)
		}
		if err != nil {
			return err
		}
		if instance.ID == "" {
			if opts.latest {
				return showAbsentSandbox(output, "", opts.dryRun)
			}
			return fmt.Errorf("no managed Agent37 environment named %s", opts.name)
		}
		if opts.dryRun {
			return previewSandboxLifecycle(output, instance, opts.action)
		}
		opts.name = instance.Metadata["environment"]
		opts.targetID = instance.ID
		if opts.pickDelete {
			return runRecordedDown(ctx, client, opts, output, diagnostic)
		}
	}
	if state == "" {
		state, err = statePath()
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		return err
	}
	for {
		if opts.generatedName {
			progress.setPhase("Selecting a sandbox name")
		}
		lock, err := lockSandbox(ctx, client, state, &opts)
		if err != nil {
			return err
		}
		err = operate(ctx, client, enrollment, opts, state, output, diagnostic, progress)
		unlockErr := lock.Unlock()
		if opts.generatedName && errors.Is(err, errSandboxNameTaken) && unlockErr == nil {
			continue
		}
		return cmp.Or(err, unlockErr)
	}
}

func lockSandbox(ctx context.Context, client *agent37.Client, state string, opts *options) (*flock.Flock, error) {
	if opts.generatedName {
		instances, err := client.List(ctx)
		if err != nil {
			return nil, err
		}
		name, lock, err := reserveSandboxName(ctx, state, opts.namePrefix, instances)
		opts.name = name
		return lock, err
	}
	lock := sandboxLock(state, opts.name)
	locked, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err == nil && !locked {
		err = errors.New("could not acquire the sandbox lifecycle lock")
	}
	return lock, err
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		for line := range strings.SplitSeq(err.Error(), "\n") {
			if strings.TrimSpace(line) != "" && strings.TrimSpace(line) != context.Canceled.Error() {
				notice(os.Stderr, failure, "t3-agent37: %s", line)
			}
		}
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		os.Exit(1)
	}
}
