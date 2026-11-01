package ui

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/masilia/deploykit/internal/runner"
)

func play(p *Pretty, events ...runner.Event) {
	for _, e := range events {
		p.Handle(e)
	}
}

func TestPrettyFoldsTheOutputOfASuccessfulStep(t *testing.T) {
	var buf bytes.Buffer
	p := NewPretty(&buf, false, false)
	play(p,
		runner.Event{Kind: runner.StepStart, Step: "build"},
		runner.Event{Kind: runner.Output, Step: "build", Line: "compiling 2000 modules"},
		runner.Event{Kind: runner.StepDone, Step: "build", Duration: 11200 * time.Millisecond},
		runner.Event{Kind: runner.StepSkip, Step: "lock", Line: "already done"},
		runner.Event{Kind: runner.StepPlan, Step: "upload", Line: "rsync the release"},
		runner.Event{Kind: runner.Notice, Line: "release 1.2.3 is already in the registry"},
	)
	out := buf.String()
	if strings.Contains(out, "compiling") {
		t.Errorf("the output of a successful step is folded away:\n%s", out)
	}
	for _, want := range []string{"› build", "✔ build", "11.2s", "✔ lock", "already done", "◌ upload", "would rsync the release", "› release 1.2.3 is already in the registry"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("a buffer is not a terminal, no escape sequence expected:\n%q", out)
	}
}

func TestPrettyVerboseShowsTheOutput(t *testing.T) {
	var buf bytes.Buffer
	p := NewPretty(&buf, true, false)
	play(p,
		runner.Event{Kind: runner.StepStart, Step: "build"},
		runner.Event{Kind: runner.Output, Step: "build", Line: "compiling 2000 modules"},
		runner.Event{Kind: runner.StepDone, Step: "build", Duration: time.Second},
	)
	if !strings.Contains(buf.String(), "      compiling 2000 modules") {
		t.Errorf("verbose output lacks the line:\n%s", buf.String())
	}
}

func TestPrettyShowsTheOutputThatLedToAFailure(t *testing.T) {
	var buf bytes.Buffer
	p := NewPretty(&buf, false, false)
	play(p,
		runner.Event{Kind: runner.StepStart, Step: "migrate"},
		runner.Event{Kind: runner.Output, Step: "migrate", Line: "ERROR: column x does not exist"},
		runner.Event{Kind: runner.StepFailed, Step: "migrate", Err: errors.New("exit status 1"), Duration: 1400 * time.Millisecond},
	)
	out := buf.String()
	for _, want := range []string{"✖ migrate", "1.4s", "      ERROR: column x does not exist", "    exit status 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("failure output lacks %q:\n%s", want, out)
		}
	}
}

func TestPrettyKeepsOnlyTheEndOfAVeryLongFailureOutput(t *testing.T) {
	var buf bytes.Buffer
	p := NewPretty(&buf, false, false)
	p.Handle(runner.Event{Kind: runner.StepStart, Step: "build"})
	for i := 0; i < 100; i++ {
		p.Handle(runner.Event{Kind: runner.Output, Step: "build", Line: "line " + string(rune('A'+i%26)) + strings.Repeat("x", i)})
	}
	p.Handle(runner.Event{Kind: runner.StepFailed, Step: "build", Err: errors.New("boom")})
	if n := strings.Count(buf.String(), "\n"); n > 40 {
		t.Errorf("%d lines printed, the output of a failure is cut to its end", n)
	}
}

// The spinner redraws a line in place while a step runs; it must not race with the events (run with -race).
func TestPrettyAnimatesWithoutRacing(t *testing.T) {
	var buf lockedBuffer
	p := NewPretty(&buf, true, true)
	p.Handle(runner.Event{Kind: runner.StepStart, Step: "install"})
	time.Sleep(250 * time.Millisecond)
	p.Handle(runner.Event{Kind: runner.Output, Step: "install", Line: "added 12 packages"})
	time.Sleep(120 * time.Millisecond)
	p.Handle(runner.Event{Kind: runner.StepDone, Step: "install", Duration: 400 * time.Millisecond})
	p.Handle(runner.Event{Kind: runner.StepStart, Step: "build"})
	p.Handle(runner.Event{Kind: runner.StepFailed, Step: "build", Err: errors.New("no"), Duration: time.Second})
	out := buf.String()
	if !strings.Contains(out, "\r\x1b[2K") || !strings.Contains(out, "⠋ install") || !strings.Contains(out, "✔ install") || !strings.Contains(out, "✖ build") {
		t.Errorf("animated output:\n%q", out)
	}
	if strings.Contains(out, "⠋ build") && strings.Contains(out, "⠙ install") == false {
		t.Errorf("the spinner should have advanced during the first step:\n%q", out)
	}
}

// lockedBuffer lets the test read what the spinner goroutine wrote.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestTableAlignsStyledCellsOnTheirVisibleWidth(t *testing.T) {
	var buf bytes.Buffer
	s := NewStyles(&buf)
	out := s.Table([]string{"TARGET", "STATE"}, [][]string{{"prod", "OK"}, {"staging-eu", "unreachable"}}, func(col int, v string) lipgloss.Style {
		if col == 1 {
			return s.State(v)
		}
		return s.Plain()
	})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 || !strings.Contains(lines[0], "TARGET") || !strings.Contains(lines[1], "──────") {
		t.Fatalf("table:\n%s", out)
	}
	col := strings.Index(lines[2], "OK")
	if strings.Index(lines[3], "unreachable") != col || strings.Index(lines[0], "STATE") != col {
		t.Errorf("columns are not aligned:\n%s", out)
	}
}

func TestPrettyAlignsDetailsWithTheDurationColumn(t *testing.T) {
	var buf bytes.Buffer
	p := NewPretty(&buf, false, false)
	play(p,
		runner.Event{Kind: runner.StepSkip, Step: "lock", Line: "already done"},
		runner.Event{Kind: runner.StepPlan, Step: "upload", Line: "rsync the release"},
		runner.Event{Kind: runner.StepStart, Step: "build"},
		runner.Event{Kind: runner.StepDone, Step: "build", Duration: 3 * time.Second},
	)
	var cols []int
	for _, l := range strings.Split(buf.String(), "\n") {
		for _, marker := range []string{"already done", "would rsync", "3s"} {
			if i := strings.Index(l, marker); i >= 0 {
				cols = append(cols, len([]rune(l[:i])))
			}
		}
	}
	if len(cols) != 3 || cols[0] != cols[1] || cols[1] != cols[2] {
		t.Errorf("details must start in one column, got %v:\n%s", cols, buf.String())
	}
}
