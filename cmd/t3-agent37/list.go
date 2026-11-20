package main

import (
	"cmp"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/MarioGK/t3-agent37/internal/agent37"
	"github.com/charmbracelet/x/term"
)

func printSandboxList(output io.Writer, instances []agent37.Instance, asJSON bool) error {
	var managed []agent37.Instance
	for _, instance := range instances {
		if isManaged(instance) {
			managed = append(managed, instance)
		}
	}
	if asJSON {
		rows := make([]map[string]any, 0, len(managed))
		for _, instance := range managed {
			rows = append(rows, summary(instance))
		}
		return jsonOutput(output, rows, true)
	}

	var width int
	if file, ok := output.(*os.File); ok && term.IsTerminal(file.Fd()) {
		width, _, _ = term.GetSize(file.Fd())
	}
	_, err := lipgloss.Fprintln(output, renderSandboxList(managed, width))
	return err
}

func renderSandboxList(managed []agent37.Instance, width int) string {
	slices.SortFunc(managed, func(a, b agent37.Instance) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})

	accent := lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	strong := lipgloss.NewStyle().Bold(true)
	muted := lipgloss.NewStyle().Faint(true)
	title := accent.Render("Agent37 sandboxes")
	if len(managed) == 0 {
		message := "No sandboxes yet. Run " + strong.Render("t3-agent37 up") + " to create one."
		if width > 0 {
			message = lipgloss.Wrap(message, width, "")
		}
		return title + "\n\n" + message
	}
	title += muted.Render(fmt.Sprintf("  ·  %d total", len(managed)))

	providerLabels := map[string]string{"cliproxy": "CLIProxy", "opencode": "OpenCode", "codex": "Codex", "claude": "Claude Code"}
	rows := make([][]string, 0, len(managed))
	for _, instance := range managed {
		provider := displayProvider(instance)
		rows = append(rows, []string{
			cmp.Or(listCell(instance.Name), listCell(instance.Metadata["environment"]), "—"),
			listStatus(cmp.Or(listCell(instance.Status), "unknown")),
			cmp.Or(providerLabels[provider], listCell(provider), "—"),
			cmp.Or(listCell(instance.Template), "—"),
			cmp.Or(listCell(instance.ID), "—"),
		})
	}
	grid := table.New().
		Headers("SANDBOX", "STATUS", "PROVIDER", "TEMPLATE", "ID").
		Rows(rows...).
		BorderTop(false).BorderBottom(false).BorderLeft(false).BorderRight(false).BorderColumn(false).
		BorderStyle(muted).
		StyleFunc(func(row, col int) lipgloss.Style {
			style := lipgloss.NewStyle()
			switch {
			case row == table.HeaderRow:
				style = accent
			case col == 0:
				style = strong
			case col == 4:
				style = muted
			}
			if col == 4 {
				return style
			}
			return style.PaddingRight(2)
		}).String()
	if width == 0 || lipgloss.Width(grid) <= width {
		return title + "\n\n" + grid
	}

	var compact strings.Builder
	compact.WriteString(lipgloss.Wrap(title, width, ""))
	for _, row := range rows {
		block := strong.Render(row[0]) + "  " + row[1] + "\n" +
			muted.Render("Provider  ") + row[2] + "\n" +
			muted.Render("Template  ") + row[3] + "\n" +
			muted.Render("ID        ") + row[4]
		compact.WriteString("\n\n" + lipgloss.Wrap(block, width, ""))
	}
	return compact.String()
}

func listStatus(status string) string {
	symbol, color := "○", ""
	switch status {
	case "running":
		symbol, color = "●", "2"
	case "sleeping", "provisioning", "starting", "stopping":
		symbol, color = "◐", "3"
	case "error", "failed":
		symbol, color = "×", "1"
	}
	label := symbol + " " + status
	if color != "" {
		return lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render(label)
	}
	return label
}

func listCell(text string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, text))
}
