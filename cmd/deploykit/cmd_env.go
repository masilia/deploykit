package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/masilia/deploykit/internal/compose"
	"github.com/masilia/deploykit/internal/envfile"
	"github.com/masilia/deploykit/internal/inventory"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/transport"
	"github.com/masilia/deploykit/internal/ui"
)

// envContext is what the env commands need to know about one compose target.
type envContext struct {
	t        *inventory.Target
	cfg      *manifest.Compose
	site     transport.Transport
	file     compose.RemoteEnv
	stack    *compose.Stack
	declared map[string]bool // the secrets the manifest generates
}

func (a *app) envContext(targetID string) (*envContext, error) {
	p, err := a.load()
	if err != nil {
		return nil, err
	}
	if p.m.Runtime.Type != manifest.RuntimeCompose {
		return nil, fmt.Errorf("env is for compose targets (a php-fpm site keeps its variables in shared/.env.local on the server)")
	}
	t, err := p.target(targetID)
	if err != nil {
		return nil, err
	}
	cfg, err := p.m.Compose()
	if err != nil {
		return nil, err
	}
	spec, err := p.m.EnvConfig()
	if err != nil {
		return nil, err
	}
	site, err := a.ssh(t, p.m, roleSite)
	if err != nil {
		return nil, err
	}
	project, err := t.Project(p.m.Project)
	if err != nil {
		return nil, err
	}
	vars := t.Vars()
	vars["project"] = project
	ec := &envContext{
		t: t, cfg: cfg, site: site, declared: map[string]bool{},
		file:  compose.RemoteEnv{T: site, Path: strings.TrimSuffix(t.Get("path"), "/") + "/" + spec.File},
		stack: &compose.Stack{T: site, Root: t.Get("path"), Project: project, Cfg: cfg, Vars: vars, Sleep: a.sleep},
	}
	for k := range spec.Secrets {
		ec.declared[k] = true
	}
	return ec, nil
}

func (a *app) envCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "List, read and change the variables of a compose target's server .env",
		Long: "The server .env holds the configuration and the secrets of a compose stack. These commands work on it over SSH, so it can be\n" +
			"managed without a file manager: list shows every variable with the secrets masked, get reads one (a secret needs --reveal), and\n" +
			"set changes one or several without a full deploy. A value never travels on a command line: it comes from a hidden prompt, from\n" +
			"stdin or from a file, and reaches the server on stdin.",
	}
	cmd.AddCommand(a.envListCmd(), a.envGetCmd(), a.envSetCmd())
	return cmd
}

func (a *app) envListCmd() *cobra.Command {
	var target string
	cmd := &cobra.Command{
		Use: "list", Short: "List the variables of the server .env; secrets are masked", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ec, err := a.envContext(target)
			if err != nil {
				return err
			}
			doc, err := ec.file.Load(cmd.Context())
			if err != nil {
				return err
			}
			keys := doc.Keys()
			width, secrets := 0, 0
			for _, k := range keys {
				width = max(width, len(k))
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "%s  %s  (%d variables)\n\n", ec.t.ID, ec.file.Path, len(keys))
			for _, k := range keys {
				v := doc.Get(k, "")
				secret := envfile.IsSecret(k, v, ec.declared)
				if secret {
					secrets++
				}
				_, _ = fmt.Fprintf(out, "%-*s  %s\n", width, k, envfile.Display(v, secret))
			}
			if secrets > 0 {
				_, _ = fmt.Fprintf(out, "\n%d secret(s) masked. Keep an off-server copy of them (a password manager): without BACKUP_PASSPHRASE, for\n"+
					"example, the encrypted backups cannot be read. Read one with: deploykit env get -t %s KEY --reveal\n", secrets, ec.t.ID)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&target, "target", "t", "", "target id")
	return cmd
}

func (a *app) envGetCmd() *cobra.Command {
	var target string
	var reveal bool
	cmd := &cobra.Command{
		Use: "get KEY", Short: "Print the value of one variable (a secret needs --reveal)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := args[0]
			ec, err := a.envContext(target)
			if err != nil {
				return err
			}
			doc, err := ec.file.Load(cmd.Context())
			if err != nil {
				return err
			}
			if !doc.Has(key) {
				return fmt.Errorf("%s is not set in %s", key, ec.file.Path)
			}
			v := doc.Get(key, "")
			if envfile.IsSecret(key, v, ec.declared) && !reveal {
				return fmt.Errorf("%s is a secret: add --reveal to print it (it will appear on your terminal)", key)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), v)
			return err
		},
	}
	cmd.Flags().StringVarP(&target, "target", "t", "", "target id")
	cmd.Flags().BoolVar(&reveal, "reveal", false, "print a secret")
	return cmd
}

func (a *app) envSetCmd() *cobra.Command {
	var target, fromFile string
	var apply bool
	cmd := &cobra.Command{
		Use:   "set [KEY]",
		Short: "Set one variable (value from a hidden prompt or stdin) or several (--from-file), without a full deploy",
		Long: "With KEY, the value is read from a hidden prompt at a terminal, or from stdin (a trailing newline is dropped; an empty value\n" +
			"is refused). With --from-file, the file holds KEY=VALUE lines and must be readable by you only. Nothing is printed back but the\n" +
			"keys. The new values take effect when the services are recreated: add --apply to run `docker compose up -d` now, or deploy.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (len(args) == 1) == (fromFile != "") {
				return fmt.Errorf("give either a KEY or --from-file")
			}
			ec, err := a.envContext(target)
			if err != nil {
				return err
			}
			var as []envfile.Assignment
			if fromFile != "" {
				if as, err = loadPrivateEnv(fromFile); err != nil {
					return err
				}
				if len(as) == 0 {
					return fmt.Errorf("%s holds no KEY=VALUE line", fromFile)
				}
			} else {
				if !envfile.ValidKey(args[0]) {
					return fmt.Errorf("invalid variable name %q", args[0])
				}
				v, err := a.readEnvValue(args[0], envfile.IsSecret(args[0], "", ec.declared))
				if err != nil {
					return err
				}
				as = []envfile.Assignment{{Key: args[0], Value: v}}
			}
			for _, x := range as {
				if x.Key == ec.cfg.VersionState.CurrentKey || x.Key == ec.cfg.VersionState.PreviousKey {
					return fmt.Errorf("%s is managed by deploy and rollback: deploy the version you want instead", x.Key)
				}
			}
			ctx := cmd.Context()
			doc, err := ec.file.Load(ctx)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			changed := 0
			for _, x := range as {
				state := "new"
				if doc.Has(x.Key) {
					state = "changed"
					if doc.Get(x.Key, "") == x.Value {
						state = "unchanged"
					}
				}
				if state != "unchanged" {
					changed++
				}
				verb := map[bool]string{true: "would set", false: "set"}[a.dryRun]
				_, _ = fmt.Fprintf(out, "%s %s (%s)\n", verb, x.Key, state)
				if err := doc.Set(x.Key, x.Value); err != nil {
					return err
				}
			}
			if a.dryRun {
				return nil
			}
			if changed == 0 {
				_, _ = fmt.Fprintln(out, "nothing to change")
				return nil
			}
			if err := ec.file.Save(ctx, doc); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "%s updated on the server\n", ec.file.Path)
			if !apply {
				_, _ = fmt.Fprintln(out, "the running containers still have the old values: run again with --apply, or deploy")
				return nil
			}
			return a.applyEnv(ctx, ec, out)
		},
	}
	cmd.Flags().StringVarP(&target, "target", "t", "", "target id")
	cmd.Flags().StringVar(&fromFile, "from-file", "", "a file of KEY=VALUE lines, mode 600")
	cmd.Flags().BoolVar(&apply, "apply", false, "recreate the services whose environment changed (docker compose up -d)")
	return cmd
}

// lockHeld says whether an operation (an update, an install, a backup) holds the operations lock of the deployment root.
func (ec *envContext) lockHeld(ctx context.Context) bool {
	lock := ec.cfg.Lock
	if lock == "" {
		return false
	}
	_, err := ec.site.Run(ctx, transport.Spec{Argv: []string{"test", "-e", strings.TrimSuffix(ec.t.Get("path"), "/") + "/" + lock}})
	return err == nil
}

// applyEnv recreates what the new .env changes. It refuses while an operation holds the lock: an update in progress is
// about to start or stop the same containers.
func (a *app) applyEnv(ctx context.Context, ec *envContext, out io.Writer) error {
	if ec.lockHeld(ctx) {
		return fmt.Errorf("an operation holds the lock (%s): the .env is saved; apply it when the operation is over", ec.cfg.Lock)
	}
	_, _ = fmt.Fprintln(out, "recreating the services whose environment changed")
	_, err := ec.stack.Run(ctx, out, "up", "-d")
	return err
}

// readEnvValue reads one value: a hidden prompt at a terminal (a visible one for a value that is not a secret), stdin otherwise.
func (a *app) readEnvValue(key string, secret bool) (string, error) {
	in := a.in
	if in == nil {
		terminal := term.IsTerminal(os.Stdin.Fd())
		if a.interactive != nil {
			terminal = a.interactive()
		}
		if terminal {
			var v string
			input := huh.NewInput().Title(key).Value(&v)
			if secret {
				input = input.EchoMode(huh.EchoModePassword)
			}
			if err := huh.NewForm(huh.NewGroup(input)).WithTheme(ui.FormTheme()).Run(); err != nil {
				return "", err
			}
			if v == "" {
				return "", fmt.Errorf("empty value: use --from-file with a KEY= line to set an empty one")
			}
			return v, nil
		}
		in = os.Stdin
	}
	data, err := io.ReadAll(io.LimitReader(in, 1<<20))
	if err != nil {
		return "", err
	}
	v := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if v == "" {
		return "", fmt.Errorf("no value on stdin: pipe it in, or use --from-file with a KEY= line to set an empty one")
	}
	return v, nil
}

// targetSecretsPath is where the secrets of a target live on this machine, next to the manifest and out of version control.
func targetSecretsPath(p *project, id string) string {
	return filepath.Join(p.dir, "secrets", id+".env")
}

// loadTargetSecrets reads secrets/<target>.env when it exists. It returns the assignments and the path ("" when absent).
func loadTargetSecrets(p *project, id string) ([]envfile.Assignment, string, error) {
	path := targetSecretsPath(p, id)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, "", nil
	}
	as, err := loadPrivateEnv(path)
	return as, path, err
}

// loadPrivateEnv parses a KEY=VALUE file that holds secrets: like an SSH key, it must not be readable by other users.
func loadPrivateEnv(path string) ([]envfile.Assignment, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by other users (mode %03o): run chmod 600 on it", path, info.Mode().Perm())
	}
	f, err := os.Open(path) //nolint:gosec // chosen by the operator
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	as, err := envfile.ParseAssignments(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return as, nil
}
