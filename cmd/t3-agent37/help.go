package main

import (
	"io"
	"strings"

	"charm.land/lipgloss/v2"
	flag "github.com/spf13/pflag"
)

func printHelp(output io.Writer) {
	width, _ := ttyOutput(output)
	printMainHelp(output, width)
}

func printMainHelp(output io.Writer, width int) {
	_, _ = lipgloss.Fprintf(output, "%s\n%s\n\n%s\n", wrapTerminal(cliAccent.Render(owner), width),
		wrapTerminal("Run T3 Code in private Agent37 sandboxes.", width),
		indentTerminal(cliStrong.Render("t3-agent37 <command> [flags]"), width, 2))
	groups := []struct {
		title    string
		commands []string
	}{
		{"Sandbox lifecycle", []string{"up", "ls", "create", "start", "sleep", "stop", "down"}},
		{"T3 Code and access", []string{"connect", "link", "sync", "terminal", "prepare"}},
		{"Workstation", []string{"setup", "config", "completion"}},
	}
	for _, group := range groups {
		_, _ = lipgloss.Fprintf(output, "\n%s\n", wrapTerminal(cliAccent.Render(group.title), width))
		entries := make([]detail, 0, len(group.commands))
		stack := false
		for _, name := range group.commands {
			command, _ := findCommand(name)
			label, description := name, command.summary
			switch name {
			case "ls":
				label = "ls, list"
			case "completion":
				description = "Generate shell completions"
			}
			entries = append(entries, detail{label, description})
			stack = stack || width > 0 && width < 16+lipgloss.Width(description)
		}
		for _, entry := range entries {
			if stack {
				printHelpEntry(output, width, entry.label, entry.value)
				continue
			}
			_, _ = lipgloss.Fprintf(output, "  %s  %s\n", cliStrong.Width(12).Render(entry.label), entry.value)
		}
	}
	_, _ = lipgloss.Fprintf(output, "\n%s\n", wrapTerminal(cliAccent.Render("Help and version"), width))
	printHelpEntry(output, width, "t3-agent37 help <command>", "Show command options")
	printHelpEntry(output, width, "t3-agent37 --version", "Print the installed version")
}

func printCommandHelp(output io.Writer, command cliCommand, flags *flag.FlagSet) {
	width, _ := ttyOutput(output)
	printCommandHelpAtWidth(output, command, flags, width)
}

func printCommandHelpAtWidth(output io.Writer, command cliCommand, flags *flag.FlagSet, width int) {
	usage := command.name
	description := command.summary
	if command.sandbox {
		usage += " <name>"
	}
	switch command.name {
	case "up":
		usage = "up [count|name]"
	case "down":
		usage = "down [name|--all|--pick]"
	case "create", "sleep", "link":
		usage = command.name + " [name]"
	}
	switch command.name {
	case "up":
		description = "Create a sandbox, create a batch with a count such as 10, or start an existing sandbox by name."
	case "config":
		description = "Edit sandbox defaults in a menu, save a size with --ram or --cpu, or check it with --show-size."
	case "create":
		description += ". Omit the name to use the lowest available number."
	case "down":
		description = "Permanently delete a sandbox and its disk. Omit the name for the newest sandbox, use --all for every managed sandbox, or use --pick to choose in a menu. Retries keep the saved target while cleanup is pending."
	case "sleep":
		description = "Suspend a sandbox and keep its disk and T3 links. Omit the name for the newest sandbox. Run up <name> to wake it."
	}
	flagHelp := strings.TrimRight(flags.FlagUsagesWrapped(max(0, width)), "\n")
	if flagHelp != "" {
		usage += " [flags]"
	}
	_, _ = lipgloss.Fprintf(output, "%s\n%s\n", wrapTerminal(cliAccent.Render(owner+" "+command.name), width), wrapTerminal(description, width))
	_, _ = lipgloss.Fprintf(output, "\n%s\n%s\n", wrapTerminal(cliAccent.Render("Usage"), width), indentTerminal(owner+" "+usage, width, 2))
	if flagHelp != "" {
		_, _ = lipgloss.Fprintf(output, "\n%s\n", wrapTerminal(cliAccent.Render("Options"), width))
		if width == 0 || width >= 56 {
			_, _ = lipgloss.Fprintln(output, flagHelp)
			return
		}
		flags.VisitAll(func(option *flag.Flag) {
			if option.Hidden {
				return
			}
			argument, description := flag.UnquoteUsage(option)
			label := "--" + option.Name
			if argument != "" {
				label += " " + argument
			} else if option.DefValue == "true" {
				label += "[=false]"
				description += " (default true)"
			}
			printHelpEntry(output, width, label, description)
		})
	}
}

func printHelpEntry(output io.Writer, width int, label, description string) {
	_, _ = lipgloss.Fprintf(output, "%s\n%s\n", indentTerminal(cliStrong.Render(label), width, 2), indentTerminal(description, width, 4))
}
