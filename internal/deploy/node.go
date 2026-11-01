package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/masilia/deploykit/internal/inventory"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/releasedir"
	"github.com/masilia/deploykit/internal/runner"
	"github.com/masilia/deploykit/internal/tmpl"
	"github.com/masilia/deploykit/internal/transport"
)

// Options are the per-run switches of a deploy.
type Options struct {
	Ref          string // git ref to build; default: the manifest's source.ref
	FromWorktree bool   // build the working tree instead of a clean export (prints a warning if dirty)
	SkipInstall  bool
	SkipChecks   bool
	BuildOnly    bool // build, assemble and smoke-test locally, upload nothing (the old `deploy:dry`)
	// Also lists named commands (php-fpm runtime) to run in the live release once the deploy succeeded.
	Also []string
}

// Node deploys a release-dir artifact to a runtime behind a CloudPanel site: pm2 (Node.js) or php-fpm (PHP). The pipeline
// is the same, build here, upload a release folder, switch the current link, check, roll back; what differs is behind
// releaseRuntime.
type Node struct {
	Manifest *manifest.Manifest
	Target   *inventory.Target
	Local    transport.Transport // this machine
	Site     transport.Transport // the server, as the site user (Prelude must load node)
	// Root is the provisioning identity. It is only used by `reload: root-systemctl` of a php-fpm runtime.
	Root    transport.Transport
	Repo    string // the project's git repository
	WorkDir string // scratch space for the export and the release folder
	Author  string
	Now     func() time.Time
	Opts    Options

	rd     *manifest.ReleaseDir
	pm2    *manifest.PM2
	php    *manifest.PHPFPM
	rt     releaseRuntime
	smoke  *manifest.Smoke
	vars   tmpl.Vars
	layout Layout
	port   string

	buildDir, releaseDir string
	src                  releasedir.Source
	stamp, previous      string
	locked               bool // this run holds the deploy lock; only then may it release it
}

// NewNode validates the manifest and the target for a Node deploy.
func NewNode(n Node) (*Node, error) {
	m := n.Manifest
	if m.Artifact.Type != manifest.ArtifactReleaseDir || (m.Runtime.Type != manifest.RuntimePM2 && m.Runtime.Type != manifest.RuntimePHPFPM) {
		return nil, fmt.Errorf("the release flow needs artifact release-dir and runtime pm2 or php-fpm, got %s and %s", m.Artifact.Type, m.Runtime.Type)
	}
	var err error
	if n.rd, err = m.ReleaseDir(); err != nil {
		return nil, err
	}
	if m.Runtime.Type == manifest.RuntimePHPFPM {
		if n.php, err = m.PHP(); err != nil {
			return nil, err
		}
	} else if n.pm2, err = m.PM2(); err != nil {
		return nil, err
	}
	if !m.Smoke.IsZero() {
		if n.smoke, err = m.SmokeConfig(); err != nil {
			return nil, err
		}
	}
	n.vars = n.Target.Vars()
	n.vars["project"] = m.Project
	path := n.Target.Get("path")
	if path == "" || !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("target %s: path must be an absolute directory, got %q", n.Target.ID, path)
	}
	n.layout = Layout{Root: strings.TrimRight(path, "/")}
	if n.php != nil {
		if n.Target.Get("domain") == "" {
			return nil, fmt.Errorf("target %s: domain is required (the health check sends it as the Host header)", n.Target.ID)
		}
		n.vars["php"] = "php" + n.php.PHPVersion
		n.rt = &phpRuntime{n: &n}
	} else {
		n.port = n.Target.Get("app_port")
		if n.port == "" {
			return nil, fmt.Errorf("target %s: app_port is required", n.Target.ID)
		}
		n.rt = &pm2Runtime{n: &n, pm: PM2{T: n.Site, Name: n.pm2Name(), L: n.layout}}
	}
	if n.Now == nil {
		n.Now = time.Now
	}
	if n.Opts.Ref == "" {
		n.Opts.Ref = n.rd.Source.Ref
	}
	for _, name := range n.Opts.Also {
		if err := n.checkCommand(name); err != nil {
			return nil, err
		}
	}
	return &n, nil
}

// Layout is the remote layout of the target.
func (n *Node) Layout() Layout { return n.layout }

// Deploy builds, tests locally, uploads, switches, checks and cleans up. On a failed health check it goes back to the
// previous release. The deploy lock is held from the preflight to the end.
func (n *Node) Deploy(ctx context.Context, r *runner.Runner) error {
	if err := r.Do(ctx, n.buildSteps()...); err != nil {
		return err
	}
	if n.Opts.BuildOnly {
		r.Notice("build-only: the release in %s is ready, nothing was sent", n.releaseDir)
		return nil
	}
	if err := r.Do(ctx, n.hookSteps("before_upload", n.Manifest.Hooks.BeforeUpload)...); err != nil {
		return err
	}
	if err := r.Do(ctx, n.lockStep()); err != nil {
		return err
	}
	defer func() {
		if !n.locked { // a dry run, or the lock belongs to someone else: never touch it
			return
		}
		// The caller's context may be cancelled (Ctrl-C): still release the lock.
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := ForceUnlock(uctx, n.Site, n.layout); err != nil {
			r.Notice("warning: could not release the deploy lock: %v (remove %s/.deploy.lock)", err, n.layout.Root)
		}
	}()
	if err := r.Do(ctx, n.remoteSteps()...); err != nil {
		return err
	}
	if err := r.Do(ctx, n.hookSteps("after_switch", n.Manifest.Hooks.AfterSwitch)...); err != nil {
		return err
	}
	for _, name := range n.Opts.Also {
		if err := r.Do(ctx, n.commandStep(name)); err != nil {
			return err
		}
	}
	return nil
}

// checkCommand verifies that a named command can be run: only the php-fpm runtime declares them.
func (n *Node) checkCommand(name string) error {
	if n.php == nil {
		return fmt.Errorf("named commands (runtime.commands) belong to the php-fpm runtime, this manifest uses %s", n.Manifest.Runtime.Type)
	}
	if _, ok := n.php.Commands[name]; !ok {
		return fmt.Errorf("command %q is not declared in runtime.commands (declared: %s)", name, strings.Join(n.php.CommandNames(), ", "))
	}
	return nil
}

func (n *Node) commandStep(name string) runner.Step {
	return runner.Step{
		Name: "run " + name,
		Plan: "run " + name + " in the live release",
		Run: func(ctx context.Context, out io.Writer) error {
			cur, err := Current(ctx, n.Site, n.layout)
			if err != nil {
				return err
			}
			if cur == "" {
				return errors.New("there is no live release to run it in")
			}
			return n.rt.(*phpRuntime).runCommand(ctx, n.layout.CurrentLink(), name, out)
		},
	}
}

// RunCommand runs one named command of runtime.commands in the live release, as the site user.
func (n *Node) RunCommand(ctx context.Context, r *runner.Runner, name string) error {
	if err := n.checkCommand(name); err != nil {
		return err
	}
	return r.Do(ctx, n.commandStep(name))
}

func (n *Node) lockStep() runner.Step {
	return runner.Step{
		Name: "lock",
		Plan: "take the deploy lock on the server",
		Run: func(ctx context.Context, out io.Writer) error {
			_, err := Lock(ctx, n.Site, n.layout, fmt.Sprintf("%s since %s", n.Author, n.Now().Format(time.RFC3339)))
			var locked *ErrLocked
			if errors.As(err, &locked) {
				return fmt.Errorf("%w; if it is stale, remove %s/.deploy.lock", err, n.layout.Root)
			}
			n.locked = err == nil
			return err
		},
	}
}

func (n *Node) buildSteps() []runner.Step {
	steps := []runner.Step{{
		Name: "source",
		Plan: fmt.Sprintf("export git ref %s (or use the worktree)", n.Opts.Ref),
		Run:  n.prepareSource,
	}}
	env, envErr := releasedir.BuildEnv(n.rd, n.vars)
	for _, bs := range releasedir.BuildPlan(n.rd, releasedir.BuildOptions{SkipInstall: n.Opts.SkipInstall, SkipChecks: n.Opts.SkipChecks}) {
		steps = append(steps, runner.Step{
			Name: bs.Name,
			Plan: "run: " + bs.Command,
			Run: func(ctx context.Context, out io.Writer) error {
				if envErr != nil {
					return envErr
				}
				if image := n.rd.Build.Image; image != "" {
					return releasedir.RunBuildStepInContainer(ctx, n.Local, image, n.buildDir, n.rd.Build.Workdir, bs, env, os.Getuid(), os.Getgid(), out)
				}
				return releasedir.RunBuildStep(ctx, n.Local, filepath.Join(n.buildDir, n.rd.Build.Workdir), bs, env, out)
			},
		})
	}
	steps = append(steps, runner.Step{
		Name: "assemble",
		Plan: "assemble the release folder",
		Run: func(_ context.Context, out io.Writer) error {
			n.releaseDir = filepath.Join(n.WorkDir, "release")
			if err := os.RemoveAll(n.releaseDir); err != nil {
				return err
			}
			if err := releasedir.Assemble(n.buildDir, n.releaseDir, n.rd.Assemble); err != nil {
				return err
			}
			if err := releasedir.NormalizeTimes(n.releaseDir); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "release assembled in %s\n", n.releaseDir)
			return nil
		},
	})
	if n.smoke != nil {
		steps = append(steps, runner.Step{
			Name: "smoke test",
			Plan: "start the release locally and request the configured paths",
			Run: func(ctx context.Context, out io.Writer) error {
				checks, err := releasedir.Smoke(ctx, n.releaseDir, n.smoke)
				for _, c := range checks {
					_, _ = fmt.Fprintf(out, "%d  %s\n", c.Code, c.Path)
				}
				if err != nil {
					return fmt.Errorf("the release does not serve correctly; nothing was sent: %w", err)
				}
				return nil
			},
		})
	}
	return steps
}

func (n *Node) prepareSource(ctx context.Context, out io.Writer) error {
	var err error
	if n.Opts.FromWorktree {
		n.buildDir = n.Repo
		if n.src, err = releasedir.Describe(ctx, n.Local, n.Repo, "HEAD"); err != nil {
			return err
		}
		if n.src.Dirty {
			_, _ = fmt.Fprintln(out, "warning: the working tree has uncommitted changes; the release is built from them")
		}
	} else {
		n.buildDir = filepath.Join(n.WorkDir, "src")
		if err = os.RemoveAll(n.buildDir); err != nil {
			return err
		}
		if n.src, err = releasedir.Export(ctx, n.Local, n.Repo, n.Opts.Ref, n.buildDir); err != nil {
			return err
		}
	}
	_, _ = fmt.Fprintf(out, "ref %s, commit %s %s\n", n.Opts.Ref, n.src.Commit, n.src.Subject)
	return nil
}

func (n *Node) hookSteps(point string, hooks []manifest.Hook) []runner.Step {
	var steps []runner.Step
	for i, h := range hooks {
		steps = append(steps, runner.Step{
			Name: fmt.Sprintf("hook %s #%d", point, i+1),
			Plan: "run: " + h.Run,
			Run: func(ctx context.Context, out io.Writer) error {
				_, err := n.Local.Run(ctx, transport.Spec{Script: h.Run + "\n", Dir: n.Repo, Out: out})
				return err
			},
		})
	}
	return steps
}

func (n *Node) remoteSteps() []runner.Step {
	steps := []runner.Step{
		{
			Name: "preflight",
			Plan: n.rt.preflightPlan(),
			Run:  n.preflight,
		},
		{
			Name: "upload",
			Plan: "rsync the release into releases/<stamp>, hard-linking unchanged files",
			Run:  n.upload,
		},
	}
	if plan, ok := n.rt.preparePlan(); ok {
		steps = append(steps, runner.Step{Name: "prepare", Plan: plan, Run: n.prepare})
	}
	steps = append(steps, runner.Step{
		Name: "activate",
		Plan: n.rt.activatePlan(),
		Run:  n.activate,
	})
	if plan, ok := n.rt.afterSwitchPlan(); ok {
		steps = append(steps, runner.Step{
			Name:     "after switch",
			Plan:     plan,
			Optional: true, // the release is live and healthy: a failing cache purge is a warning, not a rollback
			Run:      func(ctx context.Context, out io.Writer) error { return n.rt.afterSwitch(ctx, out) },
		})
	}
	return append(steps, runner.Step{
		Name:     "finalize",
		Plan:     "mark the release live and prune old releases",
		Optional: true,
		Run:      n.finalize,
	})
}

func (n *Node) pm2Name() string {
	name, err := tmpl.Render(n.pm2.Name, n.vars, false)
	if err != nil {
		return n.Manifest.Project
	}
	return name
}

func (n *Node) preflight(ctx context.Context, out io.Writer) error {
	if _, err := n.Site.Run(ctx, transport.Spec{
		Script: `test -d "$DK_RELEASES" && test -d "$DK_SHARED"` + "\n",
		Env:    map[string]string{"DK_RELEASES": n.layout.Releases(), "DK_SHARED": n.layout.Shared()},
	}); err != nil {
		return fmt.Errorf("releases/ and shared/ are missing in %s: run `deploykit bootstrap` first", n.layout.Root)
	}
	return n.rt.preflight(ctx, out)
}

// preflightPM2 is the part of the preflight that only a pm2 site needs.
func (n *Node) preflightPM2(ctx context.Context, out io.Writer, pm PM2) error {
	if n.pm2.Preflight.SameArch {
		local, err := n.Local.Run(ctx, transport.Spec{Argv: []string{"uname", "-sm"}})
		if err != nil {
			return err
		}
		remote, err := n.Site.Run(ctx, transport.Spec{Argv: []string{"uname", "-sm"}})
		if err != nil {
			return err
		}
		if l, r := strings.TrimSpace(local.Stdout), strings.TrimSpace(remote.Stdout); l != r {
			return fmt.Errorf("the server is %q but this machine is %q: sharp (the image optimiser) would not load; build on a matching machine", r, l)
		}
	}
	if n.pm2.Preflight.PortFreeOnFirstDeploy {
		managed, err := pm.Managed(ctx)
		if err != nil {
			return err
		}
		if managed {
			_, _ = fmt.Fprintf(out, "port %s: the app is already running\n", n.port)
			return nil
		}
		_, err = n.Site.Run(ctx, transport.Spec{
			Script: `if ss -ltnH | awk '{print $4}' | grep -qE ":$DK_PORT\$"; then exit 3; fi` + "\n",
			Env:    map[string]string{"DK_PORT": n.port},
		})
		if err != nil {
			return fmt.Errorf("port %s is already used by another process on the server: run `deploykit bootstrap` (it picks a free port)", n.port)
		}
		_, _ = fmt.Fprintf(out, "port %s is free\n", n.port)
	}
	return nil
}

func (n *Node) upload(ctx context.Context, out io.Writer) error {
	var err error
	if n.previous, err = Current(ctx, n.Site, n.layout); err != nil {
		return err
	}
	n.stamp = NewStamp(n.Now())
	dir := n.layout.Release(n.stamp)
	if _, err = n.Site.Run(ctx, transport.Spec{Argv: []string{"mkdir", "-p", dir}}); err != nil {
		return err
	}
	meta := Meta{Stamp: n.stamp, Commit: n.src.Commit, Ref: n.Opts.Ref, Subject: n.src.Subject, Author: n.Author, Date: n.Now().UTC()}
	if err = WriteMeta(ctx, n.Site, n.layout, meta); err != nil {
		return err
	}
	spec := transport.SyncSpec{
		Src: n.releaseDir + "/", Dst: dir + "/", Delete: true, Stats: true, Checksum: true,
		Excludes: []string{".status", ".release.json"}, // protected from --delete
	}
	if n.previous != "" {
		spec.LinkDest = n.layout.Release(n.previous) // only when it exists: first deploys have no previous release
	}
	if err = n.Site.Sync(ctx, spec); err != nil {
		_ = SetStatus(ctx, n.Site, n.layout, n.stamp, StatusFailed)
		return fmt.Errorf("upload: %w", err)
	}
	if err = n.rt.afterUpload(ctx, out); err != nil {
		_ = SetStatus(ctx, n.Site, n.layout, n.stamp, StatusFailed)
		return err
	}
	_, _ = fmt.Fprintf(out, "release %s uploaded (previous: %s)\n", n.stamp, orNone(n.previous))
	return nil
}

// prepare runs the runtime's commands in the new release, before the switch: a failure leaves the live release alone.
func (n *Node) prepare(ctx context.Context, out io.Writer) error {
	if err := n.rt.prepare(ctx, out); err != nil {
		_ = SetStatus(ctx, n.Site, n.layout, n.stamp, StatusFailed)
		return err
	}
	return nil
}

func (n *Node) activate(ctx context.Context, out io.Writer) error {
	if err := Switch(ctx, n.Site, n.layout, n.stamp); err != nil {
		return n.failed(ctx, out, fmt.Errorf("switch: %w", err), false)
	}
	if err := n.rt.reload(ctx); err != nil {
		return n.failed(ctx, out, fmt.Errorf("%s: %w", n.rt.reloadLabel(), err), true)
	}
	last, ok, err := n.rt.health(ctx)
	if err != nil {
		return n.failed(ctx, out, fmt.Errorf("health check: %w", err), true)
	}
	if !ok {
		return n.failed(ctx, out, n.rt.healthFailure(last), true)
	}
	_, _ = fmt.Fprintf(out, "release %s %s\n", n.stamp, n.rt.healthy())
	return nil
}

// failed marks the new release failed and, when the link already moved, puts the previous release back.
func (n *Node) failed(ctx context.Context, out io.Writer, cause error, switched bool) error {
	// The deploy failed, possibly because ctx was cancelled; the recovery must still run.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()

	_ = SetStatus(rctx, n.Site, n.layout, n.stamp, StatusFailed)
	if !switched {
		return cause
	}
	back := false
	if n.previous == "" {
		_, _ = fmt.Fprintln(out, "no previous release to go back to")
	} else {
		_, _ = fmt.Fprintf(out, "going back to %s\n", n.previous)
		if err := Switch(rctx, n.Site, n.layout, n.previous); err != nil {
			return fmt.Errorf("%w; AND the rollback failed: %w (current may still point at %s)", cause, err, n.stamp)
		}
		if err := n.rt.reload(rctx); err != nil {
			return fmt.Errorf("%w; AND %s did not reload the previous release: %w", cause, n.rt.name(), err)
		}
		back = true
	}
	n.rt.recover(rctx, out, back)
	if n.previous != "" {
		return fmt.Errorf("%w; rolled back to %s", cause, n.previous)
	}
	return cause
}

func (n *Node) finalize(ctx context.Context, out io.Writer) error {
	if err := SetStatus(ctx, n.Site, n.layout, n.stamp, StatusLive); err != nil {
		return err
	}
	if n.previous != "" {
		if err := SetStatus(ctx, n.Site, n.layout, n.previous, StatusSuperseded); err != nil {
			return err
		}
	}
	list, err := ListReleases(ctx, n.Site, n.layout)
	if err != nil {
		return err
	}
	del := PruneList(list, n.stamp, n.rd.Keep)
	if len(del) > 0 {
		_, _ = fmt.Fprintf(out, "removing %d old release(s): %s\n", len(del), strings.Join(del, ", "))
	}
	return Remove(ctx, n.Site, n.layout, del)
}

// Rollback points the site back at an earlier release. target may be a stamp, or empty for the newest earlier release
// that did not fail. The release rolled back from is marked failed, so it is never chosen again and is pruned first.
func (n *Node) Rollback(ctx context.Context, r *runner.Runner, target string) error {
	if err := r.Do(ctx, n.lockStep()); err != nil {
		return err
	}
	defer func() {
		if !n.locked {
			return
		}
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = ForceUnlock(uctx, n.Site, n.layout)
	}()
	var current string
	steps := []runner.Step{
		{
			Name: "select release",
			Plan: "choose the release to go back to",
			Run: func(ctx context.Context, out io.Writer) error {
				var err error
				if current, err = Current(ctx, n.Site, n.layout); err != nil {
					return err
				}
				if current == "" {
					return errors.New("there is no live release to roll back from")
				}
				list, err := ListReleases(ctx, n.Site, n.layout)
				if err != nil {
					return err
				}
				for _, rel := range list {
					_, _ = fmt.Fprintf(out, "%s  %s\n", rel.Stamp, rel.Status)
				}
				if target == "" {
					if target, err = RollbackTarget(list, current); err != nil {
						return err
					}
				} else if !hasRelease(list, target) {
					return fmt.Errorf("release %s does not exist", target)
				}
				if target == current {
					return fmt.Errorf("release %s is already live", target)
				}
				_, _ = fmt.Fprintf(out, "%s -> %s\n", current, target)
				return nil
			},
		},
		{
			Name: "switch back",
			Plan: "switch the current link, " + n.rt.reloadPlan(),
			Run: func(ctx context.Context, out io.Writer) error {
				if err := Switch(ctx, n.Site, n.layout, target); err != nil {
					return err
				}
				return n.rt.reload(ctx)
			},
		},
		{
			Name: "health",
			Plan: "check that the release answers",
			Run: func(ctx context.Context, out io.Writer) error {
				last, ok, err := n.rt.health(ctx)
				if err != nil {
					return err
				}
				if !ok {
					return n.rt.rollbackFailure(last)
				}
				return nil
			},
		},
	}
	if plan, ok := n.rt.afterSwitchPlan(); ok {
		steps = append(steps, runner.Step{
			Name: "after switch", Plan: plan, Optional: true,
			Run: func(ctx context.Context, out io.Writer) error { return n.rt.afterSwitch(ctx, out) },
		})
	}
	steps = append(steps, runner.Step{
		Name:     "mark",
		Plan:     "mark the release rolled back from as failed",
		Optional: true,
		Run: func(ctx context.Context, out io.Writer) error {
			if err := SetStatus(ctx, n.Site, n.layout, current, StatusFailed); err != nil {
				return err
			}
			return SetStatus(ctx, n.Site, n.layout, target, StatusLive)
		},
	})
	return r.Do(ctx, steps...)
}

func hasRelease(list []Release, stamp string) bool {
	for _, r := range list {
		if r.Stamp == stamp {
			return true
		}
	}
	return false
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
