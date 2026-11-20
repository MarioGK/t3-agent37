package main

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/MarioGK/t3-agent37/internal/agent37"
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

	width, _ := ttyOutput(output)
	return printSandboxTable(output, managed, width, "Agent37 sandboxes")
}

func printSandboxTable(output io.Writer, managed []agent37.Instance, width int, title string) error {
	_, err := lipgloss.Fprintln(output, renderSandboxList(managed, width, title))
	return err
}

func renderSandboxList(managed []agent37.Instance, width int, heading string) string {
	slices.SortFunc(managed, func(a, b agent37.Instance) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})

	title := cliAccent.Render(heading)
	if len(managed) == 0 {
		message := "No sandboxes yet. Run " + cliStrong.Render("t3-agent37 up") + " to create one."
		if width > 0 {
			message = lipgloss.Wrap(message, width, "")
		}
		return title + "\n\n" + message
	}
	title += cliMuted.Render(fmt.Sprintf("  ·  %d total", len(managed)))

	rows := make([][]string, 0, len(managed))
	for _, instance := range managed {
		provider := displayProvider(instance)
		rows = append(rows, []string{
			cmp.Or(terminalText(instance.Name), terminalText(instance.Metadata["environment"]), "—"),
			listStatus(cmp.Or(terminalText(instance.Status), "unknown")),
			cmp.Or(terminalText(providerLabel(provider)), "—"),
			cmp.Or(terminalText(instance.Template), "—"),
			cmp.Or(terminalText(instance.ID), "—"),
		})
	}
	grid := table.New().
		Headers("SANDBOX", "STATUS", "PROVIDER", "TEMPLATE", "ID").
		Rows(rows...).
		BorderTop(false).BorderBottom(false).BorderLeft(false).BorderRight(false).BorderColumn(false).
		BorderStyle(cliMuted).
		StyleFunc(func(row, col int) lipgloss.Style {
			style := lipgloss.NewStyle()
			switch {
			case row == table.HeaderRow:
				style = cliAccent
			case col == 0:
				style = cliStrong
			case col == 4:
				style = cliMuted
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
		block := cliStrong.Render(row[0]) + "  " + row[1] + "\n" +
			cliMuted.Render("Provider  ") + row[2] + "\n" +
			cliMuted.Render("Template  ") + row[3] + "\n" +
			cliMuted.Render("ID        ") + row[4]
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
