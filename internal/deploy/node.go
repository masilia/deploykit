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
}

// Node deploys a release-dir artifact to a pm2 runtime behind a CloudPanel Node.js site.
type Node struct {
	Manifest *manifest.Manifest
	Target   *inventory.Target
	Local    transport.Transport // this machine
	Site     transport.Transport // the server, as the site user (Prelude must load node)
	Repo     string              // the project's git repository
	WorkDir  string              // scratch space for the export and the release folder
	Author   string
	Now      func() time.Time
	Opts     Options

	rd     *manifest.ReleaseDir
	pm2    *manifest.PM2
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
	if m.Artifact.Type != manifest.ArtifactReleaseDir || m.Runtime.Type != manifest.RuntimePM2 {
		return nil, fmt.Errorf("the node flow needs artifact release-dir and runtime pm2, got %s and %s", m.Artifact.Type, m.Runtime.Type)
	}
	var err error
	if n.rd, err = m.ReleaseDir(); err != nil {
		return nil, err
	}
	if n.pm2, err = m.PM2(); err != nil {
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
	n.port = n.Target.Get("app_port")
	if n.port == "" {
		return nil, fmt.Errorf("target %s: app_port is required", n.Target.ID)
	}
	if n.Now == nil {
		n.Now = time.Now
	}
	if n.Opts.Ref == "" {
		n.Opts.Ref = n.rd.Source.Ref
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
	return r.Do(ctx, n.hookSteps("after_switch", n.Manifest.Hooks.AfterSwitch)...)
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
				return releasedir.RunBuildStep(ctx, n.Local, n.buildDir, bs, env, out)
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
	pm := PM2{T: n.Site, Name: n.pm2Name(), L: n.layout}
	return []runner.Step{
		{
			Name: "preflight",
			Plan: "check the server: layout, architecture, port",
			Run:  func(ctx context.Context, out io.Writer) error { return n.preflight(ctx, out, pm) },
		},
		{
			Name: "upload",
			Plan: "rsync the release into releases/<stamp>, hard-linking unchanged files",
			Run:  func(ctx context.Context, out io.Writer) error { return n.upload(ctx, out, pm) },
		},
		{
			Name: "activate",
			Plan: "switch the current link, reload pm2, check health, roll back on failure",
			Run:  func(ctx context.Context, out io.Writer) error { return n.activate(ctx, out, pm) },
		},
		{
			Name:     "finalize",
			Plan:     "mark the release live and prune old releases",
			Optional: true,
			Run:      n.finalize,
		},
	}
}

func (n *Node) pm2Name() string {
	name, err := tmpl.Render(n.pm2.Name, n.vars, false)
	if err != nil {
		return n.Manifest.Project
	}
	return name
}

func (n *Node) preflight(ctx context.Context, out io.Writer, pm PM2) error {
	if _, err := n.Site.Run(ctx, transport.Spec{
		Script: `test -d "$DK_RELEASES" && test -d "$DK_SHARED"` + "\n",
		Env:    map[string]string{"DK_RELEASES": n.layout.Releases(), "DK_SHARED": n.layout.Shared()},
	}); err != nil {
		return fmt.Errorf("releases/ and shared/ are missing in %s: run `deploykit bootstrap` first", n.layout.Root)
	}
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

func (n *Node) upload(ctx context.Context, out io.Writer, pm PM2) error {
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
	eco, err := Ecosystem(n.pm2, n.layout, n.vars)
	if err != nil {
		return err
	}
	if err = pm.UploadEcosystem(ctx, eco); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "release %s uploaded (previous: %s)\n", n.stamp, orNone(n.previous))
	return nil
}

func (n *Node) activate(ctx context.Context, out io.Writer, pm PM2) error {
	if err := Switch(ctx, n.Site, n.layout, n.stamp); err != nil {
		return n.failed(ctx, out, pm, fmt.Errorf("switch: %w", err), false)
	}
	if err := pm.Reload(ctx); err != nil {
		return n.failed(ctx, out, pm, fmt.Errorf("pm2 reload: %w", err), true)
	}
	code, ok, err := Healthy(ctx, n.Site, n.port, n.pm2.Health)
	if err != nil {
		return n.failed(ctx, out, pm, fmt.Errorf("health check: %w", err), true)
	}
	if !ok {
		return n.failed(ctx, out, pm, fmt.Errorf("the new release does not answer %d on port %s%s (last code %s)", n.pm2.Health.Expect, n.port, n.pm2.Health.Path, code), true)
	}
	_, _ = fmt.Fprintf(out, "release %s answers %d on %s\n", n.stamp, n.pm2.Health.Expect, n.pm2.Health.Path)
	return nil
}

// failed marks the new release failed and, when the link already moved, puts the previous release back.
func (n *Node) failed(ctx context.Context, out io.Writer, pm PM2, cause error, switched bool) error {
	// The deploy failed, possibly because ctx was cancelled; the recovery must still run.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()

	_ = SetStatus(rctx, n.Site, n.layout, n.stamp, StatusFailed)
	if !switched {
		return cause
	}
	if n.previous == "" {
		_, _ = fmt.Fprintln(out, "no previous release to go back to")
	} else {
		_, _ = fmt.Fprintf(out, "going back to %s\n", n.previous)
		if err := Switch(rctx, n.Site, n.layout, n.previous); err != nil {
			return fmt.Errorf("%w; AND the rollback failed: %w (current may still point at %s)", cause, err, n.stamp)
		}
		if err := pm.Reload(rctx); err != nil {
			return fmt.Errorf("%w; AND pm2 did not reload the previous release: %w", cause, err)
		}
	}
	if logs, err := pm.Logs(rctx, 30); err == nil && logs != "" {
		_, _ = fmt.Fprintln(out, logs)
	}
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
	pm := PM2{T: n.Site, Name: n.pm2Name(), L: n.layout}
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
	return r.Do(ctx,
		runner.Step{
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
		runner.Step{
			Name: "switch back",
			Plan: "switch the current link, reload pm2",
			Run: func(ctx context.Context, out io.Writer) error {
				if err := Switch(ctx, n.Site, n.layout, target); err != nil {
					return err
				}
				return pm.Reload(ctx)
			},
		},
		runner.Step{
			Name: "health",
			Plan: "check that the release answers",
			Run: func(ctx context.Context, out io.Writer) error {
				code, ok, err := Healthy(ctx, n.Site, n.port, n.pm2.Health)
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("the site answers %s after the rollback, check `pm2 logs %s`", code, pm.Name)
				}
				return nil
			},
		},
		runner.Step{
			Name:     "mark",
			Plan:     "mark the release rolled back from as failed",
			Optional: true,
			Run: func(ctx context.Context, out io.Writer) error {
				if err := SetStatus(ctx, n.Site, n.layout, current, StatusFailed); err != nil {
					return err
				}
				return SetStatus(ctx, n.Site, n.layout, target, StatusLive)
			},
		},
	)
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
