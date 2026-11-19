package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
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
	tailscale := "auto"
	if values.Tailscale != nil {
		tailscale = "off"
		if *values.Tailscale {
			tailscale = "on"
		}
	}
	var save bool
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Name prefix").
				Description("Used for numbered names such as agent-1.").
				Value(&values.NamePrefix).
				Validate(validateNamePrefix),
			huh.NewInput().
				Title("Agent37 template").
				Description("Use name@revision to pin it, or leave blank for the provider default.").
				Placeholder("terrascale-agent37@3").
				Value(&values.Template),
		),
		huh.NewGroup(
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
			huh.NewSelect[string]().
				Title("Tailscale enrollment").
				Description("Automatic enrolls TerraScale sandboxes. Enrollment requires OAuth credentials.").
				Options(
					huh.NewOption("Automatic", "auto"),
					huh.NewOption("On", "on"),
					huh.NewOption("Off", "off"),
				).
				Value(&tailscale),
		),
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
	if tailscale != "auto" {
		enabled := tailscale == "on"
		values.Tailscale = &enabled
	}
	if err := file.save(ctx); err != nil {
		return err
	}
	fmt.Fprintln(output, "Saved sandbox defaults to", file.path)
	return nil
}
