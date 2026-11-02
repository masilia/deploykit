package compose

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/runner"
	"github.com/masilia/deploykit/internal/transport"
)

const installEnv = "REGISTRY=ghcr.io/acme\nAPP_VERSION=1.0.0\nDOMAIN=app.example.com\nADMIN_EMAIL=boss@example.com\nADMIN_FIRST_NAME=Boss\nADMIN_LAST_NAME=Man\n" +
	"LETSENCRYPT_EMAIL=ops@example.com\nGHCR_USER=bot\nGHCR_TOKEN=ghp_tok\nPOSTGRES_USER=acme\n"

func newInstall(t *testing.T, tls string, env string, pre func(*transport.Fake)) (*Install, *transport.Fake, *[]string) {
	t.Helper()
	s, f := newStack(t, tls)
	if pre != nil {
		pre(f)
	}
	f.Fail("test -f .installed", 1, "") // a fresh server, unless a test registered a rule before this one
	f.OnFunc(func(sp transport.Spec) bool { return strings.Join(sp.Argv, " ") == "cat /opt/app/.env" }, transport.Result{Stdout: env})
	f.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "docker logs --since") }, transport.Result{})
	f.OnFunc(func(sp transport.Spec) bool { return sp.Env["DK_SVC"] != "" }, transport.Result{Stdout: "true healthy\n"})
	f.OnFunc(func(sp transport.Spec) bool { return len(sp.Argv) > 0 && sp.Argv[0] == "curl" }, transport.Result{Stdout: `{"status":"ok"}`})
	m, err := manifest.Load("../../examples/compose-app/deploykit.yaml")
	if err != nil {
		t.Fatal(err)
	}
	envSpec, _ := m.EnvConfig()
	variant := "certbot"
	if tls == "cloudpanel" {
		variant = "cloudpanel"
	}
	ax, err := m.Ingress.Variant(variant)
	if err != nil {
		t.Fatal(err)
	}
	var standalone *manifest.Standalone
	if tls != "cloudpanel" {
		standalone = &manifest.Standalone{}
		if err := ax.Decode(standalone); err != nil {
			t.Fatal(err)
		}
	}
	crons, err := CronsFor(m, standalone, "/opt/app/.deploykit/deploykit")
	if err != nil {
		t.Fatal(err)
	}
	var notices []string
	_ = notices
	return &Install{Stack: s, Env: envSpec, Standalone: standalone, CloudPanelPort: "8081", Crons: crons, BaseURL: "https://app.example.com"}, f, &notices
}

func TestInstallStandalone(t *testing.T) {
	in, f, _ := newInstall(t, "certbot", installEnv, nil)
	var notices []string
	rep := reporterFunc(func(e runner.Event) {
		if e.Kind == runner.Notice {
			notices = append(notices, e.Line)
		}
	})
	if err := in.Run(t.Context(), &runner.Runner{Reporter: rep}); err != nil {
		t.Fatal(err)
	}
	trace := f.Trace()
	inOrderTrace(t, trace,
		"$ test -f .installed",
		"stdin <", // secrets saved on stdin
		"docker login ghcr.io -u bot --password-stdin",
		"docker-compose.prod.yml pull",
		"up -d db redis hub", // data services only
		"lexik:jwt:generate-keypair --skip-if-exists",
		"doctrine:migrations:migrate -n",
		"app:seed-roles",
		"app:create-user",
		"up -d",
		"certbot/certbot certonly --webroot", // standalone TLS
		"crontab -",
		"$ touch .installed",
	)
	if strings.Contains(trace, "ghp_tok") {
		t.Error("the registry token must travel on stdin only")
	}
	if len(notices) != 3 || !strings.Contains(notices[0], "BACKUP_PASSPHRASE") || !strings.Contains(notices[1], "APP_ENCRYPTION_KEY") {
		t.Errorf("the off-server reminders (backup passphrase, AI secret key) must be shown: %v", notices)
	}
	// The generic reminder names every secret the manifest generates and the command that reads one, never a value.
	if len(notices) == 3 {
		for _, want := range []string{"APP_SECRET", "POSTGRES_PASSWORD", "BACKUP_PASSPHRASE", "deploykit env get", "--reveal"} {
			if !strings.Contains(notices[2], want) {
				t.Errorf("the secrets reminder lacks %q: %s", want, notices[2])
			}
		}
	}
	// Every cron of the manifest is scheduled with the server binary.
	var cron string
	for _, c := range f.Calls() {
		if c.Spec != nil && strings.Join(c.Spec.Argv, " ") == "crontab -" {
			cron = c.Stdin
		}
	}
	for _, want := range []string{"deploykit server backup", "deploykit server monitor", "deploykit server restore-test", "deploykit server renew-certs", "deploykit server cron monthly-report"} {
		if !strings.Contains(cron, want) {
			t.Errorf("crontab lacks %q:\n%s", want, cron)
		}
	}
}

type reporterFunc func(runner.Event)

func (f reporterFunc) Handle(e runner.Event) { f(e) }

func TestInstallBehindCloudPanelHasNoCertbotAndNoRenewCron(t *testing.T) {
	in, f, _ := newInstall(t, "cloudpanel", installEnv, nil)
	if err := in.Run(t.Context(), &runner.Runner{}); err != nil {
		t.Fatal(err)
	}
	trace := f.Trace()
	if strings.Contains(trace, "certbot") {
		t.Error("CloudPanel terminates TLS: no certbot")
	}
	if !strings.Contains(trace, "docker-compose.cloudpanel.yaml") {
		t.Error("the loopback override must be in effect")
	}
	for _, c := range f.Calls() {
		if c.Spec != nil && strings.Join(c.Spec.Argv, " ") == "crontab -" && strings.Contains(c.Stdin, "renew-certs") {
			t.Error("no certificate renewal cron behind CloudPanel")
		}
	}
}

func TestInstallRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		env  string
		pre  func(*transport.Fake)
		want string
	}{
		"already installed":    {installEnv, func(f *transport.Fake) { f.On("test -f .installed", transport.Result{}) }, "already installed"},
		"placeholder left":     {strings.Replace(installEnv, "ADMIN_EMAIL=boss@example.com", "ADMIN_EMAIL=CHANGE_ME@example.com", 1), nil, "ADMIN_EMAIL"},
		"invalid domain":       {strings.Replace(installEnv, "app.example.com", "not a domain", 1), nil, "not a valid domain"},
		"no letsencrypt email": {strings.Replace(installEnv, "LETSENCRYPT_EMAIL=ops@example.com", "LETSENCRYPT_EMAIL=CHANGE_ME", 1), nil, "LETSENCRYPT_EMAIL"},
		"gpg missing":          {installEnv, func(f *transport.Fake) { f.Fail("command -v gpg", 1, "") }, "gpg not found"},
	} {
		t.Run(name, func(t *testing.T) {
			in, f, _ := newInstall(t, "certbot", tc.env, tc.pre)
			err := in.Run(t.Context(), &runner.Runner{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if strings.Contains(f.Trace(), "pull") || strings.Contains(f.Trace(), "touch") {
				t.Errorf("nothing may be installed after a refused check:\n%s", f.Trace())
			}
		})
	}
}

func TestInstallDoesNotMarkInstalledWhenAStepFails(t *testing.T) {
	in, f, _ := newInstall(t, "certbot", installEnv, func(f *transport.Fake) { f.Fail("doctrine:migrations:migrate", 1, "boom") })
	if err := in.Run(t.Context(), &runner.Runner{}); err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(f.Trace(), "touch .installed") {
		t.Error("a failed installation must stay re-runnable: no marker")
	}
}

func TestStandaloneTLSScriptIsValidShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	in, f, _ := newInstall(t, "certbot", installEnv, nil)
	if err := in.Run(t.Context(), &runner.Runner{}); err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, c := range f.Calls() {
		if c.Spec != nil && strings.Contains(c.Spec.Script, "certbot/certbot") {
			cmd := exec.CommandContext(context.Background(), "sh", "-n")
			cmd.Stdin = strings.NewReader(c.Spec.Script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("invalid shell: %v\n%s", err, out)
			}
			checked++
			if c.Spec.Env["DK_CONF"] != "docker/nginx/tls/app.example.com.conf" || c.Spec.Env["DK_LE"] != "acme_letsencrypt" {
				t.Errorf("env = %v", c.Spec.Env)
			}
		}
	}
	if checked != 1 {
		t.Errorf("checked %d TLS scripts", checked)
	}
}
