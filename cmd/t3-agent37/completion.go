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
	name, action, summary, group string
	sandbox                      bool
}

var commands = []cliCommand{
	{name: "setup", group: "workstation", summary: "Set up Agent37, Tailscale, and the model provider"},
	{name: "config", group: "workstation", summary: "Edit sandbox defaults"},
	{name: "create", group: "sandbox", summary: "Create a sandbox", sandbox: true},
	{name: "up", group: "sandbox", summary: "Create sandboxes or start one by name", sandbox: true},
	{name: "start", group: "sandbox", summary: "Start an existing sandbox", sandbox: true},
	{name: "down", group: "sandbox", summary: "Permanently delete sandboxes and their disks", sandbox: true},
	{name: "sleep", group: "sandbox", summary: "Suspend a sandbox and keep its disk and T3 links", sandbox: true},
	{name: "stop", group: "sandbox", summary: "Stop a sandbox and keep its disk", sandbox: true},
	{name: "list", group: "sandbox", summary: "List managed sandboxes"},
	{name: "ls", group: "sandbox", action: "list", summary: "List managed sandboxes"},
	{name: "sync", group: "t3", summary: "Link T3 environments and remove stale links"},
	{name: "link", group: "t3", summary: "Pair existing sandboxes with T3 Code", sandbox: true},
	{name: "connect", group: "t3", summary: "Show a sandbox's T3 Code address", sandbox: true},
	{name: "prepare", group: "t3", summary: "Refresh a sandbox's model catalog", sandbox: true},
	{name: "terminal", group: "t3", summary: "Show the Agent37 terminal link", sandbox: true},
}

//go:embed completion.bash
var bashCompletion string

func newCLI(args []string, output, diagnostic io.Writer, execute func(context.Context, options) error) (*cobra.Command, error) {
	root := &cobra.Command{
		Use: owner, Short: "Run T3 Code in private Agent37 sandboxes.", Version: version,
		SilenceErrors: true, SilenceUsage: true, Args: cobra.NoArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE:              func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	}
	root.SetOut(output)
	root.SetErr(diagnostic)
	root.SetArgs(append([]string{}, args...))
	root.SetVersionTemplate("{{.Version}}\n")
	root.SetHelpCommandGroupID("reference")
	root.AddGroup(
		&cobra.Group{ID: "sandbox", Title: "Sandbox lifecycle"},
		&cobra.Group{ID: "t3", Title: "T3 Code and access"},
		&cobra.Group{ID: "workstation", Title: "Workstation"},
		&cobra.Group{ID: "reference", Title: "Help and version"},
	)
	root.SetHelpFunc(func(cmd *cobra.Command, _ []string) {
		width, _ := ttyOutput(cmd.OutOrStdout())
		if cmd == root {
			printMainHelp(cmd, width)
			return
		}
		printCommandHelpAtWidth(cmd, width)
	})
	root.AddCommand(completionCommand(root))
	root.AddCommand(&cobra.Command{
		Use: "version", Short: "Print the CLI version", GroupID: "reference", Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) { cmd.Println(version) }, ValidArgsFunction: cobra.NoFileCompletions,
	})
	completing := len(args) > 0 && slices.Contains([]string{"__complete", "__completeNoDesc"}, args[0])
	for _, spec := range commands {
		opts := options{action: cmp.Or(spec.action, spec.name), up: spec.name == "up"}
		command := &cobra.Command{
			Use: commandUsage(spec), Short: spec.summary, Long: commandDescription(spec), GroupID: spec.group,
			ValidArgsFunction: cobra.NoFileCompletions,
			RunE: func(cmd *cobra.Command, positionals []string) error {
				resolved, err := resolveOptions(opts, cmd.Flags(), positionals)
				if err != nil {
					return err
				}
				return execute(cmd.Context(), resolved)
			},
		}
		if spec.sandbox {
			command.ValidArgsFunction = completeSandboxNames
		}
		root.AddCommand(command)
		// For shell completion only, offer the flags supported by named up.
		if completing && spec.name == "up" && len(args) > 3 && args[1] == "up" && namePattern.MatchString(args[2]) && !batchCountArgument(args[2]) {
			addCommandFlags(command.Flags(), &options{action: "start"})
			command.Flags().Bool("no-desktop", false, "Skip the T3 Code desktop pairing request")
			addWorkspaceFlag(command.Flags())
			continue
		}
		addCommandFlags(command.Flags(), &opts)
		if completing {
			markCompletionGroups(command, opts.action)
		}
		if opts.cpu != nil {
			for name, values := range map[string][]string{"cpu": {"2", "4"}, "ram": {"4", "8", "16", "32"}} {
				if err := command.RegisterFlagCompletionFunc(name, cobra.FixedCompletions(values, cobra.ShellCompDirectiveNoFileComp)); err != nil {
					return nil, err
				}
			}
		}
		if opts.action == "create" || opts.action == "up" {
			if err := command.RegisterFlagCompletionFunc("provider", cobra.FixedCompletions(
				slices.Sorted(maps.Keys(providers)), cobra.ShellCompDirectiveNoFileComp)); err != nil {
				return nil, err
			}
			if err := command.RegisterFlagCompletionFunc("template", completeTemplates); err != nil {
				return nil, err
			}
		}
		if opts.action == "up" {
			if err := command.RegisterFlagCompletionFunc("parallel", cobra.FixedCompletions([]string{"4", "8", "16", "24", "32"}, cobra.ShellCompDirectiveNoFileComp)); err != nil {
				return nil, err
			}
		}
	}
	return root, nil
}

func markCompletionGroups(command *cobra.Command, action string) {
	switch action {
	case "setup":
		command.MarkFlagsMutuallyExclusive("check", "verify", "repair-tailscale")
		command.MarkFlagsMutuallyExclusive("dry-run", "check")
		command.MarkFlagsMutuallyExclusive("dry-run", "verify")
	case "create":
		command.MarkFlagsMutuallyExclusive("yes", "dry-run")
	case "down":
		command.MarkFlagsMutuallyExclusive("all", "pick")
	case "config":
		command.MarkFlagsMutuallyExclusive("show-size", "cpu")
		command.MarkFlagsMutuallyExclusive("show-size", "ram")
	}
}

func completionCommand(root *cobra.Command) *cobra.Command {
	command := &cobra.Command{Use: "completion", Short: "Generate shell completions", GroupID: "workstation",
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }, Args: cobra.NoArgs}
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
	if len(args) > 0 {
		return nil, directive
	}
	if command.Name() == "down" {
		all, _ := command.Flags().GetBool("all")
		pick, _ := command.Flags().GetBool("pick")
		if all || pick {
			return nil, directive
		}
	}
	var names []string
	if command.Name() == "up" {
		for _, count := range []string{"2", "5", "10"} {
			if strings.HasPrefix(count, prefix) {
				names = append(names, count+"\tCreate a batch of "+count+" sandboxes")
			}
		}
		if !onlyStartFlags(command.Flags()) {
			return names, directive
		}
	}
	ctx, cancel := context.WithTimeout(command.Context(), completionAPIDeadline)
	defer cancel()
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
		return names, directive
	}
	instances, err := agent37.New(key).List(ctx)
	if err != nil {
		return names, directive
	}
	seen := make(map[string]bool)
	for _, instance := range instances {
		name := instance.Metadata["environment"]
		if command.Name() == "up" && batchCountArgument(name) {
			continue
		}
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
