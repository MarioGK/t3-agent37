package main

import (
	"bytes"
	json "encoding/json/v2"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/MarioGK/t3-agent37/internal/agent37"
)

func TestSandboxListFormats(t *testing.T) {
	t.Setenv("TTY_FORCE", "")
	t.Setenv("CLICOLOR_FORCE", "")
	managed := agent37.Instance{
		ID: "rm17e5y9cm", Name: "proxyqa-bczaut-1", Template: "terrascale-agent37@5", Status: "running",
		Metadata: map[string]string{"managed_by": owner, "provider": "cliproxy"},
	}
	deleted := managed
	deleted.Name, deleted.Status = "deleted-sandbox", "deleted"
	unmanaged := agent37.Instance{Name: "unmanaged-sandbox", Status: "running"}
	instances := []agent37.Instance{deleted, managed, unmanaged}
	var output bytes.Buffer
	if err := printSandboxList(&output, instances, false); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, want := range []string{"1 total", "SANDBOX", "● running", "CLIProxy", managed.Name, managed.Template, managed.ID} {
		if !strings.Contains(text, want) {
			t.Errorf("list missing %q in %q", want, text)
		}
	}
	for _, unwanted := range []string{deleted.Name, unmanaged.Name, "\x1b"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("list included %q in %q", unwanted, text)
		}
	}
	output.Reset()
	if err := printSandboxList(&output, instances, true); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]string
	if err := json.Unmarshal(output.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0]) != 5 || rows[0]["provider"] != "cliproxy" || rows[0]["id"] != managed.ID ||
		rows[0]["name"] != managed.Name || rows[0]["status"] != managed.Status || rows[0]["template"] != managed.Template {
		t.Fatalf("unexpected JSON list: %s", output.String())
	}
}

func TestSandboxListFitsNarrowTerminal(t *testing.T) {
	const width = 34
	instance := agent37.Instance{ID: "rm17e5y9cm", Name: "proxyqa-bczaut-1", Template: "terrascale-agent37@5", Status: "sleeping"}
	text := renderSandboxList([]agent37.Instance{instance}, width, "Agent37 sandboxes")
	if !strings.Contains(text, instance.Template) || !strings.Contains(text, instance.ID) || !strings.Contains(text, "◐ sleeping") {
		t.Fatalf("compact list lost values: %q", text)
	}
	if got := lipgloss.Width(text); got > width {
		t.Errorf("compact list uses %d columns, want at most %d", got, width)
	}
	for _, width := range []int{12, 18, 28, 40, 56, 80} {
		for _, instances := range [][]agent37.Instance{nil, {instance}} {
			text := renderSandboxList(instances, width, "Agent37 sandboxes")
			if got := lipgloss.Width(text); got > width {
				t.Errorf("list with %d rows uses %d columns, want at most %d: %q", len(instances), got, width, text)
			}
		}
	}
}

func TestSandboxListKeepsCallerOrderAndSortsDisplayNames(t *testing.T) {
	instances := []agent37.Instance{
		{Name: "zeta", ID: "zzzzzzzzzz"},
		{Name: "\x1b[31m\x1b[0m", ID: "aaaaaaaaaa", Metadata: map[string]string{"environment": "alpha"}},
		{Name: "beta", ID: "bbbbbbbbbb"},
	}
	text := renderSandboxList(instances, 80, "Agent37 sandboxes")
	if strings.Index(text, "alpha") < 0 || strings.Index(text, "alpha") >= strings.Index(text, "beta") ||
		strings.Index(text, "beta") >= strings.Index(text, "zeta") {
		t.Fatalf("list should sort the names it shows: %q", text)
	}
	if instances[0].Name != "zeta" {
		t.Errorf("rendering changed caller order: %+v", instances)
	}
}
