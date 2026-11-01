// Package runner executes named steps and reports what happens as events (decision D5).
//
// Renderers never see commands, only events, so the same flow drives the plain log used by CI and cron and the TUI.
// Steps are idempotent: Check says whether the work is already done, Run does it. Rollback belongs to the runtime
// that owns the flow, not to individual steps.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Kind identifies an event.
type Kind int

// Event kinds, in the order a step emits them.
const (
	StepStart  Kind = iota // a step begins
	Output                 // one line of command output
	StepDone               // the step ran and succeeded
	StepSkip               // Check reported the work already done
	StepPlan               // dry run: the step would run
	StepFailed             // the step failed
	Notice                 // free-form message between steps
)

// Event is what a Reporter receives.
type Event struct {
	Kind     Kind
	Step     string
	Line     string        // Output, Notice, and the reason of StepSkip / StepPlan
	Err      error         // StepFailed
	Duration time.Duration // StepDone, StepFailed, StepSkip
	Time     time.Time
}

// Reporter consumes events. Implementations must be safe to call from one goroutine at a time.
type Reporter interface {
	Handle(Event)
}

// Step is one unit of work.
type Step struct {
	Name string
	// Plan describes what Run would do, shown in a dry run.
	Plan string
	// Check, when set, returns true if the step's effect is already in place. It must be read-only: it also runs in a
	// dry run.
	Check func(ctx context.Context) (done bool, err error)
	// Run does the work. Everything written to out is reported as Output lines.
	Run func(ctx context.Context, out io.Writer) error
	// Optional marks a step whose failure is reported but does not stop the run (a certificate that cannot be issued
	// yet, a failed off-site copy).
	Optional bool
}

// StepError is the error returned when a step fails.
type StepError struct {
	Step string
	Err  error
}

func (e *StepError) Error() string { return fmt.Sprintf("step %q failed: %v", e.Step, e.Err) }
func (e *StepError) Unwrap() error { return e.Err }

// Runner runs steps and emits events.
type Runner struct {
	Reporter Reporter
	DryRun   bool
	// Secrets are replaced by *** in every Output line, Notice and error message before it is reported.
	Secrets []string
	// Now is the clock, replaced in tests. Default time.Now.
	Now func() time.Time
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) emit(e Event) {
	if r.Reporter == nil {
		return
	}
	e.Time = r.now()
	e.Line = r.redact(e.Line)
	if e.Err != nil {
		if msg := r.redact(e.Err.Error()); msg != e.Err.Error() {
			e.Err = errors.New(msg)
		}
	}
	r.Reporter.Handle(e)
}

func (r *Runner) redact(s string) string {
	for _, secret := range r.Secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "***")
		}
	}
	return s
}

// Notice reports a message that belongs to no step.
func (r *Runner) Notice(format string, args ...any) {
	r.emit(Event{Kind: Notice, Line: fmt.Sprintf(format, args...)})
}

// Do runs the steps in order and stops at the first failure of a non-optional step.
func (r *Runner) Do(ctx context.Context, steps ...Step) error {
	for _, s := range steps {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.one(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) one(ctx context.Context, s Step) error {
	start := r.now()
	since := func() time.Duration { return r.now().Sub(start) }
	r.emit(Event{Kind: StepStart, Step: s.Name})

	if s.Check != nil {
		done, err := s.Check(ctx)
		if err != nil {
			return r.fail(s, fmt.Errorf("check: %w", err), since())
		}
		if done {
			r.emit(Event{Kind: StepSkip, Step: s.Name, Line: "already done", Duration: since()})
			return nil
		}
	}
	if r.DryRun {
		r.emit(Event{Kind: StepPlan, Step: s.Name, Line: s.Plan})
		return nil
	}
	if s.Run == nil {
		return r.fail(s, errors.New("step has nothing to run"), since())
	}
	out := &lineWriter{emit: func(line string) { r.emit(Event{Kind: Output, Step: s.Name, Line: line}) }}
	err := s.Run(ctx, out)
	out.Flush()
	if err != nil {
		return r.fail(s, err, since())
	}
	r.emit(Event{Kind: StepDone, Step: s.Name, Duration: since()})
	return nil
}

func (r *Runner) fail(s Step, err error, d time.Duration) error {
	r.emit(Event{Kind: StepFailed, Step: s.Name, Err: err, Duration: d})
	if s.Optional {
		return nil
	}
	return &StepError{Step: s.Name, Err: err}
}

// lineWriter turns a byte stream into one callback per line.
type lineWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	emit func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		data := w.buf.Bytes()
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		w.emit(strings.TrimRight(string(data[:i]), "\r"))
		w.buf.Next(i + 1)
	}
	return len(p), nil
}

// Flush reports a trailing line that had no newline.
func (w *lineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() > 0 {
		w.emit(strings.TrimRight(w.buf.String(), "\r\n"))
		w.buf.Reset()
	}
}
