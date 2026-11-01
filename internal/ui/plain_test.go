package ui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/masilia/deploykit/internal/runner"
)

func clock() func() time.Time {
	t := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	return func() time.Time { t = t.Add(1500 * time.Millisecond); return t }
}

func run(t *testing.T, verbose, dry bool) string {
	t.Helper()
	var out bytes.Buffer
	r := &runner.Runner{Reporter: NewPlain(&out, verbose), DryRun: dry, Now: clock(), Secrets: []string{"hunter2"}}
	r.Notice("target srv, token hunter2")
	_ = r.Do(context.Background(),
		runner.Step{Name: "build", Plan: "run npm run build", Run: func(_ context.Context, w io.Writer) error {
			_, _ = fmt.Fprintln(w, "compiled 3 pages")
			return nil
		}},
		runner.Step{Name: "site", Check: func(context.Context) (bool, error) { return true, nil }},
		runner.Step{Name: "upload", Run: func(_ context.Context, w io.Writer) error {
			_, _ = fmt.Fprintln(w, "rsync: connection reset")
			return errors.New("exit status 12")
		}},
	)
	return out.String()
}

func TestPlainFoldsOutputOfSuccessfulSteps(t *testing.T) {
	want := `[10:00:01] target srv, token ***
[10:00:04] › build
[10:00:09] ✔ build (4.5s)
[10:00:12] › site
[10:00:15] ✔ site (already done)
[10:00:18] › upload
[10:00:22]     rsync: connection reset
[10:00:22] ✖ upload (4.5s): exit status 12
`
	if got := run(t, false, false); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestPlainVerboseShowsAllOutput(t *testing.T) {
	if got := run(t, true, false); !bytes.Contains([]byte(got), []byte("    compiled 3 pages\n")) {
		t.Errorf("verbose output lacks the step output:\n%s", got)
	}
}

func TestPlainDryRun(t *testing.T) {
	got := run(t, false, true)
	if !bytes.Contains([]byte(got), []byte("◌ build: would run npm run build\n")) || !bytes.Contains([]byte(got), []byte("◌ upload: would run\n")) {
		t.Errorf("dry run lines missing:\n%s", got)
	}
	if bytes.Contains([]byte(got), []byte("✖")) {
		t.Errorf("a dry run cannot fail here:\n%s", got)
	}
}

func TestPlainHasNoControlSequences(t *testing.T) {
	for _, b := range []byte(run(t, true, false)) {
		if b == 0x1b || b == '\r' {
			t.Fatalf("plain output contains control byte %#x", b)
		}
	}
}
