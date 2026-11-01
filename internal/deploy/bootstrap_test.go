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

type resolver map[string]bool

func (r resolver) LookupHost(_ context.Context, h string) ([]string, error) {
	if r[h] {
		return []string{"203.0.113.9"}, nil
	}
	return nil, errors.New("no such host")
}

type bootstrapFixture struct {
	b      *Bootstrap
	root   *transport.Fake
	site   *transport.Fake
	target string // path of the target file
	ev     *eventLog
}

type eventLog struct{ notices, failed []string }

func (e *eventLog) Handle(ev runner.Event) {
	switch ev.Kind {
	case runner.Notice:
		e.notices = append(e.notices, ev.Line)
	case runner.StepFailed:
		e.failed = append(e.failed, ev.Step+": "+ev.Err.Error())
	}
}

func newBootstrap(t *testing.T, dns resolver) *bootstrapFixture {
	t.Helper()
	m, err := manifest.Load(filepath.Join("..", "..", "examples", "nextjs-cloudpanel", "deploykit.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "demo.env")
	content := "HOST=srv\nROOT_USER=root\nSITE_USER=demosite\nDOMAIN=demo.example\nPATH_ON_SERVER=/home/demosite/htdocs/demo.example\nAPP_PORT=3001\nSITE_URL=https://demo.example\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	target, err := inventory.Open(dir, "demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	root, site := transport.NewFake("root@srv"), transport.NewFake("demosite@srv")
	root.OnFunc(func(s transport.Spec) bool { return strings.Join(s.Argv, " ") == "uname -sm" }, transport.Result{Stdout: "Linux x86_64\n"})
	return &bootstrapFixture{
		b: &Bootstrap{
			Manifest: m, Target: target, Root: root, Site: site, DNS: dns,
			ReadFile: func(p string) ([]byte, error) {
				if strings.HasSuffix(p, "id_ed25519.pub") {
					return []byte("ssh-ed25519 AAAAKEY me@laptop\n"), nil
				}
				return nil, os.ErrNotExist
			},
			HomeDir: "/home/me",
		},
		root: root, site: site, target: path, ev: &eventLog{},
	}
}

func (f *bootstrapFixture) absentSite() {
	f.root.OnFunc(func(s transport.Spec) bool { return strings.Contains(s.Script, "CP_PATH") }, transport.Result{ExitCode: 1})
}

func TestBootstrapFreshServer(t *testing.T) {
	f := newBootstrap(t, resolver{"demo.example": true, "www.demo.example": true})
	f.absentSite()
	f.root.On("ss -ltnH", transport.Result{Stdout: "22\n80\n3000\n3001\n3003\n"})
	f.site.On("pm2 startup", transport.Result{Stdout: "sudo env PATH=$PATH:/usr/local/bin /usr/local/lib/node_modules/pm2/bin/pm2 startup systemd -u demosite --hp /home/demosite\n"})

	if err := f.b.Run(t.Context(), &runner.Runner{Reporter: f.ev}); err != nil {
		t.Fatal(err)
	}
	trace := f.root.Trace()
	inOrder(t, trace,
		"command -v clpctl",
		"ss -ltnH",
		"clpctl site:add:nodejs",
		"clpctl lets-encrypt:install:certificate",
		"mkdir -p /home/demosite/htdocs/demo.example/releases /home/demosite/htdocs/demo.example/shared",
		"CP_KEY=ssh-ed25519 AAAAKEY me@laptop",
		"env PATH=$PATH:/usr/local/bin",
	)
	if !strings.Contains(trace, "env CP_PORT=3002") {
		t.Errorf("3001 is taken: the site must use 3002:\n%s", trace)
	}
	// The picked port is written back to the target, nothing else changes.
	data, _ := os.ReadFile(f.target)
	if !strings.Contains(string(data), "APP_PORT=3002\n") || !strings.Contains(string(data), "DOMAIN=demo.example\n") {
		t.Errorf("target file = %q", data)
	}
	// The generated password is shown once, as a notice, and is not in the trace.
	var shown string
	for _, n := range f.ev.notices {
		if strings.Contains(n, "password") {
			shown = n
		}
	}
	if shown == "" {
		t.Fatalf("the site password must be shown to the operator once: %v", f.ev.notices)
	}
	if strings.Contains(trace, strings.TrimSpace(shown[strings.LastIndex(shown, ": ")+2:])) {
		t.Error("the password leaked into the command trace")
	}
	if len(f.ev.failed) != 0 {
		t.Errorf("failed steps: %v", f.ev.failed)
	}
	if !strings.Contains(f.site.Trace(), "command -v pm2") {
		t.Errorf("pm2 must be checked as the site user:\n%s", f.site.Trace())
	}
}

func TestBootstrapIsIdempotentWhenTheSiteExists(t *testing.T) {
	f := newBootstrap(t, resolver{"demo.example": true})
	// SiteExists answers yes by default.
	before, _ := os.ReadFile(f.target)
	if err := f.b.Run(t.Context(), &runner.Runner{Reporter: f.ev}); err != nil {
		t.Fatal(err)
	}
	trace := f.root.Trace()
	if strings.Contains(trace, "site:add") || strings.Contains(trace, "ss -ltnH") {
		t.Errorf("an existing site must not be recreated or re-ported:\n%s", trace)
	}
	if after, _ := os.ReadFile(f.target); string(after) != string(before) {
		t.Error("the target file must not change")
	}
	for _, n := range f.ev.notices {
		if strings.Contains(n, "password") {
			t.Error("no password is generated for an existing site")
		}
	}
	if !strings.Contains(trace, "CP_KEY") {
		t.Error("the later steps still run (they are idempotent)")
	}
}

func TestBootstrapCertificateFailureIsOnlyAWarning(t *testing.T) {
	f := newBootstrap(t, resolver{}) // nothing resolves
	f.absentSite()
	if err := f.b.Run(t.Context(), &runner.Runner{Reporter: f.ev}); err != nil {
		t.Fatalf("a missing DNS must not stop the bootstrap: %v", err)
	}
	if len(f.ev.failed) != 1 || !strings.HasPrefix(f.ev.failed[0], "certificate:") || !strings.Contains(f.ev.failed[0], "run bootstrap again") {
		t.Errorf("failed = %v", f.ev.failed)
	}
	if strings.Contains(f.root.Trace(), "lets-encrypt") {
		t.Error("no certificate may be requested before the DNS resolves")
	}
	if !strings.Contains(f.root.Trace(), "releases") {
		t.Error("the remaining steps must still run")
	}
}

func TestBootstrapStopsWithoutClpctl(t *testing.T) {
	f := newBootstrap(t, resolver{})
	f.root.OnFunc(func(s transport.Spec) bool { return strings.Contains(s.Script, "command -v clpctl") }, transport.Result{ExitCode: 1})
	err := f.b.Run(t.Context(), &runner.Runner{})
	if err == nil || !strings.Contains(err.Error(), "clpctl not found") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(f.root.Trace(), "site:add") {
		t.Error("nothing may be provisioned")
	}
}

func TestBootstrapNeedsAPublicKeyBeforeTouchingTheServer(t *testing.T) {
	f := newBootstrap(t, resolver{})
	f.b.ReadFile = func(string) ([]byte, error) { return nil, os.ErrNotExist }
	if err := f.b.Run(t.Context(), &runner.Runner{}); err == nil || !strings.Contains(err.Error(), "--pubkey") {
		t.Errorf("err = %v", err)
	}
	if len(f.root.Calls()) != 0 {
		t.Error("the server must not be contacted without a key to install")
	}
}

func TestBootstrapDryRunChangesNothing(t *testing.T) {
	f := newBootstrap(t, resolver{"demo.example": true})
	f.absentSite()
	f.root.On("ss -ltnH", transport.Result{Stdout: "3001\n"})
	before, _ := os.ReadFile(f.target)
	if err := f.b.Run(t.Context(), &runner.Runner{DryRun: true, Reporter: f.ev}); err != nil {
		t.Fatal(err)
	}
	trace := f.root.Trace()
	for _, forbidden := range []string{"site:add", "lets-encrypt", "mkdir -p", "CP_KEY", "ss -ltnH"} {
		if strings.Contains(trace, forbidden) {
			t.Errorf("a dry run must not run %q:\n%s", forbidden, trace)
		}
	}
	if after, _ := os.ReadFile(f.target); string(after) != string(before) {
		t.Error("a dry run must not rewrite the target")
	}
}
