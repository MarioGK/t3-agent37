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
	title := cliAccent.Render(terminalText(heading))
	if len(managed) == 0 {
		message := "No sandboxes yet. Run " + cliStrong.Render("t3-agent37 up") + " to create one."
		return wrapTerminal(title, width) + "\n\n" + wrapTerminal(message, width)
	}
	title = wrapTerminal(title+cliMuted.Render(fmt.Sprintf("  ·  %d total", len(managed))), width)

	rows := make([][]string, 0, len(managed))
	for _, instance := range managed {
		provider := displayProvider(instance)
		rows = append(rows, []string{
			sandboxDisplayName(instance),
			listStatus(cmp.Or(terminalText(instance.Status), "unknown")),
			cmp.Or(terminalText(providerLabel(provider)), "—"),
			cmp.Or(terminalText(instance.Template), "—"),
			cmp.Or(terminalText(instance.ID), "—"),
		})
	}
	slices.SortFunc(rows, func(a, b []string) int {
		return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[4], b[4]))
	})
	headers := []string{"SANDBOX", "STATUS", "PROVIDER", "TEMPLATE", "ID"}
	if width == 0 || sandboxTableWidth(headers, rows) <= width {
		return title + "\n\n" + renderSandboxTable(headers, rows)
	}

	var compact strings.Builder
	compact.WriteString(title)
	for _, row := range rows {
		block := cliBlue.Render(row[0]) + "  " + row[1] + "\n" +
			cliMuted.Render("Provider  ") + cliPurple.Render(row[2]) + "\n" +
			cliMuted.Render("Template  ") + row[3] + "\n" +
			cliMuted.Render("ID        ") + row[4]
		compact.WriteString("\n\n" + wrapTerminal(block, width))
	}
	return compact.String()
}

func sandboxDisplayName(instance agent37.Instance) string {
	return cmp.Or(terminalText(instance.Metadata["environment"]), terminalText(instance.Name), "—")
}

const sandboxColumnPadding = 2

func sandboxTableWidth(headers []string, rows [][]string) int {
	var width int
	for col, header := range headers {
		column := lipgloss.Width(header)
		for _, row := range rows {
			column = max(column, lipgloss.Width(row[col]))
		}
		width += column
	}
	return width + sandboxColumnPadding*(len(headers)-1)
}

func renderSandboxTable(headers []string, rows [][]string) string {
	return table.New().
		Headers(headers...).
		Rows(rows...).
		BorderTop(false).BorderBottom(false).BorderLeft(false).BorderRight(false).BorderColumn(false).
		BorderStyle(cliMuted).
		StyleFunc(func(row, col int) lipgloss.Style {
			style := lipgloss.NewStyle()
			switch {
			case row == table.HeaderRow:
				style = cliAccent
			case col == 0:
				style = cliBlue
			case col == 2:
				style = cliPurple
			case col == 4:
				style = cliMuted
			}
			if col == 4 {
				return style
			}
			return style.PaddingRight(sandboxColumnPadding)
		}).String()
}

func listStatus(status string) string {
	symbol, color := "○", ""
	switch status {
	case "running":
		symbol, color = "●", "2"
	case "sleeping":
		symbol, color = "◐", "13"
	case "provisioning", "starting", "stopping":
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
