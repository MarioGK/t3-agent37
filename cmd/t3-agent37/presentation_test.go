package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

func TestTerminalTextStripsTerminalCommands(t *testing.T) {
	const input = "\x1b[31malpha\x1b[0m\u202e\n\t\x1b]8;;https://ignored.invalid\x1b\\beta\x1b]8;;\x1b\\\u2028"
	if got := terminalText(input); got != "alpha  beta" {
		t.Errorf("terminal text = %q, want visible text without ANSI, bidi or line controls", got)
	}
}

func TestDetailsFitNarrowTerminalAndKeepLinkIntact(t *testing.T) {
	const title = "T3 Code pairing · expires in 5 minutes"
	const address = "https://example.invalid/pair?secret=a-phrasal-long-value"
	const width = 24
	fields := []detail{
		{"A long label for the template", "terrascale-agent37@5-extra-long-fixture"},
		{"Status", "Provisioning a new sandbox"},
	}
	if got := lipgloss.Width(renderDetails(title, width, fields)); got > width {
		t.Fatalf("details use %d columns, want at most %d", got, width)
	}
	if text := renderDetails(title, width, append(fields, detail{"Open", address})); !strings.Contains(text, "\n    "+address+"\n") &&
		!strings.HasSuffix(text, "\n    "+address) {
		t.Errorf("terminal details split the URL: %q", text)
	}
}

func TestHelpFitsTerminalAndKeepsGroupAlignment(t *testing.T) {
	t.Setenv("TTY_FORCE", "")
	t.Setenv("CLICOLOR_FORCE", "")
	for _, width := range []int{12, 24, 41, 56, 80} {
		var output bytes.Buffer
		root, err := newCLI(nil, &output, io.Discard, func(context.Context, options) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		up, _, err := root.Find([]string{"up"})
		if err != nil {
			t.Fatal(err)
		}
		printMainHelp(root, width)
		if got := lipgloss.Width(output.String()); got > width {
			t.Errorf("main help width %d at %d columns: %q", got, width, output.String())
		}
		if width == 41 && !strings.Contains(output.String(), "  ls, list\n    List managed sandboxes") {
			t.Errorf("commands in the same section should use the same layout: %q", output.String())
		}
		output.Reset()
		printCommandHelpAtWidth(up, width)
		if got := lipgloss.Width(output.String()); got > width {
			t.Errorf("command help width %d at %d columns: %q", got, width, output.String())
		}
	}
}
