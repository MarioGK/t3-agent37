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
	p := startLaunchProgress(&bytes.Buffer{}, 2, "Creating and setting up sandboxes")
	defer p.close()
	if line := ansi.Strip(p.line(80)); !strings.Contains(line, "0/2 ready") || strings.Contains(line, "━") {
		t.Fatalf("initial progress: %q", line)
	}
	p.allocated()
	p.provisioned(true)
	p.provisioned(false)
	p.setPhase("Waiting for T3 Code")
	line := ansi.Strip(p.line(80))
	if strings.Count(line, "━") != 11 || !strings.Contains(line, "1/2 ready · 1 failed · Waiting for T3 Code") {
		t.Fatalf("partial progress: %q", line)
	}
	for _, width := range []int{12, 24, 40, 80} {
		if got := ansi.StringWidth(p.line(width)); got >= width {
			t.Fatalf("progress wraps at terminal width %d: displayed width %d", width, got)
		}
	}
}

func TestLaunchProgressSerializesRedirectedLogsWithoutControlCodes(t *testing.T) {
	var output bytes.Buffer
	p := startLaunchProgress(&output, 10, "Creating")
	var group sync.WaitGroup
	for i := range 10 {
		group.Go(func() {
			p.allocated()
			_, _ = fmt.Fprintf(p, "sandbox %d\n", i)
			p.provisioned(true)
		})
	}
	group.Wait()
	p.close()
	if p.ready != 10 || p.created != 10 || strings.Count(output.String(), "\n") != 10 || strings.Contains(output.String(), "\x1b") {
		t.Fatalf("unexpected redirected output: %q", output.String())
	}
}

func TestDeleteProgressCountsConfirmedCloudDeletes(t *testing.T) {
	p := startSandboxProgress(&bytes.Buffer{}, 3, "Cleaning up T3 links", true)
	p.allocated()
	p.allocated()
	p.provisioned(false)
	if line := ansi.Strip(p.line(80)); !strings.Contains(line, "2/3 deleted · 1 failed · Cleaning up T3 links") {
		t.Fatalf("bulk deletion progress: %q", line)
	}
}
