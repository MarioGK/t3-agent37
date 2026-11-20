package main

import (
	"cmp"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/MarioGK/t3-agent37/internal/agent37"
	"github.com/charmbracelet/x/term"
)

const accentColor = "6"

var (
	cliAccent = lipgloss.NewStyle().Foreground(lipgloss.Color(accentColor)).Bold(true)
	cliStrong = lipgloss.NewStyle().Bold(true)
	cliMuted  = lipgloss.NewStyle().Faint(true)
)

func ttyOutput(output io.Writer) (width int, terminal bool) {
	file, ok := output.(*os.File)
	if !ok || !term.IsTerminal(file.Fd()) {
		return 0, false
	}
	width, _, _ = term.GetSize(file.Fd())
	return width, true
}

func terminalText(text string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, text))
}

type messageKind struct{ mark, color string }

var (
	info    = messageKind{"›", accentColor}
	success = messageKind{"✓", "2"}
	warning = messageKind{"!", "3"}
	failure = messageKind{"×", "1"}
)

func notice(output io.Writer, kind messageKind, format string, args ...any) {
	mark := lipgloss.NewStyle().Foreground(lipgloss.Color(kind.color)).Bold(true).Render(kind.mark)
	message := terminalText(fmt.Sprintf(format, args...))
	if width, terminal := ttyOutput(output); terminal && width > 4 && !strings.Contains(message, "https://") {
		message = strings.ReplaceAll(lipgloss.Wrap(message, width-4, ""), "\n", "\n    ")
	}
	_, _ = lipgloss.Fprintf(output, "  %s %s\n", mark, message)
}

type detail struct{ label, value string }

func printDetails(output io.Writer, title string, fields ...detail) error {
	width, _ := ttyOutput(output)
	var text strings.Builder
	text.WriteString(cliAccent.Render(terminalText(title)))
	maxLabel := 0
	for _, field := range fields {
		maxLabel = max(maxLabel, lipgloss.Width(terminalText(field.label)))
	}
	if len(fields) > 0 {
		text.WriteByte('\n')
	}
	for _, field := range fields {
		label, value := terminalText(field.label), terminalText(field.value)
		if width > 0 && 4+maxLabel+lipgloss.Width(value) > width {
			fmt.Fprintf(&text, "\n  %s\n    %s", cliMuted.Render(label), value)
			continue
		}
		fmt.Fprintf(&text, "\n  %s%s  %s", cliMuted.Render(label), strings.Repeat(" ", maxLabel-lipgloss.Width(label)), value)
	}
	_, err := lipgloss.Fprintln(output, text.String())
	return err
}

var providerLabels = map[string]string{"cliproxy": "CLIProxy", "opencode": "OpenCode", "codex": "Codex", "claude": "Claude Code"}

func providerLabel(provider string) string {
	return cmp.Or(providerLabels[provider], provider)
}

func printSandboxResult(output io.Writer, title string, instance agent37.Instance, address string, extra ...detail) error {
	fields := []detail{
		{"Sandbox", cmp.Or(instance.Name, instance.Metadata["environment"])},
		{"Status", cmp.Or(instance.Status, "unknown")},
		{"Provider", providerLabel(displayProvider(instance))},
		{"Template", cmp.Or(instance.Template, "—")},
		{"Sandbox ID", instance.ID},
	}
	if address != "" {
		fields = append(fields, detail{"T3 Code", address})
	}
	return printDetails(output, title, append(fields, extra...)...)
}

func showLifecycleResult(output io.Writer, title string, instance agent37.Instance, address string) error {
	if _, terminal := ttyOutput(output); terminal {
		return printSandboxResult(output, title, instance, address)
	}
	row := summary(instance)
	if address != "" {
		row["t3_url"] = address
	}
	return jsonOutput(output, row, false)
}
