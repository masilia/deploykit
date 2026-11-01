package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

type recorder struct{ events []Event }

func (r *recorder) Handle(e Event) { r.events = append(r.events, e) }

func (r *recorder) kinds() string {
	var parts []string
	for _, e := range r.events {
		name := [...]string{"start", "out", "done", "skip", "plan", "failed", "notice"}[e.Kind]
		if e.Step != "" {
			name += ":" + e.Step
		}
		parts = append(parts, name)
	}
	return strings.Join(parts, " ")
}

// fakeClock advances one second per reading, so durations are deterministic.
func fakeClock() func() time.Time {
	t := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return func() time.Time { t = t.Add(time.Second); return t }
}

func TestDoRunsStepsInOrder(t *testing.T) {
	rec := &recorder{}
	r := &Runner{Reporter: rec, Now: fakeClock()}
	err := r.Do(t.Context(),
		Step{Name: "a", Run: func(_ context.Context, out io.Writer) error { _, _ = fmt.Fprintln(out, "hello"); return nil }},
		Step{Name: "b", Run: func(context.Context, io.Writer) error { return nil }},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.kinds(); got != "start:a out:a done:a start:b done:b" {
		t.Errorf("events = %s", got)
	}
	if rec.events[1].Line != "hello" || rec.events[2].Duration <= 0 {
		t.Errorf("output %q, duration %v", rec.events[1].Line, rec.events[2].Duration)
	}
}

func TestDoStopsAtFirstFailure(t *testing.T) {
	rec := &recorder{}
	boom := errors.New("boom")
	ran := false
	err := (&Runner{Reporter: rec}).Do(t.Context(),
		Step{Name: "a", Run: func(context.Context, io.Writer) error { return boom }},
		Step{Name: "b", Run: func(context.Context, io.Writer) error { ran = true; return nil }},
	)
	var se *StepError
	if !errors.As(err, &se) || se.Step != "a" || !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if ran {
		t.Error("step b must not run after a failure")
	}
	if got := rec.kinds(); got != "start:a failed:a" {
		t.Errorf("events = %s", got)
	}
}

func TestOptionalStepFailureDoesNotStop(t *testing.T) {
	rec := &recorder{}
	err := (&Runner{Reporter: rec}).Do(t.Context(),
		Step{Name: "cert", Optional: true, Run: func(context.Context, io.Writer) error { return errors.New("dns not ready") }},
		Step{Name: "next", Run: func(context.Context, io.Writer) error { return nil }},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.kinds(); got != "start:cert failed:cert start:next done:next" {
		t.Errorf("events = %s", got)
	}
}

func TestCheckSkipsDoneWork(t *testing.T) {
	rec := &recorder{}
	ran := false
	err := (&Runner{Reporter: rec}).Do(t.Context(), Step{
		Name:  "site",
		Check: func(context.Context) (bool, error) { return true, nil },
		Run:   func(context.Context, io.Writer) error { ran = true; return nil },
	})
	if err != nil || ran {
		t.Fatalf("err %v, ran %v", err, ran)
	}
	if got := rec.kinds(); got != "start:site skip:site" {
		t.Errorf("events = %s", got)
	}
}

func TestCheckErrorFailsTheStep(t *testing.T) {
	err := (&Runner{}).Do(t.Context(), Step{
		Name:  "x",
		Check: func(context.Context) (bool, error) { return false, errors.New("ssh down") },
		Run:   func(context.Context, io.Writer) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "ssh down") {
		t.Errorf("err = %v", err)
	}
}

func TestDryRunRunsChecksButNeverRun(t *testing.T) {
	rec := &recorder{}
	checked, ran := false, false
	err := (&Runner{Reporter: rec, DryRun: true}).Do(t.Context(),
		Step{Name: "a", Plan: "create the site",
			Check: func(context.Context) (bool, error) { checked = true; return false, nil },
			Run:   func(context.Context, io.Writer) error { ran = true; return nil }},
		Step{Name: "b", Plan: "never shown", Check: func(context.Context) (bool, error) { return true, nil }},
	)
	if err != nil || !checked || ran {
		t.Fatalf("err %v, checked %v, ran %v", err, checked, ran)
	}
	if got := rec.kinds(); got != "start:a plan:a start:b skip:b" {
		t.Errorf("events = %s", got)
	}
	if rec.events[1].Line != "create the site" {
		t.Errorf("plan line = %q", rec.events[1].Line)
	}
}

func TestSecretsAreRedacted(t *testing.T) {
	rec := &recorder{}
	r := &Runner{Reporter: rec, Secrets: []string{"s3cr3t", ""}}
	r.Notice("token is s3cr3t")
	_ = r.Do(t.Context(), Step{Name: "x", Run: func(_ context.Context, out io.Writer) error {
		_, _ = fmt.Fprintln(out, "login with s3cr3t ok")
		return errors.New("failed using s3cr3t")
	}})
	for _, e := range rec.events {
		if strings.Contains(e.Line, "s3cr3t") || (e.Err != nil && strings.Contains(e.Err.Error(), "s3cr3t")) {
			t.Errorf("event leaks the secret: %+v", e)
		}
	}
	if rec.events[0].Line != "token is ***" {
		t.Errorf("notice = %q", rec.events[0].Line)
	}
}

func TestOutputIsSplitIntoLines(t *testing.T) {
	rec := &recorder{}
	_ = (&Runner{Reporter: rec}).Do(t.Context(), Step{Name: "x", Run: func(_ context.Context, out io.Writer) error {
		_, _ = out.Write([]byte("one\ntw"))
		_, _ = out.Write([]byte("o\r\nthree"))
		return nil
	}})
	var lines []string
	for _, e := range rec.events {
		if e.Kind == Output {
			lines = append(lines, e.Line)
		}
	}
	if got := strings.Join(lines, "|"); got != "one|two|three" {
		t.Errorf("lines = %s", got)
	}
}

func TestCancelledContextStopsBetweenSteps(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	err := (&Runner{}).Do(ctx,
		Step{Name: "a", Run: func(context.Context, io.Writer) error { cancel(); return nil }},
		Step{Name: "b", Run: func(context.Context, io.Writer) error { t.Error("b must not run"); return nil }},
	)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestStepWithoutRunFails(t *testing.T) {
	if err := (&Runner{}).Do(t.Context(), Step{Name: "empty"}); err == nil {
		t.Error("a step with neither Check nor Run must fail")
	}
}
