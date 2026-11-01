// Package transport is the single door through which deploykit touches machines (ADR 0003).
//
// Every effect, on the workstation or on a server, is a Transport call: Local runs on this machine, SSH drives a
// server with the system ssh and rsync, Fake records the calls for tests. Flows are written once against the
// interface and compared with goldens through Fake.Trace.
package transport

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// Spec describes one command.
//
// Exactly one of Script and Argv is set. A Script is a shell script delivered on the stdin of a shell, never as a
// quoted argument, so apostrophes and newlines are safe (pitfall 4 of the Node scripts). Argv is a program and its
// arguments; Local executes it directly, SSH quotes it.
type Spec struct {
	Script string
	Argv   []string

	// Env is exported before the command runs. Secrets is exported the same way but masked in traces and logs.
	// Neither ever appears in an argument list: on SSH they travel inside the script on stdin.
	Env     map[string]string
	Secrets map[string]string

	// Dir is the working directory (Local) or the directory to cd into first (SSH).
	Dir string

	// Stdin is data fed to an Argv program (docker login --password-stdin). It cannot be combined with Script,
	// because a Script already occupies stdin, nor with Secrets on SSH.
	Stdin io.Reader

	// Out, when set, receives stdout and stderr interleaved as the command produces them.
	Out io.Writer
}

// Result is the captured outcome of a command.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// ExitError reports a command that ran and exited non-zero. Run still returns the Result.
type ExitError struct {
	Code   int
	Stderr string
	Where  string
}

func (e *ExitError) Error() string {
	msg := fmt.Sprintf("%s: exit status %d", e.Where, e.Code)
	if s := strings.TrimSpace(e.Stderr); s != "" {
		if len(s) > 300 {
			s = s[:300] + "…"
		}
		msg += ": " + s
	}
	return msg
}

// SyncSpec describes an rsync of Src (this machine) to Dst (a path on the transport's machine).
// Trailing slashes keep rsync's meaning: "dir/" copies the contents.
type SyncSpec struct {
	Src      string
	Dst      string
	Delete   bool
	LinkDest string   // hard-link unchanged files from this directory on the destination; empty to disable
	Excludes []string // --exclude patterns
	Stats    bool     // --info=stats1
	// Checksum compares file contents instead of size and modification time (-c). Use it with LinkDest when the source
	// has no meaningful mtimes (a git export), so unchanged files are hard-linked and changed ones are always sent.
	Checksum bool
}

// Transport runs commands and copies files on one machine.
type Transport interface {
	// Run executes spec. A non-zero exit returns the Result and a *ExitError.
	Run(ctx context.Context, spec Spec) (Result, error)
	// Sync copies Src to Dst with rsync.
	Sync(ctx context.Context, spec SyncSpec) error
	// Describe names the machine for logs: "local" or "user@host:port".
	Describe() string
}

// validate checks the invariants shared by every implementation.
func (s Spec) validate() error {
	if (s.Script == "") == (len(s.Argv) == 0) {
		return fmt.Errorf("transport: exactly one of Script and Argv must be set")
	}
	if s.Stdin != nil && s.Script != "" {
		return fmt.Errorf("transport: Stdin cannot be combined with Script")
	}
	return nil
}
