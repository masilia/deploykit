package deploy

import (
	"context"
	"errors"
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

// addrResolver answers each name with the address it is given; a name it does not know does not resolve.
type addrResolver map[string]string

func (r addrResolver) LookupHost(_ context.Context, h string) ([]string, error) {
	if a, ok := r[h]; ok {
		return []string{a}, nil
	}
	return nil, errors.New("no such host")
}

const standaloneTarget = "DEPLOY_HOST=srv\nDEPLOY_USER=deploy\nDEPLOY_PATH=/opt/app-sa\nDOMAIN=app.example.com\nTLS_MODE=certbot\n"

// freshServer is a server nothing was installed on: no marker, no deployment root, Docker present, port 80 free.
func freshServer(t *testing.T, dns resolver) (*ComposeBootstrap, *transport.Fake, *eventLog) {
	t.Helper()
	b, root, _, ev := newComposeBootstrap(t, standaloneTarget, dns)
	root.Fail("test -f /opt/app-sa/.installed", 1, "")
	root.Fail("test -d /opt/app-sa", 1, "")
	root.On("docker compose version", transport.Result{Stdout: "5.7.0\n"})
	root.On("sport = :80", transport.Result{})
	return b, root, ev
}

func TestComposeBootstrapStandaloneChecksTheServerThenCreatesTheRoot(t *testing.T) {
	b, root, ev := freshServer(t, nil)
	b.DNS = addrResolver{"app.example.com": "203.0.113.7", "srv": "203.0.113.7"}
	if err := b.Run(t.Context(), &runner.Runner{Reporter: ev}); err != nil {
		t.Fatalf("%v (failed steps: %v)", err, ev.failed)
	}
	trace := root.Trace()
	for _, want := range []string{"docker compose version --short", "ss -ltnpH 'sport = :80'", "env DK_ROOT=/opt/app-sa", "env DK_USER=deploy"} {
		if !strings.Contains(trace, want) {
			t.Errorf("trace lacks %q:\n%s", want, trace)
		}
	}
	if strings.Contains(trace, "clpctl") {
		t.Errorf("there is no CloudPanel to provision:\n%s", trace)
	}
	joined := strings.Join(ev.notices, "\n")
	if !strings.Contains(joined, "80 and 443") || !strings.Contains(joined, "LETSENCRYPT_EMAIL") {
		t.Errorf("the notices say what the provider's side must allow and what is left to set: %v", ev.notices)
	}
}

func TestComposeBootstrapStandaloneRefusesWhatWouldMakeTheCertificateFail(t *testing.T) {
	cases := map[string]struct {
		prepare func(root *transport.Fake) addrResolver
		want    string
	}{
		"port 80 is taken": {func(root *transport.Fake) addrResolver {
			return addrResolver{"app.example.com": "203.0.113.7", "srv": "203.0.113.7"}
		}, "port 80 is already in use"},
		"the domain points elsewhere": {func(*transport.Fake) addrResolver {
			return addrResolver{"app.example.com": "198.51.100.1", "srv": "203.0.113.7"}
		}, "points at 198.51.100.1 but the server is 203.0.113.7"},
		"the domain does not resolve": {func(*transport.Fake) addrResolver {
			return addrResolver{"srv": "203.0.113.7"}
		}, "does not resolve yet"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b, root, _, _ := newComposeBootstrap(t, standaloneTarget, nil)
			root.Fail("test -f /opt/app-sa/.installed", 1, "")
			root.Fail("test -d /opt/app-sa", 1, "")
			root.On("docker compose version", transport.Result{Stdout: "5.7.0\n"})
			if name == "port 80 is taken" {
				root.On("sport = :80", transport.Result{Stdout: "LISTEN 0 511 0.0.0.0:80 0.0.0.0:* users:((\"nginx\",pid=812,fd=6))\n"})
			} else {
				root.On("sport = :80", transport.Result{})
			}
			b.DNS = tc.prepare(root)
			err := b.Run(t.Context(), &runner.Runner{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			if strings.Contains(root.Trace(), "DK_ROOT") {
				t.Errorf("nothing is created when a check fails:\n%s", root.Trace())
			}
		})
	}
}

func TestComposeBootstrapStandaloneNeedsDockerAndSkipsTheChecksOnceInstalled(t *testing.T) {
	b, root, _, _ := newComposeBootstrap(t, standaloneTarget, nil)
	b.DNS = addrResolver{}
	root.Fail("docker compose version", 127, "docker: command not found")
	if err := b.Run(t.Context(), &runner.Runner{}); err == nil || !strings.Contains(err.Error(), "install Docker Engine") {
		t.Errorf("a server without Docker is refused with what to do: %v", err)
	}

	// Installed: the stack's own proxy holds port 80 and the domain may resolve through a cache: the first-install checks do not apply.
	b, root, _, _ = newComposeBootstrap(t, standaloneTarget, nil)
	b.DNS = addrResolver{}
	root.On("docker compose version", transport.Result{Stdout: "5.7.0\n"})
	if err := b.Run(t.Context(), &runner.Runner{}); err != nil { // the Fake answers yes to `test -f .installed` and `test -d root`
		t.Fatal(err)
	}
	if trace := root.Trace(); strings.Contains(trace, "sport = :80") || strings.Contains(trace, "DK_ROOT") {
		t.Errorf("an installed stack is neither checked for port 80 nor recreated:\n%s", trace)
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
