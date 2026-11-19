package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/term"
)

func deletePickerAvailable(output io.Writer) bool {
	file, ok := output.(*os.File)
	return ok && term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(file.Fd())
}

func pickSandboxToDelete(ctx context.Context, instances []agent37.Instance, preview bool, output io.Writer) (agent37.Instance, error) {
	var candidates []agent37.Instance
	for _, instance := range instances {
		if isManaged(instance) && agent37.ValidateID(instance.ID) == nil &&
			namePattern.MatchString(instance.Metadata["environment"]) {
			candidates = append(candidates, instance)
		}
	}
	if len(candidates) == 0 {
		fmt.Fprintln(output, "No managed Agent37 sandboxes available to delete.")
		return agent37.Instance{}, nil
	}
	slices.SortFunc(candidates, func(a, b agent37.Instance) int {
		return cmp.Or(cmp.Compare(b.Created, a.Created), strings.Compare(a.Metadata["environment"], b.Metadata["environment"]))
	})

	choices := make([]huh.Option[int], 0, len(candidates))
	for index, instance := range candidates {
		status := strings.Map(func(r rune) rune {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				return -1
			}
			return r
		}, instance.Status)
		choices = append(choices, huh.NewOption(fmt.Sprintf("%s  [%s]  %s", instance.Metadata["environment"], status, instance.ID), index))
	}
	var selected int
	picker := huh.NewSelect[int]().
		Title("Select a sandbox").
		Options(choices...).
		Value(&selected).
		Height(12)
	groups := []*huh.Group{huh.NewGroup(picker)}
	confirmed := false
	if !preview {
		confirmation := huh.NewConfirm().
			TitleFunc(func() string {
				target := candidates[selected]
				return fmt.Sprintf("Permanently delete %s (%s) and its disk?", target.Metadata["environment"], target.ID)
			}, &selected).
			Affirmative("Delete").
			Negative("Cancel").
			Value(&confirmed)
		groups = append(groups, huh.NewGroup(confirmation))
	}
	err := huh.NewForm(groups...).WithInput(os.Stdin).WithOutput(output).RunWithContext(ctx)
	if errors.Is(err, huh.ErrUserAborted) || (err == nil && !preview && !confirmed) {
		fmt.Fprintln(output, "Deletion canceled.")
		return agent37.Instance{}, nil
	}
	if err != nil {
		return agent37.Instance{}, err
	}
	return candidates[selected], nil
}
