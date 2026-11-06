package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/masilia/deploykit/internal/compose"
	"github.com/masilia/deploykit/internal/envfile"
	"github.com/masilia/deploykit/internal/image"
	"github.com/masilia/deploykit/internal/inventory"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/runner"
	"github.com/masilia/deploykit/internal/tmpl"
	"github.com/masilia/deploykit/internal/transport"
)

// ServerPlatform maps `uname -sm` to Go's GOOS and GOARCH.
func ServerPlatform(uname string) (goos, goarch string, err error) {
	f := strings.Fields(uname)
	if len(f) != 2 {
		return "", "", fmt.Errorf("unexpected uname output %q", uname)
	}
	switch f[0] {
	case "Linux":
		goos = "linux"
	case "Darwin":
		goos = "darwin"
	default:
		return "", "", fmt.Errorf("unsupported server system %q", f[0])
	}
	switch f[1] {
	case "x86_64", "amd64":
		goarch = "amd64"
	case "aarch64", "arm64":
		goarch = "arm64"
	default:
		return "", "", fmt.Errorf("unsupported server architecture %q", f[1])
	}
	return goos, goarch, nil
}

// Compose deploys a compose stack: it makes sure the images exist, uploads the files and the server binary of the same
// version, configures .env and runs `deploykit server install|update` on the server, so a dropped SSH connection cannot
// leave an update half done (decision D3).
type Compose struct {
	Manifest *manifest.Manifest
	Target   *inventory.Target
	Site     transport.Transport // the server, as the deploy user, in the deployment root
	Repo     string              // the project repository (source of the files to upload)
	Version  string
	Install  bool
	// Release builds and pushes the images when they are not in the registry; nil skips that check.
	Release *image.Release
	// EnvFile holds KEY=VALUE pairs injected into the server .env (secrets travel on stdin).
	EnvFile []envfile.Assignment
	// Binary is the deploykit binary to upload; BinaryExplicit means the operator chose it, so the platform check is
	// skipped. LocalOS and LocalArch default to the running binary's.
	Binary         string
	BinaryExplicit bool
	LocalOS        string
	LocalArch      string
	// TargetFile is the target's file, uploaded so the server knows its own settings (never the whole inventory).
	TargetFile string
	// Registry is written to the server .env as REGISTRY when set.
	Registry string
}

func (c *Compose) root() string { return strings.TrimRight(c.Target.Get("path"), "/") }

func (c *Compose) remote(rel string) string { return c.root() + "/" + rel }

// Run executes the deploy.
func (c *Compose) Run(ctx context.Context, r *runner.Runner) error {
	root := c.root()
	if root == "" || !strings.HasPrefix(root, "/") {
		return fmt.Errorf("target %s: path must be an absolute directory", c.Target.ID)
	}
	cfg, err := c.Manifest.Compose()
	if err != nil {
		return err
	}
	sync, err := c.Manifest.SyncConfig()
	if err != nil {
		return err
	}
	env, err := c.Manifest.EnvConfig()
	if err != nil {
		return err
	}
	if c.Release != nil {
		present, err := c.Release.Present(ctx)
		if err != nil {
			return err
		}
		if present {
			r.Notice("release %s is already in the registry: no build", c.Version)
		} else if err := r.Do(ctx, c.Release.Steps()...); err != nil {
			return err
		}
	}
	steps := []runner.Step{
		{Name: "connect", Plan: "check the SSH access", Run: func(ctx context.Context, out io.Writer) error {
			_, err := c.Site.Run(ctx, transport.Spec{Argv: []string{"true"}})
			return err
		}},
		{Name: "upload files", Plan: "rsync the compose files and the manifest", Run: func(ctx context.Context, out io.Writer) error {
			return c.uploadFiles(ctx, out, cfg, sync)
		}},
		{Name: "server binary", Plan: "upload the deploykit binary of this version", Run: c.uploadBinary},
		{Name: "prepare .env", Plan: "check the installation state and create .env from its template", Run: func(ctx context.Context, out io.Writer) error {
			return c.prepareEnv(ctx, out, cfg, env)
		}},
		{Name: "configure .env", Plan: "set the version and the injected keys", Run: func(ctx context.Context, out io.Writer) error {
			return c.configureEnv(ctx, out, cfg)
		}},
		{Name: "run on the server", Plan: "deploykit server " + c.verb(), Run: func(ctx context.Context, out io.Writer) error {
			_, err := c.Site.Run(ctx, transport.Spec{
				Argv: []string{c.remote(".deploykit/deploykit"), "server", c.verb(), "--manifest", c.remote("deploykit.yaml")},
				Dir:  root, Out: out,
			})
			if err != nil {
				return fmt.Errorf("the %s failed on the server (see its output above; rollback: deploy an earlier version): %w", c.verb(), err)
			}
			return nil
		}},
	}
	return r.Do(ctx, steps...)
}

// generatedFiles lists the files the server creates inside the deployment root, relative to it: with the standalone ingress, the TLS
// configuration of the domain, and of each extra domain, that the install makes from a template. Losing one at the next deploy would
// take its HTTPS down at the next reload of the proxy.
func (c *Compose) generatedFiles() []string {
	vars := c.Target.Vars()
	vars["project"] = c.Manifest.Project
	ax := c.Manifest.Ingress.Axis
	if sel := c.Manifest.Ingress.Select; sel != "" {
		key, err := tmpl.Render(sel, vars, false)
		if err != nil {
			return nil
		}
		if ax, err = c.Manifest.Ingress.Variant(key); err != nil {
			return nil
		}
	}
	if ax.Type != manifest.IngressStandalone {
		return nil
	}
	var st manifest.Standalone
	if err := ax.Decode(&st); err != nil || st.TLS.DomainConf.Target == "" {
		return nil
	}
	target, err := tmpl.Render(st.TLS.DomainConf.Target, vars, false)
	if err != nil {
		return nil
	}
	files := []string{path.Clean(target)}
	for _, e := range st.TLS.ExtraDomains {
		if e.Target != "" {
			files = append(files, path.Clean(e.Target))
		}
	}
	return files
}

func (c *Compose) verb() string {
	if c.Install {
		return "install"
	}
	return "update"
}

func (c *Compose) uploadFiles(ctx context.Context, out io.Writer, cfg *manifest.Compose, sync *manifest.SyncSpec) error {
	if _, err := c.Site.Run(ctx, transport.Spec{Argv: []string{"mkdir", "-p", c.root(), c.remote(".deploykit"), c.remote("log")}}); err != nil {
		return fmt.Errorf("cannot create %s with this user (as root: mkdir -p %s && chown <user> %s): %w", c.root(), c.root(), c.root(), err)
	}
	files := append([]string(nil), sync.Files...)
	files = append(files, cfg.Files...)
	for _, o := range cfg.Overrides {
		files = append(files, o.File)
	}
	generated := c.generatedFiles()
	seen := map[string]bool{}
	for _, f := range files {
		if seen[f] {
			continue
		}
		seen[f] = true
		src := filepath.Join(c.Repo, f)
		dir := strings.HasSuffix(f, "/")
		if _, err := os.Stat(src); err != nil {
			if os.IsNotExist(err) {
				_, _ = fmt.Fprintf(out, "skipped %s (not in the project)\n", f)
				continue
			}
			return err
		}
		spec := transport.SyncSpec{Src: src, Dst: c.remote(f), Excludes: sync.Never}
		if dir {
			// filepath.Join drops the trailing slash, which rsync needs to mirror the contents instead of nesting the folder.
			spec.Src += "/"
			spec.Delete = true
			// A folder is mirrored with --delete: what the server wrote inside it must not be deleted by the next deploy.
			for _, g := range generated {
				if rel, ok := strings.CutPrefix(g, f); ok && rel != "" {
					spec.Excludes = append(slices.Clone(spec.Excludes), "/"+rel)
				}
			}
		} else if _, err := c.Site.Run(ctx, transport.Spec{Argv: []string{"mkdir", "-p", path.Dir(spec.Dst)}}); err != nil {
			return err
		}
		if err := c.Site.Sync(ctx, spec); err != nil {
			return fmt.Errorf("upload %s: %w", f, err)
		}
	}
	// The manifest, and only this target's settings: the inventory of the other targets never reaches a server.
	if err := c.Site.Sync(ctx, transport.SyncSpec{Src: c.Manifest.Path(), Dst: c.remote("deploykit.yaml")}); err != nil {
		return err
	}
	target, err := os.ReadFile(c.TargetFile)
	if err != nil {
		return err
	}
	return writeFile(ctx, c.Site, c.remote(".deploykit/target.env"), string(target))
}

func fileSHA(p string) (string, error) {
	f, err := os.Open(p) //nolint:gosec // the path is the operator's own binary
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (c *Compose) uploadBinary(ctx context.Context, out io.Writer) error {
	if !c.BinaryExplicit {
		res, err := c.Site.Run(ctx, transport.Spec{Argv: []string{"uname", "-sm"}})
		if err != nil {
			return err
		}
		goos, goarch, err := ServerPlatform(res.Stdout)
		if err != nil {
			return err
		}
		lo, la := c.LocalOS, c.LocalArch
		if lo == "" {
			lo = runtime.GOOS
		}
		if la == "" {
			la = runtime.GOARCH
		}
		if lo != goos || la != goarch {
			return fmt.Errorf("this deploykit is %s/%s but the server is %s/%s: pass --server-binary with a %s/%s build", lo, la, goos, goarch, goos, goarch)
		}
	}
	local, err := fileSHA(c.Binary)
	if err != nil {
		return err
	}
	dst := c.remote(".deploykit/deploykit")
	res, _ := c.Site.Run(ctx, transport.Spec{Script: `sha256sum "$DK_BIN" 2>/dev/null | cut -d' ' -f1` + "\n", Env: map[string]string{"DK_BIN": dst}})
	if strings.TrimSpace(res.Stdout) == local {
		_, _ = fmt.Fprintln(out, "the server already has this exact binary: "+c.serverVersion(ctx, dst))
		return nil
	}
	if err := c.Site.Sync(ctx, transport.SyncSpec{Src: c.Binary, Dst: dst}); err != nil {
		return err
	}
	if _, err := c.Site.Run(ctx, transport.Spec{Argv: []string{"chmod", "755", dst}}); err != nil {
		return err
	}
	res, err = c.Site.Run(ctx, transport.Spec{Script: `sha256sum "$DK_BIN" | cut -d' ' -f1` + "\n", Env: map[string]string{"DK_BIN": dst}})
	if err != nil {
		return err
	}
	if strings.TrimSpace(res.Stdout) != local {
		return fmt.Errorf("the uploaded binary does not match (sha256 differs): upload again")
	}
	_, _ = fmt.Fprintf(out, "binary uploaded (sha256 %s…): %s\n", local[:12], c.serverVersion(ctx, dst))
	return nil
}

// serverVersion asks the uploaded binary for its own version, so the deploy log says which build runs on the server.
func (c *Compose) serverVersion(ctx context.Context, binary string) string {
	res, err := c.Site.Run(ctx, transport.Spec{Argv: []string{binary, "version"}})
	if v := strings.TrimSpace(res.Stdout); err == nil && v != "" {
		return v
	}
	return "version unreadable"
}

func (c *Compose) prepareEnv(ctx context.Context, out io.Writer, cfg *manifest.Compose, env *manifest.EnvSpec) error {
	installed := func() bool {
		_, err := c.Site.Run(ctx, transport.Spec{Argv: []string{"test", "-f", c.remote(cfg.InstalledMarker)}})
		return err == nil
	}
	envPath := c.remote(env.File)
	hasEnv := func() bool {
		_, err := c.Site.Run(ctx, transport.Spec{Argv: []string{"test", "-f", envPath}})
		return err == nil
	}
	if c.Install {
		if installed() {
			return fmt.Errorf("already installed (%s exists): deploy without --install", cfg.InstalledMarker)
		}
		if !hasEnv() {
			if env.Template == "" {
				return fmt.Errorf("no %s on the server and the manifest has no env.template", env.File)
			}
			if _, err := c.Site.Run(ctx, transport.Spec{Script: `set -e
umask 077
cp "$DK_TEMPLATE" "$DK_ENV"
chmod 600 "$DK_ENV"
`, Env: map[string]string{"DK_TEMPLATE": c.remote(env.Template), "DK_ENV": envPath}}); err != nil {
				return fmt.Errorf("cannot create %s from %s: %w", env.File, env.Template, err)
			}
			_, _ = fmt.Fprintf(out, "%s created from %s (mode 600)\n", env.File, env.Template)
		}
		return nil
	}
	if !hasEnv() {
		return fmt.Errorf("no %s on the server: first deployment? use --install", env.File)
	}
	return nil
}

// configureEnv writes the injected keys, the version and the registry into the server .env. The version being replaced
// is remembered as the previous one, which is what the automatic rollback returns to.
func (c *Compose) configureEnv(ctx context.Context, out io.Writer, cfg *manifest.Compose) error {
	env, err := c.Manifest.EnvConfig()
	if err != nil {
		return err
	}
	file := compose.RemoteEnv{T: c.Site, Path: c.remote(env.File)}
	doc, err := file.Load(ctx)
	if err != nil {
		return err
	}
	// Keys the manifest takes from the target (env.from_target_on_deploy), such as PROXY_PORT: bootstrap may have picked a
	// free port that differs from the template's, and compose reads the port from this .env. REGISTRY has its own path
	// below, and an explicit --env value wins over the target because it is applied afterwards.
	for _, key := range env.FromTargetOnDeploy {
		if key == "REGISTRY" {
			continue
		}
		if v := c.Target.Get(strings.ToLower(key)); v != "" {
			if err := doc.Set(key, v); err != nil {
				return err
			}
		}
	}
	if err := doc.Apply(c.EnvFile); err != nil {
		return err
	}
	if len(c.EnvFile) > 0 {
		_, _ = fmt.Fprintf(out, "%d key(s) injected\n", len(c.EnvFile))
	}
	vs := cfg.VersionState
	if c.Version != "" {
		// On a first install the version in .env is the template's placeholder, not a release that was ever deployed:
		// recording it as "previous" would make an automatic rollback aim at images that do not exist.
		if prev := doc.Get(vs.CurrentKey, ""); !c.Install && prev != "" && prev != c.Version {
			if err := doc.Set(vs.PreviousKey, prev); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "version replaced: %s -> %s (previous kept as %s)\n", prev, c.Version, vs.PreviousKey)
		}
		if err := doc.Set(vs.CurrentKey, c.Version); err != nil {
			return err
		}
		if c.Registry != "" {
			if err := doc.Set("REGISTRY", c.Registry); err != nil {
				return err
			}
		}
	} else {
		_, _ = fmt.Fprintf(out, "no --version: %s left as it is\n", vs.CurrentKey)
	}
	return file.Save(ctx, doc)
}
