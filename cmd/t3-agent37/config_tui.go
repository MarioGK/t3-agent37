package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/huh/v2"
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
	var save bool
	form := huh.NewForm(
		sandboxDefaults(values, "Use a template with Tailscale installed. Add @revision to pin it."),
		providerOptions(values),
		huh.NewGroup(
			huh.NewConfirm().
				Title("Save sandbox defaults?").
				Affirmative("Save").
				Negative("Cancel").
				Value(&save),
		),
	).WithInput(os.Stdin).WithOutput(output)
	err = form.RunWithContext(ctx)
	if errors.Is(err, huh.ErrUserAborted) || (err == nil && !save) {
		fmt.Fprintln(output, "Config changes canceled.")
		return nil
	}
	if err != nil {
		return err
	}
	values.Template = strings.TrimSpace(values.Template)
	values.Tailscale = nil
	if err := file.save(ctx); err != nil {
		return err
	}
	fmt.Fprintln(output, "Saved sandbox defaults to", file.path)
	return nil
}

func sandboxDefaults(values *workstationConfig, templateHint string) *huh.Group {
	return huh.NewGroup(
		huh.NewInput().
			Title("Name prefix").
			Description("Used for numbered names such as agent-1.").
			Value(&values.NamePrefix).
			Validate(validateNamePrefix),
		huh.NewInput().
			Title("Agent37 template").
			Description(templateHint).
			Placeholder("terrascale-agent37@3").
			Value(&values.Template),
	)
}

func providerOptions(values *workstationConfig) *huh.Group {
	return huh.NewGroup(
		huh.NewSelect[string]().
			Title("Model provider").
			Options(
				huh.NewOption("Automatic", ""),
				huh.NewOption("Agent37 OpenCode", "opencode"),
				huh.NewOption("CLIProxy", "cliproxy"),
				huh.NewOption("Codex", "codex"),
				huh.NewOption("Claude Code", "claude"),
			).
			Value(&values.Provider),
	)
}
