package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/masilia/deploykit/internal/compose"
	"github.com/masilia/deploykit/internal/envfile"
	"github.com/masilia/deploykit/internal/inventory"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/notify"
	"github.com/masilia/deploykit/internal/tmpl"
	"github.com/masilia/deploykit/internal/ui"
)

// serverCtx is everything a `deploykit server …` command needs, loaded from the deployment root it runs in.
type serverCtx struct {
	m          *manifest.Manifest
	root       string
	target     *inventory.Target
	stack      *compose.Stack
	envFile    compose.RemoteEnv
	standalone *manifest.Standalone
	tlsMode    string
	domain     string
}

// serverLoad reads the manifest and the target file that `deploy` uploaded next to it. The server only ever sees its
// own target, never the inventory of the others.
func (a *app) serverLoad(targetFile string) (*serverCtx, error) {
	p, err := a.load()
	if err != nil {
		return nil, err
	}
	if p.m.Runtime.Type != manifest.RuntimeCompose {
		return nil, fmt.Errorf("`server` commands manage compose stacks; this manifest uses runtime %s", p.m.Runtime.Type)
	}
	if targetFile == "" {
		targetFile = filepath.Join(p.dir, ".deploykit", "target.env")
	}
	id := strings.TrimSuffix(filepath.Base(targetFile), ".env")
	target, err := inventory.OpenOrNew(filepath.Dir(targetFile), id, nil)
	if err != nil {
		return nil, err
	}
	if _, statErr := os.Stat(targetFile); statErr != nil {
		return nil, fmt.Errorf("target file %s not found: it is uploaded by `deploykit deploy`", targetFile)
	}
	cfg, err := p.m.Compose()
	if err != nil {
		return nil, err
	}
	project, err := target.Project(p.m.Project)
	if err != nil {
		return nil, err
	}
	vars := target.Vars()
	vars["project"] = project
	s := &serverCtx{m: p.m, root: p.dir, target: target, tlsMode: target.Get("tls_mode"), domain: target.Get("domain")}
	ing := p.m.Ingress.Axis
	if p.m.Ingress.Select != "" {
		key, rerr := tmpl.Render(p.m.Ingress.Select, vars, false)
		if rerr != nil {
			return nil, fmt.Errorf("ingress.select: %w", rerr)
		}
		if ing, err = p.m.Ingress.Variant(key); err != nil {
			return nil, err
		}
	}
	if ing.Type == manifest.IngressStandalone {
		s.standalone = &manifest.Standalone{}
		if err := ing.Decode(s.standalone); err != nil {
			return nil, err
		}
	}
	s.stack = &compose.Stack{T: a.local, Root: p.dir, Project: project, Cfg: cfg, Vars: vars, Sleep: a.sleep, HostHeader: hostHeader(target, s.tlsMode, s.domain)}
	s.envFile = compose.RemoteEnv{T: a.local, Path: filepath.Join(p.dir, ".env")}
	return s, nil
}

func (s *serverCtx) env(ctx context.Context) (*envfile.Doc, error) { return s.envFile.Load(ctx) }

// baseURL is where the health probes reach the application. Behind CloudPanel that is the loopback proxy, with the
// public domain as the Host header: it needs neither DNS nor a certificate, so a new instance can be checked at once.
// Otherwise it is the public URL.
func (s *serverCtx) baseURL(env *envfile.Doc) string {
	return healthBase(s.target, s.tlsMode, env.Get("DOMAIN", s.domain))
}

// healthBase computes the probe base URL of a target; see serverCtx.baseURL.
func healthBase(t *inventory.Target, tlsMode, domain string) string {
	if tlsMode == "cloudpanel" && t.Get("proxy_port") != "" {
		return "http://127.0.0.1:" + t.Get("proxy_port")
	}
	return "https://" + domain
}

// hostHeader is the Host header the probes send ("" when they use the public URL).
func hostHeader(t *inventory.Target, tlsMode, domain string) string {
	if tlsMode == "cloudpanel" && t.Get("proxy_port") != "" {
		return domain
	}
	return ""
}

// slack builds the notifier from SLACK_WEBHOOK_URL; without it notifications are silently skipped.
func slack(env *envfile.Doc) func(context.Context, string) {
	n := notify.Slack{URL: env.Get("SLACK_WEBHOOK_URL", "")}
	return func(ctx context.Context, text string) {
		if err := n.Send(ctx, text); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "warning:", err)
		}
	}
}

func (a *app) serverCmd() *cobra.Command {
	var targetFile string
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Operations that run on the server, inside a deployment root (compose stacks)",
	}
	cmd.PersistentFlags().StringVar(&targetFile, "target-file", "", "this server's target file (default: .deploykit/target.env)")
	load := func() (*serverCtx, error) { return a.serverLoad(targetFile) }

	cmd.AddCommand(
		a.serverUpdate(load), a.serverInstall(load), a.serverBackup(load), a.serverHealthcheck(load),
		a.serverMonitor(load), a.serverRestoreTest(load), a.serverRenewCerts(load), a.serverCron(load), a.serverCrons(load),
	)
	return cmd
}

type loader func() (*serverCtx, error)

func (a *app) cronBin() string {
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "deploykit"
}

func (a *app) serverUpdate(load loader) *cobra.Command {
	return &cobra.Command{
		Use:   "update",
		Short: "Update the stack to the version in .env: backup, migrate, restart, verify, roll back on failure",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signalContext(cmd)
			defer stop()
			s, err := load()
			if err != nil {
				return err
			}
			env, err := s.env(ctx)
			if err != nil {
				return err
			}
			envSpec, err := s.m.EnvConfig()
			if err != nil {
				return err
			}
			img, err := s.m.Image()
			if err != nil {
				return err
			}
			crons, err := compose.CronsFor(s.m, s.standalone, a.cronBin())
			if err != nil {
				return err
			}
			u := &compose.Update{
				Stack: s.stack, Env: envSpec, BaseURL: s.baseURL(env), PID: os.Getpid(), Notify: slack(env), Crons: crons, Now: a.now, Standalone: s.standalone,
			}
			for _, i := range img.Images {
				u.Images = append(u.Images, i.Name)
			}
			if s.m.Backup.Kind != 0 {
				data, backup, err := s.m.BackupConfig()
				if err != nil {
					return err
				}
				u.Backup = &compose.Backup{Stack: s.stack, Data: data, Cfg: backup, Env: env, Now: a.now}
			}
			return u.Run(ctx, a.runner(a.dryRun))
		},
	}
}

func (a *app) serverInstall(load loader) *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "First installation of the stack",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signalContext(cmd)
			defer stop()
			s, err := load()
			if err != nil {
				return err
			}
			envSpec, err := s.m.EnvConfig()
			if err != nil {
				return err
			}
			crons, err := compose.CronsFor(s.m, s.standalone, a.cronBin())
			if err != nil {
				return err
			}
			base := healthBase(s.target, s.tlsMode, s.domain)
			if env, envErr := s.env(ctx); envErr == nil {
				base = s.baseURL(env)
			}
			in := &compose.Install{
				Stack: s.stack, Env: envSpec, Standalone: s.standalone, CloudPanelPort: s.target.Get("proxy_port"), Crons: crons, BaseURL: base,
			}
			return in.Run(ctx, a.runner(a.dryRun))
		},
	}
}

func (a *app) serverBackup(load loader) *cobra.Command {
	return &cobra.Command{
		Use:   "backup",
		Short: "Make a verified, encrypted backup",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signalContext(cmd)
			defer stop()
			s, err := load()
			if err != nil {
				return err
			}
			env, err := s.env(ctx)
			if err != nil {
				return err
			}
			data, backup, err := s.m.BackupConfig()
			if err != nil {
				return err
			}
			if !a.dryRun {
				unlock, err := s.stack.Lock(ctx, os.Getpid())
				if err != nil {
					return err
				}
				defer func() { _ = unlock(context.WithoutCancel(ctx)) }()
			}
			bk := &compose.Backup{Stack: s.stack, Data: data, Cfg: backup, Env: env, Now: a.now}
			if err := bk.Run(ctx, a.runner(a.dryRun)); err != nil {
				return err
			}
			if !a.dryRun {
				_, _ = fmt.Fprintf(a.out, "backup written: %s\nKeep %s OFF this server: without it the backups cannot be read if the server is lost.\n", bk.Path(), backup.Encryption.PassphraseKey)
			}
			return nil
		},
	}
}

func (a *app) serverHealthcheck(load loader) *cobra.Command {
	var base string
	cmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "Wait for the services and run the health probes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := load()
			if err != nil {
				return err
			}
			env, err := s.env(cmd.Context())
			if err != nil {
				return err
			}
			if base == "" {
				base = s.baseURL(env)
			}
			return a.printHealth(cmd.Context(), s.stack, base)
		},
	}
	cmd.Flags().StringVar(&base, "base-url", "", "URL to probe (default: the loopback proxy behind CloudPanel, https://<domain> otherwise)")
	return cmd
}

// printHealth runs the probes and prints one line each; it fails when any probe fails.
func (a *app) printHealth(ctx context.Context, st *compose.Stack, base string) error {
	results, err := st.Health(ctx, base)
	if err != nil {
		return err
	}
	a.printProbes(results, base)
	if !compose.HealthOK(results) {
		return fmt.Errorf("health check failed")
	}
	a.printLine("healthy: "+base, func(st *ui.Styles) string { return "\n" + st.OK("healthy") })
	return nil
}

func (a *app) serverMonitor(load loader) *cobra.Command {
	var test bool
	cmd := &cobra.Command{
		Use:   "monitor",
		Short: "Supervision check (run by cron every few minutes): alerts after repeated failures",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := load()
			if err != nil {
				return err
			}
			env, err := s.env(cmd.Context())
			if err != nil {
				return err
			}
			notifier := slack(env)
			host, _ := os.Hostname()
			if test {
				if env.Get("SLACK_WEBHOOK_URL", "") == "" {
					return fmt.Errorf("SLACK_WEBHOOK_URL is missing from .env")
				}
				notifier(cmd.Context(), fmt.Sprintf(":white_check_mark: Supervision test: %s (v%s, %s): the webhook works.", s.domain, env.Get("APP_VERSION", "?"), host))
				_, _ = fmt.Fprintln(a.out, "test message sent")
				return nil
			}
			spec, err := s.m.MonitorConfig()
			if err != nil {
				return err
			}
			base := s.baseURL(env)
			m := &compose.Monitor{
				Stack: s.stack, Spec: spec, Env: env, BaseURL: base, Host: env.Get("DOMAIN", s.domain),
				Version: env.Get("APP_VERSION", "unknown"), Server: host, Notify: notifier, Now: a.now, DryRun: a.dryRun,
			}
			d, err := m.Run(cmd.Context())
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(a.out, "diagnosis: %s%s\n", d.Level, withReasons(d))
			return nil
		},
	}
	cmd.Flags().BoolVar(&test, "test", false, "send a test message to the webhook and exit")
	return cmd
}

func withReasons(d compose.Diagnosis) string {
	if len(d.Reasons) == 0 {
		return ""
	}
	return " — " + d.Text()
}

func (a *app) serverRestoreTest(load loader) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "restore-test",
		Short: "Prove the newest backup can be restored, into a throwaway database",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signalContext(cmd)
			defer stop()
			s, err := load()
			if err != nil {
				return err
			}
			env, err := s.env(ctx)
			if err != nil {
				return err
			}
			data, backup, err := s.m.BackupConfig()
			if err != nil {
				return err
			}
			spec, err := s.m.RestoreTestConfig()
			if err != nil {
				return err
			}
			rt := &compose.RestoreTest{
				Stack: s.stack, Data: data, Backup: backup, Spec: spec, Env: env, File: file,
				Domain: env.Get("DOMAIN", s.domain), PID: os.Getpid(), Notify: slack(env),
			}
			return rt.Run(ctx, a.runner(a.dryRun))
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "backup file to test (default: the newest)")
	return cmd
}

func (a *app) serverRenewCerts(load loader) *cobra.Command {
	return &cobra.Command{
		Use:   "renew-certs",
		Short: "Renew the Let's Encrypt certificates (standalone ingress)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := load()
			if err != nil {
				return err
			}
			if s.standalone == nil {
				_, _ = fmt.Fprintln(a.out, "TLS is managed by CloudPanel: nothing to renew here")
				return nil
			}
			vols := s.standalone.TLS.Volumes
			if len(vols) < 2 {
				return fmt.Errorf("ingress.tls.volumes must list the letsencrypt and certbot-www volumes")
			}
			_, err = s.stack.Script(cmd.Context(), a.out, `set -e
docker volume inspect "$DK_LE" >/dev/null 2>&1 || { echo "volume $DK_LE not found" >&2; exit 1; }
docker run --rm -v "$DK_LE:/etc/letsencrypt" -v "$DK_WWW:/var/www/certbot" certbot/certbot renew --non-interactive
compose exec -T proxy nginx -s reload
`, map[string]string{"DK_LE": s.stack.Project + "_" + vols[0], "DK_WWW": s.stack.Project + "_" + vols[1]}, nil)
			return err
		},
	}
}

func (a *app) serverCron(load loader) *cobra.Command {
	return &cobra.Command{
		Use:   "cron NAME",
		Short: "Run one of the project's cron jobs from the manifest",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := load()
			if err != nil {
				return err
			}
			jobs, err := s.m.CronJobs()
			if err != nil {
				return err
			}
			for _, j := range jobs {
				if j.Name == args[0] {
					_, err := s.stack.Script(cmd.Context(), a.out, j.Run+"\n", nil, nil)
					return err
				}
			}
			return fmt.Errorf("no cron job named %q in the manifest", args[0])
		},
	}
}

func (a *app) serverCrons(load loader) *cobra.Command {
	var remove bool
	cmd := &cobra.Command{
		Use:   "crons",
		Short: "Install (or with --remove, delete) the crontab entries of this deployment root",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := load()
			if err != nil {
				return err
			}
			crons, err := compose.CronsFor(s.m, s.standalone, a.cronBin())
			if err != nil {
				return err
			}
			if remove {
				crons = nil // an empty set removes this root's block and its legacy lines, and nothing else
			}
			if a.dryRun {
				_, _ = fmt.Fprint(a.out, compose.MergeCrontab("", s.root, crons))
				return nil
			}
			if err := s.stack.InstallCrons(cmd.Context(), crons); err != nil {
				return err
			}
			if remove {
				_, _ = fmt.Fprintf(a.out, "crontab entries of %s removed\n", s.root)
				return nil
			}
			_, _ = fmt.Fprintf(a.out, "%d cron entr(ies) installed for %s\n", len(crons), s.root)
			return nil
		},
	}
	cmd.Flags().BoolVar(&remove, "remove", false, "remove this root's entries instead of installing them")
	return cmd
}
