package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/masilia/deploykit/internal/inventory"
	"github.com/masilia/deploykit/internal/manifest"
)

// writeAll renders the answers into a fresh directory, as `init` would.
func writeAll(t *testing.T, a Answers) string {
	t.Helper()
	files, err := Files(a)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	dir := t.TempDir()
	for _, f := range files {
		p := filepath.Join(dir, f.Path)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, f.Content, f.Mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// check loads what was rendered with the real loader and decodes every typed section the answers should have produced.
func check(t *testing.T, a Answers) (*manifest.Manifest, string) {
	t.Helper()
	dir := writeAll(t, a)
	m, err := manifest.Load(filepath.Join(dir, "deploykit.yaml"))
	if err != nil {
		data, _ := os.ReadFile(filepath.Join(dir, "deploykit.yaml"))
		t.Fatalf("Load: %v\n%s", err, data)
	}
	if err := m.Validate(); err != nil {
		data, _ := os.ReadFile(filepath.Join(dir, "deploykit.yaml"))
		t.Fatalf("Validate: %v\n%s", err, data)
	}
	tg, err := inventory.Open(filepath.Join(dir, "targets"), a.Target.ID, nil)
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	if tg.Get("host") != a.Target.Host || tg.Get("domain") != a.Target.Domain || tg.Get("path") != a.Target.Path {
		t.Errorf("target fields: host %q domain %q path %q", tg.Get("host"), tg.Get("domain"), tg.Get("path"))
	}
	return m, dir
}

func nextAnswers(t *testing.T) Answers {
	t.Helper()
	dir := files(t, map[string]string{"package.json": nextPackage})
	a := DefaultAnswers(Detect(dir))
	a.Target.ID, a.Target.Host, a.Target.Domain = "prod", "srv.example.com", "www.example.com"
	a.Target.SiteUser, a.Target.Path = "acmesite", "/home/acmesite/htdocs/www.example.com"
	return a
}

func composeAnswers(t *testing.T) Answers {
	t.Helper()
	dir := files(t, map[string]string{
		"docker-compose.prod.yml":       composeProd,
		"docker-compose.cloudpanel.yml": "services: {}\n",
		"docker/Dockerfile.api":         "FROM php\n",
		"docker/Dockerfile.front":       "FROM node\n",
		"deployment/.env.example":       "APP_VERSION=0.0.0\n",
	})
	a := DefaultAnswers(Detect(dir))
	a.MigrateCommand = "php bin/console doctrine:migrations:migrate -n"
	a.Target.ID, a.Target.Host, a.Target.Domain = "client-a", "srv.example.com", "app.example.com"
	a.Target.SiteUser = "client-a"
	FillDefaults(&a)
	return a
}

func TestPM2ManifestIsValid(t *testing.T) {
	a := nextAnswers(t)
	m, _ := check(t, a)
	pm2, err := m.PM2()
	if err != nil || pm2.Health.Path != "/" {
		t.Fatalf("PM2: %+v, %v", pm2, err)
	}
	rd, err := m.ReleaseDir()
	if err != nil || len(rd.Build.Checks) != 3 || rd.Build.Node != "22" || rd.Keep != 3 {
		t.Fatalf("ReleaseDir: %+v, %v", rd, err)
	}
	if _, err := m.SmokeConfig(); err != nil {
		t.Fatalf("smoke: %v", err)
	}
}

func TestPM2WithoutOptionalSections(t *testing.T) {
	a := nextAnswers(t)
	a.Features = nil
	m, dir := check(t, a)
	data, _ := os.ReadFile(filepath.Join(dir, "deploykit.yaml"))
	if !m.Smoke.IsZero() || strings.Contains(string(data), "smoke:") || strings.Contains(string(data), "checks:") {
		t.Errorf("the unchosen sections must be absent:\n%s", data)
	}
}

func TestComposeEveryIngressAndFeatureSet(t *testing.T) {
	for name, mutate := range map[string]func(*Answers){
		"select, all features": func(a *Answers) {
			a.Ingress = IngressSelect
			a.Features = []string{FeatBackup, FeatRestoreTest, FeatMonitor, FeatCrons}
		},
		"cloudpanel only": func(a *Answers) { a.Ingress = IngressCloudPanel },
		"standalone only": func(a *Answers) {
			a.Ingress = IngressStandalone
			a.Target.SiteUser, a.Target.ProxyPort = "", 0
		},
		"no feature": func(a *Answers) { a.Features = nil },
		"no migration, no override": func(a *Answers) {
			a.MigrateCommand, a.Override = "", ""
		},
		"mysql without backups": func(a *Answers) {
			a.Database, a.Features = "mysql", []string{FeatMonitor}
		},
	} {
		t.Run(name, func(t *testing.T) {
			a := composeAnswers(t)
			mutate(&a)
			m, dir := check(t, a)
			c, err := m.Compose()
			if err != nil {
				t.Fatal(err)
			}
			if len(c.Services) != 8 || len(c.Health.Probes) != 1 {
				t.Errorf("services %d, probes %d", len(c.Services), len(c.Health.Probes))
			}
			if a.Has(FeatBackup) {
				if _, _, err := m.BackupConfig(); err != nil {
					t.Errorf("backup: %v", err)
				}
			}
			if a.Has(FeatMonitor) {
				if _, err := m.MonitorConfig(); err != nil {
					t.Errorf("monitor: %v", err)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "deploy.env.example")); err != nil {
				t.Error("a compose scaffold writes deploy.env.example")
			}
		})
	}
}

func TestComposeSelectKeepsTheOverrideConditional(t *testing.T) {
	a := composeAnswers(t)
	a.Ingress = IngressSelect
	m, _ := check(t, a)
	c, _ := m.Compose()
	if len(c.Files) != 1 || len(c.Overrides) != 1 || !strings.Contains(c.Overrides[0].When, "cloudpanel") {
		t.Errorf("files %v, overrides %+v", c.Files, c.Overrides)
	}
	sync, _ := m.SyncConfig()
	if !strings.Contains(strings.Join(sync.Files, " "), "docker-compose.cloudpanel.yml") || !strings.Contains(strings.Join(sync.Files, " "), "deployment/.env.example") || !strings.Contains(strings.Join(sync.Files, " "), "docker/") {
		t.Errorf("the server must receive the override, the env template and docker/: %v", sync.Files)
	}
}

func TestQuotingKeepsAHostileValueInsideItsScalar(t *testing.T) {
	a := composeAnswers(t)
	a.MigrateCommand = `sh -c "echo 'a: b' # not a comment"; echo {{ env.X }}`
	m, _ := check(t, a)
	c, _ := m.Compose()
	if got := c.App.Commands["migrate"].Run; got != a.MigrateCommand {
		t.Errorf("migrate = %q, want %q", got, a.MigrateCommand)
	}
}

func TestAnswersAreValidatedBeforeAnythingIsRendered(t *testing.T) {
	a := composeAnswers(t)
	a.Project, a.Target.Host, a.Target.Path, a.Target.SSHPort = "Bad Name", "bad host", "relative", 0
	a.Features = []string{FeatRestoreTest}
	_, err := Manifest(a)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"project", "host", "path", "ssh port", "restore test needs backups"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}

func TestTargetFiles(t *testing.T) {
	a := composeAnswers(t)
	a.Ingress, a.Target.TLSMode = IngressSelect, "certbot"
	body := a.Target.EnvFile(a.Runtime, a.Ingress)
	for _, want := range []string{"HOST=srv.example.com", "SSH_USER=deploy", "TLS_MODE=certbot", "CONTRACT=ondemand"} {
		if !strings.Contains(body, want) {
			t.Errorf("target file lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "PROXY_PORT") {
		t.Errorf("a certbot target has no proxy port:\n%s", body)
	}
	a.Target.TLSMode = "cloudpanel"
	if body := a.Target.EnvFile(a.Runtime, a.Ingress); !strings.Contains(body, "PROXY_PORT=8081") || !strings.Contains(body, "SITE_USER=client-a") {
		t.Errorf("a CloudPanel target needs its proxy port and site user:\n%s", body)
	}
}
