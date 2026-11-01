package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/masilia/deploykit/internal/inventory"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/runner"
	"github.com/masilia/deploykit/internal/transport"
)

func newComposeBootstrap(t *testing.T, targetBody string, dns resolver) (*ComposeBootstrap, *transport.Fake, string, *eventLog) {
	t.Helper()
	m, err := manifest.Load(filepath.Join("..", "..", "examples", "compose-app", "deploykit.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "cp.env")
	if err := os.WriteFile(path, []byte(targetBody), 0o600); err != nil {
		t.Fatal(err)
	}
	target, err := inventory.Open(dir, "cp", nil)
	if err != nil {
		t.Fatal(err)
	}
	root := transport.NewFake("root@srv")
	return &ComposeBootstrap{Manifest: m, Target: target, Root: root, DNS: dns}, root, path, &eventLog{}
}

const cpTarget = "DEPLOY_HOST=srv\nDEPLOY_USER=deploy\nDEPLOY_PATH=/opt/app-cp-test\nDOMAIN=app-cp-test.example.com\nTLS_MODE=cloudpanel\nPROXY_PORT=8081\nSITE_USER=appcptest\n"

func TestComposeBootstrapCreatesTheReverseProxySite(t *testing.T) {
	b, root, path, ev := newComposeBootstrap(t, cpTarget, resolver{})
	root.OnFunc(func(s transport.Spec) bool { return strings.Contains(s.Script, "CP_PATH") }, transport.Result{ExitCode: 1}) // no vhost yet
	root.OnFunc(func(s transport.Spec) bool { return strings.Join(s.Argv, " ") == "test -d /opt/app-cp-test" }, transport.Result{ExitCode: 1})
	root.On("ss -ltnH", transport.Result{Stdout: "8080\n8081\n"})
	if err := b.Run(t.Context(), &runner.Runner{Reporter: ev}); err != nil {
		t.Fatal(err)
	}
	trace := root.Trace()
	for _, want := range []string{
		"clpctl site:add:reverse-proxy",
		"env CP_URL=http://127.0.0.1:8082", // 8081 is the live stack's: the next free port
		"env CP_DOMAIN=app-cp-test.example.com",
		"env CP_USER=appcptest",
		"env DK_ROOT=/opt/app-cp-test",
		"env DK_USER=deploy",
	} {
		if !strings.Contains(trace, want) {
			t.Errorf("trace lacks %q:\n%s", want, trace)
		}
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "PROXY_PORT=8082\n") {
		t.Errorf("the chosen port must be written back to the target: %q", data)
	}
	var sawPassword, sawNginx bool
	for _, n := range ev.notices {
		sawPassword = sawPassword || strings.Contains(n, "password")
		sawNginx = sawNginx || strings.Contains(n, "X-Accel-Buffering")
	}
	if !sawPassword || !sawNginx {
		t.Errorf("notices = %v", ev.notices)
	}
	if len(ev.failed) != 1 || !strings.HasPrefix(ev.failed[0], "certificate:") {
		t.Errorf("only the certificate (no DNS) may fail: %v", ev.failed)
	}
}

func TestComposeBootstrapIsIdempotent(t *testing.T) {
	b, root, path, _ := newComposeBootstrap(t, cpTarget, resolver{})
	before, _ := os.ReadFile(path)
	// SiteExists and `test -d` answer yes by default.
	if err := b.Run(t.Context(), &runner.Runner{}); err != nil {
		t.Fatal(err)
	}
	trace := root.Trace()
	if strings.Contains(trace, "site:add") || strings.Contains(trace, "ss -ltnH") || strings.Contains(trace, "mkdir -p") {
		t.Errorf("an existing site and root must not be recreated:\n%s", trace)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("the target must not change")
	}
}

func TestComposeBootstrapRefusesAStandaloneTarget(t *testing.T) {
	b, root, _, _ := newComposeBootstrap(t, strings.Replace(cpTarget, "TLS_MODE=cloudpanel", "TLS_MODE=certbot", 1), resolver{})
	err := b.Run(t.Context(), &runner.Runner{})
	if err == nil || !strings.Contains(err.Error(), "nothing to provision") {
		t.Fatalf("err = %v", err)
	}
	if len(root.Calls()) != 0 {
		t.Error("nothing may reach the server")
	}
}

func TestComposeBootstrapNeedsItsKeys(t *testing.T) {
	for name, body := range map[string]string{
		"site user":   strings.Replace(cpTarget, "SITE_USER=appcptest\n", "", 1),
		"proxy port":  strings.Replace(cpTarget, "PROXY_PORT=8081\n", "", 1),
		"path":        strings.Replace(cpTarget, "DEPLOY_PATH=/opt/app-cp-test\n", "DEPLOY_PATH=relative\n", 1),
		"deploy user": strings.Replace(cpTarget, "DEPLOY_USER=deploy", "DEPLOY_USER=Bad User", 1),
	} {
		b, root, _, _ := newComposeBootstrap(t, body, resolver{})
		if err := b.Run(t.Context(), &runner.Runner{}); err == nil {
			t.Errorf("%s: want an error", name)
		}
		if len(root.Calls()) != 0 {
			t.Errorf("%s: the server must not be contacted with an incomplete target", name)
		}
	}
}

func TestComposeBootstrapDryRun(t *testing.T) {
	b, root, path, _ := newComposeBootstrap(t, cpTarget, resolver{})
	before, _ := os.ReadFile(path)
	if err := b.Run(t.Context(), &runner.Runner{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"site:add", "mkdir -p", "chown", "lets-encrypt"} {
		if strings.Contains(root.Trace(), forbidden) {
			t.Errorf("a dry run must not run %q", forbidden)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("a dry run must not rewrite the target")
	}
}
