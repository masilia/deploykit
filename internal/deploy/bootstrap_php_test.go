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

const systemTemplates = "| Generic | | System |\n| Symfony 7 | public | System |\n"

// newPHPBootstrap builds the fixture; templates is what `clpctl vhost-templates:list` prints (fake rules are first-match).
func newPHPBootstrap(t *testing.T, templates string) *bootstrapFixture {
	t.Helper()
	m, err := manifest.Load(filepath.Join("..", "..", "examples", "php-ibexa", "deploykit.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "demo.env")
	content := "HOST=srv\nROOT_USER=root\nSITE_USER=demosite\nDOMAIN=demo.example\nPATH_ON_SERVER=/home/demosite/htdocs/demo.example\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	target, err := inventory.Open(dir, "demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	root, site := transport.NewFake("root@srv"), transport.NewFake("demosite@srv")
	base, err := os.ReadFile(filepath.Join("..", "cloudpanel", "testdata", "symfony7.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	root.On("clpctl vhost-templates:list", transport.Result{Stdout: templates})
	root.On("vhost-template:view", transport.Result{Stdout: string(base)})
	return &bootstrapFixture{
		b: &Bootstrap{
			Manifest: m, Target: target, Root: root, Site: site, DNS: resolver{"demo.example": true},
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

func TestBootstrapPHPCreatesTheTemplateThenTheSite(t *testing.T) {
	f := newPHPBootstrap(t, systemTemplates)
	f.absentSite()
	if err := f.b.Run(t.Context(), &runner.Runner{Reporter: f.ev}); err != nil {
		t.Fatalf("%v (failed: %v)", err, f.ev.failed)
	}
	trace := f.root.Trace()
	inOrder(t, trace,
		"command -v clpctl",
		"clpctl vhost-templates:list",
		"vhost-template:view",
		`#{"phpVersion":"8.2","rootDirectory":"current/public"}`,
		"$realpath_root$fastcgi_script_name",
		"clpctl vhost-template:add",
		"clpctl site:add:php",
		"clpctl lets-encrypt:install:certificate",
		"mkdir -p /home/demosite/htdocs/demo.example/releases /home/demosite/htdocs/demo.example/shared",
		"CP_KEY=ssh-ed25519 AAAAKEY me@laptop",
	)
	for _, want := range []string{"CP_NAME=deploykit-ibexa-site", "CP_VHOST=deploykit-ibexa-site", "CP_PHP=8.2", "CP_BASE=Symfony 7"} {
		if !strings.Contains(trace, want) {
			t.Errorf("trace lacks %q:\n%s", want, trace)
		}
	}
	for _, unwanted := range []string{"ss -ltnH", "nodejs", "pm2"} {
		if strings.Contains(trace, unwanted) || strings.Contains(f.site.Trace(), unwanted) {
			t.Errorf("a php site has no %q step", unwanted)
		}
	}
	var shown bool
	for _, n := range f.ev.notices {
		shown = shown || strings.Contains(n, "password")
	}
	if !shown {
		t.Errorf("the site password is shown once: %v", f.ev.notices)
	}
	if len(f.ev.failed) != 0 {
		t.Errorf("failed steps: %v", f.ev.failed)
	}
}

func TestBootstrapPHPIsIdempotent(t *testing.T) {
	// the template already exists and so does the site (SiteExists answers yes by default)
	f := newPHPBootstrap(t, systemTemplates+"| deploykit-ibexa-site | current/public | Custom |\n")
	if err := f.b.Run(t.Context(), &runner.Runner{Reporter: f.ev}); err != nil {
		t.Fatal(err)
	}
	trace := f.root.Trace()
	if strings.Contains(trace, "site:add") || strings.Contains(trace, "vhost-template:add") {
		t.Errorf("nothing may be created twice:\n%s", trace)
	}
}

func TestBootstrapPHPRefusesDifferentVersions(t *testing.T) {
	f := newPHPBootstrap(t, systemTemplates)
	m, err := manifest.Parse([]byte(strings.Replace(mustRead(t, filepath.Join("..", "..", "examples", "php-ibexa", "deploykit.yaml")), `php_version: "8.2"
    # deploykit`, `php_version: "8.3"
    # deploykit`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	f.b.Manifest = m
	err = f.b.Run(t.Context(), &runner.Runner{Reporter: f.ev})
	if err == nil || !strings.Contains(err.Error(), "differ") {
		t.Errorf("err = %v", err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
