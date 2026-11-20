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
		{"Sandbox lifecycle", []string{"up", "ls", "create", "start", "down", "stop", "delete"}},
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
			case "delete":
				label = "delete, del"
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
	case "up", "create", "down", "delete", "del", "link":
		usage = command.name + " [name]"
	}
	switch command.name {
	case "up", "create":
		description += ". Omit the name to use the lowest available number."
	case "down":
		description = "Delete the newest managed sandbox and its disk. After deletion, bare down keeps the same target until a newer sandbox exists. Pass a name to stop that sandbox and keep its disk."
	case "delete", "del":
		description = "Permanently delete a sandbox and its disk. Omit the name to open the picker."
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
