package deploy

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/masilia/deploykit/internal/cloudpanel"
	"github.com/masilia/deploykit/internal/inventory"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/runner"
	"github.com/masilia/deploykit/internal/secrets"
	"github.com/masilia/deploykit/internal/tmpl"
	"github.com/masilia/deploykit/internal/transport"
)

// Bootstrap provisions a CloudPanel Node.js site once: port, site, certificate, folders, SSH key, pm2 and its start at
// boot. Every step checks its state first, so it can be run again (legacy bootstrap.sh).
type Bootstrap struct {
	Manifest *manifest.Manifest
	Target   *inventory.Target
	Root     transport.Transport // root (or sudoer) on the server
	Site     transport.Transport // the site user; usable once the SSH key is installed
	DNS      cloudpanel.Resolver

	// PublicKey is the public key file to install for the site user. Empty: ~/.ssh/id_ed25519.pub, then id_rsa.pub.
	PublicKey string
	// ReadFile and HomeDir are replaced in tests.
	ReadFile func(string) ([]byte, error)
	HomeDir  string
}

// Run executes the bootstrap steps.
func (b *Bootstrap) Run(ctx context.Context, r *runner.Runner) error {
	var cfg manifest.CloudPanel
	if b.Manifest.Ingress.Select != "" || b.Manifest.Ingress.Type != manifest.IngressCloudPanel {
		return fmt.Errorf("bootstrap needs a single cloudpanel ingress")
	}
	if err := b.Manifest.Ingress.Decode(&cfg); err != nil {
		return err
	}
	if cfg.Site.Type != "nodejs" {
		return fmt.Errorf("this bootstrap provisions nodejs sites, not %q", cfg.Site.Type)
	}
	vars := b.Target.Vars()
	vars["project"] = b.Manifest.Project
	render := func(s string) (string, error) { return tmpl.Render(s, vars, false) }

	domain, user, root := b.Target.Get("domain"), b.Target.Get("site_user"), b.Target.Get("path")
	if user == "" {
		return fmt.Errorf("target %s: site_user is required", b.Target.ID)
	}
	if root == "" || !strings.HasPrefix(root, "/") {
		return fmt.Errorf("target %s: path must be an absolute directory", b.Target.ID)
	}
	port, err := strconv.Atoi(b.Target.Get("app_port"))
	if err != nil {
		return fmt.Errorf("target %s: app_port must be a number", b.Target.ID)
	}
	var requireDNS []string
	for _, h := range cfg.TLS.RequireDNS {
		rendered, err := render(h)
		if err != nil {
			return fmt.Errorf("ingress.tls.require_dns: %w", err)
		}
		requireDNS = append(requireDNS, rendered)
	}
	pubkey, err := b.publicKey()
	if err != nil {
		return err
	}
	pw := cfg.Site.UserPassword.Generate
	if pw == 0 {
		pw = 24
	}

	cp := &cloudpanel.Client{Root: b.Root, DNS: b.DNS}
	layout := Layout{Root: strings.TrimRight(root, "/")}
	siteExists := func(ctx context.Context) (bool, error) { return cp.SiteExists(ctx, root, domain) }

	return r.Do(ctx,
		runner.Step{
			Name: "server",
			Plan: "check that clpctl is installed",
			Run: func(ctx context.Context, out io.Writer) error {
				ok, err := cp.HasClpctl(ctx)
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("clpctl not found: is this a CloudPanel server, and is the provisioning user root?")
				}
				res, err := b.Root.Run(ctx, transport.Spec{Argv: []string{"uname", "-sm"}})
				if err == nil {
					_, _ = fmt.Fprintf(out, "%s, clpctl found\n", strings.TrimSpace(res.Stdout))
				}
				return nil
			},
		},
		runner.Step{
			Name:  "port",
			Plan:  "pick a free application port and write it back to the target",
			Check: siteExists,
			Run: func(ctx context.Context, out io.Writer) error {
				used, err := cp.UsedPorts(ctx)
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(out, "ports in use on the server: %v\n", used)
				picked := cloudpanel.PickPort(port, used)
				if picked != port {
					_, _ = fmt.Fprintf(out, "port %d is taken, using %d\n", port, picked)
					if err := b.Target.Set("app_port", strconv.Itoa(picked)); err != nil {
						return err
					}
					if err := b.Target.Save(); err != nil {
						return err
					}
					port = picked
				} else {
					_, _ = fmt.Fprintf(out, "port %d is free\n", port)
				}
				return nil
			},
		},
		runner.Step{
			Name:  "site " + domain,
			Plan:  "create the CloudPanel Node.js site with clpctl",
			Check: siteExists,
			Run: func(ctx context.Context, out io.Writer) error {
				password, err := secrets.Password(pw)
				if err != nil {
					return err
				}
				if err := cp.AddSite(ctx, cloudpanel.SiteSpec{
					Kind: cloudpanel.SiteNodeJS, Domain: domain, SiteUser: user, Password: password,
					NodeVersion: cfg.Site.NodeVersion, AppPort: port,
				}); err != nil {
					return err
				}
				// Shown once, as a notice so a folded step cannot hide it: it only serves SFTP and the file manager (ADR 0006).
				r.Notice("site user %s created. Its password (only for the CloudPanel file manager / SFTP, deploys use your SSH key): %s", user, password)
				return nil
			},
		},
		runner.Step{
			Name:     "certificate",
			Plan:     "request a Let's Encrypt certificate (needs DNS pointing at the server)",
			Optional: true,
			Run: func(ctx context.Context, out io.Writer) error {
				if err := cp.InstallCertificate(ctx, domain, requireDNS); err != nil {
					return fmt.Errorf("%w (run bootstrap again once the DNS points at this server)", err)
				}
				return nil
			},
		},
		runner.Step{
			Name: "folders",
			Plan: "create releases/ and shared/ owned by the site user",
			Run: func(ctx context.Context, out io.Writer) error {
				return cp.EnsureLayout(ctx, layout.Root, user, []string{"releases", "shared"})
			},
		},
		runner.Step{
			Name: "ssh key",
			Plan: "install your public key for the site user",
			Run:  func(ctx context.Context, out io.Writer) error { return cp.InstallAuthorizedKey(ctx, user, pubkey) },
		},
		runner.Step{
			Name: "pm2",
			Plan: "make sure pm2 is available to the site user",
			Run: func(ctx context.Context, out io.Writer) error {
				_, err := b.Site.Run(ctx, transport.Spec{
					// `pm2 -v` prints a banner on stdout when it has to spawn its daemon; only the last line is the version.
					Script: `command -v pm2 >/dev/null || npm install -g pm2` + "\n" + `echo "pm2 $(pm2 -v 2>/dev/null | tail -n 1), node $(node -v)"` + "\n",
					Out:    out,
				})
				return err
			},
		},
		runner.Step{
			Name: "pm2 at boot",
			Plan: "enable the pm2 systemd unit of the site user",
			Run: func(ctx context.Context, out io.Writer) error {
				changed, err := cp.EnablePM2Boot(ctx, b.Site, user)
				if err != nil {
					return fmt.Errorf("%w (run the command pm2 printed as root on the server)", err)
				}
				if !changed {
					_, _ = fmt.Fprintln(out, "pm2 startup is already set up")
				}
				return nil
			},
		},
	)
}

// publicKey reads the key to install: the explicit file, or the first default that exists.
func (b *Bootstrap) publicKey() (string, error) {
	read := b.ReadFile
	if read == nil {
		read = os.ReadFile
	}
	candidates := []string{b.PublicKey}
	if b.PublicKey == "" {
		home := b.HomeDir
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		candidates = []string{filepath.Join(home, ".ssh", "id_ed25519.pub"), filepath.Join(home, ".ssh", "id_rsa.pub")}
	}
	for _, c := range candidates {
		if data, err := read(c); err == nil && strings.TrimSpace(string(data)) != "" {
			return strings.TrimSpace(string(data)), nil
		}
	}
	return "", fmt.Errorf("no public key found (tried %s); pass one with --pubkey", strings.Join(candidates, ", "))
}
