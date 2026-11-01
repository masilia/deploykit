package compose

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/masilia/deploykit/internal/envfile"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/runner"
	"github.com/masilia/deploykit/internal/tmpl"
	"github.com/masilia/deploykit/internal/transport"
)

// RollbackDecision says whether a failed update goes back to the previous version automatically: there must be a
// distinct, known previous version, and AUTO_ROLLBACK must not be 0. The database schema is never undone, so
// migrations must stay compatible with the previous version.
func RollbackDecision(cur, prev string, autoRollback bool) bool {
	return autoRollback && prev != "" && prev != "unknown" && prev != cur
}

// Update brings the stack to the version written in .env: lock, disk check, backup, pull, migrate, restart, verify,
// automatic rollback (legacy update.sh). It runs on the server with the Local transport.
type Update struct {
	Stack  *Stack
	Env    *manifest.EnvSpec
	Backup *Backup // nil when the manifest has no backup step
	// Images are the repository names (without registry) cleaned after a successful update.
	Images  []string
	BaseURL string
	PID     int
	Now     func() time.Time
	// Notify receives operational messages; failures are ignored by the caller.
	Notify func(ctx context.Context, text string)
	// Crons are installed by the monitoring_cron step; empty means nothing to schedule.
	Crons []CronEntry

	file   RemoteEnv
	doc    *envfile.Doc
	cur    string
	prev   string
	unlock func(context.Context) error
}

func (u *Update) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func (u *Update) notify(ctx context.Context, text string) {
	if u.Notify != nil {
		u.Notify(ctx, text)
	}
}

// Run executes the update steps listed by the manifest. The lock is released whatever happens.
func (u *Update) Run(ctx context.Context, r *runner.Runner) error {
	steps := []runner.Step{u.checkStep()}
	for _, name := range u.Stack.Cfg.Update.Steps {
		more, err := u.steps(name)
		if err != nil {
			return err
		}
		steps = append(steps, more...)
	}
	steps = append(steps, u.finishSteps()...)
	defer func() {
		c := context.WithoutCancel(ctx)
		if u.Backup != nil {
			u.Backup.Cleanup(c)
		}
		if u.unlock != nil {
			_ = u.unlock(c)
		}
	}()
	return r.Do(ctx, steps...)
}

func (u *Update) checkStep() runner.Step {
	return runner.Step{
		Name: "check installation",
		Plan: "check that the stack is installed and read the versions",
		Run: func(ctx context.Context, out io.Writer) error {
			c := u.Stack.Cfg
			if _, err := u.Stack.T.Run(ctx, transport.Spec{Argv: []string{"test", "-f", c.InstalledMarker}, Dir: u.Stack.Root}); err != nil {
				return fmt.Errorf("no installation found (%s is missing): use install first", c.InstalledMarker)
			}
			u.file = RemoteEnv{T: u.Stack.T, Path: u.Stack.Root + "/" + c.VersionState.File}
			var err error
			if u.doc, err = u.file.Load(ctx); err != nil {
				return err
			}
			u.cur = u.doc.Get(c.VersionState.CurrentKey, "")
			u.prev = u.doc.Get(c.VersionState.PreviousKey, "")
			_, _ = fmt.Fprintf(out, "target %s (replaced version: %s)\n", orUnknown(u.cur), orUnknown(u.prev))
			return nil
		},
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// steps maps a manifest step name to the work it does.
func (u *Update) steps(name string) ([]runner.Step, error) {
	s := u.Stack
	switch name {
	case "lock":
		return []runner.Step{{Name: "lock", Plan: "take the operations lock", Run: func(ctx context.Context, _ io.Writer) error {
			unlock, err := s.Lock(ctx, u.PID)
			u.unlock = unlock
			return err
		}}}, nil
	case "disk_check":
		return []runner.Step{{Name: "disk space", Plan: "check the free space before pulling images", Run: u.diskCheck}}, nil
	case "backup":
		if u.Backup == nil {
			return nil, fmt.Errorf("the update lists a backup step but the manifest has no data/backup sections")
		}
		return u.Backup.Steps(), nil
	case "pull":
		return []runner.Step{{Name: "pull images", Plan: "docker compose pull", Run: func(ctx context.Context, out io.Writer) error {
			if _, err := s.Run(ctx, out, "pull"); err != nil {
				return fmt.Errorf("pull failed (check the registry login): %w", err)
			}
			return nil
		}}}, nil
	case "migrate":
		return []runner.Step{{Name: "migrations", Plan: "run the migrations and their hooks", Run: u.migrate}}, nil
	case "secrets":
		return []runner.Step{{Name: "secrets", Plan: "generate the secrets that are missing", Run: u.secrets}}, nil
	case "up":
		return []runner.Step{{Name: "restart", Plan: "docker compose up -d", Run: func(ctx context.Context, out io.Writer) error {
			_, err := s.Run(ctx, out, "up", "-d")
			if err != nil {
				return fmt.Errorf("start failed: %w", err)
			}
			return nil
		}}}, nil
	case "proxy_reload":
		return []runner.Step{{Name: "proxy reload", Plan: "reload the proxy", Optional: true, Run: u.proxyReload}}, nil
	case "verify":
		return []runner.Step{{Name: "verify", Plan: "wait for the health checks; roll back on failure", Run: u.verify}}, nil
	}
	return nil, fmt.Errorf("unknown update step %q", name)
}

func (u *Update) diskCheck(ctx context.Context, out io.Writer) error {
	res, err := u.Stack.T.Run(ctx, transport.Spec{Script: "docker info -f '{{.DockerRootDir}}' 2>/dev/null || echo /var/lib/docker\n"})
	if err != nil {
		return err
	}
	root := strings.TrimSpace(res.Stdout)
	min := 2000
	if v, rerr := tmpl.Render(u.Stack.Cfg.Update.Disk.MinFreeMB, EnvVars(u.doc), false); rerr == nil {
		if n, convErr := strconv.Atoi(v); convErr == nil {
			min = n
		}
	}
	free, err := u.Stack.FreeMB(ctx, root)
	if err != nil {
		return err
	}
	if free < min {
		return fmt.Errorf("not enough disk space to pull the images: %d MB free on %s (minimum %d MB)", free, root, min)
	}
	_, _ = fmt.Fprintf(out, "%d MB free on %s\n", free, root)
	return nil
}

func (u *Update) migrate(ctx context.Context, out io.Writer) error {
	hint := fmt.Sprintf("manual rollback: set %s=%s in .env and run the update again", u.Stack.Cfg.VersionState.CurrentKey, orUnknown(u.prev))
	if err := u.Stack.RunApp(ctx, "migrate", u.doc, out); err != nil {
		return fmt.Errorf("migrations failed (%s): %w", hint, err)
	}
	for _, hook := range u.Stack.Cfg.App.Hooks.AfterMigrate {
		if err := u.Stack.RunShell(ctx, hook, nil, u.doc, out); err != nil {
			return fmt.Errorf("a post-migration hook failed; the database is migrated, run it by hand (%s): %w", hook, err)
		}
	}
	return nil
}

func (u *Update) secrets(ctx context.Context, out io.Writer) error {
	generated, err := EnsureSecrets(u.doc, u.Env.Secrets)
	if err != nil {
		return err
	}
	if len(generated) == 0 {
		return nil
	}
	_, _ = fmt.Fprintf(out, "secrets generated: %s\n", strings.Join(generated, ", "))
	return u.file.Save(ctx, u.doc)
}

func (u *Update) proxyService() string {
	for _, svc := range u.Stack.Cfg.Services {
		if svc.Role == "proxy" {
			return svc.Name
		}
	}
	return ""
}

func (u *Update) proxyReload(ctx context.Context, out io.Writer) error {
	proxy := u.proxyService()
	if proxy == "" {
		return nil
	}
	_, err := u.Stack.Run(ctx, nil, "exec", "-T", proxy, "nginx", "-s", "reload")
	return err
}

// healthy runs the probes, then the commands listed in health.after (the ORM mapping check).
func (u *Update) healthy(ctx context.Context, out io.Writer) (bool, error) {
	results, err := u.Stack.Health(ctx, u.BaseURL)
	if err != nil {
		return false, err
	}
	ok := HealthOK(results)
	for _, r := range results {
		if !r.OK {
			_, _ = fmt.Fprintf(out, "KO %s: %s\n", r.Name, r.Detail)
		}
	}
	if !ok {
		return false, nil
	}
	for _, name := range u.Stack.Cfg.Health.After {
		if err := u.Stack.ExecApp(ctx, name); err != nil {
			_, _ = fmt.Fprintf(out, "KO %s: %v\n", name, err)
			return false, nil
		}
	}
	return true, nil
}

func (u *Update) verify(ctx context.Context, out io.Writer) error {
	ok, err := u.healthy(ctx, out)
	if err != nil {
		return err
	}
	if ok {
		return u.Stack.Journal(ctx, u.now(), fmt.Sprintf("OK   %s (replaces %s)", orUnknown(u.cur), orUnknown(u.prev)))
	}
	_ = u.Stack.Journal(ctx, u.now(), fmt.Sprintf("KO   %s (after %s)", orUnknown(u.cur), orUnknown(u.prev)))

	auto := u.doc.Get("AUTO_ROLLBACK", "1") != "0"
	key := u.Stack.Cfg.VersionState.CurrentKey
	if !RollbackDecision(u.cur, u.prev, auto) {
		u.notify(ctx, fmt.Sprintf(":rotating_light: Update failed: the health check failed on %s (version %s) and the automatic rollback is disabled or impossible: manual intervention needed.", u.Stack.Project, orUnknown(u.cur)))
		return fmt.Errorf("health check failed; manual rollback: set %s=%s in .env and run the update again", key, orUnknown(u.prev))
	}

	_, _ = fmt.Fprintf(out, "FAILED: going back to version %s\n", u.prev)
	if err := u.doc.Set(key, u.prev); err != nil {
		return err
	}
	if err := u.file.Save(ctx, u.doc); err != nil {
		return err
	}
	_, _ = u.Stack.Run(ctx, nil, "up", "-d")
	_ = u.proxyReload(ctx, out)
	if back, herr := u.healthy(ctx, out); herr == nil && back {
		_ = u.Stack.Journal(ctx, u.now(), fmt.Sprintf("ROLLBACK %s (failure of %s)", u.prev, u.cur))
		u.notify(ctx, fmt.Sprintf(":warning: Update failed on %s: version %s failed, automatic return to %s done and verified.", u.Stack.Project, orUnknown(u.cur), u.prev))
		return fmt.Errorf("version %s failed: automatic return to %s done and verified (see the service logs)", orUnknown(u.cur), u.prev)
	}
	u.notify(ctx, fmt.Sprintf(":rotating_light: Update failed on %s: version %s failed AND the return to %s failed too: manual intervention needed.", u.Stack.Project, orUnknown(u.cur), u.prev))
	return fmt.Errorf("version %s failed AND the return to %s failed too: manual intervention needed", orUnknown(u.cur), u.prev)
}

// finishSteps cleans up after a successful update: old images, then the steps listed in update.after.
func (u *Update) finishSteps() []runner.Step {
	var steps []runner.Step
	if len(u.Stack.Cfg.Update.Cleanup.KeepVersions) > 0 || u.Stack.Cfg.Update.Cleanup.PruneDangling {
		steps = append(steps, runner.Step{Name: "clean images", Plan: "remove images older than the current and previous versions", Optional: true, Run: u.cleanImages})
	}
	for _, name := range u.Stack.Cfg.Update.After {
		if cmd, ok := u.Stack.Cfg.App.Commands[name]; ok && name != "monitoring_cron" {
			// A command that runs in the live container once the update is verified. Its failure is reported but does
			// not undo the update: the project's own update.sh treats the secret re-encryption the same way.
			steps = append(steps, runner.Step{Name: name, Plan: "run " + cmd.Run + " in the running " + u.Stack.Cfg.App.Service + " container", Optional: true, Run: func(ctx context.Context, out io.Writer) error {
				return u.Stack.ExecApp(ctx, name)
			}})
			continue
		}
		if name == "monitoring_cron" {
			steps = append(steps, runner.Step{Name: "monitoring cron", Plan: "install the supervision crons", Optional: true, Run: func(ctx context.Context, out io.Writer) error {
				if len(u.Crons) == 0 {
					return nil
				}
				return u.Stack.InstallCrons(ctx, u.Crons)
			}})
		}
	}
	steps = append(steps, runner.Step{Name: "notify", Plan: "announce the new version", Optional: true, Run: func(ctx context.Context, out io.Writer) error {
		u.notify(ctx, fmt.Sprintf(":rocket: %s updated: version %s is live (previous %s).", u.Stack.Project, orUnknown(u.cur), orUnknown(u.prev)))
		return nil
	}})
	return steps
}

func (u *Update) cleanImages(ctx context.Context, out io.Writer) error {
	registry := u.doc.Get("REGISTRY", "")
	if registry != "" {
		for _, name := range u.Images {
			_, err := u.Stack.T.Run(ctx, transport.Spec{
				Script: `docker image ls "$DK_REPO" --format '{{.Tag}}' 2>/dev/null | grep -v -x -e "$DK_CUR" -e "$DK_PREV" -e '<none>' | while IFS= read -r tag; do docker image rm "$DK_REPO:$tag" >/dev/null 2>&1 || true; done` + "\n",
				Env:    map[string]string{"DK_REPO": registry + "/" + name, "DK_CUR": u.cur, "DK_PREV": u.prev},
			})
			if err != nil {
				return err
			}
		}
	}
	if u.Stack.Cfg.Update.Cleanup.PruneDangling {
		_, err := u.Stack.T.Run(ctx, transport.Spec{Argv: []string{"docker", "image", "prune", "-f"}})
		return err
	}
	return nil
}
