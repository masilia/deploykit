package deploy

import (
	"context"
	"fmt"
	"io"
)

// releaseRuntime is what differs between the runtimes a release folder can be served by. The deploy itself (build, upload,
// switch the current link, roll back, prune) is the same for all of them.
type releaseRuntime interface {
	name() string

	preflightPlan() string
	preflight(ctx context.Context, out io.Writer) error
	// afterUpload runs once the release is on the server, before anything can reach it.
	afterUpload(ctx context.Context, out io.Writer) error

	preparePlan() (plan string, ok bool)
	prepare(ctx context.Context, out io.Writer) error

	activatePlan() string
	reloadPlan() string
	reloadLabel() string
	// reload makes the server run the release the current link points at.
	reload(ctx context.Context) error
	// health reports whether the release answers; last is what was seen (an HTTP code).
	health(ctx context.Context) (last string, ok bool, err error)
	healthy() string
	healthFailure(last string) error
	rollbackFailure(last string) error

	afterSwitchPlan() (plan string, ok bool)
	afterSwitch(ctx context.Context, out io.Writer) error
	// recover runs after a failed activation. back is true when the previous release is live again.
	recover(ctx context.Context, out io.Writer, back bool)
}

// pm2Runtime runs the release under pm2, behind a CloudPanel Node.js site.
type pm2Runtime struct {
	n  *Node
	pm PM2
}

func (r *pm2Runtime) name() string { return "pm2" }

func (r *pm2Runtime) preflightPlan() string { return "check the server: layout, architecture, port" }

func (r *pm2Runtime) preflight(ctx context.Context, out io.Writer) error {
	return r.n.preflightPM2(ctx, out, r.pm)
}

func (r *pm2Runtime) afterUpload(ctx context.Context, _ io.Writer) error {
	eco, err := Ecosystem(r.n.pm2, r.n.layout, r.n.vars)
	if err != nil {
		return err
	}
	return r.pm.UploadEcosystem(ctx, eco)
}

func (r *pm2Runtime) preparePlan() (string, bool)              { return "", false }
func (r *pm2Runtime) prepare(context.Context, io.Writer) error { return nil }
func (r *pm2Runtime) activatePlan() string {
	return "switch the current link, reload pm2, check health, roll back on failure"
}
func (r *pm2Runtime) reloadPlan() string                           { return "reload pm2" }
func (r *pm2Runtime) reloadLabel() string                          { return "pm2 reload" }
func (r *pm2Runtime) reload(ctx context.Context) error             { return r.pm.Reload(ctx) }
func (r *pm2Runtime) afterSwitchPlan() (string, bool)              { return "", false }
func (r *pm2Runtime) afterSwitch(context.Context, io.Writer) error { return nil }

func (r *pm2Runtime) health(ctx context.Context) (string, bool, error) {
	return Healthy(ctx, r.n.Site, r.n.port, r.n.pm2.Health)
}

func (r *pm2Runtime) healthy() string {
	return fmt.Sprintf("answers %d on %s", r.n.pm2.Health.Expect, r.n.pm2.Health.Path)
}

func (r *pm2Runtime) healthFailure(last string) error {
	h := r.n.pm2.Health
	return fmt.Errorf("the new release does not answer %d on port %s%s (last code %s)", h.Expect, r.n.port, h.Path, last)
}

func (r *pm2Runtime) rollbackFailure(last string) error {
	return fmt.Errorf("the site answers %s after the rollback, check `pm2 logs %s`", last, r.pm.Name)
}

func (r *pm2Runtime) recover(ctx context.Context, out io.Writer, _ bool) {
	if logs, err := r.pm.Logs(ctx, 30); err == nil && logs != "" {
		_, _ = fmt.Fprintln(out, logs)
	}
}
