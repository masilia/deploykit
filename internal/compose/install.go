package compose

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/masilia/deploykit/internal/envfile"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/runner"
	"github.com/masilia/deploykit/internal/tmpl"
	"github.com/masilia/deploykit/internal/transport"
)

var domainPattern = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?\.)+[A-Za-z]{2,}$`)

// Install performs the first installation of the stack on its server (legacy install.sh). It runs with the Local
// transport on the server. Steps are the ones the manifest lists under install.steps.
type Install struct {
	Stack *Stack
	Env   *manifest.EnvSpec
	// Standalone is the TLS configuration when the stack terminates TLS itself; nil behind CloudPanel.
	Standalone *manifest.Standalone
	// Domain and CloudPanelPort describe the ingress for messages.
	CloudPanelPort string
	Crons          []CronEntry
	Notify         func(ctx context.Context, text string)
	BaseURL        string

	file RemoteEnv
	doc  *envfile.Doc
}

// Run executes the install steps. The caller has already copied the manifest's template to .env when it was absent.
func (in *Install) Run(ctx context.Context, r *runner.Runner) error {
	steps := []runner.Step{in.prepare()}
	for _, name := range in.Stack.Cfg.Install.Steps {
		more, err := in.steps(name)
		if err != nil {
			return err
		}
		steps = append(steps, more...)
	}
	steps = append(steps, runner.Step{
		Name: "mark installed",
		Plan: "create the installation marker",
		Run: func(ctx context.Context, out io.Writer) error {
			_, err := in.Stack.T.Run(ctx, transport.Spec{Argv: []string{"touch", in.Stack.Cfg.InstalledMarker}, Dir: in.Stack.Root})
			if err == nil {
				for _, msg := range in.Stack.Cfg.Install.Reminders {
					r.Notice("IMPORTANT: %s", msg)
				}
			}
			return err
		},
	})
	return r.Do(ctx, steps...)
}

func (in *Install) prepare() runner.Step {
	return runner.Step{
		Name: "check",
		Plan: "check the tools and the .env values",
		Run: func(ctx context.Context, out io.Writer) error {
			c := in.Stack.Cfg
			if _, err := in.Stack.T.Run(ctx, transport.Spec{Argv: []string{"test", "-f", c.InstalledMarker}, Dir: in.Stack.Root}); err == nil {
				return fmt.Errorf("already installed (%s exists): use update", c.InstalledMarker)
			}
			in.file = RemoteEnv{T: in.Stack.T, Path: in.Stack.Root + "/" + in.Env.File}
			var err error
			if in.doc, err = in.file.Load(ctx); err != nil {
				return fmt.Errorf("%w (copy the template to %s first)", err, in.Env.File)
			}
			for _, key := range in.Env.Prompts {
				v := in.doc.Get(key, "")
				if v == "" || strings.HasPrefix(v, "CHANGE_ME") {
					return fmt.Errorf("set %s in %s, then run the installation again", key, in.Env.File)
				}
			}
			domain := in.doc.Get("DOMAIN", "")
			if !domainPattern.MatchString(domain) {
				return fmt.Errorf("DOMAIN %q is not a valid domain name", domain)
			}
			if in.Standalone != nil {
				if e := in.doc.Get("LETSENCRYPT_EMAIL", ""); e == "" || strings.HasPrefix(e, "CHANGE_ME") {
					return fmt.Errorf("set LETSENCRYPT_EMAIL in %s, then run the installation again", in.Env.File)
				}
			}
			for _, tool := range []string{"docker", "gpg", "crontab"} {
				if _, err := in.Stack.T.Run(ctx, transport.Spec{Script: "command -v " + tool + " >/dev/null\n"}); err != nil {
					return fmt.Errorf("%s not found on the server", tool)
				}
			}
			return nil
		},
	}
}

func (in *Install) steps(name string) ([]runner.Step, error) {
	s := in.Stack
	svcByRole := func(roles ...string) []string {
		var names []string
		for _, svc := range s.Cfg.Services {
			for _, r := range roles {
				if svc.Role == r {
					names = append(names, svc.Name)
				}
			}
		}
		return names
	}
	app := func(command string) []runner.Step {
		return []runner.Step{{Name: command, Plan: "run the app command " + command, Run: func(ctx context.Context, out io.Writer) error {
			return s.RunApp(ctx, command, in.doc, out)
		}}}
	}
	switch name {
	case "secrets":
		return []runner.Step{{Name: "secrets", Plan: "generate the secrets that are missing", Run: func(ctx context.Context, out io.Writer) error {
			generated, err := EnsureSecrets(in.doc, in.Env.Secrets)
			if err != nil {
				return err
			}
			if len(generated) > 0 {
				_, _ = fmt.Fprintf(out, "secrets generated: %s\n", strings.Join(generated, ", "))
				return in.file.Save(ctx, in.doc)
			}
			return nil
		}}}, nil
	case "images_pull":
		return []runner.Step{
			{Name: "registry login", Plan: "docker login, when the .env holds credentials", Optional: true, Run: in.login},
			{Name: "pull images", Plan: "docker compose pull", Run: func(ctx context.Context, out io.Writer) error {
				_, err := s.Run(ctx, out, "pull")
				return err
			}},
		}, nil
	case "up_data":
		return []runner.Step{{Name: "start data services", Plan: "docker compose up -d for the data services", Run: func(ctx context.Context, out io.Writer) error {
			names := svcByRole("db", "cache", "realtime")
			if len(names) == 0 {
				return nil
			}
			_, err := s.Run(ctx, out, append([]string{"up", "-d"}, names...)...)
			return err
		}}}, nil
	case "jwt_keypair", "migrate", "seed_roles", "create_admin":
		return app(name), nil
	case "up_all":
		return []runner.Step{{Name: "start the stack", Plan: "docker compose up -d", Run: func(ctx context.Context, out io.Writer) error {
			_, err := s.Run(ctx, out, "up", "-d")
			return err
		}}}, nil
	case "tls":
		return []runner.Step{{Name: "tls", Plan: "issue the certificate (standalone) or leave TLS to CloudPanel", Run: in.tls}}, nil
	case "crons", "monitoring_cron":
		return []runner.Step{{Name: name, Plan: "install the crons", Run: func(ctx context.Context, out io.Writer) error {
			if len(in.Crons) == 0 {
				return nil
			}
			return s.InstallCrons(ctx, in.Crons)
		}}}, nil
	case "health":
		return []runner.Step{{Name: "health", Plan: "wait for the stack and run the probes", Run: func(ctx context.Context, out io.Writer) error {
			res, err := s.Health(ctx, in.BaseURL)
			if err != nil {
				return err
			}
			for _, r := range res {
				if !r.OK {
					_, _ = fmt.Fprintf(out, "KO %s: %s\n", r.Name, r.Detail)
				}
			}
			if !HealthOK(res) {
				return fmt.Errorf("the stack is not healthy after the installation")
			}
			return nil
		}}}, nil
	}
	return nil, fmt.Errorf("unknown install step %q", name)
}

func (in *Install) login(ctx context.Context, out io.Writer) error {
	user, token := in.doc.Get("GHCR_USER", ""), in.doc.Get("GHCR_TOKEN", "")
	if user == "" || token == "" {
		return nil
	}
	host, _, _ := strings.Cut(in.doc.Get("REGISTRY", "ghcr.io"), "/")
	_, err := in.Stack.T.Run(ctx, transport.Spec{Argv: []string{"docker", "login", host, "-u", user, "--password-stdin"}, Stdin: strings.NewReader(token)})
	return err
}

func (in *Install) tls(ctx context.Context, out io.Writer) error {
	if in.Standalone == nil {
		_, _ = fmt.Fprintf(out, "TLS is terminated by CloudPanel: point the site at 127.0.0.1:%s\n", in.CloudPanelPort)
		return nil
	}
	st := in.Standalone
	if len(st.TLS.Volumes) < 2 {
		return fmt.Errorf("ingress.tls.volumes must list the letsencrypt and certbot-www volumes")
	}
	domain := in.doc.Get("DOMAIN", "")
	vars := tmpl.Vars{"target": map[string]string{"domain": domain}, "env": envMap(in.doc)}
	target, err := tmpl.Render(st.TLS.DomainConf.Target, vars, false)
	if err != nil {
		return err
	}
	project := in.Stack.Project
	_, err = in.Stack.Script(ctx, out, `set -e
if docker volume inspect "$DK_LE" >/dev/null 2>&1 && docker run --rm -v "$DK_LE:/etc/letsencrypt" `+helperImage+` test -d "/etc/letsencrypt/live/$DK_DOMAIN"; then
  echo "certificate already present for $DK_DOMAIN"
else
  docker run --rm -v "$DK_WWW:/var/www/certbot" -v "$DK_LE:/etc/letsencrypt" certbot/certbot certonly --webroot -w /var/www/certbot -d "$DK_DOMAIN" --agree-tos -m "$DK_EMAIL" --non-interactive \
    || { echo "certificate issuance failed (does the DNS point at this machine? is port 80 open?)" >&2; exit 1; }
fi
if [ ! -f "$DK_CONF" ]; then
  sed "s|__DOMAIN__|$DK_DOMAIN|g" "$DK_TEMPLATE" > "$DK_CONF"
  compose exec -T proxy nginx -s reload
  echo "TLS enabled for $DK_DOMAIN"
fi
`, map[string]string{
		"DK_LE": project + "_" + st.TLS.Volumes[0], "DK_WWW": project + "_" + st.TLS.Volumes[1],
		"DK_DOMAIN": domain, "DK_EMAIL": in.doc.Get("LETSENCRYPT_EMAIL", ""), "DK_CONF": target, "DK_TEMPLATE": st.TLS.DomainConf.Template,
	}, nil)
	return err
}

// CronsFor lists the crons a deployment root needs, from the manifest. bin is the deploykit binary path on the server.
func CronsFor(m *manifest.Manifest, standalone *manifest.Standalone, bin string) ([]CronEntry, error) {
	var out []CronEntry
	if m.Backup.Kind != 0 {
		_, b, err := m.BackupConfig()
		if err != nil {
			return nil, err
		}
		if b.Schedule != "" {
			out = append(out, CronEntry{Name: "backup", Schedule: b.Schedule, Command: bin + " server backup"})
		}
	}
	if m.RestoreTest.Kind != 0 {
		rt, err := m.RestoreTestConfig()
		if err != nil {
			return nil, err
		}
		if rt.Schedule != "" {
			out = append(out, CronEntry{Name: "restore-test", Schedule: rt.Schedule, Command: bin + " server restore-test"})
		}
	}
	if m.Monitor.Kind != 0 {
		mon, err := m.MonitorConfig()
		if err != nil {
			return nil, err
		}
		out = append(out, CronEntry{Name: "monitor", Schedule: mon.Schedule, Command: bin + " server monitor"})
	}
	if standalone != nil && standalone.TLS.Renew.Schedule != "" {
		out = append(out, CronEntry{Name: "renew-certs", Schedule: standalone.TLS.Renew.Schedule, Command: bin + " server renew-certs"})
	}
	jobs, err := m.CronJobs()
	if err != nil {
		return nil, err
	}
	for _, c := range jobs {
		out = append(out, CronEntry{Name: c.Name, Schedule: c.Schedule, Command: bin + " server cron " + c.Name})
	}
	return out, nil
}
