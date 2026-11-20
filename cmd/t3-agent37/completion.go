package main

import (
	"cmp"
	"context"
	_ "embed"
	"io"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	"github.com/spf13/cobra"
)

type cliCommand struct {
	name, action, summary string
	sandbox               bool
}

var commands = []cliCommand{
	{name: "setup", summary: "Set up Agent37, Tailscale, and the model provider"},
	{name: "config", summary: "Edit sandbox defaults"},
	{name: "create", summary: "Create a sandbox", sandbox: true},
	{name: "up", summary: "Create a sandbox or start one by name", sandbox: true},
	{name: "start", summary: "Start an existing sandbox", sandbox: true},
	{name: "down", action: "stop", summary: "Delete the newest sandbox or stop one by name", sandbox: true},
	{name: "stop", summary: "Stop a sandbox and keep its disk", sandbox: true},
	{name: "delete", summary: "Permanently delete a sandbox", sandbox: true},
	{name: "del", action: "delete", summary: "Permanently delete a sandbox", sandbox: true},
	{name: "list", summary: "List managed sandboxes"},
	{name: "ls", action: "list", summary: "List managed sandboxes"},
	{name: "sync", summary: "Link T3 environments and remove stale links"},
	{name: "link", summary: "Pair existing sandboxes with T3 Code", sandbox: true},
	{name: "connect", summary: "Show a sandbox's T3 Code address", sandbox: true},
	{name: "prepare", summary: "Refresh a sandbox's model catalog", sandbox: true},
	{name: "terminal", summary: "Show the Agent37 terminal link", sandbox: true},
}

//go:embed completion.bash
var bashCompletion string

func findCommand(name string) (cliCommand, bool) {
	for _, command := range commands {
		if command.name == name {
			return command, true
		}
	}
	return cliCommand{}, false
}

func runCompletion(ctx context.Context, args []string, output, diagnostic io.Writer) error {
	// Operational commands keep their parser. Cobra uses the same flags to
	// generate scripts and answer completion requests without running commands.
	root := &cobra.Command{
		Use: owner, SilenceErrors: true, SilenceUsage: true,
		ValidArgsFunction: cobra.NoFileCompletions,
	}
	root.SetOut(output)
	root.SetErr(diagnostic)
	root.SetArgs(args)
	root.Flags().Bool("version", false, "Print the CLI version")
	root.AddCommand(completionCommand(root))
	root.AddCommand(&cobra.Command{
		Use: "version", Short: "Print the CLI version",
		Run: func(*cobra.Command, []string) {}, ValidArgsFunction: cobra.NoFileCompletions,
	})
	for _, spec := range commands {
		command := &cobra.Command{
			Use: spec.name, Short: spec.summary,
			Run: func(*cobra.Command, []string) {}, ValidArgsFunction: cobra.NoFileCompletions,
		}
		if spec.sandbox {
			command.Use += " [name]"
			command.ValidArgsFunction = completeSandboxNames
		}
		root.AddCommand(command)
		// A name selects the start form of up, which has no provisioning flags.
		if spec.name == "up" && len(args) > 3 && args[1] == "up" && namePattern.MatchString(args[2]) {
			addCommandFlags(command.Flags(), &options{action: "start"})
			continue
		}
		opts := options{action: cmp.Or(spec.action, spec.name)}
		addCommandFlags(command.Flags(), &opts)
		switch opts.action {
		case "setup":
			command.MarkFlagsMutuallyExclusive("check", "verify", "repair-tailscale")
			command.MarkFlagsMutuallyExclusive("dry-run", "check")
			command.MarkFlagsMutuallyExclusive("dry-run", "verify")
		case "create":
			command.MarkFlagsMutuallyExclusive("yes", "dry-run")
		}
		if opts.action == "create" || opts.action == "up" {
			if err := command.RegisterFlagCompletionFunc("provider", cobra.FixedCompletions(
				slices.Sorted(maps.Keys(providers)), cobra.ShellCompDirectiveNoFileComp)); err != nil {
				return err
			}
			if err := command.RegisterFlagCompletionFunc("template", completeTemplates); err != nil {
				return err
			}
		}
	}
	return root.ExecuteContext(ctx)
}

func completionCommand(root *cobra.Command) *cobra.Command {
	command := &cobra.Command{Use: "completion", Short: "Generate shell completion scripts"}
	command.SetHelpFunc(func(cmd *cobra.Command, _ []string) {
		if cmd == command {
			_ = printDetails(cmd.OutOrStdout(), "Shell completions", detail{"Usage", "t3-agent37 completion <shell>"},
				detail{"Shells", "bash, fish, zsh, powershell"})
			return
		}
		_ = printDetails(cmd.OutOrStdout(), "Generate "+cmd.Name()+" completions", detail{"Usage", cmd.CommandPath()})
	})
	for _, shell := range []struct {
		name     string
		generate func(io.Writer) error
	}{
		{name: "bash", generate: func(out io.Writer) error {
			_, err := io.WriteString(out, bashCompletion)
			return err
		}},
		{name: "fish", generate: func(out io.Writer) error { return root.GenFishCompletion(out, true) }},
		{name: "zsh", generate: root.GenZshCompletion},
		{name: "powershell", generate: root.GenPowerShellCompletionWithDesc},
	} {
		command.AddCommand(&cobra.Command{
			Use: shell.name, Short: "Generate " + shell.name + " completion",
			Args: cobra.NoArgs, ValidArgsFunction: cobra.NoFileCompletions,
			RunE: func(cmd *cobra.Command, _ []string) error { return shell.generate(cmd.OutOrStdout()) },
		})
	}
	return command
}

const completionAPIDeadline = 1200 * time.Millisecond

func completeSandboxNames(command *cobra.Command, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
	directive := cobra.ShellCompDirectiveNoFileComp
	if len(args) > 0 || (command.Name() == "up" && command.Flags().NFlag() > 0 &&
		!(command.Flags().NFlag() == 1 && command.Flags().Changed("github"))) {
		return nil, directive
	}
	ctx, cancel := context.WithTimeout(command.Context(), completionAPIDeadline)
	defer cancel()
	var names []string
	if command.Name() == "create" {
		config, configErr := loadWorkstationConfig()
		state, stateErr := statePath()
		if configErr == nil && stateErr == nil {
			name, err := previewSandboxName(ctx, state, config.NamePrefix)
			if err == nil && strings.HasPrefix(name, prefix) {
				names = append(names, name+"\tNext available name")
			}
		}
		return names, directive
	}
	key, err := agent37APIKey()
	if err != nil {
		return nil, directive
	}
	instances, err := agent37.New(key).List(ctx)
	if err != nil {
		return nil, directive
	}
	seen := make(map[string]bool)
	for _, instance := range instances {
		name := instance.Metadata["environment"]
		if isManaged(instance) && namePattern.MatchString(name) && strings.HasPrefix(name, prefix) && !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	slices.Sort(names)
	return names, directive
}

var templateChoice = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}(@[a-zA-Z0-9][a-zA-Z0-9._-]*)?$`)

func completeTemplates(command *cobra.Command, _ []string, prefix string) ([]string, cobra.ShellCompDirective) {
	names := make(map[string]struct{})
	add := func(candidate string) {
		if strings.HasPrefix(candidate, prefix) && templateChoice.MatchString(candidate) {
			names[candidate] = struct{}{}
		}
	}
	if config, err := loadWorkstationConfig(); err == nil {
		add(config.Template)
	}
	if key, err := agent37APIKey(); err == nil {
		ctx, cancel := context.WithTimeout(command.Context(), completionAPIDeadline)
		defer cancel()
		client := agent37.New(key)
		if name, _, pinned := strings.Cut(prefix, "@"); pinned {
			if templateChoice.MatchString(name) {
				if template, err := client.GetTemplate(ctx, name); err == nil && template.Scope == "workspace" {
					for _, revision := range template.Revisions {
						if revision.Number > 0 {
							add(name + "@" + strconv.Itoa(revision.Number))
						}
					}
				}
			}
		} else if templates, err := client.ListTemplates(ctx); err == nil {
			for _, template := range templates {
				if template.Scope == "workspace" {
					add(template.Name)
					if template.Revision > 0 {
						add(template.Name + "@" + strconv.Itoa(template.Revision))
					}
				}
			}
		}
	}
	return slices.Sorted(maps.Keys(names)), cobra.ShellCompDirectiveNoFileComp
}
