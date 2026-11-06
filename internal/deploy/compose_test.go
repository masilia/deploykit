package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/masilia/deploykit/internal/envfile"
	"github.com/masilia/deploykit/internal/image"
	"github.com/masilia/deploykit/internal/inventory"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/runner"
	"github.com/masilia/deploykit/internal/transport"
)

const composeRoot = "/opt/app"

type composeFixture struct {
	c    *Compose
	site *transport.Fake
	repo string
	bin  string
}

func newComposeFixture(t *testing.T, pre func(*composeFixture)) *composeFixture {
	t.Helper()
	repo := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile("../../examples/compose-app/deploykit.yaml")
	if err != nil {
		t.Fatal(err)
	}
	write("deploykit.yaml", string(data))
	write("docker-compose.prod.yml", "services: {}\n")
	write("docker-compose.cloudpanel.yaml", "services: {}\n")
	write("deployment/.env.example", "DOMAIN=CHANGE_ME\n")
	write("docker/nginx/default.conf", "server {}\n")
	write("targets/acme.env", "DEPLOY_HOST=srv\nDEPLOY_USER=deploy\nDEPLOY_PATH="+composeRoot+"\nDOMAIN=app.acme.fr\nTLS_MODE=certbot\n")
	write("targets/other-client.env", "DEPLOY_HOST=other.secret.example\n")
	bin := filepath.Join(t.TempDir(), "deploykit")
	if err := os.WriteFile(bin, []byte("binary-v1"), 0o700); err != nil { //nolint:gosec // test double
		t.Fatal(err)
	}

	m, err := manifest.Load(filepath.Join(repo, "deploykit.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	target, err := inventory.Open(filepath.Join(repo, "targets"), "acme", nil)
	if err != nil {
		t.Fatal(err)
	}
	fx := &composeFixture{repo: repo, bin: bin, site: transport.NewFake("deploy@srv")}
	fx.c = &Compose{
		Manifest: m, Target: target, Site: fx.site, Repo: repo, Version: "1.2.0",
		Binary: bin, LocalOS: "linux", LocalArch: "amd64", TargetFile: filepath.Join(repo, "targets", "acme.env"),
	}
	if pre != nil {
		pre(fx)
	}
	// Defaults of a healthy server, after the test's own rules. The server first has no binary, then the one we upload.
	shaCalls := 0
	fx.site.OnFunc(func(sp transport.Spec) bool { // first look: the server has no binary yet
		if !strings.Contains(sp.Script, "sha256sum") {
			return false
		}
		shaCalls++
		return shaCalls == 1
	}, transport.Result{})
	fx.site.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "sha256sum") }, transport.Result{Stdout: mustSHA(t, bin) + "\n"})
	fx.site.OnFunc(func(s transport.Spec) bool { return strings.Join(s.Argv, " ") == "uname -sm" }, transport.Result{Stdout: "Linux x86_64\n"})
	fx.site.OnFunc(func(s transport.Spec) bool { return strings.Join(s.Argv, " ") == "cat "+composeRoot+"/.env" }, transport.Result{Stdout: "DOMAIN=app.acme.fr\nAPP_VERSION=1.1.0\n"})
	return fx
}

func mustSHA(t *testing.T, p string) string {
	t.Helper()
	s, err := fileSHA(p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (fx *composeFixture) run(t *testing.T) error {
	t.Helper()
	return fx.c.Run(t.Context(), &runner.Runner{})
}

func (fx *composeFixture) savedEnv() string {
	var out string
	for _, c := range fx.site.Calls() {
		if c.Spec != nil && strings.Contains(strings.Join(c.Spec.Argv, " "), `mv -f "$tmp" "$1"`) {
			out = c.Stdin
		}
	}
	return out
}

func TestComposeUpdate(t *testing.T) {
	fx := newComposeFixture(t, nil)
	fx.c.EnvFile = []envfile.Assignment{{Key: "GHCR_TOKEN", Value: "ghp_secret_value"}}
	fx.c.Registry = "ghcr.io/acme"
	if err := fx.run(t); err != nil {
		t.Fatal(err)
	}
	trace := fx.site.Trace()
	inOrder(t, trace,
		"$ true",
		"$ mkdir -p "+composeRoot+" "+composeRoot+"/.deploykit "+composeRoot+"/log",
		"sync "+fx.repo+"/docker-compose.prod.yml -> "+composeRoot+"/docker-compose.prod.yml",
		"sync "+fx.repo+"/docker/ -> "+composeRoot+"/docker/ --delete --exclude=deploy.env --exclude=secrets/ --exclude=targets/",
		"sync "+fx.repo+"/deploykit.yaml -> "+composeRoot+"/deploykit.yaml",
		"/.deploykit/target.env",
		"$ uname -sm",
		"sha256sum",
		"sync "+fx.bin+" -> "+composeRoot+"/.deploykit/deploykit",
		"$ chmod 755 "+composeRoot+"/.deploykit/deploykit",
		"$ test -f "+composeRoot+"/.env",
		"stdin <", // .env saved
		"$ "+composeRoot+"/.deploykit/deploykit server update --manifest "+composeRoot+"/deploykit.yaml",
	)
	if strings.Contains(trace, "ghp_secret_value") {
		t.Error("the injected secret must travel on stdin only")
	}
	env := fx.savedEnv()
	for _, want := range []string{"GHCR_TOKEN=ghp_secret_value\n", "APP_VERSION=1.2.0\n", "APP_VERSION_PREV=1.1.0\n", "REGISTRY=ghcr.io/acme\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("server .env lacks %q:\n%s", want, env)
		}
	}
	// The server receives its own target only.
	var uploaded string
	for _, c := range fx.site.Calls() {
		if c.Spec != nil && len(c.Spec.Argv) > 0 && strings.HasSuffix(c.Spec.Argv[len(c.Spec.Argv)-1], "/.deploykit/target.env") {
			uploaded = c.Stdin
		}
	}
	if !strings.Contains(uploaded, "app.acme.fr") || strings.Contains(uploaded, "other.secret.example") {
		t.Errorf("target.env = %q", uploaded)
	}
	for _, c := range fx.site.Calls() {
		if c.Sync != nil && strings.Contains(c.Sync.Src, "other-client") {
			t.Error("another client's inventory file was synced")
		}
	}
	// The remote command runs in the deployment root.
	last := fx.site.Calls()[len(fx.site.Calls())-1].Spec
	if last.Dir != composeRoot {
		t.Errorf("remote command dir = %q", last.Dir)
	}
}

func TestComposeReportsTheVersionOfTheBinaryItUploaded(t *testing.T) {
	fx := newComposeFixture(t, func(fx *composeFixture) {
		fx.site.OnFunc(func(s transport.Spec) bool { return len(s.Argv) == 2 && s.Argv[1] == "version" }, transport.Result{Stdout: "deploykit v0.1.0 (commit abc1234, built 2026-10-28)\n"})
	})
	var lines []string
	rep := noteReporter(func(e runner.Event) {
		if e.Kind == runner.Output {
			lines = append(lines, e.Line)
		}
	})
	if err := fx.c.Run(t.Context(), &runner.Runner{Reporter: rep}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "deploykit v0.1.0 (commit abc1234, built 2026-10-28)") {
		t.Errorf("the deploy log must say which build is on the server: %v", lines)
	}
}

func TestComposeRollbackKeepsTheReplacedVersionAsPrevious(t *testing.T) {
	fx := newComposeFixture(t, func(fx *composeFixture) {
		fx.site.OnFunc(func(s transport.Spec) bool { return strings.Join(s.Argv, " ") == "cat "+composeRoot+"/.env" }, transport.Result{Stdout: "APP_VERSION=1.2.0\nAPP_VERSION_PREV=1.1.0\n"})
	})
	fx.c.Version = "1.1.0" // deploying an older version is how a rollback is done
	if err := fx.run(t); err != nil {
		t.Fatal(err)
	}
	env := fx.savedEnv()
	if !strings.Contains(env, "APP_VERSION=1.1.0\n") || !strings.Contains(env, "APP_VERSION_PREV=1.2.0\n") {
		t.Errorf("env = %q", env)
	}
}

func TestComposeRedeployOfTheSameVersionLeavesPreviousAlone(t *testing.T) {
	fx := newComposeFixture(t, nil)
	fx.c.Version = "1.1.0"
	if err := fx.run(t); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fx.savedEnv(), "APP_VERSION_PREV") {
		t.Errorf("redeploying the same version must not overwrite the previous one: %q", fx.savedEnv())
	}
}

func TestComposeInstallCreatesEnvFromTheTemplate(t *testing.T) {
	fx := newComposeFixture(t, func(fx *composeFixture) {
		fx.site.Fail("test -f "+composeRoot+"/.installed", 1, "")
		fx.site.Fail("test -f "+composeRoot+"/.env", 1, "")
		fx.site.OnFunc(func(s transport.Spec) bool { return strings.Join(s.Argv, " ") == "cat "+composeRoot+"/.env" }, transport.Result{Stdout: "DOMAIN=CHANGE_ME\nAPP_VERSION=1.0.0\n"})
	})
	fx.c.Install = true
	if err := fx.run(t); err != nil {
		t.Fatal(err)
	}
	trace := fx.site.Trace()
	inOrder(t, trace, "env DK_ENV="+composeRoot+"/.env", "env DK_TEMPLATE="+composeRoot+"/deployment/.env.example", "umask 077", "server install")
	// The template says APP_VERSION=... as a placeholder; it must not become the "previous" version.
	if env := fx.savedEnv(); strings.Contains(env, "APP_VERSION_PREV") || !strings.Contains(env, "APP_VERSION=1.2.0\n") {
		t.Errorf("a first install has no previous version, saved .env: %q", env)
	}
}

func TestComposeInstallRefusesAnInstalledServer(t *testing.T) {
	fx := newComposeFixture(t, nil) // `test -f .installed` succeeds by default
	fx.c.Install = true
	err := fx.run(t)
	if err == nil || !strings.Contains(err.Error(), "already installed") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(fx.site.Trace(), "server install") {
		t.Error("nothing may run")
	}
}

func TestComposeUpdateNeedsAnEnv(t *testing.T) {
	fx := newComposeFixture(t, func(fx *composeFixture) { fx.site.Fail("test -f "+composeRoot+"/.env", 1, "") })
	err := fx.run(t)
	if err == nil || !strings.Contains(err.Error(), "--install") {
		t.Fatalf("err = %v", err)
	}
}

func TestComposeBinaryHandling(t *testing.T) {
	t.Run("identical binary is not uploaded again", func(t *testing.T) {
		sum, _ := fileSHA(newComposeFixture(t, nil).bin)
		fx := newComposeFixture(t, func(fx *composeFixture) {
			s, _ := fileSHA(fx.bin)
			_ = sum
			fx.site.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "sha256sum") }, transport.Result{Stdout: s + "\n"})
		})
		if err := fx.run(t); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(fx.site.Trace(), "sync "+fx.bin) {
			t.Error("the same binary must not be uploaded again")
		}
	})
	t.Run("platform mismatch is explained", func(t *testing.T) {
		fx := newComposeFixture(t, nil)
		fx.c.LocalOS, fx.c.LocalArch = "darwin", "arm64"
		err := fx.run(t)
		if err == nil || !strings.Contains(err.Error(), "darwin/arm64") || !strings.Contains(err.Error(), "linux/amd64") || !strings.Contains(err.Error(), "--server-binary") {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(fx.site.Trace(), "server update") {
			t.Error("nothing may run with a binary the server cannot execute")
		}
	})
	t.Run("an explicit binary skips the platform check", func(t *testing.T) {
		fx := newComposeFixture(t, nil)
		fx.c.LocalOS, fx.c.LocalArch, fx.c.BinaryExplicit = "darwin", "arm64", true
		if err := fx.run(t); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a corrupted upload is detected", func(t *testing.T) {
		fx := newComposeFixture(t, func(fx *composeFixture) {
			fx.site.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "sha256sum") }, transport.Result{Stdout: "deadbeef\n"})
		})
		err := fx.run(t)
		if err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestComposeSkipsMissingOptionalFiles(t *testing.T) {
	fx := newComposeFixture(t, nil)
	var notes []string
	rep := noteReporter(func(e runner.Event) {
		if e.Kind == runner.Output {
			notes = append(notes, e.Line)
		}
	})
	if err := fx.c.Run(t.Context(), &runner.Runner{Reporter: rep}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "skipped docs/DEPLOYMENT.md (not in the project)") {
		t.Errorf("output = %v", notes)
	}
}

type noteReporter func(runner.Event)

func (f noteReporter) Handle(e runner.Event) { f(e) }

func TestComposeUsesThePresentReleaseWithoutBuilding(t *testing.T) {
	docker := transport.NewFake("docker")
	m, _ := manifest.Load("../../examples/compose-app/deploykit.yaml")
	img, _ := m.Image()
	fx := newComposeFixture(t, nil)
	fx.c.Release = &image.Release{Cfg: img, Registry: "ghcr.io/acme", Version: "1.2.0", Docker: docker, Git: transport.NewFake("git")}
	var notices []string
	rep := noteReporter(func(e runner.Event) {
		if e.Kind == runner.Notice {
			notices = append(notices, e.Line)
		}
	})
	if err := fx.c.Run(t.Context(), &runner.Runner{Reporter: rep}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(docker.Trace(), "docker build") {
		t.Error("an existing release must not be rebuilt")
	}
	if len(notices) == 0 || !strings.Contains(notices[0], "already in the registry") {
		t.Errorf("notices = %v", notices)
	}
}

func TestServerPlatform(t *testing.T) {
	for in, want := range map[string]string{"Linux x86_64": "linux/amd64", "Linux aarch64": "linux/arm64", "Darwin arm64": "darwin/arm64"} {
		os, arch, err := ServerPlatform(in)
		if err != nil || os+"/"+arch != want {
			t.Errorf("ServerPlatform(%q) = %s/%s, %v", in, os, arch, err)
		}
	}
	for _, bad := range []string{"", "Linux", "FreeBSD amd64", "Linux riscv64"} {
		if _, _, err := ServerPlatform(bad); err == nil {
			t.Errorf("ServerPlatform(%q) must fail", bad)
		}
	}
}

func TestComposeDryRunTouchesNothing(t *testing.T) {
	fx := newComposeFixture(t, nil)
	if err := fx.c.Run(t.Context(), &runner.Runner{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if n := len(fx.site.Calls()); n != 0 {
		t.Errorf("a dry run contacted the server %d times:\n%s", n, fx.site.Trace())
	}
}

func TestComposeTakesTheKeysListedInFromTargetOnDeployFromTheTarget(t *testing.T) {
	fx := newComposeFixture(t, nil)
	// bootstrap picked a free loopback port and wrote it to the target; the server .env still holds the template's.
	if err := os.WriteFile(filepath.Join(fx.repo, "targets", "acme.env"), []byte("DEPLOY_HOST=srv\nDEPLOY_USER=deploy\nDEPLOY_PATH="+composeRoot+"\nDOMAIN=app.acme.fr\nTLS_MODE=certbot\nPROXY_PORT=8082\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target, err := inventory.Open(filepath.Join(fx.repo, "targets"), "acme", nil)
	if err != nil {
		t.Fatal(err)
	}
	fx.c.Target = target
	text, err := os.ReadFile(filepath.Join(fx.repo, "deploykit.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse([]byte(strings.Replace(string(text), "from_target_on_deploy: [REGISTRY]", "from_target_on_deploy: [REGISTRY, PROXY_PORT]", 1)))
	if err != nil {
		t.Fatal(err)
	}
	fx.c.Manifest = m
	if err := fx.run(t); err != nil {
		t.Fatal(err)
	}
	if env := fx.savedEnv(); !strings.Contains(env, "PROXY_PORT=8082") {
		t.Errorf("the target's proxy port must reach the server .env:\n%s", env)
	}
}

// A folder is mirrored with --delete. The standalone ingress makes the TLS configuration of the domain inside docker/ on the first
// install: the next deploy must not delete it, or the HTTPS goes away at the next reload of the proxy.
// Each extra domain has a server-generated file of its own, with a fixed name, and the mirror must keep it too.
func TestComposeKeepsTheExtraDomainFilesAcrossDeploys(t *testing.T) {
	fx := newComposeFixture(t, func(fx *composeFixture) {
		path := filepath.Join(fx.repo, "deploykit.yaml")
		data, err := os.ReadFile(path) //nolint:gosec // test file
		if err != nil {
			t.Fatal(err)
		}
		const anchor = "        renew: { schedule: \"15 3 * * *\" }"
		if !strings.Contains(string(data), anchor) {
			t.Fatal("the example manifest changed: update the anchor")
		}
		extra := "        extra_domains:\n          - { domain: \"{{ env.ADMIN_HOST | default: '' }}\", template: docker/nginx/tls/admin.conf.example, target: docker/nginx/tls/admin.conf }\n"
		patched := strings.Replace(string(data), anchor, extra+anchor, 1)
		if err := os.WriteFile(path, []byte(patched), 0o600); err != nil {
			t.Fatal(err)
		}
		if fx.c.Manifest, err = manifest.Load(path); err != nil {
			t.Fatal(err)
		}
	})
	if err := fx.run(t); err != nil {
		t.Fatal(err)
	}
	var line string
	for _, l := range strings.Split(fx.site.Trace(), "\n") {
		if strings.Contains(l, "-> "+composeRoot+"/docker/ ") {
			line = l
		}
	}
	for _, want := range []string{"--exclude=/nginx/tls/app.acme.fr.conf", "--exclude=/nginx/tls/admin.conf"} {
		if !strings.Contains(line, want) {
			t.Errorf("the mirror must keep %s:\n%s", want, line)
		}
	}
}

func TestComposeKeepsTheGeneratedTLSConfigurationAcrossDeploys(t *testing.T) {
	syncLine := func(trace string) string {
		for _, l := range strings.Split(trace, "\n") {
			if strings.Contains(l, "-> "+composeRoot+"/docker/ ") {
				return l
			}
		}
		return ""
	}

	fx := newComposeFixture(t, nil) // the acme target uses TLS_MODE=certbot: the standalone ingress
	if err := fx.run(t); err != nil {
		t.Fatal(err)
	}
	line := syncLine(fx.site.Trace())
	if !strings.Contains(line, "--delete") || !strings.Contains(line, "--exclude=/nginx/tls/app.acme.fr.conf") {
		t.Errorf("the folder is mirrored, minus the file the server generated:\n%s", line)
	}
	for _, l := range strings.Split(fx.site.Trace(), "\n") {
		if strings.Contains(l, "docker-compose.prod.yml ->") && strings.Contains(l, "nginx/tls") {
			t.Errorf("only a folder that holds the file needs the exclusion: %s", l)
		}
	}

	cp := newComposeFixture(t, nil)
	if err := os.WriteFile(filepath.Join(cp.repo, "targets", "acme.env"), []byte("DEPLOY_HOST=srv\nDEPLOY_USER=deploy\nDEPLOY_PATH="+composeRoot+"\nDOMAIN=app.acme.fr\nTLS_MODE=cloudpanel\nSITE_USER=acme\nPROXY_PORT=8081\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target, err := inventory.Open(filepath.Join(cp.repo, "targets"), "acme", nil)
	if err != nil {
		t.Fatal(err)
	}
	cp.c.Target = target
	if err := cp.run(t); err != nil {
		t.Fatal(err)
	}
	if line := syncLine(cp.site.Trace()); strings.Contains(line, "nginx/tls") {
		t.Errorf("behind CloudPanel nothing is generated: %s", line)
	}
}
