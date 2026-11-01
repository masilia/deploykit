package transport

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
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
		"-o", "StrictHostKeyChecking=accept-new",
	}
	return append(opts, s.ExtraOpts...)
}

// Run implements Transport.
//
// A Script, or an Argv without Stdin, is sent as a script on the stdin of the remote shell, together with the Env and
// Secrets exports: nothing sensitive reaches an argument list. An Argv with Stdin must keep stdin for the data, so
// the command line travels as a quoted argument and Secrets are refused.
func (s *SSH) Run(ctx context.Context, spec Spec) (Result, error) {
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
	cmd := exec.CommandContext(ctx, bin, rsyncArgs(spec, remoteShell)...) //nolint:gosec // arguments are built by rsyncArgs
	_, err := capture(cmd, nil, "rsync "+s.Describe())
	return err
}
