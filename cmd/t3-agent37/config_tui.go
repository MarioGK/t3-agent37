package main

import (
	"context"
	"errors"
	"io"
	"strings"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

func editConfig(ctx context.Context, output io.Writer) error {
	if !terminalAvailable(output) {
		return errors.New("the config editor requires a terminal")
	}
	file, err := readWorkstationConfig()
	if err != nil {
		return err
	}
	values := &file.values
	shareGitHub := values.GitHubAccess == nil || *values.GitHubAccess
	var save bool
	fields := sandboxDefaultFields(values, "Use a Tailscale-ready template. Add @revision to pin it.")
	fields = append(fields,
		providerChoice(values).Inline(true),
		huh.NewConfirm().
			Title("Pass GitHub access to new sandboxes?").
			Inline(true).
			WithButtonAlignment(lipgloss.Left).
			Value(&shareGitHub),
		huh.NewConfirm().
			Title("Save these defaults?").
			WithButtonAlignment(lipgloss.Left).
			Affirmative("Save").
			Negative("Cancel").
			Value(&save),
	)
	form := terminalForm(output, huh.NewGroup(fields...).Title("Sandbox defaults"))
	err = form.RunWithContext(ctx)
	if errors.Is(err, huh.ErrUserAborted) || (err == nil && !save) {
		notice(output, info, "Config changes canceled.")
		return nil
	}
	if err != nil {
		return err
	}
	values.Template = strings.TrimSpace(values.Template)
	values.Tailscale = nil
	values.GitHubAccess = &shareGitHub
	if err := file.save(ctx); err != nil {
		return err
	}
	return printDetails(output, "Sandbox defaults saved", detail{"File", file.path})
}

func sandboxDefaultFields(values *workstationConfig, templateHint string) []huh.Field {
	return []huh.Field{
		huh.NewInput().
			Title("Name prefix").
			Description("Used for numbered sandbox names.").
			Value(&values.NamePrefix).
			Validate(validateNamePrefix),
		huh.NewInput().
			Title("Agent37 template").
			Description(templateHint).
			Placeholder("terrascale-agent37").
			Value(&values.Template),
	}
}

func providerChoice(values *workstationConfig) *huh.Select[string] {
	return huh.NewSelect[string]().
		Title("Model provider").
		Options(
			huh.NewOption("Automatic", ""),
			huh.NewOption("Agent37 OpenCode", "opencode"),
			huh.NewOption("CLIProxy", "cliproxy"),
			huh.NewOption("Codex", "codex"),
			huh.NewOption("Claude Code", "claude"),
		).
		Value(&values.Provider)
}
