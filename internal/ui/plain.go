// Package ui renders runner events. Plain is the always-available renderer: timestamped lines with no colour and no
// cursor control, safe for CI, cron, pipes and NO_COLOR terminals. The TUI renderer is built on the same events.
package ui

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/masilia/deploykit/internal/runner"
)

// Plain writes one line per event.
type Plain struct {
	mu sync.Mutex
	w  io.Writer
	// Verbose also prints command output of steps that succeed. Failed steps always show their output.
	Verbose bool

	buffered map[string][]string
}

// NewPlain returns a Plain renderer writing to w. Output of a step is held back and shown only if the step fails,
// unless verbose is set (the "folded output" of the TUI design, in a form a log can express).
func NewPlain(w io.Writer, verbose bool) *Plain {
	return &Plain{w: w, Verbose: verbose, buffered: map[string][]string{}}
}

// Handle implements runner.Reporter.
func (p *Plain) Handle(e runner.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ts := e.Time.Format("15:04:05")
	switch e.Kind {
	case runner.StepStart:
		p.line(ts, "› %s", e.Step)
	case runner.Output:
		if p.Verbose {
			p.line(ts, "    %s", e.Line)
		} else {
			p.buffered[e.Step] = append(p.buffered[e.Step], e.Line)
		}
	case runner.StepDone:
		delete(p.buffered, e.Step)
		p.line(ts, "✔ %s (%s)", e.Step, round(e.Duration))
	case runner.StepSkip:
		delete(p.buffered, e.Step)
		p.line(ts, "✔ %s (%s)", e.Step, e.Line)
	case runner.StepPlan:
		if e.Line != "" {
			p.line(ts, "◌ %s: would %s", e.Step, e.Line)
		} else {
			p.line(ts, "◌ %s: would run", e.Step)
		}
	case runner.StepFailed:
		for _, l := range p.buffered[e.Step] {
			p.line(ts, "    %s", l)
		}
		delete(p.buffered, e.Step)
		p.line(ts, "✖ %s (%s): %v", e.Step, round(e.Duration), e.Err)
	case runner.Notice:
		p.line(ts, "%s", e.Line)
	}
}

func (p *Plain) line(ts, format string, args ...any) {
	_, _ = fmt.Fprintf(p.w, "[%s] %s\n", ts, fmt.Sprintf(format, args...))
}

func round(d time.Duration) time.Duration {
	if d >= time.Second {
		return d.Round(100 * time.Millisecond)
	}
	return d.Round(time.Millisecond)
}
