package main

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
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

var nameAdjectives = [...]string{"brisk", "calm", "clear", "crisp", "keen", "quiet", "steady", "swift"}
var nameAnimals = [...]string{"badger", "falcon", "fox", "heron", "lynx", "otter", "raven", "tern"}

func randomSandboxName() string {
	adjective := nameAdjectives[rand.IntN(len(nameAdjectives))]
	animal := nameAnimals[rand.IntN(len(nameAnimals))]
	return adjective + "-" + animal + "-" + strings.ToLower(cryptorand.Text()[:10])
}

type options struct {
	action, name, provider, template string
	port                             int
	yes, dryRun, pair, tailscale     bool
	deleteSandbox                    bool
	generatedName                    bool
}

func parse(args []string, output io.Writer) (options, error) {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(output, "Usage: t3-agent37 <create|list|up|down|connect|prepare|terminal> [name] [flags]\n       t3-agent37 --version")
		return options{}, flag.ErrHelp
	}
	if len(args) == 1 && (args[0] == "--version" || args[0] == "version") {
		fmt.Fprintln(output, version)
		return options{}, flag.ErrHelp
	}
	opts := options{action: args[0]}
	switch opts.action {
	case "up":
		opts.action = "start"
	case "down":
		opts.action = "stop"
	case "create", "connect", "start", "stop", "prepare", "terminal", "list":
	default:
		return opts, fmt.Errorf("unknown command %q", opts.action)
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(output)
	flags.Usage = func() {
		fmt.Fprintf(output, "Usage: t3-agent37 %s [name] [flags]\n", args[0])
		if opts.action == "create" {
			fmt.Fprintln(output, "Omit the name to generate one.")
		}
		flags.PrintDefaults()
	}
	var noTailscale bool
	if opts.action == "create" {
		flags.StringVar(&opts.provider, "provider", "", "Provider: opencode, cliproxy, codex or claude")
		flags.StringVar(&opts.template, "template", "", "Agent37 template, optionally pinned by revision")
		flags.BoolVar(&opts.yes, "yes", false, "Authorize paid provisioning")
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
	if opts.action == "create" || opts.action == "connect" {
		flags.IntVar(&opts.port, "port", 0, "Local relay port")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return opts, err
	}
	if opts.action == "list" && flags.NArg() == 0 {
		return opts, nil
	}
	switch {
	case opts.action == "create" && flags.NArg() == 0:
		opts.generatedName = true
	case opts.action != "list" && flags.NArg() == 1 && namePattern.MatchString(flags.Arg(0)):
		opts.name = flags.Arg(0)
	default:
		return opts, errors.New("expected a name with 1 to 43 lowercase letters, digits, or hyphens")
	}
	if opts.action == "stop" && opts.yes != opts.deleteSandbox {
		return opts, errors.New("use --delete and --yes together to permanently delete the sandbox")
	}
	if flags.Changed("port") && (opts.port < 1024 || opts.port > 65535) {
		return opts, errors.New("port must be between 1024 and 65535")
	}
	if opts.action == "create" {
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
			opts.tailscale = terra && !noTailscale
		}
		if opts.yes == opts.dryRun {
			return opts, errors.New("choose exactly one of --yes and --dry-run")
		}
		if opts.generatedName {
			opts.name = randomSandboxName()
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
	var found agent37.Instance
	for _, instance := range instances {
		if instance.Status == "deleted" || instance.Metadata["managed_by"] != owner ||
			instance.Metadata["environment"] != name {
			continue
		}
		if found.ID != "" {
			return agent37.Instance{}, fmt.Errorf("more than one managed instance matches %s", name)
		}
		found = instance
	}
	return found, nil
}

func getOrCreate(ctx context.Context, client *agent37.Client, opts options, state string) (agent37.Instance, error) {
	marker := filepath.Join(state, opts.name+".pending")
	existing, err := lookup(ctx, client, opts.name)
	if err != nil || existing.ID != "" {
		if existing.ID != "" {
			if opts.generatedName {
				return existing, fmt.Errorf("generated name %s is already in use. Run create again", opts.name)
			}
			_ = os.Remove(marker)
		}
		return existing, err
	}
	file, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if os.IsExist(err) {
		return existing, fmt.Errorf("a create operation for %s is pending; retry to check its state", opts.name)
	}
	if err != nil {
		return existing, err
	}
	_ = file.Close()
	keepMarker := false
	defer func() {
		if !keepMarker {
			_ = os.Remove(marker)
		}
	}()
	existing, err = lookup(ctx, client, opts.name)
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
	if opts.dryRun {
		return jsonOutput(output, createPayload(opts, nil), true)
	}
	key := os.Getenv("AGENT37_API_KEY")
	if key == "" {
		return errors.New("load AGENT37_API_KEY into the environment")
	}
	client := agent37.New(key)
	if opts.action == "list" {
		instances, err := client.List(ctx)
		if err != nil {
			return err
		}
		rows := make([]map[string]any, 0)
		for _, instance := range instances {
			if instance.Status != "deleted" && instance.Metadata["managed_by"] == owner {
				rows = append(rows, summary(instance))
			}
		}
		return jsonOutput(output, rows, true)
	}
	state, err := stateDir()
	if err != nil {
		return err
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
