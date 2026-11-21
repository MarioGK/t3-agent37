package main

import (
	"io"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"
	flag "github.com/spf13/pflag"
)

func printMainHelp(root *cobra.Command, width int) {
	output := root.OutOrStdout()
	_, _ = lipgloss.Fprintf(output, "%s\n%s\n\n%s\n", wrapTerminal(cliAccent.Render(root.Name()), width),
		wrapTerminal(root.Short, width),
		indentTerminal(cliStrong.Render("t3-agent37 <command> [flags]"), width, 2))
	for _, group := range root.Groups() {
		if group.ID == "reference" {
			continue
		}
		_, _ = lipgloss.Fprintf(output, "\n%s\n", wrapTerminal(cliPurple.Render(group.Title), width))
		var entries []detail
		stack := false
		for _, command := range root.Commands() {
			if command.GroupID != group.ID || !command.IsAvailableCommand() || command.Name() == "list" {
				continue
			}
			label := command.Name()
			if label == "ls" {
				label += ", list"
			}
			entries = append(entries, detail{label, command.Short})
			stack = stack || width > 0 && width < 16+lipgloss.Width(command.Short)
		}
		for _, entry := range entries {
			if stack {
				printHelpEntry(output, width, entry.label, entry.value)
				continue
			}
			_, _ = lipgloss.Fprintf(output, "  %s  %s\n", cliBlue.Width(12).Render(entry.label), entry.value)
		}
	}
	_, _ = lipgloss.Fprintf(output, "\n%s\n", wrapTerminal(cliPurple.Render("Help and version"), width))
	printHelpEntry(output, width, "t3-agent37 help <command>", "Show command options")
	printHelpEntry(output, width, "t3-agent37 --version", "Print the installed version")
}

func commandUsage(command cliCommand) string {
	switch command.name {
	case "up":
		return "up [count|name]"
	case "down":
		return "down [name|--all|--pick]"
	case "create", "sleep", "link":
		return command.name + " [name]"
	}
	if command.sandbox {
		return command.name + " <name>"
	}
	return command.name
}

func commandDescription(command cliCommand) string {
	switch command.name {
	case "up":
		return "Create a sandbox, create a batch with a count such as 10, or start an existing sandbox by name."
	case "config":
		return "Edit sandbox defaults in a menu, save a size with --ram or --cpu, or check it with --show-size."
	case "create":
		return command.summary + ". Omit the name to use the lowest available number."
	case "down":
		return "Permanently delete a sandbox and its disk. Omit the name for the newest sandbox, use --all for every managed sandbox, or use --pick to choose in a menu. Retries keep the saved target while cleanup is pending."
	case "sleep":
		return "Suspend a sandbox and keep its disk and T3 links. Omit the name for the newest sandbox. Run up <name> to wake it."
	}
	return command.summary
}

func printCommandHelpAtWidth(command *cobra.Command, width int) {
	output := command.OutOrStdout()
	description := command.Long
	if description == "" {
		description = command.Short
	}
	_, _ = lipgloss.Fprintf(output, "%s\n%s\n", wrapTerminal(cliAccent.Render(command.CommandPath()), width), wrapTerminal(description, width))
	_, _ = lipgloss.Fprintf(output, "\n%s\n%s\n", wrapTerminal(cliPurple.Render("Usage"), width), indentTerminal(command.UseLine(), width, 2))
	flags := command.Flags()
	if !flags.HasAvailableFlags() {
		return
	}
	_, _ = lipgloss.Fprintln(output, "\n"+cliPurple.Render("Options"))
	if width == 0 || width >= 56 {
		_, _ = lipgloss.Fprintln(output, strings.TrimRight(flags.FlagUsagesWrapped(width), "\n"))
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

func printHelpEntry(output io.Writer, width int, label, description string) {
	_, _ = lipgloss.Fprintf(output, "%s\n%s\n", indentTerminal(cliBlue.Render(label), width, 2), indentTerminal(description, width, 4))
}
