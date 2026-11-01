package releasedir

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/tmpl"
	"github.com/masilia/deploykit/internal/transport"
)

// Source identifies what is being built.
type Source struct {
	Commit  string // short sha
	Subject string
	Dirty   bool // the worktree has uncommitted changes (only meaningful with --from-worktree)
}

// Export writes a clean copy of ref into dest with git archive, so uncommitted changes never ship (ADR 0005), and
// returns the commit it resolved to.
func Export(ctx context.Context, local transport.Transport, repo, ref, dest string) (Source, error) {
	src, err := Describe(ctx, local, repo, ref)
	if err != nil {
		return Source{}, err
	}
	_, err = local.Run(ctx, transport.Spec{
		Script:  `set -e` + "\n" + `mkdir -p "$DK_DEST"` + "\n" + `git -C "$DK_REPO" archive "$DK_REF" | tar -x -C "$DK_DEST"` + "\n",
		Env:     map[string]string{"DK_REPO": repo, "DK_REF": ref, "DK_DEST": dest},
		Secrets: nil,
	})
	if err != nil {
		return Source{}, fmt.Errorf("git archive %s: %w", ref, err)
	}
	return src, nil
}

// Describe resolves ref in repo to its short commit and subject, and reports whether the worktree is dirty.
func Describe(ctx context.Context, local transport.Transport, repo, ref string) (Source, error) {
	res, err := local.Run(ctx, transport.Spec{
		Script: `set -e
git -C "$DK_REPO" rev-parse --short=7 "$DK_REF^{commit}"
git -C "$DK_REPO" log -1 --format=%s "$DK_REF^{commit}"
git -C "$DK_REPO" status --porcelain --untracked-files=no | head -1
`,
		Env: map[string]string{"DK_REPO": repo, "DK_REF": ref},
	})
	if err != nil {
		return Source{}, fmt.Errorf("cannot resolve %s in %s: %w", ref, repo, err)
	}
	lines := strings.Split(strings.TrimRight(res.Stdout, "\n"), "\n")
	if len(lines) < 2 {
		return Source{}, fmt.Errorf("unexpected git output for %s", ref)
	}
	return Source{Commit: lines[0], Subject: lines[1], Dirty: len(lines) > 2 && strings.TrimSpace(lines[2]) != ""}, nil
}

// BuildOptions are the per-run switches of the build.
type BuildOptions struct {
	SkipInstall bool
	SkipChecks  bool
}

// BuildStep is one command of the build, in the order it must run.
type BuildStep struct {
	Name    string
	Command string
}

// BuildPlan lists the commands to run in dir, in order: install, checks, build. Templates in the build environment
// are not part of the commands; see BuildEnv.
func BuildPlan(rd *manifest.ReleaseDir, opts BuildOptions) []BuildStep {
	var steps []BuildStep
	if rd.Build.Install != "" && !opts.SkipInstall {
		steps = append(steps, BuildStep{Name: "install", Command: rd.Build.Install})
	}
	if !opts.SkipChecks {
		for _, c := range rd.Build.Checks {
			steps = append(steps, BuildStep{Name: "check: " + c, Command: c})
		}
	}
	if rd.Build.Command != "" {
		steps = append(steps, BuildStep{Name: "build", Command: rd.Build.Command})
	}
	return steps
}

// BuildEnv renders the build environment templates ({{ target.site_url }}…) for one target.
func BuildEnv(rd *manifest.ReleaseDir, vars tmpl.Vars) (map[string]string, error) {
	env := make(map[string]string, len(rd.Build.Env))
	for k, v := range rd.Build.Env {
		rendered, err := tmpl.Render(v, vars, false)
		if err != nil {
			return nil, fmt.Errorf("build.env.%s: %w", k, err)
		}
		env[k] = rendered
	}
	return env, nil
}

// RunBuildStep runs one build command in dir with the build environment, streaming its output to out.
func RunBuildStep(ctx context.Context, local transport.Transport, dir string, step BuildStep, env map[string]string, out io.Writer) error {
	_, err := local.Run(ctx, transport.Spec{Script: step.Command + "\n", Dir: dir, Env: env, Out: out})
	return err
}
