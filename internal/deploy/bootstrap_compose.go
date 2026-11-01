package deploy

import (
	"context"
	"fmt"
	"io"
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

// ComposeBootstrap prepares a server for a compose stack that sits behind CloudPanel: it picks the loopback port,
// creates the CloudPanel reverse-proxy site that forwards the domain to it, asks for the certificate and creates the
// deployment root for the deploy user. The stack itself is installed afterwards by `deploy --install`. Every step checks
// its state first, so it can be run again.
type ComposeBootstrap struct {
	Manifest *manifest.Manifest
	Target   *inventory.Target
	Root     transport.Transport // root (or a sudoer) on the server
	DNS      cloudpanel.Resolver
}

// Run executes the steps.
func (b *ComposeBootstrap) Run(ctx context.Context, r *runner.Runner) error {
	vars := b.Target.Vars()
	vars["project"] = b.Manifest.Project
	ax := b.Manifest.Ingress.Axis
	if b.Manifest.Ingress.Select != "" {
		key, err := tmpl.Render(b.Manifest.Ingress.Select, vars, false)
		if err != nil {
			return fmt.Errorf("ingress.select: %w", err)
		}
		if ax, err = b.Manifest.Ingress.Variant(key); err != nil {
			return err
		}
	}
	if ax.Type != manifest.IngressCloudPanel {
		return fmt.Errorf("target %s uses the %s ingress: nothing to provision on CloudPanel (use `deploy --install`)", b.Target.ID, ax.Type)
	}
	var cfg manifest.CloudPanel
	if err := ax.Decode(&cfg); err != nil {
		return err
	}
	if cfg.Site.Type != "reverse-proxy" {
		return fmt.Errorf("this bootstrap provisions reverse-proxy sites, not %q", cfg.Site.Type)
	}
	domain, siteUser, deployUser, root := b.Target.Get("domain"), b.Target.Get("site_user"), b.Target.Get("ssh_user"), strings.TrimRight(b.Target.Get("path"), "/")
	switch {
	case siteUser == "":
		return fmt.Errorf("target %s: site_user is required (the system user of the CloudPanel site)", b.Target.ID)
	case deployUser == "" || !cloudpanel.ValidSiteUser(deployUser):
		return fmt.Errorf("target %s: ssh_user %q is not a valid user name", b.Target.ID, deployUser)
	case !strings.HasPrefix(root, "/"):
		return fmt.Errorf("target %s: path must be an absolute directory", b.Target.ID)
	}
	port, err := strconv.Atoi(b.Target.Get("proxy_port"))
	if err != nil {
		return fmt.Errorf("target %s: proxy_port must be a number", b.Target.ID)
	}
	var requireDNS []string
	for _, h := range cfg.TLS.RequireDNS {
		rendered, err := tmpl.Render(h, vars, false)
		if err != nil {
			return fmt.Errorf("ingress.tls.require_dns: %w", err)
		}
		requireDNS = append(requireDNS, rendered)
	}
	pw := cfg.Site.UserPassword.Generate
	if pw == 0 {
		pw = 24
	}

	cp := &cloudpanel.Client{Root: b.Root, DNS: b.DNS}
	// A reverse-proxy site has no folder of its own that we own, so existence is the vhost.
	siteExists := func(ctx context.Context) (bool, error) { return cp.SiteExists(ctx, "", domain) }

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
				return nil
			},
		},
		runner.Step{
			Name:  "port",
			Plan:  "pick a free loopback port for the stack's proxy and write it back to the target",
			Check: siteExists,
			Run: func(ctx context.Context, out io.Writer) error {
				used, err := cp.UsedPorts(ctx)
				if err != nil {
					return err
				}
				picked := cloudpanel.PickPort(port, used)
				if picked != port {
					_, _ = fmt.Fprintf(out, "port %d is taken, using %d\n", port, picked)
					if err := b.Target.Set("proxy_port", strconv.Itoa(picked)); err != nil {
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
			Plan:  "create the CloudPanel reverse-proxy site towards the loopback port",
			Check: siteExists,
			Run: func(ctx context.Context, out io.Writer) error {
				password, err := secrets.Password(pw)
				if err != nil {
					return err
				}
				if err := cp.AddSite(ctx, cloudpanel.SiteSpec{
					Kind: cloudpanel.SiteReverseProxy, Domain: domain, SiteUser: siteUser, Password: password,
					ProxyURL: "http://127.0.0.1:" + strconv.Itoa(port),
				}); err != nil {
					return err
				}
				r.Notice("site user %s created. Its password (only for the CloudPanel file manager / SFTP): %s", siteUser, password)
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
			Name: "deployment root",
			Plan: "create " + root + " owned by " + deployUser,
			Check: func(ctx context.Context) (bool, error) {
				_, err := b.Root.Run(ctx, transport.Spec{Argv: []string{"test", "-d", root}})
				return err == nil, nil
			},
			Run: func(ctx context.Context, out io.Writer) error {
				_, err := b.Root.Run(ctx, transport.Spec{
					Script: "set -e\nmkdir -p \"$DK_ROOT\"\nchown \"$DK_USER:$DK_USER\" \"$DK_ROOT\"\nchmod 750 \"$DK_ROOT\"\n",
					Env:    map[string]string{"DK_ROOT": root, "DK_USER": deployUser},
				})
				return err
			},
		},
		runner.Step{
			Name: "reminders",
			Plan: "list what clpctl cannot do",
			Run: func(ctx context.Context, out io.Writer) error {
				r.Notice("CloudPanel's nginx buffers server-sent events for HTTP/1.1 clients unless the upstream sends `X-Accel-Buffering: no`. clpctl cannot edit a vhost, so have the stack's own proxy send that header on the Mercure location (add_header X-Accel-Buffering no;); no vhost directive and no client_max_body_size is needed (docs/cloudpanel-cli.md)")
				r.Notice("next: deploykit deploy --install --version <X.Y.Z> -t %s", b.Target.ID)
				return nil
			},
		},
	)
}
