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
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

const accentColor = "6"

var (
	cliAccent = lipgloss.NewStyle().Foreground(lipgloss.Color(accentColor)).Bold(true)
	cliBlue   = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	cliPurple = lipgloss.NewStyle().Foreground(lipgloss.Color("13")).Bold(true)
	cliGreen  = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
	cliAmber  = lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Bold(true)
	cliStrong = lipgloss.NewStyle().Bold(true)
	cliMuted  = lipgloss.NewStyle().Faint(true)
)

func ttyOutput(output io.Writer) (width int, terminal bool) {
	if progress, ok := output.(*launchProgress); ok {
		output = progress.output
	}
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
	}, ansi.Strip(text)))
}

func wrapTerminal(text string, width int) string {
	if width <= 0 || lipgloss.Width(text) <= width {
		return text
	}
	var lines []string
	for line := range strings.SplitSeq(lipgloss.Wrap(text, width, ""), "\n") {
		// Lip Gloss can leave a trailing space past the last column when the
		// preceding word fits exactly. Keep ANSI resets while removing the space.
		if lipgloss.Width(line) > width {
			visible := strings.TrimRight(ansi.Strip(line), " ")
			line = ansi.Truncate(line, lipgloss.Width(visible), "")
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func indentTerminal(text string, width, padding int) string {
	if width > 0 {
		padding = min(padding, max(0, width-2))
	}
	indent := strings.Repeat(" ", padding)
	return indent + strings.ReplaceAll(wrapTerminal(text, width-padding), "\n", "\n"+indent)
}

func terminalLink(text string) bool {
	return strings.HasPrefix(text, "https://") || strings.HasPrefix(text, "http://") || strings.HasPrefix(text, "t3code://")
}

type messageKind struct{ mark, color string }

var (
	info    = messageKind{"›", accentColor}
	success = messageKind{"✓", "10"}
	warning = messageKind{"!", "11"}
	failure = messageKind{"×", "9"}
)

func notice(output io.Writer, kind messageKind, format string, args ...any) {
	mark := lipgloss.NewStyle().Foreground(lipgloss.Color(kind.color)).Bold(true).Render(kind.mark)
	message := terminalText(fmt.Sprintf(format, args...))
	width, terminal := ttyOutput(output)
	if width > 4 && lipgloss.Width(message) > width-4 {
		var words []string
		var lines []string
		for word := range strings.FieldsSeq(message) {
			if terminalLink(word) {
				if len(words) != 0 {
					lines = append(lines, wrapTerminal(strings.Join(words, " "), width-4))
					words = words[:0]
				}
				lines = append(lines, word)
				continue
			}
			words = append(words, word)
		}
		if len(words) != 0 {
			lines = append(lines, wrapTerminal(strings.Join(words, " "), width-4))
		}
		message = strings.ReplaceAll(strings.Join(lines, "\n"), "\n", "\n    ")
	}
	// Bubble Tea applies the terminal's color profile to queued notices.
	if _, progress := output.(*launchProgress); progress && terminal && os.Getenv("TERM") != "dumb" && os.Getenv("NO_COLOR") == "" {
		_, _ = fmt.Fprintf(output, "  %s %s\n", mark, message)
		return
	}
	_, _ = lipgloss.Fprintf(output, "  %s %s\n", mark, message)
}

type detail struct{ label, value string }

func printDetails(output io.Writer, title string, fields ...detail) error {
	width, _ := ttyOutput(output)
	_, err := lipgloss.Fprintln(output, renderDetails(title, width, fields))
	return err
}

func renderDetails(title string, width int, fields []detail) string {
	var text strings.Builder
	text.WriteString(wrapTerminal(cliAccent.Render(terminalText(title)), width))
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
			fmt.Fprintf(&text, "\n%s\n", indentTerminal(cliMuted.Render(label), width, 2))
			if terminalLink(value) {
				fmt.Fprintf(&text, "    %s", value)
			} else {
				text.WriteString(indentTerminal(value, width, 4))
			}
			continue
		}
		fmt.Fprintf(&text, "\n  %s%s  %s", cliMuted.Render(label), strings.Repeat(" ", maxLabel-lipgloss.Width(label)), value)
	}
	return text.String()
}

var providerLabels = map[string]string{"cliproxy": "CLIProxy", "opencode": "OpenCode", "codex": "Codex", "claude": "Claude Code"}

func providerLabel(provider string) string {
	return cmp.Or(providerLabels[provider], provider)
}

func printSandboxResult(output io.Writer, title string, instance agent37.Instance, address string, extra ...detail) error {
	fields := []detail{
		{"Sandbox", sandboxDisplayName(instance)},
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
