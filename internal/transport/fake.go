package transport

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// Call is one recorded Transport call.
type Call struct {
	Spec  *Spec     // set for Run
	Stdin string    // contents read from Spec.Stdin
	Sync  *SyncSpec // set for Sync
}

type rule struct {
	match func(Spec) bool
	res   Result
}

// Fake records every call and replays canned results. It is how flows are tested without touching a machine, and
// how their command traces are compared with goldens derived from the reference scripts.
type Fake struct {
	Name string

	mu    sync.Mutex
	calls []Call
	rules []rule
}

// NewFake returns a Fake described as name.
func NewFake(name string) *Fake { return &Fake{Name: name} }

// Describe implements Transport.
func (f *Fake) Describe() string { return f.Name }

// On makes every Run whose script, or joined argv, contains substr return res. The first matching rule wins; calls
// that match no rule succeed with empty output. A non-zero res.ExitCode yields a *ExitError, like a real command.
func (f *Fake) On(substr string, res Result) *Fake {
	return f.OnFunc(func(s Spec) bool { return strings.Contains(commandText(s), substr) }, res)
}

// OnFunc is On with an arbitrary matcher.
func (f *Fake) OnFunc(match func(Spec) bool, res Result) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append(f.rules, rule{match: match, res: res})
	return f
}

// Fail is On for a command that exits with code.
func (f *Fake) Fail(substr string, code int, stderr string) *Fake {
	return f.On(substr, Result{ExitCode: code, Stderr: stderr})
}

// Run implements Transport.
func (f *Fake) Run(_ context.Context, spec Spec) (Result, error) {
	if err := spec.validate(); err != nil {
		return Result{}, err
	}
	call := Call{Spec: &spec}
	if spec.Stdin != nil {
		data, err := io.ReadAll(spec.Stdin)
		if err != nil {
			return Result{}, err
		}
		call.Stdin = string(data)
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	var res Result
	for _, r := range f.rules {
		if r.match(spec) {
			res = r.res
			break
		}
	}
	f.mu.Unlock()

	if spec.Out != nil {
		_, _ = io.WriteString(spec.Out, res.Stdout+res.Stderr)
	}
	if res.ExitCode != 0 {
		return res, &ExitError{Code: res.ExitCode, Stderr: res.Stderr, Where: f.Name}
	}
	return res, nil
}

// Sync implements Transport.
func (f *Fake) Sync(_ context.Context, spec SyncSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Sync: &spec})
	return nil
}

// Calls returns a copy of the recorded calls.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// Trace renders the recorded calls as stable text for golden files. Secret values are masked; Env values are shown.
func (f *Fake) Trace() string {
	var b strings.Builder
	for _, c := range f.Calls() {
		if c.Sync != nil {
			b.WriteString(syncLine(*c.Sync) + "\n")
			continue
		}
		b.WriteString("run " + f.Name + "\n")
		writeVars(&b, "env", c.Spec.Env, false)
		writeVars(&b, "secret", c.Spec.Secrets, true)
		if c.Spec.Dir != "" {
			b.WriteString("  dir " + c.Spec.Dir + "\n")
		}
		if c.Spec.Script != "" {
			for _, line := range strings.Split(strings.TrimRight(c.Spec.Script, "\n"), "\n") {
				b.WriteString("  | " + line + "\n")
			}
		} else {
			b.WriteString("  $ " + QuoteArgs(c.Spec.Argv) + "\n")
		}
		if c.Stdin != "" {
			b.WriteString("  stdin <" + fmt.Sprintf("%d bytes", len(c.Stdin)) + ">\n")
		}
	}
	return b.String()
}

func writeVars(b *strings.Builder, label string, m map[string]string, mask bool) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := m[k]
		if mask {
			v = "***"
		}
		b.WriteString("  " + label + " " + k + "=" + v + "\n")
	}
}

func syncLine(s SyncSpec) string {
	line := "sync " + s.Src + " -> " + s.Dst
	if s.Delete {
		line += " --delete"
	}
	if s.LinkDest != "" {
		line += " --link-dest=" + s.LinkDest
	}
	for _, e := range s.Excludes {
		line += " --exclude=" + e
	}
	return line
}

func commandText(s Spec) string {
	if s.Script != "" {
		return s.Script
	}
	return strings.Join(s.Argv, " ")
}
