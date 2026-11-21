package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// launchProgress serializes sandbox milestones and log lines from workers.
type launchProgress struct {
	progressState
	output  io.Writer
	mu      sync.Mutex
	program *tea.Program
	done    chan struct{}
}

type progressState struct {
	total                          int
	deleting, allocating, pairing  bool
	phase                          string
	preflight, synced, meshChecked bool
	created, ready, paired, failed int
}

func startLaunchProgress(output io.Writer, opts options) *launchProgress {
	p := &launchProgress{output: output, progressState: progressState{total: max(opts.batchSize, 1), phase: "Looking up " + opts.name}}
	if opts.action == "create" {
		p.allocating = true
		p.pairing = opts.generatedName && !opts.noDesktop
		p.phase = "Checking Tailscale OAuth"
	}
	p.start()
	return p
}

func startSandboxProgress(output io.Writer, total int, phase string, deleting bool) *launchProgress {
	p := &launchProgress{output: output, progressState: progressState{total: total, phase: phase, deleting: deleting}}
	p.start()
	return p
}

func (p *launchProgress) start() {
	width, terminal := ttyOutput(p.output)
	if !terminal || os.Getenv("TERM") == "dumb" {
		return
	}
	p.program = tea.NewProgram(newProgressView(p.progressState, width),
		tea.WithOutput(p.output), tea.WithInput(nil), tea.WithoutSignalHandler(), tea.WithFPS(30))
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		_, _ = p.program.Run()
	}()
}

func (p *launchProgress) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.program != nil {
		p.program.Send(progressFinished{})
		<-p.done
		p.program = nil
		if p.meshChecked {
			width, _ := ttyOutput(p.output)
			_, _ = lipgloss.Fprintln(p.output, newProgressView(p.progressState, width).line())
		}
	}
}

func (p *launchProgress) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.program != nil {
		select {
		case <-p.done:
		default:
			// Send honors shutdown, unlike Program.Println in this version.
			p.program.Send(tea.Println(strings.TrimSuffix(string(data), "\n"))())
			return len(data), nil
		}
	}
	return p.output.Write(data)
}

func (p *launchProgress) setPhase(phase string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.phase = phase
	p.sendState()
}

func (p *launchProgress) allocated() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.created++
	p.sendState()
}

func (p *launchProgress) provisioned(ready bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ready {
		p.ready++
	} else {
		p.failed++
	}
	p.sendState()
}

func (p *launchProgress) preflightDone(phase string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.preflight = true
	p.phase = phase
	p.sendState()
}

func (p *launchProgress) pairedDesktop(confirmed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if confirmed {
		p.paired++
	} else {
		p.failed++
	}
	p.sendState()
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
}

func (p progressState) percent() int {
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

func (p *launchProgress) sendState() {
	if p.program != nil {
		p.program.Send(p.progressState)
	}
}

type progressFinished struct{}

type progressView struct {
	progressState
	width   int
	bar     progress.Model
	spinner spinner.Model
	hidden  bool
}

func newProgressView(state progressState, width int) progressView {
	return progressView{
		progressState: state, width: width,
		bar: progress.New(progress.WithoutPercentage(), progress.WithFillCharacters('━', '─'),
			progress.WithColors(lipgloss.Color("#49D9C4"), lipgloss.Color("#758BFF"), lipgloss.Color("#BC91F5"))),
		spinner: spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(cliAccent)),
	}
}

func (m progressView) Init() tea.Cmd { return m.spinner.Tick }

func (m progressView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case progressState:
		m.progressState = msg
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case progressFinished:
		m.hidden = true
		return m, tea.Quit
	}
	return m, nil
}

func (m progressView) View() tea.View {
	if m.hidden {
		return tea.NewView("")
	}
	return tea.NewView(m.line())
}

func (p progressView) line() string {
	width := p.width
	if width <= 0 {
		width = 80
	}
	percent := p.percent()
	percentage := cliAccent.Render(fmt.Sprintf("%3d%%", percent))
	ready := cliGreen.Render(fmt.Sprintf("%d/%d", p.ready, p.total)) + " ready"
	status := percentage + " · " + ready
	if p.deleting {
		status = percentage + " · " + cliPurple.Render(fmt.Sprintf("%d/%d", p.created, p.total)) + " deleted"
	} else if p.pairing {
		status += " · " + cliPurple.Render(fmt.Sprintf("%d/%d", p.paired, p.total)) + " paired"
	}
	if p.failed > 0 {
		status += " · " + cliAmber.Render(fmt.Sprintf("%d failed", p.failed))
	}
	barWidth := min(22, width-lipgloss.Width(status)-lipgloss.Width(p.phase)-7)
	mark, phase := p.spinner.View(), cliMuted.Render(p.phase)
	if p.meshChecked {
		mark, phase = cliAmber.Render("!"), cliAmber.Render(p.phase)
		if percent == 100 {
			mark, phase = cliGreen.Render("✓"), cliGreen.Render(p.phase)
		}
	}
	line := mark + " " + status
	if barWidth >= 4 {
		p.bar.SetWidth(barWidth)
		line = mark + " " + p.bar.ViewAs(float64(percent)/100) + " " + status
	}
	return ansi.Truncate(line+" · "+phase, max(0, width-1), "…")
}
