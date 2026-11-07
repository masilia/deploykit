package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/masilia/deploykit/internal/deploy"
	"github.com/masilia/deploykit/internal/fleet"
	"github.com/masilia/deploykit/internal/inventory"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/transport"
)

// signalContext is cancelled by Ctrl-C and SIGTERM.
func signalContext(cmd *cobra.Command) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
}

// nodeProject loads the manifest and target for a pm2 / CloudPanel Node.js command.
func (a *app) nodeProject(targetID string) (*project, *inventory.Target, error) {
	p, err := a.load()
	if err != nil {
		return nil, nil, err
	}
	if !p.m.ReleaseBased() {
		return nil, nil, fmt.Errorf("this command is for pm2 and php-fpm targets, this manifest uses runtime %s (compose: `deploy --version`, `deploy --rollback`, `fleet`, `server`)", p.m.Runtime.Type)
	}
	t, err := p.target(targetID)
	return p, t, err
}

func (a *app) newNode(p *project, t *inventory.Target, opts deploy.Options, workDir string) (*deploy.Node, error) {
	site, err := a.ssh(t, p.m, roleSite)
	if err != nil {
		return nil, err
	}
	var root transport.Transport
	if php, err := p.m.PHP(); err == nil && php.Reload == "root-systemctl" {
		if root, err = a.ssh(t, p.m, roleRoot); err != nil {
			return nil, err
		}
	}
	return deploy.NewNode(deploy.Node{
		Manifest: p.m, Target: t, Local: a.local, Site: site, Root: root,
		Repo: p.repo, WorkDir: workDir, Author: currentUser(), Now: a.now, Opts: opts,
	})
}

// nodeDeploy builds and deploys one pm2 target.
func (a *app) nodeDeploy(ctx context.Context, errOut io.Writer, p *project, targetID string, opts deploy.Options) error {
	t, err := p.target(targetID)
	if err != nil {
		return err
	}
	// Outside the repository, always: Next.js resolves its tracing root from the topmost lockfile above the
	// build, so a build inside <repo>/.deploykit nests server.js under .deploykit/src in the standalone output.
	work, err := os.MkdirTemp("", "deploykit-")
	if err != nil {
		return err
	}
	if opts.BuildOnly {
		_, _ = fmt.Fprintf(errOut, "build kept in %s\n", work)
	} else {
		defer func() { _ = os.RemoveAll(work) }()
	}
	n, err := a.newNode(p, t, opts, work)
	if err != nil {
		return err
	}
	return a.flow("deploy", p.m.Project+" → "+t.ID, func() error { return n.Deploy(ctx, a.runner(a.dryRun)) })
}

// nodeStatus queries the live release of each pm2 target.
func (a *app) nodeStatus(ctx context.Context, p *project, targets []*inventory.Target) ([]fleet.NodeRow, error) {
	rows := make([]fleet.NodeRow, len(targets))
	for i, t := range targets {
		rows[i] = fleet.NodeDescribe(t)
		site, err := a.ssh(t, p.m, roleSite)
		if err != nil {
			return nil, err
		}
		layout := deploy.Layout{Root: t.Get("path")}
		cur, err := deploy.Current(ctx, site, layout)
		if err != nil {
			continue
		}
		list, err := deploy.ListReleases(ctx, site, layout)
		if err != nil {
			continue
		}
		rows[i].Reachable, rows[i].Live = true, cur
		for _, r := range list {
			if r.Stamp == cur && r.Meta != nil {
				rows[i].Commit = r.Meta.Commit
			}
			if rows[i].Previous == "" && r.Stamp != cur && r.Status != deploy.StatusFailed {
				rows[i].Previous = r.Stamp
			}
		}
	}
	return rows, nil
}

// nodeHealth asks the server of a pm2 target to fetch its health path, as the end of a deploy does.
func (a *app) nodeHealth(ctx context.Context, p *project, t *inventory.Target) error {
	if p.m.Runtime.Type == manifest.RuntimePHPFPM {
		php, err := p.m.PHP()
		if err != nil {
			return err
		}
		site, err := a.ssh(t, p.m, roleSite)
		if err != nil {
			return err
		}
		h := php.Health
		last, ok, err := deploy.HealthyHTTP(ctx, site, h.Backend, t.Get("domain"), h.Path, h.Expect, h.Tries, h.Interval)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s answers %s, expected one of %v", h.Path, orDash(last), h.Expect)
		}
		return nil
	}
	cfg, err := p.m.PM2()
	if err != nil {
		return err
	}
	site, err := a.ssh(t, p.m, roleSite)
	if err != nil {
		return err
	}
	code, ok, err := deploy.Healthy(ctx, site, t.Get("app_port"), cfg.Health)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s answers %s, expected %d", cfg.Health.Path, orDash(code), cfg.Health.Expect)
	}
	return nil
}

func (a *app) deployCmd() *cobra.Command {
	var (
		target string
		opts   deploy.Options
		cf     composeFlags
	)
	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Deploy a release (pm2: build, test, upload, switch; compose: release, upload, install or update on the server)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signalContext(cmd)
			defer stop()
			p, err := a.load()
			if err != nil {
				return err
			}
			switch p.m.Runtime.Type {
			case manifest.RuntimeCompose:
				return a.composeDeploy(ctx, p, target, cf)
			case manifest.RuntimePM2, manifest.RuntimePHPFPM:
				if cf.binaryOnly {
					return fmt.Errorf("--binary-only is for compose stacks: a %s site keeps no deploykit on its server", p.m.Runtime.Type)
				}
			default:
				return fmt.Errorf("runtime %s is not implemented yet (pm2, php-fpm and compose are)", p.m.Runtime.Type)
			}
			return a.nodeDeploy(ctx, cmd.ErrOrStderr(), p, target, opts)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&target, "target", "t", "", "target id (default: the only target of the inventory)")
	f.StringVar(&opts.Ref, "ref", "", "pm2: git ref to build (default: the manifest's source.ref)")
	f.BoolVar(&opts.FromWorktree, "from-worktree", false, "pm2: build the working tree instead of a clean export of the ref")
	f.BoolVar(&opts.SkipInstall, "skip-install", false, "pm2: skip the dependency install")
	f.BoolVar(&opts.SkipChecks, "skip-checks", false, "pm2: skip lint and type checks")
	f.BoolVar(&opts.BuildOnly, "build-only", false, "pm2, php-fpm: build and smoke-test locally, upload nothing")
	f.StringArrayVar(&opts.Also, "also", nil, "php-fpm: a named command of runtime.commands to run in the live release after the deploy (repeatable)")
	f.StringVar(&cf.version, "version", "", "compose: version to deploy (X.Y.Z); an older one is a rollback")
	f.StringVar(&cf.rollback, "rollback", "", "compose: alias of --version, to make the intent explicit")
	f.BoolVar(&cf.install, "install", false, "compose: first installation instead of an update")
	f.StringVar(&cf.envFile, "env", "", "compose: KEY=VALUE file injected into the server .env (secrets travel on stdin)")
	f.BoolVar(&cf.gates, "gates", false, "compose: run the quality gates before building a missing release")
	f.StringVar(&cf.binary, "server-binary", "", "compose: deploykit binary to upload (needed when this machine is not the server's platform)")
	f.BoolVar(&cf.binaryOnly, "binary-only", false, "compose: only put this deploykit on the server: no files, no .env, no update, no restart")
	return cmd
}

// runCmd runs a named command of the manifest in the live release.
func (a *app) runCmd() *cobra.Command {
	var target string
	var execRunning bool
	cmd := &cobra.Command{
		Use:   "run [command]",
		Short: "Run a named command of the manifest on a target: php-fpm runtime.commands in the live release, compose app.commands in the stack",
		Long: "php-fpm: runs a command of runtime.commands in the live release as the site user.\n" +
			"compose: runs a command of runtime.app.commands in a one-off container of the app service (docker compose run --rm), or with --exec in\n" +
			"the container that is already running. Without a command, it lists the ones the manifest declares. Secrets a command takes from the\n" +
			"server .env (env_from) reach it through the environment, never on a command line. The hooks of an update (after_migrate) are not run:\n" +
			"name each command you want. It refuses while an update, an install or a backup holds the operations lock.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signalContext(cmd)
			defer stop()
			if m, err := a.load(); err == nil && m.m.Runtime.Type == manifest.RuntimeCompose {
				return a.runCompose(ctx, cmd, target, args, execRunning)
			}
			if len(args) != 1 {
				return fmt.Errorf("name the command to run (a key of runtime.commands)")
			}
			p, t, err := a.nodeProject(target)
			if err != nil {
				return err
			}
			n, err := a.newNode(p, t, deploy.Options{}, os.TempDir())
			if err != nil {
				return err
			}
			return a.flow("run "+args[0], p.m.Project+" → "+t.ID, func() error { return n.RunCommand(ctx, a.runner(a.dryRun), args[0]) })
		},
	}
	cmd.Flags().StringVarP(&target, "target", "t", "", "target id")
	cmd.Flags().BoolVar(&execRunning, "exec", false, "compose: run in the running container of the app service instead of a one-off one")
	return cmd
}

func (a *app) rollbackCmd() *cobra.Command {
	var target, to string
	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "Go back to the previous release that did not fail (or to --to)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signalContext(cmd)
			defer stop()
			p, t, err := a.nodeProject(target)
			if err != nil {
				return err
			}
			n, err := a.newNode(p, t, deploy.Options{}, os.TempDir())
			if err != nil {
				return err
			}
			return a.flow("rollback", p.m.Project+" → "+t.ID, func() error { return n.Rollback(ctx, a.runner(a.dryRun), to) })
		},
	}
	cmd.Flags().StringVarP(&target, "target", "t", "", "target id")
	cmd.Flags().StringVar(&to, "to", "", "release (YYYYMMDDHHMMSS) to go back to")
	return cmd
}

func (a *app) bootstrapCmd() *cobra.Command {
	var target, pubkey string
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Provision the CloudPanel site once (pm2: port, site, certificate, folders, key, pm2; compose: port, reverse-proxy site, certificate, deployment root). Safe to re-run",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signalContext(cmd)
			defer stop()
			p, err := a.load()
			if err != nil {
				return err
			}
			t, err := p.target(target)
			if err != nil {
				return err
			}
			root, err := a.ssh(t, p.m, roleRoot)
			if err != nil {
				return err
			}
			switch p.m.Runtime.Type {
			case manifest.RuntimeCompose:
				return a.flow("bootstrap", p.m.Project+" → "+t.ID, func() error {
					return (&deploy.ComposeBootstrap{Manifest: p.m, Target: t, Root: root, DNS: a.dns}).Run(ctx, a.runner(a.dryRun))
				})
			case manifest.RuntimePM2, manifest.RuntimePHPFPM:
				site, err := a.ssh(t, p.m, roleSite)
				if err != nil {
					return err
				}
				b := &deploy.Bootstrap{Manifest: p.m, Target: t, Root: root, Site: site, DNS: a.dns, PublicKey: pubkey}
				return a.flow("bootstrap", p.m.Project+" → "+t.ID, func() error { return b.Run(ctx, a.runner(a.dryRun)) })
			}
			return fmt.Errorf("runtime %s is not implemented yet", p.m.Runtime.Type)
		},
	}
	cmd.Flags().StringVarP(&target, "target", "t", "", "target id")
	cmd.Flags().StringVar(&pubkey, "pubkey", "", "pm2: public key file to install for the site user (default: ~/.ssh/id_ed25519.pub, then id_rsa.pub)")
	return cmd
}

func (a *app) statusCmd() *cobra.Command {
	var target string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "pm2: the live release and the releases kept; compose: version, previous version and backup age",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := a.load()
			if err != nil {
				return err
			}
			t, err := p.target(target)
			if err != nil {
				return err
			}
			if p.m.Runtime.Type == manifest.RuntimeCompose {
				rows, err := a.composeStatus(cmd.Context(), p, []*inventory.Target{t})
				if err != nil {
					return err
				}
				a.printFleetTable(rows)
				return nil
			}
			site, err := a.ssh(t, p.m, roleSite)
			if err != nil {
				return err
			}
			layout := deploy.Layout{Root: t.Get("path")}
			cur, err := deploy.Current(cmd.Context(), site, layout)
			if err != nil {
				return err
			}
			list, err := deploy.ListReleases(cmd.Context(), site, layout)
			if err != nil {
				return err
			}
			a.printReleases(t.ID, site.Describe(), cur, list)
			return nil
		},
	}
	cmd.Flags().StringVarP(&target, "target", "t", "", "target id")
	return cmd
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
