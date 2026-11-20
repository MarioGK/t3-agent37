package main

import (
	"io"
	"strings"

	"charm.land/lipgloss/v2"
	flag "github.com/spf13/pflag"
)

func printHelp(output io.Writer) {
	_, _ = lipgloss.Fprintf(output, "%s\n%s\n\n  %s\n", cliAccent.Render(owner),
		"Run T3 Code in private Agent37 sandboxes.", cliStrong.Render("t3-agent37 <command> [flags]"))
	groups := []struct {
		title    string
		commands []string
	}{
		{"Sandbox lifecycle", []string{"up", "ls", "create", "start", "down", "stop", "delete"}},
		{"T3 Code and access", []string{"connect", "link", "sync", "terminal", "prepare"}},
		{"Workstation", []string{"setup", "config", "completion"}},
	}
	width, _ := ttyOutput(output)
	for _, group := range groups {
		_, _ = lipgloss.Fprintf(output, "\n%s\n", cliAccent.Render(group.title))
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
			if width > 0 && width < 16+lipgloss.Width(description) {
				printHelpEntry(output, width, label, description)
				continue
			}
			_, _ = lipgloss.Fprintf(output, "  %s  %s\n", cliStrong.Width(12).Render(label), description)
		}
	}
	_, _ = lipgloss.Fprintf(output, "\n%s\n", cliAccent.Render("Help and version"))
	printHelpEntry(output, width, "t3-agent37 help <command>", "Show command options")
	printHelpEntry(output, width, "t3-agent37 --version", "Print the installed version")
}

func printCommandHelp(output io.Writer, command cliCommand, flags *flag.FlagSet) {
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
		description = "Delete the newest managed sandbox and its disk. Pass a name to stop that sandbox and keep its disk."
	case "delete", "del":
		description = "Permanently delete a sandbox and its disk. Omit the name to open the picker."
	}
	width, _ := ttyOutput(output)
	flagHelp := strings.TrimRight(flags.FlagUsagesWrapped(max(0, width)), "\n")
	if flagHelp != "" {
		usage += " [flags]"
	}
	if width > 0 {
		description = lipgloss.Wrap(description, width, "")
	}
	_, _ = lipgloss.Fprintf(output, "%s\n%s\n", cliAccent.Render(owner+" "+command.name), description)
	_, _ = lipgloss.Fprintf(output, "\n%s\n  %s\n", cliAccent.Render("Usage"), owner+" "+usage)
	if flagHelp != "" {
		_, _ = lipgloss.Fprintf(output, "\n%s\n", cliAccent.Render("Options"))
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
			}
			printHelpEntry(output, width, label, description)
		})
	}
}

func printHelpEntry(output io.Writer, width int, label, description string) {
	if width > 0 {
		description = lipgloss.Wrap(description, max(1, width-4), "")
	}
	_, _ = lipgloss.Fprintf(output, "  %s\n    %s\n", cliStrong.Render(label), strings.ReplaceAll(description, "\n", "\n    "))
}
