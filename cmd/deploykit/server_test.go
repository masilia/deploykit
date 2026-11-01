package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/masilia/deploykit/internal/transport"
)

type serverHarness struct {
	*harness
	root  string
	local *transport.Fake
}

func newServerHarness(t *testing.T, tls, envText string) *serverHarness {
	t.Helper()
	root := t.TempDir()
	manifestYAML, err := os.ReadFile(filepath.Join("..", "..", "examples", "compose-app", "deploykit.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for rel, content := range map[string]string{
		"deploykit.yaml":        string(manifestYAML),
		".deploykit/target.env": "HOST=srv\nDOMAIN=app.example.com\nTLS_MODE=" + tls + "\nPROXY_PORT=8081\nSITE_USER=appsite\n",
	} {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out := &bytes.Buffer{}
	a := newApp(out)
	a.manifestPath = filepath.Join(root, "deploykit.yaml")
	a.sleep = func(time.Duration) {}
	fake := transport.NewFake("local")
	fake.OnFunc(func(sp transport.Spec) bool { return strings.Join(sp.Argv, " ") == "cat "+root+"/.env" }, transport.Result{Stdout: envText})
	fake.OnFunc(func(sp transport.Spec) bool { return sp.Env["DK_SVC"] != "" }, transport.Result{Stdout: "true healthy\n"})
	fake.OnFunc(func(sp transport.Spec) bool { return len(sp.Argv) > 0 && sp.Argv[0] == "curl" }, transport.Result{Stdout: `{"status":"ok","data":{"status":"ok"}}`})
	fake.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "df -Pm") }, transport.Result{Stdout: "9000\n"})
	a.local = fake
	return &serverHarness{harness: &harness{app: a, out: out}, root: root, local: fake}
}

const envText = "DOMAIN=app.example.com\nAPP_VERSION=1.0.0\nBACKUP_PASSPHRASE=pw\nHEALTH_TOKEN=tok\n"

func TestServerHealthcheck(t *testing.T) {
	h := newServerHarness(t, "certbot", envText)
	h.local.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "docker logs --since") }, transport.Result{})
	// Probes: the first rule returning a body with "status" satisfies every HTTP probe.
	if err := h.run("server", "healthcheck"); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	out := h.out.String()
	if !strings.Contains(out, "OK health-api") || !strings.Contains(out, "healthy: https://app.example.com") {
		t.Errorf("output:\n%s", out)
	}
}

func TestServerHealthcheckFails(t *testing.T) {
	h := newServerHarness(t, "certbot", envText)
	h.local.OnFunc(func(sp transport.Spec) bool { return sp.Env["DK_SVC"] == "api" }, transport.Result{Stdout: "true unhealthy\n"})
	// Put the failing rule first by registering a new Fake with it in front.
	f := transport.NewFake("local")
	f.OnFunc(func(sp transport.Spec) bool { return sp.Env["DK_SVC"] == "api" }, transport.Result{Stdout: "true unhealthy\n"})
	f.OnFunc(func(sp transport.Spec) bool { return strings.Join(sp.Argv, " ") == "cat "+h.root+"/.env" }, transport.Result{Stdout: envText})
	f.OnFunc(func(sp transport.Spec) bool { return sp.Env["DK_SVC"] != "" }, transport.Result{Stdout: "true healthy\n"})
	f.OnFunc(func(sp transport.Spec) bool { return len(sp.Argv) > 0 && sp.Argv[0] == "curl" }, transport.Result{Stdout: `{"status":"ok","data":{"status":"ok"}}`})
	f.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "df -Pm") }, transport.Result{Stdout: "9000\n"})
	h.app.local = f
	err := h.run("server", "healthcheck")
	if err == nil || !strings.Contains(err.Error(), "health check failed") || !strings.Contains(h.out.String(), "KO api: not healthy (unhealthy)") {
		t.Errorf("err = %v\n%s", err, h.out)
	}
}

func TestServerMonitorTestSendsToTheWebhook(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}))
	defer srv.Close()
	h := newServerHarness(t, "certbot", envText+"SLACK_WEBHOOK_URL="+srv.URL+"\n")
	if err := h.run("server", "monitor", "--test"); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	if !strings.Contains(body, "Supervision test") || !strings.Contains(body, "v1.0.0") {
		t.Errorf("webhook received %q", body)
	}
	h2 := newServerHarness(t, "certbot", envText)
	if err := h2.run("server", "monitor", "--test"); err == nil || !strings.Contains(err.Error(), "SLACK_WEBHOOK_URL") {
		t.Errorf("without a webhook: %v", err)
	}
}

func TestServerMonitorDiagnosesBehindCloudPanelThroughTheLoopbackProxy(t *testing.T) {
	h := newServerHarness(t, "cloudpanel", envText)
	h.local.Fail("test -d .lock-ops", 1, "") // no operation running
	if err := h.run("server", "monitor", "--dry-run"); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	trace := h.local.Trace()
	if !strings.Contains(trace, "http://127.0.0.1:8081/api/v1/health") || !strings.Contains(trace, "Host: app.example.com") {
		t.Errorf("trace:\n%s", trace)
	}
	if !strings.Contains(h.out.String(), "diagnosis: ok") {
		t.Errorf("output:\n%s", h.out)
	}
}

func TestServerCronsDryRunPrintsTheCrontab(t *testing.T) {
	h := newServerHarness(t, "certbot", envText)
	if err := h.run("server", "crons", "--dry-run"); err != nil {
		t.Fatal(err)
	}
	out := h.out.String()
	for _, want := range []string{"# BEGIN deploykit " + h.root, "server backup >>", "server monitor >>", "server restore-test >>", "server renew-certs >>", "server cron monthly-report >>"} {
		if !strings.Contains(out, want) {
			t.Errorf("crontab lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(h.local.Trace(), "crontab -") {
		t.Error("--dry-run must not install anything")
	}
}

func TestServerCronRunsAManifestJob(t *testing.T) {
	h := newServerHarness(t, "certbot", envText)
	if err := h.run("server", "cron", "monthly-report"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.local.Trace(), "app:report:monthly") {
		t.Errorf("trace:\n%s", h.local.Trace())
	}
	if err := h.run("server", "cron", "nope"); err == nil || !strings.Contains(err.Error(), "no cron job named") {
		t.Errorf("err = %v", err)
	}
}

func TestServerRenewCerts(t *testing.T) {
	h := newServerHarness(t, "certbot", envText)
	if err := h.run("server", "renew-certs"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.local.Trace(), "certbot/certbot renew --non-interactive") || !strings.Contains(h.local.Trace(), "env DK_LE=acme_letsencrypt") {
		t.Errorf("trace:\n%s", h.local.Trace())
	}
	cp := newServerHarness(t, "cloudpanel", envText)
	if err := cp.run("server", "renew-certs"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cp.out.String(), "managed by CloudPanel") || strings.Contains(cp.local.Trace(), "certbot") {
		t.Errorf("behind CloudPanel nothing is renewed here:\n%s", cp.out)
	}
}

func TestServerNeedsItsTargetFile(t *testing.T) {
	h := newServerHarness(t, "certbot", envText)
	if err := os.Remove(filepath.Join(h.root, ".deploykit", "target.env")); err != nil {
		t.Fatal(err)
	}
	if err := h.run("server", "healthcheck"); err == nil || !strings.Contains(err.Error(), "target file") {
		t.Errorf("err = %v", err)
	}
}

func TestServerRefusesANodeManifest(t *testing.T) {
	h := newHarness(newProjectDir(t, cliManifest))
	if err := h.run("server", "healthcheck", "--target-file", "x.env"); err == nil || !strings.Contains(err.Error(), "compose") {
		t.Errorf("err = %v", err)
	}
}

func TestServerBackupTakesTheLockAndKeepsThePassphraseReminder(t *testing.T) {
	h := newServerHarness(t, "certbot", envText)
	h.local.OnFunc(func(sp transport.Spec) bool { return strings.Join(sp.Argv, " ") == "mktemp -d" }, transport.Result{Stdout: "/tmp/bk\n"})
	h.local.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "df -Pm") }, transport.Result{Stdout: "9000\n"})
	if err := h.run("server", "backup"); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	trace := h.local.Trace()
	if !strings.Contains(trace, "env DK_LOCK=.lock-ops") || !strings.Contains(trace, "$ rm -rf .lock-ops") {
		t.Errorf("the lock must be taken and released:\n%s", trace)
	}
	if !strings.Contains(h.out.String(), "BACKUP_PASSPHRASE OFF this server") {
		t.Errorf("output:\n%s", h.out)
	}
}
