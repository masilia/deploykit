package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/masilia/deploykit/internal/compose"
	"github.com/masilia/deploykit/internal/deploy"
	"github.com/masilia/deploykit/internal/envfile"
	"github.com/masilia/deploykit/internal/fleet"
	"github.com/masilia/deploykit/internal/image"
	"github.com/masilia/deploykit/internal/inventory"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/tmpl"
	"github.com/masilia/deploykit/internal/ui"
)

type composeFlags struct {
	version, rollback, envFile, binary string
	install, gates                     bool
}

// registryOf renders the manifest's registry template with the target and global configuration.
func registryOf(img *manifest.Image, t *inventory.Target) (string, error) {
	cfg := map[string]string{"REGISTRY": t.Get("REGISTRY"), "GHCR_USER": t.Get("GHCR_USER"), "GHCR_TOKEN": t.Get("GHCR_TOKEN")}
	if cfg["REGISTRY"] == "" && strings.Contains(img.Registry, "config.REGISTRY") {
		return "", fmt.Errorf("REGISTRY is not set: put it in the target file or in the global configuration file (for example REGISTRY=ghcr.io/my-org)")
	}
	return tmpl.Render(img.Registry, tmpl.Vars{"config": cfg}, false)
}

// newRelease builds the image release for a version, or nil when the manifest does not ask to ensure it.
func (a *app) newRelease(p *project, t *inventory.Target, version string, gates bool, work string) (*image.Release, string, error) {
	img, err := p.m.Image()
	if err != nil {
		return nil, "", err
	}
	registry, err := registryOf(img, t)
	if err != nil {
		return nil, "", fmt.Errorf("artifact.registry: %w", err)
	}
	r := &image.Release{
		Cfg: img, Registry: registry, Version: version, Repo: p.repo, WorkDir: work, Gates: gates,
		Creds: image.Credentials{Host: img.RegistryLogin.Host, User: t.Get("GHCR_USER"), Token: t.Get("GHCR_TOKEN")},
		Git:   a.local, Docker: a.local,
	}
	if err := r.Validate(); err != nil {
		return nil, "", err
	}
	return r, registry, nil
}

func (a *app) composeDeploy(ctx context.Context, p *project, targetID string, f composeFlags) error {
	t, err := p.target(targetID)
	if err != nil {
		return err
	}
	if f.rollback != "" {
		if f.version != "" && f.version != f.rollback {
			return fmt.Errorf("--version and --rollback disagree")
		}
		f.version = f.rollback
	}
	var release *image.Release
	var registry string
	if f.version != "" {
		work, err := os.MkdirTemp("", "deploykit-release-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(work) }()
		rel, reg, err := a.newRelease(p, t, f.version, f.gates, work)
		if err != nil {
			return err
		}
		registry = reg
		if img, _ := p.m.Image(); img != nil && img.EnsureRelease {
			release = rel
		}
	}
	site, err := a.ssh(t, p.m, roleSite)
	if err != nil {
		return err
	}
	c := &deploy.Compose{
		Manifest: p.m, Target: t, Site: site, Repo: p.repo, Version: f.version, Install: f.install, TargetFile: t.Path, Release: release, Registry: registry,
	}
	if f.envFile != "" {
		data, err := os.Open(f.envFile) //nolint:gosec // chosen by the operator
		if err != nil {
			return err
		}
		defer func() { _ = data.Close() }()
		if c.EnvFile, err = envfile.ParseAssignments(data); err != nil {
			return fmt.Errorf("--env %s: %w", f.envFile, err)
		}
		if len(c.EnvFile) == 0 {
			return fmt.Errorf("--env %s holds no KEY=VALUE line", f.envFile)
		}
	}
	if f.binary != "" {
		c.Binary, c.BinaryExplicit = f.binary, true
	} else if c.Binary, err = os.Executable(); err != nil {
		return err
	}
	subtitle := p.m.Project + " → " + t.ID
	if f.version != "" {
		subtitle += "  " + f.version
	}
	return a.flow("deploy", subtitle, func() error { return c.Run(ctx, a.runner(a.dryRun)) })
}

func (a *app) releaseCmd() *cobra.Command {
	var version string
	var gates bool
	var target string
	cmd := &cobra.Command{
		Use:   "release",
		Short: "Build the images from the git tag of a version and push them to the registry",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signalContext(cmd)
			defer stop()
			p, err := a.load()
			if err != nil {
				return err
			}
			if p.m.Artifact.Type != manifest.ArtifactImage {
				return fmt.Errorf("`release` builds images; this manifest's artifact is %s (use `deploy`)", p.m.Artifact.Type)
			}
			if version == "" {
				return fmt.Errorf("--version X.Y.Z is required")
			}
			t, err := p.target(target)
			if err != nil {
				return err
			}
			work, err := os.MkdirTemp("", "deploykit-release-")
			if err != nil {
				return err
			}
			defer func() { _ = os.RemoveAll(work) }()
			rel, _, err := a.newRelease(p, t, version, gates, work)
			if err != nil {
				return err
			}
			return a.flow("release", p.m.Project+"  "+version, func() error { return a.runner(a.dryRun).Do(ctx, rel.Steps()...) })
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "version to release (X.Y.Z)")
	cmd.Flags().BoolVar(&gates, "gates", false, "run the quality gates (make ci) first")
	cmd.Flags().StringVarP(&target, "target", "t", "", "target whose registry settings to use")
	return cmd
}

// composeQuery describes where a compose target keeps its state.
func composeQuery(cfg *manifest.Compose, bk *manifest.BackupSpec, t *inventory.Target) fleet.Query {
	dir := "backups"
	if bk != nil {
		dir = bk.Dir
	}
	return fleet.Query{
		Root: t.Get("path"), EnvFile: cfg.VersionState.File, CurrentKey: cfg.VersionState.CurrentKey,
		PreviousKey: cfg.VersionState.PreviousKey, Marker: cfg.InstalledMarker, BackupDir: dir,
	}
}

// composeStatus renders the status table of the given targets, querying them over SSH.
func (a *app) composeStatus(ctx context.Context, p *project, targets []*inventory.Target) ([]fleet.Row, error) {
	cfg, err := p.m.Compose()
	if err != nil {
		return nil, err
	}
	var bk *manifest.BackupSpec
	if p.m.Backup.Kind != 0 {
		if _, bk, err = p.m.BackupConfig(); err != nil {
			return nil, err
		}
	}
	rows := make([]fleet.Row, len(targets))
	for i, t := range targets {
		rows[i] = fleet.Describe(t)
		site, err := a.ssh(t, p.m, roleSite)
		if err != nil {
			return nil, err
		}
		if fetched, ferr := composeQuery(cfg, bk, t).Fetch(ctx, site); ferr == nil {
			fetched.Target, fetched.Domain, fetched.Host, fetched.Contract = rows[i].Target, rows[i].Domain, rows[i].Host, rows[i].Contract
			rows[i] = fetched
		}
	}
	return rows, nil
}

// fleetSelection resolves --all / --target / --supervised into targets.
func (a *app) fleetSelection(p *project, ids []string, all, supervised bool) ([]*inventory.Target, error) {
	ts, err := inventory.Select(p.inventoryDir(), ids, all, supervised, p.global)
	if err != nil {
		return nil, err
	}
	if len(ts) == 0 {
		if supervised {
			_, _ = fmt.Fprintln(a.out, "no supervised target: nothing to do")
			return nil, nil
		}
		return nil, fmt.Errorf("no target selected: use --all or --target ID")
	}
	return ts, nil
}

type fleetFlags struct {
	ids                          []string
	all, supervised, continueErr bool
	version                      string
	node                         deploy.Options // pm2 deploy options
}

func (a *app) fleetCmd() *cobra.Command {
	var ff fleetFlags
	cmd := &cobra.Command{Use: "fleet", Short: "Operate on several targets of the inventory"}
	pf := cmd.PersistentFlags()
	pf.StringArrayVarP(&ff.ids, "target", "t", nil, "target id (repeatable)")
	pf.BoolVar(&ff.all, "all", false, "every target of the inventory")
	pf.BoolVar(&ff.supervised, "supervised", false, "only targets with contract supervised")

	status := &cobra.Command{
		Use: "status", Short: "Version, previous version and backup age of each target", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := a.load()
			if err != nil {
				return err
			}
			ts, err := a.fleetSelection(p, ff.ids, ff.all, ff.supervised)
			if err != nil || ts == nil {
				return err
			}
			if p.m.Runtime.Type == manifest.RuntimePM2 {
				rows, err := a.nodeStatus(cmd.Context(), p, ts)
				if err != nil {
					return err
				}
				a.printNodeTable(rows)
				for _, r := range rows {
					if !r.Reachable {
						return fmt.Errorf("at least one target is unreachable")
					}
				}
				return nil
			}
			rows, err := a.composeStatus(cmd.Context(), p, ts)
			if err != nil {
				return err
			}
			a.printFleetTable(rows)
			for _, r := range rows {
				if !r.Reachable {
					return fmt.Errorf("at least one target is unreachable")
				}
			}
			return nil
		},
	}
	health := &cobra.Command{
		Use: "healthcheck", Short: "Run the health probes on each target", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := a.load()
			if err != nil {
				return err
			}
			ts, err := a.fleetSelection(p, ff.ids, ff.all, ff.supervised)
			if err != nil || ts == nil {
				return err
			}
			var failed []string
			st := a.styles()
			if a.styled() {
				_, _ = fmt.Fprint(a.out, st.Banner("healthcheck", fmt.Sprintf("%d target(s)", len(ts)))+"\n")
			}
			for _, t := range ts {
				if !a.styled() {
					_, _ = fmt.Fprintf(a.out, "healthcheck %s …\n", t.ID)
				}
				if err := a.fleetHealth(cmd.Context(), p, t); err != nil {
					failed = append(failed, t.ID)
					a.printLine(fmt.Sprintf("%s: FAILED: %v", t.ID, err), func(st *ui.Styles) string {
						return "  " + st.Fail("%s  %s", st.Bold.Render(t.ID), st.Danger.Render(err.Error()))
					})
					continue
				}
				a.printLine(t.ID+": OK", func(st *ui.Styles) string {
					return "  " + st.OK("%s  %s", st.Bold.Render(t.ID), st.Muted.Render("healthy"))
				})
			}
			if len(failed) > 0 {
				return fmt.Errorf("healthcheck failed on %v", failed)
			}
			return nil
		},
	}
	backups := &cobra.Command{
		Use: "backups", Short: "Check that every target has a fresh backup", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := a.load()
			if err != nil {
				return err
			}
			ts, err := a.fleetSelection(p, ff.ids, ff.all, ff.supervised)
			if err != nil || ts == nil {
				return err
			}
			if p.m.Runtime.Type == manifest.RuntimePM2 {
				return fmt.Errorf("backups apply to compose targets: a pm2 site keeps no data of its own (its database, if any, is backed up elsewhere)")
			}
			fl, err := p.m.FleetConfig()
			if err != nil {
				return err
			}
			rows, err := a.composeStatus(cmd.Context(), p, ts)
			if err != nil {
				return err
			}
			bad := false
			var table [][]string
			for _, r := range rows {
				if !r.Reachable {
					table = append(table, []string{r.Target, "-", "unreachable"})
					bad = true
					continue
				}
				label, ok := fleet.BackupState(r.BackupAge, fl.BackupStaleHours)
				age := "-"
				if r.BackupAge >= 0 {
					age = fmt.Sprintf("%dh", r.BackupAge)
				}
				table = append(table, []string{r.Target, age, label})
				bad = bad || !ok
			}
			a.printBackups(table)
			if bad {
				return fmt.Errorf("at least one backup is missing, stale or unreachable")
			}
			return nil
		},
	}
	deployCmd := &cobra.Command{
		Use: "deploy", Short: "Deploy one version to several targets, in rollout order", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signalContext(cmd)
			defer stop()
			p, err := a.load()
			if err != nil {
				return err
			}
			if !ff.all && len(ff.ids) == 0 {
				return fmt.Errorf("deploy needs an explicit target: --target ID or --all (a fleet deploy is never implicit)")
			}
			pm2 := p.m.Runtime.Type == manifest.RuntimePM2
			switch {
			case pm2 && ff.version != "":
				return fmt.Errorf("a pm2 site has no version: use --ref to choose what to build (default: the manifest's source.ref)")
			case !pm2 && ff.version == "":
				return fmt.Errorf("deploy needs --version X.Y.Z")
			}
			ts, err := a.fleetSelection(p, ff.ids, ff.all, ff.supervised)
			if err != nil || ts == nil {
				return err
			}
			if pm2 {
				_, err = fleet.Deploy(ctx, ts, ff.continueErr, func(s string) { _, _ = fmt.Fprintln(a.out, s) },
					func(ctx context.Context, t *inventory.Target) error {
						return a.nodeDeploy(ctx, cmd.ErrOrStderr(), p, t.ID, ff.node)
					})
				return err
			}
			_, err = fleet.Deploy(ctx, ts, ff.continueErr, func(s string) { _, _ = fmt.Fprintln(a.out, s) },
				func(ctx context.Context, t *inventory.Target) error {
					return a.composeDeploy(ctx, p, t.ID, composeFlags{version: ff.version})
				})
			return err
		},
	}
	deployCmd.Flags().StringVar(&ff.version, "version", "", "compose: version to deploy (X.Y.Z)")
	deployCmd.Flags().StringVar(&ff.node.Ref, "ref", "", "pm2: git ref to build (default: the manifest's source.ref)")
	deployCmd.Flags().BoolVar(&ff.node.SkipInstall, "skip-install", false, "pm2: skip the dependency install")
	deployCmd.Flags().BoolVar(&ff.node.SkipChecks, "skip-checks", false, "pm2: skip lint and type checks")
	deployCmd.Flags().BoolVar(&ff.continueErr, "continue-on-error", false, "do not stop at the first failing target")
	cmd.AddCommand(status, health, backups, deployCmd)
	return cmd
}

// fleetHealth runs the health check of one target, whatever its runtime.
func (a *app) fleetHealth(ctx context.Context, p *project, t *inventory.Target) error {
	if p.m.Runtime.Type == manifest.RuntimePM2 {
		return a.nodeHealth(ctx, p, t)
	}
	return a.targetHealth(ctx, p, t)
}

// targetHealth runs the health probes of one target over SSH.
func (a *app) targetHealth(ctx context.Context, p *project, t *inventory.Target) error {
	cfg, err := p.m.Compose()
	if err != nil {
		return err
	}
	site, err := a.ssh(t, p.m, roleSite)
	if err != nil {
		return err
	}
	project, err := t.Project(p.m.Project)
	if err != nil {
		return err
	}
	vars := t.Vars()
	vars["project"] = project
	st := &compose.Stack{T: site, Root: t.Get("path"), Project: project, Cfg: cfg, Vars: vars, Sleep: a.sleep,
		HostHeader: hostHeader(t, t.Get("tls_mode"), t.Get("domain"))}
	results, err := st.Health(ctx, healthBase(t, t.Get("tls_mode"), t.Get("domain")))
	if err != nil {
		return err
	}
	if !compose.HealthOK(results) {
		for _, r := range results {
			if !r.OK {
				return fmt.Errorf("%s: %s", r.Name, r.Detail)
			}
		}
	}
	return nil
}

func (a *app) healthcheckCmd() *cobra.Command {
	var target string
	cmd := &cobra.Command{
		Use: "healthcheck", Short: "Run the health probes of a compose target over SSH", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := a.load()
			if err != nil {
				return err
			}
			if p.m.Runtime.Type != manifest.RuntimeCompose {
				return fmt.Errorf("healthcheck is for compose targets; pm2 deploys check health themselves")
			}
			t, err := p.target(target)
			if err != nil {
				return err
			}
			cfg, err := p.m.Compose()
			if err != nil {
				return err
			}
			site, err := a.ssh(t, p.m, roleSite)
			if err != nil {
				return err
			}
			project, err := t.Project(p.m.Project)
			if err != nil {
				return err
			}
			vars := t.Vars()
			vars["project"] = project
			st := &compose.Stack{T: site, Root: t.Get("path"), Project: project, Cfg: cfg, Vars: vars, Sleep: a.sleep,
				HostHeader: hostHeader(t, t.Get("tls_mode"), t.Get("domain"))}
			return a.printHealth(cmd.Context(), st, healthBase(t, t.Get("tls_mode"), t.Get("domain")))
		},
	}
	cmd.Flags().StringVarP(&target, "target", "t", "", "target id")
	return cmd
}
