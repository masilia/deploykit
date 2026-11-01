package deploy

import (
	"context"
	"fmt"
	"io"

	"github.com/masilia/deploykit/internal/cloudpanel"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/runner"
	"github.com/masilia/deploykit/internal/secrets"
	"github.com/masilia/deploykit/internal/tmpl"
)

// phpPlan carries what Bootstrap.Run worked out, for the steps of a PHP site.
type phpPlan struct {
	cfg                manifest.CloudPanel
	cp                 *cloudpanel.Client
	layout             Layout
	domain, user, root string
	pubkey             string
	password           int
	requireDNS         []string
	siteExists         func(context.Context) (bool, error)
}

// runPHP provisions a CloudPanel PHP site whose document root is the public folder of the current release: a vhost template
// is derived from a base one (root directory current/<web_root>, script path resolved with $realpath_root), the site is
// created with it, and releases/ and shared/ are made. There is no port to pick and nothing to start.
func (b *Bootstrap) runPHP(ctx context.Context, r *runner.Runner, p phpPlan) error {
	php, err := b.Manifest.PHP()
	if err != nil {
		return err
	}
	tpl := p.cfg.Site.VhostTemplate
	if tpl.Name == "" {
		return fmt.Errorf("ingress.site.vhost_template is required for a php site: a name, or {name, base} to derive one")
	}
	if p.cfg.Site.PHPVersion != "" && p.cfg.Site.PHPVersion != php.PHPVersion {
		return fmt.Errorf("ingress.site.php_version %s and runtime.php_version %s differ", p.cfg.Site.PHPVersion, php.PHPVersion)
	}
	vars := b.Target.Vars()
	vars["project"] = b.Manifest.Project
	render := func(s string) (string, error) { return tmpl.Render(s, vars, false) }
	tplName, err := render(tpl.Name)
	if err != nil {
		return fmt.Errorf("ingress.site.vhost_template.name: %w", err)
	}
	return r.Do(ctx,
		runner.Step{
			Name: "server",
			Plan: "check that clpctl is installed",
			Run: func(ctx context.Context, out io.Writer) error {
				ok, err := p.cp.HasClpctl(ctx)
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("clpctl not found: is this a CloudPanel server, and is the provisioning user root?")
				}
				_, _ = fmt.Fprintln(out, "clpctl found")
				return nil
			},
		},
		runner.Step{
			Name: "vhost template " + tplName,
			Plan: "derive the template from " + tpl.Base + " with the root directory current/" + php.WebRoot,
			Run: func(ctx context.Context, out io.Writer) error {
				if tpl.Base == "" {
					_, _ = fmt.Fprintf(out, "using the existing template %q\n", tplName)
					return nil
				}
				created, err := p.cp.EnsureVhostTemplate(ctx, cloudpanel.TemplateSpec{
					Name: tplName, Base: tpl.Base, RootDirectory: "current/" + php.WebRoot, PHPVersion: php.PHPVersion,
				})
				if err != nil {
					return err
				}
				if created {
					_, _ = fmt.Fprintf(out, "template %q created from %q\n", tplName, tpl.Base)
				} else {
					_, _ = fmt.Fprintf(out, "template %q already exists\n", tplName)
				}
				return nil
			},
		},
		runner.Step{
			Name:  "site " + p.domain,
			Plan:  "create the CloudPanel PHP site with clpctl",
			Check: p.siteExists,
			Run: func(ctx context.Context, out io.Writer) error {
				password, err := secrets.Password(p.password)
				if err != nil {
					return err
				}
				if err := p.cp.AddSite(ctx, cloudpanel.SiteSpec{
					Kind: cloudpanel.SitePHP, Domain: p.domain, SiteUser: p.user, Password: password,
					PHPVersion: php.PHPVersion, VhostTemplate: tplName,
				}); err != nil {
					return err
				}
				r.Notice("site user %s created. Its password (only for the CloudPanel file manager / SFTP, deploys use your SSH key): %s", p.user, password)
				return nil
			},
		},
		runner.Step{
			Name:     "certificate",
			Plan:     "request a Let's Encrypt certificate (needs DNS pointing at the server)",
			Optional: true,
			Run: func(ctx context.Context, out io.Writer) error {
				if err := p.cp.InstallCertificate(ctx, p.domain, p.requireDNS); err != nil {
					return fmt.Errorf("%w (run bootstrap again once the DNS points at this server)", err)
				}
				return nil
			},
		},
		runner.Step{
			Name: "folders",
			Plan: "create releases/ and shared/ owned by the site user",
			Run: func(ctx context.Context, out io.Writer) error {
				return p.cp.EnsureLayout(ctx, p.layout.Root, p.user, []string{"releases", "shared"})
			},
		},
		runner.Step{
			Name: "ssh key",
			Plan: "install your public key for the site user",
			Run: func(ctx context.Context, out io.Writer) error {
				return p.cp.InstallAuthorizedKey(ctx, p.user, p.pubkey)
			},
		},
		runner.Step{
			Name: "shared files",
			Plan: "remind which files the operator must create in shared/",
			Run: func(ctx context.Context, out io.Writer) error {
				for _, s := range php.Shared {
					if s.Required {
						_, _ = fmt.Fprintf(out, "create %s/%s on the server (mode 600, owned by %s): it holds the secrets, deploys refuse to run without it\n", p.layout.Shared(), s.Path, p.user)
					}
				}
				return nil
			},
		},
	)
}
