package main

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestLaunchProgressTracksMilestonesAndFitsTerminal(t *testing.T) {
	p := startLaunchProgress(&bytes.Buffer{}, options{action: "create", batchSize: 2, generatedName: true})
	defer p.close()
	if line := ansi.Strip(newProgressView(p.progressState, 80).line()); !strings.Contains(line, "0% · 0/2 ready · 0/2 paired") || strings.Contains(line, "━") {
		t.Fatalf("initial progress: %q", line)
	}
	p.preflightDone("Creating")
	p.allocated()
	p.provisioned(true)
	p.pairedDesktop(true) // The first sandbox can pair while the next is provisioning.
	p.allocated()
	p.provisioned(true)
	p.setPhase("Waiting for T3 Code")
	line := ansi.Strip(newProgressView(p.progressState, 80).line())
	if !strings.Contains(line, "66% · 2/2 ready · 1/2 paired · Waiting for T3 Code") {
		t.Fatalf("partial progress: %q", line)
	}
	p.pairedDesktop(true)
	p.setPhase("Syncing T3 environments")
	if line := ansi.Strip(newProgressView(p.progressState, 80).line()); !strings.Contains(line, "77%") || !strings.Contains(line, "Syncing T3 environments") {
		t.Fatalf("mesh progress: %q", line)
	}
	p.meshFinished(true)
	if line := ansi.Strip(newProgressView(p.progressState, 80).line()); !strings.Contains(line, "✓") || !strings.Contains(line, "100% · 2/2 ready · 2/2 paired · Complete") {
		t.Fatalf("completed progress: %q", line)
	}
	for _, width := range []int{12, 24, 40, 80} {
		if got := ansi.StringWidth(newProgressView(p.progressState, width).line()); got >= width {
			t.Fatalf("progress wraps at terminal width %d: displayed width %d", width, got)
		}
	}
}

func TestLaunchProgressSkipsInapplicableStepsAndKeepsFailuresIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts options
		want int
	}{
		{"headless", options{action: "create", generatedName: true, noDesktop: true}, 75},
		{"resume", options{action: "start"}, 66},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := startLaunchProgress(&bytes.Buffer{}, tc.opts)
			p.preflightDone("Starting sandbox")
			if tc.opts.action == "create" {
				p.allocated()
			}
			p.provisioned(true)
			p.meshFinished(false)
			if line := ansi.Strip(newProgressView(p.progressState, 80).line()); p.percent() != tc.want || strings.Contains(line, "paired") || !strings.Contains(line, "T3 mesh links pending") {
				t.Fatalf("pending progress: %q", line)
			}
			p.meshFinished(true)
			if got := p.percent(); got != 100 {
				t.Fatalf("finished progress: %d%%", got)
			}
		})
	}
	p := startLaunchProgress(&bytes.Buffer{}, options{action: "create", generatedName: true})
	p.preflightDone("Creating sandbox")
	p.allocated()
	p.provisioned(true)
	p.pairedDesktop(false)
	p.meshFinished(true)
	if line := ansi.Strip(newProgressView(p.progressState, 80).line()); p.percent() != 80 || !strings.Contains(line, "1 failed") {
		t.Fatalf("partially successful progress: %q", line)
	}
}

func TestLaunchProgressSerializesRedirectedLogsWithoutControlCodes(t *testing.T) {
	var output bytes.Buffer
	p := startLaunchProgress(&output, options{action: "create", batchSize: 10, generatedName: true})
	var group sync.WaitGroup
	for i := range 10 {
		group.Go(func() {
			p.allocated()
			_, _ = fmt.Fprintf(p, "sandbox %d\n", i)
			p.provisioned(true)
			p.pairedDesktop(true)
		})
	}
	group.Wait()
	p.close()
	if p.ready != 10 || p.created != 10 || p.paired != 10 || strings.Count(output.String(), "\n") != 10 || strings.Contains(output.String(), "\x1b") {
		t.Fatalf("unexpected redirected output: %q", output.String())
	}
}

func TestDeleteProgressCountsConfirmedCloudDeletes(t *testing.T) {
	p := startSandboxProgress(&bytes.Buffer{}, 3, "Cleaning up T3 links", true)
	p.allocated()
	p.allocated()
	p.provisioned(false)
	if line := ansi.Strip(newProgressView(p.progressState, 80).line()); !strings.Contains(line, "2/3 deleted · 1 failed · Cleaning up T3 links") {
		t.Fatalf("bulk deletion progress: %q", line)
	}
}
