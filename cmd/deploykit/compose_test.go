package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/masilia/deploykit/internal/inventory"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/transport"
)

type composeHarness struct {
	*harness
	repo  string
	sites map[string]*transport.Fake
	local *transport.Fake
}

func newComposeHarness(t *testing.T, targets map[string]string) *composeHarness {
	t.Helper()
	repo := t.TempDir()
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "compose-app", "deploykit.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"deploykit.yaml": string(data), "docker-compose.prod.yml": "services: {}\n", "docker-compose.cloudpanel.yaml": "services: {}\n", "deployment/.env.example": "DOMAIN=CHANGE_ME\n",
		"server-binary": "binary",
	}
	for id, content := range targets {
		files["targets/"+id+".env"] = content
	}
	for rel, content := range files {
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out := &bytes.Buffer{}
	a := newApp(out)
	a.manifestPath = filepath.Join(repo, "deploykit.yaml")
	a.sleep = func(time.Duration) {}
	a.now = func() time.Time { return time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC) }
	ch := &composeHarness{harness: &harness{app: a, out: out}, repo: repo, sites: map[string]*transport.Fake{}, local: transport.NewFake("local")}
	a.local = ch.local
	a.ssh = func(tg *inventory.Target, _ *manifest.Manifest, _ role) (transport.Transport, error) {
		f, ok := ch.sites[tg.ID]
		if !ok {
			f = transport.NewFake(tg.ID + "@srv")
			ch.sites[tg.ID] = f
		}
		return f, nil
	}
	return ch
}

func (ch *composeHarness) site(id string) *transport.Fake {
	f := transport.NewFake(id + "@srv")
	ch.sites[id] = f
	return f
}

const acmeTarget = "REGISTRY=ghcr.io/acme\nDEPLOY_HOST=srv1\nDEPLOY_USER=deploy\nDEPLOY_PATH=/opt/app\nDOMAIN=app.acme.fr\nCONTRAT=supervised\nROLLOUT_ORDER=20\n"
const betaTarget = "REGISTRY=ghcr.io/acme\nDEPLOY_HOST=srv2\nDEPLOY_USER=deploy\nDEPLOY_PATH=/opt/app\nDOMAIN=app.beta.fr\nROLLOUT_ORDER=10\n"

func TestFleetStatus(t *testing.T) {
	ch := newComposeHarness(t, map[string]string{"acme": acmeTarget, "beta": betaTarget})
	ch.site("acme").On("DK_ROOT", transport.Result{Stdout: "2.0.0|1.9.0|yes|3"})
	ch.site("beta").On("DK_ROOT", transport.Result{ExitCode: 255, Stderr: "ssh: connect to host srv2: timed out"})
	err := ch.run("fleet", "status", "--all")
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("an unreachable target must fail the command: %v", err)
	}
	out := ch.out.String()
	for _, want := range []string{"acme", "app.acme.fr", "2.0.0", "1.9.0", "3h", "supervised", "beta           — (unreachable: deploy@srv2)"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
}

func TestFleetSupervisedFilter(t *testing.T) {
	ch := newComposeHarness(t, map[string]string{"acme": acmeTarget, "beta": betaTarget})
	ch.site("acme").On("DK_ROOT", transport.Result{Stdout: "2.0.0|-|yes|1"})
	if err := ch.run("fleet", "status", "--all", "--supervised"); err != nil {
		t.Fatalf("%v\n%s", err, ch.out)
	}
	if strings.Contains(ch.out.String(), "beta") {
		t.Errorf("only supervised targets:\n%s", ch.out)
	}
}

func TestFleetBackups(t *testing.T) {
	ch := newComposeHarness(t, map[string]string{"acme": acmeTarget, "beta": betaTarget})
	ch.site("acme").On("DK_ROOT", transport.Result{Stdout: "2.0.0|-|yes|3"})
	ch.site("beta").On("DK_ROOT", transport.Result{Stdout: "2.0.0|-|yes|30"})
	err := ch.run("fleet", "backups", "--all")
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Errorf("err = %v", err)
	}
	out := ch.out.String()
	if !strings.Contains(out, "acme           3h         OK") || !strings.Contains(out, "STALE (>26h)") {
		t.Errorf("output:\n%s", out)
	}
}

func TestFleetDeployIsNeverImplicit(t *testing.T) {
	ch := newComposeHarness(t, map[string]string{"acme": acmeTarget})
	if err := ch.run("fleet", "deploy", "--version", "1.0.0"); err == nil || !strings.Contains(err.Error(), "explicit target") {
		t.Errorf("err = %v", err)
	}
	if err := ch.run("fleet", "deploy", "--all"); err == nil || !strings.Contains(err.Error(), "--version") {
		t.Errorf("err = %v", err)
	}
	if len(ch.sites) != 0 {
		t.Error("nothing may be contacted")
	}
}

func TestComposeDeployThroughTheCLI(t *testing.T) {
	ch := newComposeHarness(t, map[string]string{"acme": acmeTarget})
	site := ch.site("acme")
	site.OnFunc(func(sp transport.Spec) bool { return strings.Join(sp.Argv, " ") == "cat /opt/app/.env" }, transport.Result{Stdout: "APP_VERSION=1.0.0\nDOMAIN=app.acme.fr\n"})
	shas := 0
	site.OnFunc(func(sp transport.Spec) bool {
		if !strings.Contains(sp.Script, "sha256sum") {
			return false
		}
		shas++
		return shas == 1
	}, transport.Result{})
	bin := filepath.Join(ch.repo, "server-binary")
	site.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "sha256sum") }, transport.Result{Stdout: sha(t, bin) + "\n"})

	envFile := filepath.Join(t.TempDir(), "inject.env")
	if err := os.WriteFile(envFile, []byte("SLACK_WEBHOOK_URL=https://hooks.example/secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ch.run("deploy", "--version", "1.1.0", "--server-binary", bin, "--env", envFile); err != nil {
		t.Fatalf("%v\n%s", err, ch.out)
	}
	trace := site.Trace()
	for _, want := range []string{"server update --manifest /opt/app/deploykit.yaml", "sync " + ch.repo + "/docker-compose.prod.yml -> /opt/app/docker-compose.prod.yml"} {
		if !strings.Contains(trace, want) {
			t.Errorf("trace lacks %q:\n%s", want, trace)
		}
	}
	if strings.Contains(trace, "hooks.example/secret") {
		t.Error("the injected value must travel on stdin")
	}
	// The release was found in the registry (the local Fake answers `docker manifest inspect`), so nothing was built.
	if strings.Contains(ch.local.Trace(), "docker build") {
		t.Errorf("no build expected:\n%s", ch.local.Trace())
	}
	if !strings.Contains(ch.out.String(), "✔ run on the server") {
		t.Errorf("output:\n%s", ch.out)
	}
}

func sha(t *testing.T, path string) string {
	t.Helper()
	out, err := (&transport.Local{}).Run(t.Context(), transport.Spec{Argv: []string{"sha256sum", path}})
	if err != nil {
		t.Skip("sha256sum not available")
	}
	return strings.Fields(out.Stdout)[0]
}

func TestComposeDeployDryRun(t *testing.T) {
	ch := newComposeHarness(t, map[string]string{"acme": acmeTarget})
	if err := ch.run("deploy", "--version", "1.1.0", "--server-binary", filepath.Join(ch.repo, "server-binary"), "--dry-run"); err != nil {
		t.Fatalf("%v\n%s", err, ch.out)
	}
	for id, f := range ch.sites {
		if len(f.Calls()) != 0 {
			t.Errorf("dry run contacted %s:\n%s", id, f.Trace())
		}
	}
}

func TestComposeDeployRejectsConflictingVersionFlags(t *testing.T) {
	ch := newComposeHarness(t, map[string]string{"acme": acmeTarget})
	if err := ch.run("deploy", "--version", "1.1.0", "--rollback", "1.0.0"); err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Errorf("err = %v", err)
	}
}

func TestComposeDeployRejectsABadRegistryAndVersion(t *testing.T) {
	ch := newComposeHarness(t, map[string]string{"acme": strings.Replace(acmeTarget, "REGISTRY=ghcr.io/acme", "REGISTRY=my-org", 1)})
	err := ch.run("deploy", "--version", "1.1.0", "--server-binary", filepath.Join(ch.repo, "server-binary"))
	if err == nil || !strings.Contains(err.Error(), "docker.io") {
		t.Errorf("err = %v", err)
	}
	if len(ch.sites) != 0 {
		t.Error("an invalid registry must be caught before any server is contacted")
	}
	ch2 := newComposeHarness(t, map[string]string{"acme": acmeTarget})
	if err := ch2.run("deploy", "--version", "latest", "--server-binary", filepath.Join(ch2.repo, "server-binary")); err == nil || !strings.Contains(err.Error(), "invalid version") {
		t.Errorf("err = %v", err)
	}
}

func TestReleaseCommandGuards(t *testing.T) {
	ch := newComposeHarness(t, map[string]string{"acme": acmeTarget})
	if err := ch.run("release"); err == nil || !strings.Contains(err.Error(), "--version") {
		t.Errorf("err = %v", err)
	}
	node := newHarness(newProjectDir(t, cliManifest))
	if err := node.run("release", "--version", "1.0.0"); err == nil || !strings.Contains(err.Error(), "builds images") {
		t.Errorf("err = %v", err)
	}
}

func TestStatusAndHealthcheckForCompose(t *testing.T) {
	ch := newComposeHarness(t, map[string]string{"acme": acmeTarget})
	site := ch.site("acme")
	site.On("DK_ROOT", transport.Result{Stdout: "2.0.0|1.9.0|yes|5"})
	if err := ch.run("status"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ch.out.String(), "2.0.0") {
		t.Errorf("status:\n%s", ch.out)
	}

	ch2 := newComposeHarness(t, map[string]string{"acme": acmeTarget})
	s2 := ch2.site("acme")
	s2.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "docker logs --since") }, transport.Result{})
	s2.OnFunc(func(sp transport.Spec) bool { return sp.Env["DK_SVC"] != "" }, transport.Result{Stdout: "true healthy\n"})
	s2.OnFunc(func(sp transport.Spec) bool { return len(sp.Argv) > 0 && sp.Argv[0] == "curl" }, transport.Result{Stdout: `{"status":"ok"}`})
	if err := ch2.run("healthcheck"); err != nil {
		t.Fatalf("%v\n%s", err, ch2.out)
	}
	if !strings.Contains(ch2.out.String(), "healthy: https://app.acme.fr") {
		t.Errorf("healthcheck:\n%s", ch2.out)
	}
}

// Behind CloudPanel the probes go to the loopback proxy with the public domain as Host, so a new instance without DNS
// or certificate can be checked.
func TestHealthcheckBehindCloudPanelUsesTheLoopback(t *testing.T) {
	ch := newComposeHarness(t, map[string]string{"cp": acmeTarget + "TLS_MODE=cloudpanel\nPROXY_PORT=8082\n"})
	s := ch.site("cp")
	s.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "docker logs --since") }, transport.Result{})
	s.OnFunc(func(sp transport.Spec) bool { return sp.Env["DK_SVC"] != "" }, transport.Result{Stdout: "true healthy\n"})
	s.OnFunc(func(sp transport.Spec) bool { return len(sp.Argv) > 0 && sp.Argv[0] == "curl" }, transport.Result{Stdout: `{"status":"ok"}`})
	if err := ch.run("healthcheck", "-t", "cp"); err != nil {
		t.Fatalf("%v\n%s", err, ch.out)
	}
	trace := s.Trace()
	if !strings.Contains(trace, "curl -fsS -m 10 -H 'Host: app.acme.fr' http://127.0.0.1:8082/api/v1/health") {
		t.Errorf("trace:\n%s", trace)
	}
	if strings.Contains(trace, "https://app.acme.fr") {
		t.Error("no probe may depend on DNS or the certificate")
	}
}
