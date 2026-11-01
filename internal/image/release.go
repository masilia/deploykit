// Package image builds and publishes the Docker images of a release from a git tag (legacy release.sh).
//
// Images are always built from an export of the tag, never from the working tree, so uncommitted changes cannot ship.
package image

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/releasedir"
	"github.com/masilia/deploykit/internal/runner"
	"github.com/masilia/deploykit/internal/tmpl"
	"github.com/masilia/deploykit/internal/transport"
)

var registryPattern = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]+)?(/[A-Za-z0-9._/-]+)+$`)

// ValidRegistry reports whether s is an explicit "host/namespace" registry. A bare namespace such as "my-org" would be
// pushed by Docker to docker.io/my-org and be denied (legacy registry_valid).
func ValidRegistry(s string) bool {
	host, _, hasSlash := strings.Cut(s, "/")
	if !hasSlash {
		return false
	}
	if !strings.ContainsAny(host, ".:") && host != "localhost" {
		return false
	}
	return registryPattern.MatchString(s)
}

// Credentials log in to the registry before pushing. Both empty: an existing docker login is assumed.
type Credentials struct {
	Host  string
	User  string
	Token string // passed on stdin, never as an argument
}

// Built is one image to build with its tags.
type Built struct {
	Name       string
	Dockerfile string
	Context    string
	Tags       []string // full references, registry included
}

// Release builds and pushes the images of one version.
type Release struct {
	Cfg      *manifest.Image
	Registry string
	Version  string
	Repo     string
	WorkDir  string
	Gates    bool
	Creds    Credentials

	Git    transport.Transport // runs git and tar on this machine
	Docker transport.Transport // runs docker on this machine

	DryRun bool

	commit string
	tag    string
}

// Validate checks the version and registry before anything runs.
func (r *Release) Validate() error {
	if ok, _ := regexp.MatchString(r.Cfg.VersionPattern, r.Version); !ok {
		return fmt.Errorf("invalid version %q (pattern %s)", r.Version, r.Cfg.VersionPattern)
	}
	if !ValidRegistry(r.Registry) {
		return fmt.Errorf("invalid registry %q: expected host/namespace (for example ghcr.io/my-org); a bare namespace would be pushed to docker.io and denied", r.Registry)
	}
	return nil
}

// Refs lists the images to build with their full tags, for a commit.
func (r *Release) Refs(commit7 string) ([]Built, error) {
	vars := tmpl.Vars{"version": r.Version, "commit7": commit7}
	var out []Built
	for _, img := range r.Cfg.Images {
		b := Built{Name: img.Name, Dockerfile: img.Dockerfile, Context: img.Context}
		if b.Context == "" {
			b.Context = "."
		}
		for _, t := range r.Cfg.Tags {
			tag, err := tmpl.Render(t, vars, false)
			if err != nil {
				return nil, fmt.Errorf("tags: %w", err)
			}
			b.Tags = append(b.Tags, r.Registry+"/"+img.Name+":"+tag)
		}
		out = append(out, b)
	}
	return out, nil
}

// Present reports whether every image of the version is already in the registry (or, as a fallback, local), so deploy
// can skip the build. A registry that rejects the anonymous probe is tried once more after logging in.
func (r *Release) Present(ctx context.Context) (bool, error) {
	if ok := r.allInspect(ctx, "manifest", "inspect"); ok {
		return true, nil
	}
	if r.Creds.User != "" && r.Creds.Token != "" {
		if err := r.login(ctx); err == nil && r.allInspect(ctx, "manifest", "inspect") {
			return true, nil
		}
	}
	return r.allInspect(ctx, "image", "inspect"), nil // images built locally, or already pulled
}

func (r *Release) allInspect(ctx context.Context, sub ...string) bool {
	for _, img := range r.Cfg.Images {
		ref := r.Registry + "/" + img.Name + ":" + r.Version
		if _, err := r.Docker.Run(ctx, transport.Spec{Argv: append(append([]string{"docker"}, sub...), ref)}); err != nil {
			return false
		}
	}
	return true
}

func (r *Release) login(ctx context.Context) error {
	host := r.Creds.Host
	if host == "" {
		host, _, _ = strings.Cut(r.Registry, "/")
	}
	_, err := r.Docker.Run(ctx, transport.Spec{
		Argv:  []string{"docker", "login", host, "-u", r.Creds.User, "--password-stdin"},
		Stdin: strings.NewReader(r.Creds.Token),
	})
	return err
}

// Steps returns the release steps: tag, gates, login, build, push.
func (r *Release) Steps() []runner.Step {
	var built []Built
	src := filepath.Join(r.WorkDir, "src")
	steps := []runner.Step{{
		Name: "tag",
		Plan: "use the tag, or create it on HEAD when missing",
		Run: func(ctx context.Context, out io.Writer) error {
			vars := tmpl.Vars{"version": r.Version}
			tag, err := tmpl.Render(r.Cfg.Source.Ref, vars, false)
			if err != nil {
				return err
			}
			r.tag = tag
			exists, err := r.tagExists(ctx, tag)
			if err != nil {
				return err
			}
			switch {
			case exists:
				_, _ = fmt.Fprintf(out, "tag %s exists: building from it\n", tag)
			case !r.Cfg.Source.CreateTagIfMissing:
				return fmt.Errorf("tag %s does not exist and the manifest does not allow creating it", tag)
			default:
				if _, err := r.Git.Run(ctx, transport.Spec{Argv: []string{"git", "-C", r.Repo, "tag", tag}}); err != nil {
					return fmt.Errorf("creating tag %s: %w", tag, err)
				}
				_, _ = fmt.Fprintf(out, "tag %s created on HEAD (push it with: git push origin %s)\n", tag, tag)
			}
			info, err := releasedir.Describe(ctx, r.Git, r.Repo, tag)
			if err != nil {
				return err
			}
			r.commit = info.Commit
			_, _ = fmt.Fprintf(out, "source: %s = commit %s (%s)\n", tag, info.Commit, info.Subject)
			if info.Dirty {
				_, _ = fmt.Fprintln(out, "note: uncommitted changes are ignored: the build uses the tag")
			}
			return nil
		},
	}}
	if r.Gates && r.Cfg.Gates.Command != "" {
		steps = append(steps, runner.Step{
			Name: "gates",
			Plan: "run: " + r.Cfg.Gates.Command,
			Run: func(ctx context.Context, out io.Writer) error {
				_, err := r.Git.Run(ctx, transport.Spec{Script: r.Cfg.Gates.Command + "\n", Dir: r.Repo, Out: out})
				return err
			},
		})
	}
	if r.Creds.User != "" && r.Creds.Token != "" {
		steps = append(steps, runner.Step{Name: "registry login", Plan: "docker login", Run: func(ctx context.Context, out io.Writer) error { return r.login(ctx) }})
	}
	steps = append(steps, runner.Step{
		Name: "export",
		Plan: "export the tag with git archive",
		Run: func(ctx context.Context, out io.Writer) error {
			if err := os.RemoveAll(src); err != nil {
				return err
			}
			_, err := releasedir.Export(ctx, r.Git, r.Repo, r.tag, src)
			return err
		},
	})
	for _, img := range r.Cfg.Images {
		steps = append(steps, runner.Step{
			Name: "build " + img.Name,
			Plan: "docker build " + img.Dockerfile,
			Run: func(ctx context.Context, out io.Writer) error {
				var err error
				if built == nil {
					if built, err = r.Refs(r.commit); err != nil {
						return err
					}
				}
				for _, b := range built {
					if b.Name != img.Name {
						continue
					}
					argv := []string{"docker", "build", "-f", filepath.Join(src, b.Dockerfile)}
					for _, t := range b.Tags {
						argv = append(argv, "-t", t)
					}
					argv = append(argv, filepath.Join(src, b.Context))
					_, err = r.Docker.Run(ctx, transport.Spec{Argv: argv, Out: out})
					return err
				}
				return fmt.Errorf("image %s not found", img.Name)
			},
		})
	}
	steps = append(steps, runner.Step{
		Name: "push",
		Plan: "docker push every tag",
		Run: func(ctx context.Context, out io.Writer) error {
			for _, b := range built {
				for _, t := range b.Tags {
					if _, err := r.Docker.Run(ctx, transport.Spec{Argv: []string{"docker", "push", t}, Out: out}); err != nil {
						return err
					}
				}
			}
			return nil
		},
	})
	return steps
}

func (r *Release) tagExists(ctx context.Context, tag string) (bool, error) {
	_, err := r.Git.Run(ctx, transport.Spec{Argv: []string{"git", "-C", r.Repo, "rev-parse", "-q", "--verify", "refs/tags/" + tag}})
	if err == nil {
		return true, nil
	}
	var ee *transport.ExitError
	if errors.As(err, &ee) && ee.Code == 1 {
		return false, nil
	}
	return false, err
}
