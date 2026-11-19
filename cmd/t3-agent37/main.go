package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	"github.com/MarioGK/t3-agent37/internal/peers"
	"github.com/gofrs/flock"
	flag "github.com/spf13/pflag"
)

const owner = "t3-agent37"

var version = "dev"
var namePattern = regexp.MustCompile("^[a-z0-9][a-z0-9-]{0,42}$")
var templates = map[string]string{
	"codex": "agent37-codex", "claude": "agent37-claude-code",
	"opencode": "agent37-opencode", "cliproxy": "agent37-opencode",
}

type options struct {
	action, name, provider, template string
	namePrefix                       string
	targetID                         string
	port                             int
	yes, dryRun, pair, tailscale     bool
	deleteSandbox                    bool
	generatedName                    bool
	latest                           bool
	pickDelete                       bool
}

func parse(args []string, output io.Writer) (options, error) {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(output, "Usage: t3-agent37 <create|list|ls|up|down|delete|connect|prepare|terminal|config> [name] [flags]\n       t3-agent37 --version")
		return options{}, flag.ErrHelp
	}
	if len(args) == 1 && (args[0] == "--version" || args[0] == "version") {
		fmt.Fprintln(output, version)
		return options{}, flag.ErrHelp
	}
	opts := options{action: args[0]}
	switch opts.action {
	case "down":
		opts.action = "stop"
	case "ls":
		opts.action = "list"
	case "create", "up", "delete", "connect", "start", "stop", "prepare", "terminal", "list", "config":
	default:
		return opts, fmt.Errorf("unknown command %q", opts.action)
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(output)
	flags.Usage = func() {
		if opts.action == "list" {
			fmt.Fprintf(output, "Usage: t3-agent37 %s\nList managed Agent37 sandboxes.\n", args[0])
			return
		}
		if opts.action == "config" {
			fmt.Fprintln(output, "Usage: t3-agent37 config\nEdit sandbox defaults in the terminal.")
			return
		}
		if opts.action == "delete" {
			fmt.Fprintln(output, "Usage: t3-agent37 delete [name] [flags]")
			fmt.Fprintln(output, "Choose a sandbox to delete or pass its name.")
		} else {
			fmt.Fprintf(output, "Usage: t3-agent37 %s [name] [flags]\n", args[0])
		}
		if opts.action == "create" {
			fmt.Fprintln(output, "Omit the name to use the next numbered name.")
		}
		if opts.action == "up" {
			fmt.Fprintln(output, "Run without a name to provision a new sandbox and open T3 pairing.")
			fmt.Fprintln(output, "Pass a sandbox name to start it.")
		}
		if args[0] == "down" {
			fmt.Fprintln(output, "Omit the name to permanently delete the newest managed sandbox and its disk.")
			fmt.Fprintln(output, "Pass a sandbox name to stop it.")
		}
		flags.PrintDefaults()
	}
	var noTailscale bool
	if opts.action == "create" {
		flags.BoolVar(&opts.yes, "yes", false, "Authorize paid provisioning")
	}
	if opts.action == "create" || opts.action == "up" {
		flags.StringVar(&opts.provider, "provider", "", "Provider: opencode, cliproxy, codex or claude")
		flags.StringVar(&opts.template, "template", "", "Agent37 template, optionally pinned by revision")
		flags.BoolVar(&opts.dryRun, "dry-run", false, "Print the request without using the API")
		flags.BoolVar(&opts.tailscale, "tailscale", false, "Enroll the sandbox in Tailscale")
		flags.BoolVar(&noTailscale, "no-tailscale", false, "Skip Tailscale enrollment")
	}
	if opts.action == "connect" {
		flags.BoolVar(&opts.pair, "pair", false, "Open a one-time T3 pairing link")
	}
	if opts.action == "stop" {
		flags.BoolVar(&opts.deleteSandbox, "delete", false, "Permanently delete the managed sandbox and its disk")
		flags.BoolVar(&opts.yes, "yes", false, "Confirm permanent deletion")
	}
	if args[0] == "down" || opts.action == "delete" {
		flags.BoolVar(&opts.dryRun, "dry-run", false, "Show which sandbox would be deleted")
	}
	if opts.action == "create" || opts.action == "up" || opts.action == "connect" {
		flags.IntVar(&opts.port, "port", 0, "Local relay port")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return opts, err
	}
	if opts.action == "config" {
		if flags.NArg() != 0 {
			return opts, errors.New("config does not take any arguments")
		}
		return opts, nil
	}
	if opts.action == "up" {
		if flags.NArg() == 0 {
			opts.action = "create"
			opts.yes = !opts.dryRun
		} else {
			if flags.NFlag() != 0 {
				return opts, errors.New("up accepts provisioning flags only when you omit the sandbox name")
			}
			opts.action = "start"
		}
	}
	if opts.action == "list" && flags.NArg() == 0 {
		return opts, nil
	}
	if opts.action == "stop" && opts.yes != opts.deleteSandbox {
		return opts, errors.New("use --delete and --yes together to permanently delete the sandbox")
	}
	switch {
	case opts.action == "delete" && flags.NArg() == 0:
		opts.pickDelete = true
	case args[0] == "down" && flags.NArg() == 0:
		if flags.Changed("delete") && !opts.deleteSandbox {
			return opts, errors.New("specify a sandbox name to stop it and keep its disk")
		}
		opts.latest = true
		opts.deleteSandbox = true
	case opts.action == "create" && flags.NArg() == 0:
		opts.generatedName = true
	case opts.action != "list" && flags.NArg() == 1 && namePattern.MatchString(flags.Arg(0)):
		opts.name = flags.Arg(0)
	default:
		return opts, errors.New("expected a name with 1 to 43 lowercase letters, digits, or hyphens")
	}
	if opts.action == "stop" && opts.dryRun && !opts.latest {
		return opts, errors.New("omit the name to preview which sandbox bare down would delete")
	}
	if opts.action == "delete" {
		opts.action = "stop"
		opts.deleteSandbox = true
	}
	if flags.Changed("port") && (opts.port < 1024 || opts.port > 65535) {
		return opts, errors.New("port must be between 1024 and 65535")
	}
	if opts.action == "create" {
		if opts.yes == opts.dryRun {
			return opts, errors.New("choose exactly one of --yes and --dry-run")
		}
		config, err := loadWorkstationConfig()
		if err != nil {
			return opts, err
		}
		opts.namePrefix = config.NamePrefix
		if !flags.Changed("template") {
			opts.template = config.Template
		}
		if !flags.Changed("provider") {
			opts.provider = config.Provider
		}
		terra := terraTemplate(opts.template)
		if opts.provider == "" {
			opts.provider = "opencode"
			if terra {
				opts.provider = "cliproxy"
			}
		}
		template, supported := templates[opts.provider]
		if !supported {
			return opts, fmt.Errorf("unknown provider %q", opts.provider)
		}
		if opts.template == "" {
			opts.template = template
		}
		if flags.Changed("tailscale") && noTailscale {
			return opts, errors.New("choose --tailscale or --no-tailscale")
		}
		if !flags.Changed("tailscale") {
			opts.tailscale = terra
			if config.Tailscale != nil {
				opts.tailscale = *config.Tailscale
			}
			opts.tailscale = opts.tailscale && !noTailscale
		}
	}
	return opts, nil
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
	env := map[string]string{"AGENT37_MANAGED_PLUGIN_COMPOSIO_ENABLED": "false"}
	for key, value := range extraEnv {
		env[key] = value
	}
	payload := map[string]any{
		"name": opts.name, "template": opts.template, "metadata": metadata, "type": "default",
		"resources":    map[string]int{"cpu": 4, "memory": 8},
		"public_ports": []int{}, "auto_sleep": true, "idle_timeout_seconds": 900, "env": env,
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
	result := make(map[string]string)
	for _, name := range []string{"T3_AGENT37_CLIPROXY_URL", "T3_AGENT37_CLIPROXY_KEY"} {
		value := os.Getenv(name)
		if value == "" {
			return nil, fmt.Errorf("load %s for CLIProxy", name)
		}
		result[name] = value
	}
	if model := os.Getenv("T3_AGENT37_CLIPROXY_MODEL"); model != "" {
		result["T3_AGENT37_CLIPROXY_MODEL"] = model
	}
	return result, nil
}

func displayProvider(instance agent37.Instance) string {
	if provider := instance.Metadata["provider"]; provider != "" {
		return provider
	}
	return "codex"
}

func summary(instance agent37.Instance) map[string]any {
	return map[string]any{"id": instance.ID, "name": instance.Name, "status": instance.Status,
		"template": instance.Template, "provider": displayProvider(instance)}
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
			return agent37.Instance{}, errors.New("Agent37 omitted a creation time. Use down <name> --delete --yes to select the sandbox")
		}
		if instance.Created > latest.Created {
			latest, tied = instance, false
		} else if instance.Created == latest.Created {
			tied = true
		}
	}
	if tied {
		return agent37.Instance{}, errors.New("the newest managed sandboxes share a creation time. Use down <name> --delete --yes to select the sandbox")
	}
	if latest.Created != 0 &&
		(agent37.ValidateID(latest.ID) != nil || !namePattern.MatchString(latest.Metadata["environment"])) {
		return agent37.Instance{}, errors.New("Agent37 returned an invalid identity for the newest managed sandbox")
	}
	return latest, nil
}

func getOrCreate(ctx context.Context, client *agent37.Client, opts options, state string) (agent37.Instance, error) {
	marker := filepath.Join(state, opts.name+".pending")
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
	if os.IsExist(err) {
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
	existing, err := lookup(ctx, client, opts.name)
	if existing.ID != "" && opts.generatedName {
		return existing, fmt.Errorf("generated name %s is already in use. Run create again", opts.name)
	}
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
	if opts.tailscale {
		if _, _, err = peers.TailscaleCredentials(); err != nil {
			return existing, err
		}
	}
	keepMarker = true // The operation may have reached Agent37 even if the reply is lost.
	instance, err := client.Create(ctx, createPayload(opts, extra))
	if err != nil {
		var response *agent37.Error
		if errors.As(err, &response) {
			switch response.Status {
			case 400, 401, 402, 403, 404, 422, 429:
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

type relayConfig struct {
	instance     string
	port, remote int
	pairing      string
}

func operate(ctx context.Context, client *agent37.Client, opts options, state string, output, diagnostic io.Writer) (*relayConfig, error) {
	var instance agent37.Instance
	var configured []peers.Peer
	var err error
	if opts.action == "create" {
		if opts.tailscale {
			configured, err = peers.Configured()
			if err != nil {
				return nil, err
			}
		}
		instance, err = getOrCreate(ctx, client, opts, state)
	} else if opts.targetID != "" {
		instance, err = client.Get(ctx, opts.targetID)
		if err == nil && (instance.ID != opts.targetID || !isManaged(instance) || instance.Metadata["environment"] != opts.name) {
			return nil, errors.New("the selected sandbox changed. Choose a target again")
		}
	} else {
		instance, err = lookup(ctx, client, opts.name)
	}
	if err != nil {
		return nil, err
	}
	if instance.ID == "" {
		return nil, fmt.Errorf("no managed Agent37 environment named %s", opts.name)
	}
	if err := agent37.ValidateID(instance.ID); err != nil {
		return nil, err
	}
	tailnet := instance.Metadata["tailnet"] == "tailscale"
	provider := displayProvider(instance)
	switch opts.action {
	case "start", "stop":
		if opts.action == "start" && tailnet {
			configured, err = peers.Configured()
			if err != nil {
				return nil, err
			}
		}
		instance, err = lifecycle(ctx, client, instance, opts.name, opts.action, configured, diagnostic)
		if err != nil {
			return nil, err
		}
		if opts.deleteSandbox {
			if err := client.Delete(ctx, instance.ID); err != nil {
				return nil, err
			}
			instance.Status = "deleted"
			if err := releasePort(ctx, state, instance.ID); err != nil {
				fmt.Fprintf(diagnostic, "Sandbox deleted. Could not release its saved relay port: %v\n", err)
			}
		}
		return nil, jsonOutput(output, summary(instance), false)
	case "terminal":
		link, err := client.Terminal(ctx, instance.ID)
		if err == nil {
			fmt.Fprintln(output, link)
		}
		return nil, err
	case "prepare":
		return nil, prepare(ctx, client, instance, opts.name, output, diagnostic)
	}
	if opts.action == "create" && (instance.Template != opts.template || provider != opts.provider || tailnet != opts.tailscale) {
		return nil, errors.New("this name already uses a different template, provider, or Tailscale setting")
	}
	port, err := assignedPort(ctx, state, instance.ID, opts.port)
	if err != nil {
		return nil, err
	}
	relay := &relayConfig{instance: instance.ID, port: port}
	if opts.action == "create" {
		proof, err := prepareCreated(ctx, client, instance, opts.name, configured, diagnostic)
		if err != nil {
			return nil, err
		}
		relay.remote = proof.Port
		fmt.Fprintf(output, "Cloud T3 ready (%s, %s).\n", instance.ID, proof.EnvironmentID)
		if !proof.ProviderReady {
			fmt.Fprintf(output, "%s needs sign-in. Run t3-agent37 terminal %s.\n", provider, opts.name)
		}
	} else {
		if instance.Status == "stopped" {
			return nil, errors.New("the sandbox is stopped. Run t3-agent37 up first")
		}
		_, relay.remote, err = waitForT3(ctx, client, instance.ID, "", 0)
		if err != nil {
			return nil, err
		}
	}
	if opts.action == "create" || opts.pair {
		relay.pairing, err = pairLink(ctx, client, instance.ID, port)
		if err != nil {
			return nil, err
		}
	}
	return relay, nil
}

func run(ctx context.Context, args []string, output, diagnostic io.Writer) error {
	opts, err := parse(args, output)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if opts.action == "config" {
		return editConfig(ctx, diagnostic)
	}
	var state string
	if opts.generatedName {
		state, err = statePath()
		if err != nil {
			return err
		}
	}
	if opts.dryRun && opts.action == "create" {
		if opts.generatedName {
			opts.name, err = previewSandboxName(ctx, state, opts.namePrefix)
			if err != nil {
				return err
			}
		}
		return jsonOutput(output, createPayload(opts, nil), true)
	}
	if opts.pickDelete && !terminalAvailable(diagnostic) {
		return errors.New("interactive deletion requires a terminal. Use delete <name> to select a sandbox")
	}
	key, err := agent37APIKey()
	if err != nil {
		return err
	}
	client := agent37.New(key)
	if opts.action == "list" || opts.latest || opts.dryRun || opts.pickDelete {
		instances, err := client.List(ctx)
		if err != nil {
			return err
		}
		if opts.action == "list" {
			rows := make([]map[string]any, 0)
			for _, instance := range instances {
				if isManaged(instance) {
					rows = append(rows, summary(instance))
				}
			}
			return jsonOutput(output, rows, true)
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
			if !opts.latest {
				return fmt.Errorf("no managed Agent37 environment named %s", opts.name)
			}
			fmt.Fprintln(output, "No managed Agent37 sandboxes.")
			return nil
		}
		if opts.dryRun {
			target := summary(instance)
			target["action"] = "delete"
			return jsonOutput(output, target, true)
		}
		opts.name = instance.Metadata["environment"]
		opts.targetID = instance.ID
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
	if opts.generatedName {
		instances, err := client.List(ctx)
		if err != nil {
			return err
		}
		opts.name, err = reserveSandboxName(ctx, state, opts.namePrefix, instances)
		if err != nil {
			return err
		}
	}
	lock := flock.New(filepath.Join(state, opts.name+".lifecycle.lock"))
	locked, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		return err
	}
	if !locked {
		return errors.New("could not acquire the sandbox lifecycle lock")
	}
	if opts.generatedName {
		fmt.Fprintf(output, "Creating sandbox %s.\n", opts.name)
	}
	relay, err := operate(ctx, client, opts, state, output, diagnostic)
	unlockErr := lock.Unlock()
	if err != nil {
		return err
	}
	if unlockErr != nil {
		return unlockErr
	}
	if relay != nil {
		return serve(ctx, client, relay, output)
	}
	return nil
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "t3-agent37:", err)
		os.Exit(1)
	}
}
