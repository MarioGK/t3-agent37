package main

import (
	"io"
	"os"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/term"
)

func terminalAvailable(output io.Writer) bool {
	file, ok := output.(*os.File)
	return ok && term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(file.Fd())
}

func terminalForm(output io.Writer, groups ...*huh.Group) *huh.Form {
	return huh.NewForm(groups...).
		WithTheme(terminalTheme(false)).
		WithLayout(terminalFormLayout{huh.LayoutDefault}).
		WithInput(os.Stdin).
		WithOutput(output)
}

type terminalFormLayout struct{ huh.Layout }

func (terminalFormLayout) GroupWidth(_ *huh.Form, _ *huh.Group, width int) int {
	return min(72, max(1, width-2))
}

func terminalTheme(danger bool) huh.ThemeFunc {
	return func(dark bool) *huh.Styles {
		styles := huh.ThemeCharm(dark)
		pick := lipgloss.LightDark(dark)
		accent := pick(lipgloss.Color("#00685d"), lipgloss.Color("#5eead4"))
		text := pick(lipgloss.Color("#182830"), lipgloss.Color("#e5f0f3"))
		muted := pick(lipgloss.Color("#4a6270"), lipgloss.Color("#9ab2be"))
		rail := accent
		if danger {
			rail = pick(lipgloss.Color("#9c2b32"), lipgloss.Color("#ff9da3"))
		}
		styles.Form.Base = styles.Form.Base.Margin(0, 1)
		styles.FieldSeparator = styles.FieldSeparator.SetString("\n")
		styles.Group.Title = lipgloss.NewStyle().Foreground(accent).Bold(true)
		styles.Group.Description = lipgloss.NewStyle().Foreground(muted).MarginBottom(1)
		styles.Focused.Base = styles.Focused.Base.BorderStyle(lipgloss.NormalBorder()).BorderForeground(rail)
		styles.Focused.Card = styles.Focused.Base
		styles.Focused.Title = styles.Focused.Title.Foreground(text)
		if danger {
			styles.Focused.Title = styles.Focused.Title.Foreground(rail)
		}
		styles.Focused.Description = styles.Focused.Description.Foreground(muted)
		styles.Focused.SelectSelector = lipgloss.NewStyle().Foreground(accent).Bold(true).SetString("› ")
		styles.Focused.SelectedOption = lipgloss.NewStyle().Foreground(accent).Bold(true)
		styles.Focused.UnselectedOption = lipgloss.NewStyle().Foreground(text)
		styles.Focused.FocusedButton = styles.Focused.FocusedButton.
			Foreground(pick(lipgloss.Color("#ffffff"), lipgloss.Color("#062a25"))).
			Background(accent).Bold(true)
		styles.Focused.Next = styles.Focused.FocusedButton
		styles.Focused.BlurredButton = styles.Focused.BlurredButton.
			Foreground(text).Background(pick(lipgloss.Color("#e7eef1"), lipgloss.Color("#23343c")))
		styles.Focused.TextInput.Prompt = styles.Focused.TextInput.Prompt.Foreground(accent)
		styles.Focused.TextInput.Placeholder = styles.Focused.TextInput.Placeholder.Foreground(muted)
		styles.Blurred = styles.Focused
		styles.Blurred.Base = styles.Focused.Base.BorderStyle(lipgloss.HiddenBorder())
		styles.Blurred.Card = styles.Blurred.Base
		styles.Blurred.Title = styles.Blurred.Title.Foreground(muted)
		return styles
	}
}
