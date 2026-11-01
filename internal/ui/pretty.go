package ui

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/masilia/deploykit/internal/runner"
)

// nameWidth is the column the durations line up on.
const nameWidth = 34

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Pretty renders runner events for a person at a terminal: one line per step that turns from a spinner into a check
// mark with its duration, command output folded away unless the step fails (or --verbose), failures shown in red with
// the output that led to them. Plain stays the renderer for CI, cron and pipes.
type Pretty struct {
	mu sync.Mutex
	w  io.Writer
	st *Styles
	// Verbose also shows the command output of steps that succeed.
	Verbose bool

	animate  bool
	buffered map[string][]string

	running string // the step whose spinner is on screen
	stop    chan struct{}
	done    chan struct{}
}

// NewPretty returns a Pretty renderer. With animate, the running step shows a spinner redrawn in place (cursor control);
// without it each step prints a start line and an end line, which is also what a log capture needs.
func NewPretty(w io.Writer, verbose, animate bool) *Pretty {
	return &Pretty{w: w, st: NewStyles(w), Verbose: verbose, animate: animate, buffered: map[string][]string{}}
}

// Handle implements runner.Reporter.
func (p *Pretty) Handle(e runner.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch e.Kind {
	case runner.StepStart:
		p.start(e.Step)
	case runner.Output:
		if p.Verbose {
			p.pause()
			p.print("      " + p.st.Muted.Render(e.Line))
			p.resume()
		} else {
			p.buffered[e.Step] = append(p.buffered[e.Step], e.Line)
		}
	case runner.StepDone:
		p.finish()
		p.print("  " + p.st.Success.Render(IconOK) + " " + p.pad(e.Step, round(e.Duration)))
	case runner.StepSkip:
		p.finish()
		p.print("  " + p.st.Success.Render(IconOK) + " " + p.padText(e.Step, p.st.Muted.Render(e.Line)))
	case runner.StepPlan:
		p.finish()
		what := "would run"
		if e.Line != "" {
			what = "would " + e.Line
		}
		p.print("  " + p.st.Accent.Render(IconPlan) + " " + p.padText(e.Step, p.st.Muted.Render(what)))
	case runner.StepFailed:
		p.finish()
		p.print("  " + p.st.Danger.Render(IconFail) + " " + p.st.Danger.Render(p.pad(e.Step, round(e.Duration))))
		lines := p.buffered[e.Step]
		if len(lines) > 25 {
			lines = append([]string{"…"}, lines[len(lines)-24:]...)
		}
		for _, l := range lines {
			p.print("      " + p.st.Muted.Render(l))
		}
		p.print("    " + p.st.Danger.Render(fmt.Sprint(e.Err)))
	case runner.Notice:
		p.print("  " + p.st.Accent.Render(IconStep) + " " + e.Line)
	}
	switch e.Kind {
	case runner.StepDone, runner.StepSkip, runner.StepPlan, runner.StepFailed:
		delete(p.buffered, e.Step)
	}
}

func (p *Pretty) pad(name string, d time.Duration) string {
	return p.padText(name, p.st.Muted.Render(d.String()))
}

// padText puts text in the column after the step names.
func (p *Pretty) padText(name, text string) string {
	label := name
	if w := lipgloss.Width(name); w < nameWidth {
		label += strings.Repeat(" ", nameWidth-w)
	} else {
		label += " "
	}
	return label + " " + text
}

func (p *Pretty) print(line string) { _, _ = fmt.Fprintln(p.w, line) }

func (p *Pretty) start(step string) {
	if !p.animate {
		p.print("  " + p.st.Accent.Render(IconStep) + " " + step)
		return
	}
	p.running = step
	p.stop, p.done = make(chan struct{}), make(chan struct{})
	p.draw(0)
	go p.spin(p.stop, p.done, step)
}

// spin redraws the spinner line until stopped. It takes the lock for each frame, so it never interleaves with a print.
func (p *Pretty) spin(stop, done chan struct{}, step string) {
	defer close(done)
	t := time.NewTicker(90 * time.Millisecond)
	defer t.Stop()
	for i := 1; ; i++ {
		select {
		case <-stop:
			return
		case <-t.C:
			p.mu.Lock()
			if p.running == step {
				p.draw(i)
			}
			p.mu.Unlock()
		}
	}
}

func (p *Pretty) draw(frame int) {
	_, _ = fmt.Fprintf(p.w, "\r\x1b[2K  %s %s", p.st.Accent.Render(spinnerFrames[frame%len(spinnerFrames)]), p.running)
}

// pause removes the spinner line so a line of output can be printed above it; resume puts it back.
func (p *Pretty) pause() {
	if p.animate && p.running != "" {
		_, _ = fmt.Fprint(p.w, "\r\x1b[2K")
	}
}

func (p *Pretty) resume() {
	if p.animate && p.running != "" {
		p.draw(0)
	}
}

// finish stops the spinner and clears its line, leaving the cursor where the step's final line goes.
func (p *Pretty) finish() {
	if !p.animate || p.running == "" {
		return
	}
	stop, done := p.stop, p.done
	p.running = ""
	p.mu.Unlock() // the spinner goroutine needs the lock to notice it was stopped
	close(stop)
	<-done
	p.mu.Lock()
	_, _ = fmt.Fprint(p.w, "\r\x1b[2K")
}
