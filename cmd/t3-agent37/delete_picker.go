package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/MarioGK/t3-agent37/internal/agent37"
)

func pickSandboxToDelete(ctx context.Context, instances []agent37.Instance, preview bool, output io.Writer) (agent37.Instance, error) {
	var candidates []agent37.Instance
	for _, instance := range instances {
		if isManaged(instance) && agent37.ValidateID(instance.ID) == nil &&
			namePattern.MatchString(instance.Metadata["environment"]) {
			candidates = append(candidates, instance)
		}
	}
	if len(candidates) == 0 {
		notice(output, info, "No managed Agent37 sandboxes available to delete.")
		return agent37.Instance{}, nil
	}
	slices.SortFunc(candidates, func(a, b agent37.Instance) int {
		return cmp.Or(cmp.Compare(b.Created, a.Created), strings.Compare(a.Metadata["environment"], b.Metadata["environment"]))
	})

	choices := make([]huh.Option[int], 0, len(candidates))
	for index, instance := range candidates {
		status := terminalText(instance.Status)
		choices = append(choices, huh.NewOption(fmt.Sprintf("%s  ·  %s  ·  %s", instance.Metadata["environment"], cmp.Or(status, "unknown"), instance.ID), index))
	}
	var selected int
	picker := huh.NewSelect[int]().
		Title("Select a sandbox").
		Options(choices...).
		Value(&selected).
		Height(min(10, len(choices)+1))
	title := "Delete sandbox and disk"
	if preview {
		title = "Preview sandbox deletion"
	}
	groups := []*huh.Group{huh.NewGroup(picker).Title(title).Description("Search by name, status, or ID with /.")}
	confirmed := false
	if !preview {
		confirmation := huh.NewConfirm().
			TitleFunc(func() string {
				target := candidates[selected]
				return fmt.Sprintf("Delete %s (%s)?", target.Metadata["environment"], target.ID)
			}, &selected).
			Description("The sandbox and its disk will be permanently deleted.").
			WithButtonAlignment(lipgloss.Left).
			Affirmative("Delete sandbox").
			Negative("Cancel").
			Value(&confirmed)
		groups = append(groups, huh.NewGroup(confirmation.WithTheme(terminalTheme(true))).Title("Confirm deletion"))
	}
	err := terminalForm(output, groups...).RunWithContext(ctx)
	if errors.Is(err, huh.ErrUserAborted) || (err == nil && !preview && !confirmed) {
		notice(output, info, "Deletion canceled.")
		return agent37.Instance{}, nil
	}
	if err != nil {
		return agent37.Instance{}, err
	}
	return candidates[selected], nil
}
