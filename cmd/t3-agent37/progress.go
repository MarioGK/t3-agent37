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
// from provisioning workers. The bar advances as launch milestones finish.
type launchProgress struct {
	output     io.Writer
	total      int
	deleting   bool
	allocating bool
	pairing    bool

	mu                     sync.Mutex
	active                 bool
	phase                  string
	preflight, synced      bool
	meshChecked            bool
	created, ready, paired int
	failed, frame          int

	stop, done chan struct{}
	stopOnce   sync.Once
}

func startLaunchProgress(output io.Writer, opts options) *launchProgress {
	p := &launchProgress{output: output, total: max(opts.batchSize, 1), phase: "Looking up " + opts.name}
	if opts.action == "create" {
		p.allocating = true
		p.pairing = opts.generatedName && !opts.noDesktop
		p.phase = "Checking Tailscale OAuth"
	}
	p.start()
	return p
}

func startSandboxProgress(output io.Writer, total int, phase string, deleting bool) *launchProgress {
	p := &launchProgress{output: output, total: total, phase: phase, deleting: deleting}
	p.start()
	return p
}

func (p *launchProgress) start() {
	if _, terminal := ttyOutput(p.output); !terminal || os.Getenv("TERM") == "dumb" {
		return
	}
	lipgloss.EnableLegacyWindowsANSI(p.output.(*os.File))
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
		if p.meshChecked {
			width, _ := ttyOutput(p.output)
			_, _ = lipgloss.Fprintln(p.output, p.line(width))
		}
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

func (p *launchProgress) preflightDone(phase string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.preflight = true
	p.phase = phase
	p.draw()
}

func (p *launchProgress) pairedDesktop(confirmed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if confirmed {
		p.paired++
	} else {
		p.failed++
	}
	p.draw()
}

func (p *launchProgress) meshFinished(synced bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.synced = synced
	p.meshChecked = true
	switch {
	case !synced:
		p.phase = "T3 mesh links pending"
	case p.percent() == 100:
		p.phase = "Complete"
	default:
		p.phase = "Some sandboxes need attention"
	}
	p.draw()
}

func (p *launchProgress) percent() int {
	if p.deleting {
		return (p.created + p.ready) * 100 / (2 * p.total)
	}
	// Reserve a share of the bar for mesh sync regardless of batch size.
	done, total := p.ready, 1+2*p.total
	if p.preflight {
		done++
	}
	if p.allocating {
		done, total = done+p.created, total+p.total
	}
	if p.pairing {
		done, total = done+p.paired, total+p.total
	}
	if p.synced {
		done += p.total
	}
	return done * 100 / total
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
	percent := p.percent()
	status := fmt.Sprintf("%d%% · %d/%d ready", percent, p.ready, p.total)
	if p.deleting {
		status = fmt.Sprintf("%d/%d deleted", p.created, p.total)
	} else if p.pairing {
		status += fmt.Sprintf(" · %d/%d paired", p.paired, p.total)
	}
	if p.failed > 0 {
		status += fmt.Sprintf(" · %d failed", p.failed)
	}
	barWidth := min(22, width-lipgloss.Width(status)-min(18, lipgloss.Width(p.phase))-8)
	frame := progressFrames[p.frame]
	if p.meshChecked {
		frame = '!'
		if percent == 100 {
			frame = '✓'
		}
	}
	line := fmt.Sprintf("%c %s", frame, status)
	if barWidth >= 4 {
		filled := barWidth * percent / 100
		if p.deleting {
			filled = barWidth * (p.created + p.ready) / (2 * p.total)
		}
		bar := cliAccent.Render(strings.Repeat("━", filled)) + cliMuted.Render(strings.Repeat("─", barWidth-filled))
		line = fmt.Sprintf("%c %s %s", frame, bar, status)
	}
	return ansi.Truncate(line+" · "+p.phase, max(0, width-1), "…")
}
