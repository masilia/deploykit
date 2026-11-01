package manifest

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExamplesLoad(t *testing.T) {
	for _, name := range []string{"compose-app", "nextjs-cloudpanel", "php-cloudpanel"} {
		t.Run(name, func(t *testing.T) {
			m, err := Load(filepath.Join("..", "..", "examples", name, "deploykit.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if m.Project == "" || m.Path() == "" {
				t.Errorf("project %q, path %q", m.Project, m.Path())
			}
		})
	}
}

func TestNextJSExampleTypedSections(t *testing.T) {
	m, err := Load(filepath.Join("..", "..", "examples", "nextjs-cloudpanel", "deploykit.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	rd, err := m.ReleaseDir()
	if err != nil {
		t.Fatal(err)
	}
	if rd.Keep != 3 || len(rd.Assemble) != 3 || !rd.Assemble[2].Replace || rd.Build.Env["NEXT_PUBLIC_SITE_URL"] == "" {
		t.Errorf("release-dir = %+v", rd)
	}
	pm2, err := m.PM2()
	if err != nil {
		t.Fatal(err)
	}
	if pm2.Entry != "server.js" || pm2.Health.Tries != 30 || pm2.Health.Interval != time.Second || pm2.Env["HOSTNAME"] != "0.0.0.0" {
		t.Errorf("pm2 = %+v", pm2)
	}
	sm, err := m.SmokeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(sm.Paths) != 13 || sm.Start.Port.Range[0] != 3300 || sm.FollowRedirects != 3 || len(sm.Ranges) != 1 {
		t.Errorf("smoke = %+v", sm)
	}
	if len(m.Hooks.BeforeUpload) != 1 {
		t.Errorf("hooks = %+v", m.Hooks)
	}
}

func TestIngressSelectsAVariant(t *testing.T) {
	m, err := Load(filepath.Join("..", "..", "examples", "compose-app", "deploykit.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Ingress.Select != "{{ target.tls_mode | default: certbot }}" {
		t.Fatalf("select = %q", m.Ingress.Select)
	}
	cp, err := m.Ingress.Variant("cloudpanel")
	if err != nil || cp.Type != IngressCloudPanel {
		t.Fatalf("cloudpanel variant: %+v, %v", cp, err)
	}
	var cfg CloudPanel
	if err := cp.Decode(&cfg); err != nil || cfg.Site.Type != "reverse-proxy" {
		t.Errorf("decode: %+v, %v", cfg, err)
	}
	cb, err := m.Ingress.Variant("certbot")
	if err != nil || cb.Type != IngressStandalone {
		t.Errorf("certbot (the TLS_MODE value of legacy clients) selects the standalone ingress: %+v, %v", cb, err)
	}
	if st, err := m.Ingress.Variant("standalone"); err == nil {
		t.Errorf("standalone is the type, not a variant key, got %+v", st)
	}
}

const minimal = `
version: 1
project: demo
artifact:
  type: release-dir
  assemble: [{ from: ".", to: "." }]
runtime: { type: pm2, entry: server.js }
ingress:
  type: cloudpanel
  site: { type: nodejs }
`

func TestMinimalManifestAppliesDefaults(t *testing.T) {
	m, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	rd, _ := m.ReleaseDir()
	pm2, _ := m.PM2()
	if pm2.Name != "{{ project }}" {
		t.Errorf("the pm2 name defaults to the project, got %q", pm2.Name)
	}
	if rd.Keep != 3 || rd.Source.Ref != "HEAD" || pm2.Cwd != "current" || pm2.Health.Expect != 200 || pm2.OnFailure.Rollback != "previous_release" {
		t.Errorf("defaults not applied: %+v %+v", rd, pm2)
	}
}

func TestInvalidManifests(t *testing.T) {
	cases := map[string]struct{ yaml, wantErr string }{
		"unknown top-level key": {minimal + "bogus: 1\n", "bogus"},
		"unknown key in a typed section": {
			strings.Replace(minimal, "runtime: { type: pm2, entry: server.js }", "runtime: { type: pm2, entry: server.js, typo: 1 }", 1), "typo"},
		"unknown key in the ingress": {strings.Replace(minimal, "site: { type: nodejs }", "site: { type: nodejs, tipe: x }", 1), "tipe"},
		"wrong version":              {strings.Replace(minimal, "version: 1", "version: 2", 1), "version"},
		"bad project name":           {strings.Replace(minimal, "project: demo", "project: Bad Name", 1), "project"},
		"missing runtime":            {"version: 1\nproject: demo\nartifact: {type: image}\ningress: {type: cloudpanel}\n", "runtime: required"},
		"unknown artifact type":      {strings.Replace(minimal, "release-dir", "tarball", 1), "artifact.type"},
		"image on pm2":               {strings.Replace(minimal, "release-dir", "image", 1), "requires runtime compose"},
		"release-dir on compose": {
			strings.Replace(minimal, "{ type: pm2, entry: server.js }", "{ type: compose }", 1), "cannot run on runtime compose"},
		"standalone ingress on pm2": {strings.Replace(minimal, "type: cloudpanel\n  site: { type: nodejs }", "type: standalone", 1), "only available for runtime compose"},
		"wrong site type for pm2":   {strings.Replace(minimal, "type: nodejs", "type: php", 1), "site.type must be nodejs"},
		"pm2 without entry":         {strings.Replace(minimal, "entry: server.js", "name: x", 1), "entry"},
		"release-dir without steps": {strings.Replace(minimal, "assemble: [{ from: \".\", to: \".\" }]", "keep: 2", 1), "assemble"},
		"select and a stray key": {`
version: 1
project: demo
artifact: {type: image}
runtime: {type: compose}
ingress: { select: "{{ target.tls_mode }}", type: cloudpanel, variants: { a: { type: standalone } } }
`, "cannot also set"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidationReportsEveryProblem(t *testing.T) {
	_, err := Parse([]byte("version: 3\nproject: Bad\nartifact: {type: image}\nruntime: {type: compose}\ningress: {type: cloudpanel, site: {type: reverse-proxy}}\n"))
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"version", "project"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestEmptyFile(t *testing.T) {
	if _, err := Parse(nil); err == nil {
		t.Error("an empty manifest must be rejected")
	}
}

func TestComposeExampleTypedSections(t *testing.T) {
	m, err := Load(filepath.Join("..", "..", "examples", "compose-app", "deploykit.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	img, err := m.Image()
	if err != nil || len(img.Images) != 2 || img.Images[0].Name != "frontend" || !img.Gates.OptIn || !img.EnsureRelease {
		t.Fatalf("image = %+v, %v", img, err)
	}
	c, err := m.Compose()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Services) != 8 || c.VersionState.PreviousKey != "APP_VERSION_PREV" || c.Lock != ".lock-ops" || c.Health.Wait.Timeout != 180*time.Second {
		t.Errorf("compose = %+v", c)
	}
	if got := c.App.Commands["create_admin"]; got.EnvFrom["APP_CREATE_USER_PASSWORD"] != "ADMIN_PASSWORD" || !strings.Contains(got.Run, "app:create-user") {
		t.Errorf("create_admin = %+v", got)
	}
	if got := c.App.Commands["migrate"]; got.Run == "" || got.EnvFrom != nil {
		t.Errorf("migrate (a plain string) = %+v", got)
	}
	if len(c.Health.Probes) != 4 || c.Health.Probes[3].LogContains != "scheduler tick" || !c.Health.Probes[3].Wait {
		t.Errorf("probes = %+v", c.Health.Probes)
	}

	env, _ := m.EnvConfig()
	if len(env.Secrets) != 8 || env.Secrets["APP_SECRET"].HexBytes != 32 || env.FromTargetOnDeploy[0] != "REGISTRY" {
		t.Errorf("env = %+v", env)
	}
	data, backup, err := m.BackupConfig()
	if err != nil || data.Database.Service != "db" || backup.NamePrefix != "acme-backup" || backup.Retention.DailyDays != 7 || backup.Retention.MonthlyDays != 30 || backup.Encryption.PassphraseKey != "BACKUP_PASSPHRASE" {
		t.Errorf("backup = %+v / %+v, %v", data, backup, err)
	}
	mon, err := m.MonitorConfig()
	if err != nil || len(mon.Checks) != 4 || mon.Checks[0].Kind != "services" || mon.Checks[2].Header["X-Health-Token"] == "" || mon.Checks[3].Kind != "disk" {
		t.Errorf("monitor = %+v, %v", mon, err)
	}
	fleet, _ := m.FleetConfig()
	if fleet.BackupStaleHours != 26 || fleet.ContractKey != "CONTRACT" {
		t.Errorf("fleet = %+v", fleet)
	}
}

func TestComposeValidationIsStrict(t *testing.T) {
	base := `
version: 1
project: demo
artifact: { type: image, registry: ghcr.io/org, images: [{ name: web, dockerfile: Dockerfile }] }
runtime: { type: compose, files: [docker-compose.yml], services: [{ name: web, role: app }] }
ingress: { type: standalone }
`
	if _, err := Parse([]byte(base)); err != nil {
		t.Fatalf("base manifest: %v", err)
	}
	for name, tc := range map[string]struct{ yaml, want string }{
		"typo in the runtime":    {strings.Replace(base, "files:", "fils: [x], files:", 1), "fils"},
		"no registry":            {strings.Replace(base, "registry: ghcr.io/org, ", "", 1), "registry"},
		"no services":            {strings.Replace(base, ", services: [{ name: web, role: app }]", "", 1), "services"},
		"app service not listed": {strings.Replace(base, "files: [docker-compose.yml]", "files: [docker-compose.yml], app: { service: api }", 1), "not a declared service"},
		"unknown monitor check":  {base + "monitor: { checks: [bogus] }\n", "unknown monitor check"},
		"backup without prefix":  {base + "data: { volumes: [a] }\nbackup: { dir: ./b }\n", "name_prefix"},
		"bad monthly day":        {base + "data: {}\nbackup: { name_prefix: p, retention: { monthly_on_day: 31 } }\n", "monthly_on_day"},
		"gpg without passphrase": {base + "data: {}\nbackup: { name_prefix: p, encryption: { type: gpg } }\n", "passphrase_key"},
		"secret of zero bytes":   {base + "env: { secrets: { A: { hex_bytes: 0 } } }\n", "hex_bytes"},
		"typo in standalone":     {strings.Replace(base, "ingress: { type: standalone }", "ingress: { type: standalone, tls: { typ: x } }", 1), "typ"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestYAMLSyntaxErrorsAreReportedAsSuch(t *testing.T) {
	_, err := Parse([]byte("version: 1\nproject: [unclosed\n"))
	if err == nil || strings.Contains(err.Error(), "must be 1") {
		t.Errorf("err = %v; a syntax error must not be reported as missing fields", err)
	}
}
