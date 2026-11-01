package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// Local runs commands on this machine.
type Local struct {
	// Shell runs Script specs, reading the script on stdin. Default: sh -s.
	Shell []string
	// Rsync is the rsync binary. Default: rsync.
	Rsync string
}

// NewLocal returns a Local transport with default settings.
func NewLocal() *Local { return &Local{} }

// Describe implements Transport.
func (l *Local) Describe() string { return "local" }

// Run implements Transport.
func (l *Local) Run(ctx context.Context, spec Spec) (Result, error) {
	if err := spec.validate(); err != nil {
		return Result{}, err
	}
	var cmd *exec.Cmd
	where := ""
	if spec.Script != "" {
		shell := l.Shell
		if len(shell) == 0 {
			shell = []string{"sh", "-s"}
		}
		cmd = exec.CommandContext(ctx, shell[0], shell[1:]...) //nolint:gosec // the shell is chosen by the caller
		cmd.Stdin = strings.NewReader(guardStdin(spec.Script))
		where = strings.Join(shell, " ")
	} else {
		cmd = exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...) //nolint:gosec // argv is the point of this call
		cmd.Stdin = spec.Stdin
		where = spec.Argv[0]
	}
	cmd.Dir = spec.Dir
	if len(spec.Env)+len(spec.Secrets) > 0 {
		cmd.Env = os.Environ()
		for _, m := range []map[string]string{spec.Env, spec.Secrets} {
			for k, v := range m {
				cmd.Env = append(cmd.Env, k+"="+v)
			}
		}
	}
	return capture(cmd, spec.Out, where)
}

// Sync implements Transport with a local rsync.
func (l *Local) Sync(ctx context.Context, spec SyncSpec) error {
	bin := l.Rsync
	if bin == "" {
		bin = "rsync"
	}
	cmd := exec.CommandContext(ctx, bin, rsyncArgs(spec, "")...) //nolint:gosec // arguments are built by rsyncArgs
	_, err := capture(cmd, nil, "rsync")
	return err
}

// syncWriter serialises writes so stdout and stderr can share one sink.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// capture runs cmd, collecting stdout and stderr and mirroring both to out when it is set.
func capture(cmd *exec.Cmd, out io.Writer, where string) (Result, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if out != nil {
		shared := &syncWriter{w: out}
		cmd.Stdout = io.MultiWriter(&stdout, shared)
		cmd.Stderr = io.MultiWriter(&stderr, shared)
	}
	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return res, nil
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
		return res, &ExitError{Code: res.ExitCode, Stderr: res.Stderr, Where: where}
	default:
		return res, fmt.Errorf("%s: %w", where, err)
	}
}
