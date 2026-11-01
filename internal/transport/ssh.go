package transport

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Retries and their pauses. Only a connection that was never established is retried (see connectionNotEstablished):
// once the remote script has started, running it again could repeat its effects.
const (
	defaultRetries    = 2
	defaultRetryDelay = 2 * time.Second
)

// SSH drives a server with the system ssh and rsync, so the agent, ~/.ssh/config and ProxyJump all apply (ADR 0003).
type SSH struct {
	Host string
	User string // empty: ssh decides (config or current user)
	Port int    // 0 means 22

	// Bin and Rsync override the binaries (tests use scripts that record their arguments).
	Bin   string
	Rsync string

	// Shell is the remote shell reading the script on stdin. Default: bash -l -s, a login shell so PATH is set.
	Shell string
	// Prelude runs before every script, for example the nvm loader of a CloudPanel site user.
	Prelude string
	// ExtraOpts are added to every ssh invocation, for example -i, -J.
	ExtraOpts []string
	// ConnectTimeout in seconds. Default 10.
	ConnectTimeout int
	// Retries is how many times a connection that could not be established is retried, with a doubling pause. Zero
	// means the default (2, so three attempts); a negative value disables retries.
	Retries int
	// RetryDelay is the first pause. Default 2 s.
	RetryDelay time.Duration

	sleep func(ctx context.Context, d time.Duration) error // replaced in tests
}

func (s *SSH) retries() int {
	switch {
	case s.Retries < 0:
		return 0
	case s.Retries == 0:
		return defaultRetries
	}
	return s.Retries
}

func (s *SSH) retryDelay() time.Duration {
	if s.RetryDelay > 0 {
		return s.RetryDelay
	}
	return defaultRetryDelay
}

func (s *SSH) wait(ctx context.Context, d time.Duration) error {
	if s.sleep != nil {
		return s.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// connectionNotEstablished reports an ssh that failed before it could start the remote command: it exited 255 with
// one of its own connection-phase messages and no output at all. Authentication and host-key errors are not in the
// list: retrying them cannot help.
func connectionNotEstablished(res Result) bool {
	if res.ExitCode != 255 || res.Stdout != "" {
		return false
	}
	for _, m := range []string{
		"ssh: connect to host", "ssh: Could not resolve hostname", "kex_exchange_identification",
		"Connection timed out during banner exchange",
	} {
		if strings.Contains(res.Stderr, m) {
			return true
		}
	}
	return false
}

var sessionTimedOut = regexp.MustCompile(`Connection to \S+ port \d+ timed out`)

// connectionLost reports an ssh session that broke while the remote command was running.
func connectionLost(res Result) bool {
	if res.ExitCode != 255 || connectionNotEstablished(res) {
		return false
	}
	for _, m := range []string{"Broken pipe", "closed by remote host", "Connection reset by peer", "Timeout, server", "client_loop"} {
		if strings.Contains(res.Stderr, m) {
			return true
		}
	}
	// OpenSSH 9: a session whose keep-alives went unanswered ends with "Connection to <host> port <n> timed out".
	return sessionTimedOut.MatchString(res.Stderr)
}

// transientRsync reports an rsync failure worth another attempt. rsync is idempotent (the same sources, the same
// destination), so unlike a script it can be repeated after any interrupted transfer.
func transientRsync(res Result) bool {
	switch res.ExitCode {
	case 10, 12, 30, 35, 255:
	default:
		return false
	}
	low := strings.ToLower(res.Stderr)
	for _, m := range []string{"permission denied", "no space left", "no such file", "host key verification failed"} {
		if strings.Contains(low, m) {
			return false
		}
	}
	return true
}

// Describe implements Transport.
func (s *SSH) Describe() string {
	return fmt.Sprintf("%s:%d", s.target(), s.port())
}

func (s *SSH) target() string {
	if s.User == "" {
		return s.Host
	}
	return s.User + "@" + s.Host
}

func (s *SSH) port() int {
	if s.Port == 0 {
		return 22
	}
	return s.Port
}

func (s *SSH) bin() string {
	if s.Bin == "" {
		return "ssh"
	}
	return s.Bin
}

// options returns the ssh options without the target. They double as the -e value for rsync.
func (s *SSH) options() []string {
	timeout := s.ConnectTimeout
	if timeout == 0 {
		timeout = 10
	}
	opts := []string{
		"-p", strconv.Itoa(s.port()),
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=" + strconv.Itoa(timeout),
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "StrictHostKeyChecking=accept-new",
	}
	return append(opts, s.ExtraOpts...)
}

// Run implements Transport.
//
// A Script, or an Argv without Stdin, is sent as a script on the stdin of the remote shell, together with the Env and
// Secrets exports: nothing sensitive reaches an argument list. An Argv with Stdin must keep stdin for the data, so
// the command line travels as a quoted argument and Secrets are refused.
//
// A connection that cannot be established (refused, timed out, unresolvable) is retried with a doubling pause. A
// session that breaks while the command runs is not: the error says its effect is unknown.
func (s *SSH) Run(ctx context.Context, spec Spec) (Result, error) {
	delay := s.retryDelay()
	for attempt := 0; ; attempt++ {
		res, err := s.runOnce(ctx, spec)
		var ee *ExitError
		if err == nil || !errors.As(err, &ee) {
			return res, err
		}
		if connectionNotEstablished(res) && spec.Stdin == nil && attempt < s.retries() {
			if spec.Out != nil {
				_, _ = fmt.Fprintf(spec.Out, "ssh: %s unreachable, retrying in %s (attempt %d of %d)\n", s.Describe(), delay, attempt+2, s.retries()+1)
			}
			if s.wait(ctx, delay) != nil {
				return res, err
			}
			delay *= 2
			continue
		}
		if connectionLost(res) {
			ee.Hint = "the connection broke while the command was running: it may have partly run, check the state before running it again"
		}
		return res, err
	}
}

func (s *SSH) runOnce(ctx context.Context, spec Spec) (Result, error) {
	if err := spec.validate(); err != nil {
		return Result{}, err
	}
	shell := strings.Fields(s.Shell)
	if len(shell) == 0 {
		shell = []string{"bash", "-l", "-s"}
	}
	args := append(s.options(), s.target())

	cmd := &exec.Cmd{}
	var err error
	if spec.Stdin != nil {
		if len(spec.Secrets) > 0 {
			return Result{}, fmt.Errorf("transport: Secrets cannot travel with Stdin over ssh; pass the secret as Stdin")
		}
		var exports string
		if exports, err = exportLines(spec.Env); err != nil {
			return Result{}, err
		}
		line := exports + s.Prelude + "\n"
		if spec.Dir != "" {
			line += "cd " + Quote(spec.Dir) + " && "
		}
		line += "exec " + QuoteArgs(spec.Argv)
		args = append(args, "bash", "-l", "-c", Quote(line))
		cmd = exec.CommandContext(ctx, s.bin(), args...) //nolint:gosec // arguments are built from validated parts
		cmd.Stdin = spec.Stdin
	} else {
		var script string
		if script, err = s.buildScript(spec); err != nil {
			return Result{}, err
		}
		args = append(args, shell...)
		cmd = exec.CommandContext(ctx, s.bin(), args...) //nolint:gosec // arguments are built from validated parts
		cmd.Stdin = strings.NewReader(script)
	}
	return capture(cmd, spec.Out, s.Describe())
}

func (s *SSH) buildScript(spec Spec) (string, error) {
	exports, err := exportLines(spec.Env, spec.Secrets)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if s.Prelude != "" {
		b.WriteString(s.Prelude + "\n")
	}
	b.WriteString(exports)
	if spec.Dir != "" {
		b.WriteString("cd " + Quote(spec.Dir) + " || exit 1\n")
	}
	if spec.Script != "" {
		b.WriteString(guardStdin(spec.Script))
	} else {
		b.WriteString("exec " + QuoteArgs(spec.Argv) + "\n")
	}
	return b.String(), nil
}

// Sync implements Transport: rsync over the same ssh options.
func (s *SSH) Sync(ctx context.Context, spec SyncSpec) error {
	bin := s.Rsync
	if bin == "" {
		bin = "rsync"
	}
	remoteShell := s.bin() + " " + QuoteArgs(s.options())
	spec.Dst = s.target() + ":" + spec.Dst
	delay := s.retryDelay()
	for attempt := 0; ; attempt++ {
		cmd := exec.CommandContext(ctx, bin, rsyncArgs(spec, remoteShell)...) //nolint:gosec // arguments are built by rsyncArgs
		res, err := capture(cmd, nil, "rsync "+s.Describe())
		if err == nil {
			return nil
		}
		var ee *ExitError
		if !errors.As(err, &ee) || !transientRsync(res) || attempt >= s.retries() || s.wait(ctx, delay) != nil {
			return err
		}
		delay *= 2
	}
}
