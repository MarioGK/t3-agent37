package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const clearProgressLine = "\r\x1b[2K"

var progressFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// launchProgress shares stderr with ordinary notices and serializes writes
// from provisioning workers. The bar tracks actual Agent37 and Tailscale milestones.
type launchProgress struct {
	output   io.Writer
	total    int
	deleting bool

	mu             sync.Mutex
	active         bool
	phase          string
	created, ready int
	failed, frame  int

	stop, done chan struct{}
	stopOnce   sync.Once
}

func startLaunchProgress(output io.Writer, total int, phase string) *launchProgress {
	return startSandboxProgress(output, total, phase, false)
}

func startSandboxProgress(output io.Writer, total int, phase string, deleting bool) *launchProgress {
	p := &launchProgress{output: output, total: total, phase: phase, deleting: deleting}
	if _, terminal := ttyOutput(output); !terminal || os.Getenv("TERM") == "dumb" {
		return p
	}
	lipgloss.EnableLegacyWindowsANSI(output.(*os.File))
	p.active = true
	p.stop, p.done = make(chan struct{}), make(chan struct{})
	p.draw()
	go func() {
		defer close(p.done)
		ticker := time.NewTicker(120 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-ticker.C:
				p.mu.Lock()
				p.frame = (p.frame + 1) % len(progressFrames)
				p.draw()
				p.mu.Unlock()
			}
		}
	}()
	return p
}

func (p *launchProgress) close() {
	if p.stop == nil {
		return
	}
	p.stopOnce.Do(func() {
		close(p.stop)
		<-p.done
		p.mu.Lock()
		defer p.mu.Unlock()
		p.active = false
		_, _ = io.WriteString(p.output, clearProgressLine)
	})
}

func (p *launchProgress) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active {
		_, _ = io.WriteString(p.output, clearProgressLine)
	}
	n, err := p.output.Write(data)
	p.draw()
	return n, err
}

func (p *launchProgress) setPhase(phase string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.phase = phase
	p.draw()
}

func (p *launchProgress) allocated() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.created++
	p.draw()
}

func (p *launchProgress) provisioned(ready bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ready {
		p.ready++
	} else {
		p.failed++
	}
	p.draw()
}

func (p *launchProgress) draw() {
	if !p.active {
		return
	}
	width, _ := ttyOutput(p.output)
	_, _ = io.WriteString(p.output, clearProgressLine)
	_, _ = lipgloss.Fprint(p.output, p.line(width))
}

func (p *launchProgress) line(width int) string {
	if width <= 0 {
		width = 80
	}
	status := fmt.Sprintf("%d/%d ready", p.ready, p.total)
	if p.deleting {
		status = fmt.Sprintf("%d/%d deleted", p.created, p.total)
	}
	if p.failed > 0 {
		status += fmt.Sprintf(" · %d failed", p.failed)
	}
	barWidth := min(22, width-lipgloss.Width(status)-min(18, lipgloss.Width(p.phase))-8)
	line := fmt.Sprintf("%c %s", progressFrames[p.frame], status)
	if barWidth >= 4 {
		filled := barWidth * (p.created + p.ready) / (2 * p.total)
		bar := cliAccent.Render(strings.Repeat("━", filled)) + cliMuted.Render(strings.Repeat("─", barWidth-filled))
		line = fmt.Sprintf("%c %s %s", progressFrames[p.frame], bar, status)
	}
	return ansi.Truncate(line+" · "+p.phase, max(0, width-1), "…")
}
