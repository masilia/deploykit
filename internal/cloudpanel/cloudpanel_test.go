package cloudpanel

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/masilia/deploykit/internal/transport"
)

type dns map[string]bool

func (d dns) LookupHost(_ context.Context, host string) ([]string, error) {
	if d[host] {
		return []string{"203.0.113.10"}, nil
	}
	return nil, errors.New("no such host")
}

func newClient() (*Client, *transport.Fake) {
	f := transport.NewFake("root@srv")
	return &Client{Root: f, DNS: dns{"example.com": true}}, f
}

// assertValidShell runs `sh -n` on every script the client generated.
func assertValidShell(t *testing.T, f *transport.Fake) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	for i, c := range f.Calls() {
		if c.Spec == nil || c.Spec.Script == "" {
			continue
		}
		cmd := exec.CommandContext(t.Context(), "sh", "-n")
		cmd.Stdin = strings.NewReader(c.Spec.Script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("call %d is not valid shell: %v\n%s\n%s", i, err, out, c.Spec.Script)
		}
	}
}

func TestSiteExists(t *testing.T) {
	c, f := newClient()
	if ok, err := c.SiteExists(t.Context(), "/home/u/htdocs/example.com", "example.com"); err != nil || !ok {
		t.Errorf("exists: %v, %v", ok, err)
	}
	f.Fail("CP_PATH", 1, "")
	if ok, err := c.SiteExists(t.Context(), "/p", "example.com"); err != nil || ok {
		t.Errorf("absent: %v, %v", ok, err)
	}
	c2, f2 := newClient()
	f2.Fail("CP_PATH", 255, "ssh: connection refused")
	if _, err := c2.SiteExists(t.Context(), "/p", "example.com"); err == nil {
		t.Error("a transport failure must not read as 'site absent'")
	}
	assertValidShell(t, f)
}

func TestUsedPortsAndPickPort(t *testing.T) {
	c, f := newClient()
	f.On("ss -ltnH", transport.Result{Stdout: "22\n80\n3000\n3001\n3003\n8080\nnoise\n"})
	ports, err := c.UsedPorts(t.Context())
	if err != nil || len(ports) != 6 || ports[2] != 3000 {
		t.Fatalf("ports = %v, %v", ports, err)
	}
	for _, tc := range []struct {
		requested int
		want      int
	}{{3000, 3002}, {3001, 3002}, {3002, 3002}, {3003, 3004}, {9000, 9000}} {
		if got := PickPort(tc.requested, ports); got != tc.want {
			t.Errorf("PickPort(%d) = %d, want %d", tc.requested, got, tc.want)
		}
	}
	assertValidShell(t, f)
}

func TestUsedPortsWhenNothingMatches(t *testing.T) {
	c, f := newClient()
	f.Fail("ss -ltnH", 1, "")
	if ports, err := c.UsedPorts(t.Context()); err != nil || len(ports) != 0 {
		t.Errorf("ports = %v, err = %v; grep exiting 1 means no ports", ports, err)
	}
}

func TestAddSiteNodeJSKeepsThePasswordOutOfTheScript(t *testing.T) {
	c, f := newClient()
	err := c.AddSite(t.Context(), SiteSpec{
		Kind: SiteNodeJS, Domain: "example.com", SiteUser: "examplecom", Password: "Pa55w0rd-secret",
		NodeVersion: "22", AppPort: 3001,
	})
	if err != nil {
		t.Fatal(err)
	}
	call := f.Calls()[0].Spec
	if strings.Contains(call.Script, "Pa55w0rd-secret") || call.Secrets["CP_PASSWORD"] != "Pa55w0rd-secret" {
		t.Errorf("password must travel as a secret, not in the script: %+v", call)
	}
	if !strings.Contains(call.Script, "clpctl site:add:nodejs") || !strings.Contains(call.Script, `--appPort="$CP_PORT"`) || call.Env["CP_PORT"] != "3001" {
		t.Errorf("script = %q env = %v", call.Script, call.Env)
	}
	if strings.Contains(f.Trace(), "Pa55w0rd-secret") {
		t.Error("trace leaks the password")
	}
	assertValidShell(t, f)
}

func TestAddSiteKinds(t *testing.T) {
	for _, tc := range []struct {
		spec SiteSpec
		want string
	}{
		{SiteSpec{Kind: SitePHP, PHPVersion: "8.3", VhostTemplate: "Generic"}, `--phpVersion="$CP_PHP" --vhostTemplate="$CP_VHOST"`},
		{SiteSpec{Kind: SiteReverseProxy, ProxyURL: "http://127.0.0.1:8081"}, `--reverseProxyUrl="$CP_URL"`},
		{SiteSpec{Kind: SiteStatic}, "clpctl site:add:static"},
	} {
		c, f := newClient()
		tc.spec.Domain, tc.spec.SiteUser, tc.spec.Password = "example.com", "site1", "pw123456789012"
		if err := c.AddSite(t.Context(), tc.spec); err != nil {
			t.Errorf("%s: %v", tc.spec.Kind, err)
			continue
		}
		if !strings.Contains(f.Calls()[0].Spec.Script, tc.want) {
			t.Errorf("%s: script %q lacks %q", tc.spec.Kind, f.Calls()[0].Spec.Script, tc.want)
		}
		assertValidShell(t, f)
	}
}

func TestAddSiteRejectsBadInput(t *testing.T) {
	base := SiteSpec{Kind: SiteNodeJS, Domain: "example.com", SiteUser: "site1", Password: "pw", NodeVersion: "22", AppPort: 3000}
	for name, mutate := range map[string]func(*SiteSpec){
		"domain injection": func(s *SiteSpec) { s.Domain = "a.com; rm -rf /" },
		"bad user":         func(s *SiteSpec) { s.SiteUser = "Root User" },
		"no password":      func(s *SiteSpec) { s.Password = "" },
		"no port":          func(s *SiteSpec) { s.AppPort = 0 },
		"unknown kind":     func(s *SiteSpec) { s.Kind = "cobol" },
	} {
		c, f := newClient()
		s := base
		mutate(&s)
		if err := c.AddSite(t.Context(), s); err == nil {
			t.Errorf("%s: want an error", name)
		}
		if len(f.Calls()) != 0 {
			t.Errorf("%s: nothing may reach the server on invalid input", name)
		}
	}
}

func TestInstallCertificate(t *testing.T) {
	t.Run("www resolves: added as alternative name", func(t *testing.T) {
		c, f := newClient()
		c.DNS = dns{"example.com": true, "www.example.com": true}
		if err := c.InstallCertificate(t.Context(), "example.com", []string{"example.com"}); err != nil {
			t.Fatal(err)
		}
		call := f.Calls()[0].Spec
		if call.Env["CP_SAN"] != "www.example.com" || !strings.Contains(call.Script, "--subjectAlternativeName") {
			t.Errorf("call = %+v", call)
		}
		assertValidShell(t, f)
	})
	t.Run("www does not resolve: no alternative name", func(t *testing.T) {
		c, f := newClient()
		if err := c.InstallCertificate(t.Context(), "example.com", []string{"example.com"}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(f.Calls()[0].Spec.Script, "subjectAlternativeName") {
			t.Error("www has no DNS: it must not be requested")
		}
	})
	t.Run("required DNS missing: clpctl is not called", func(t *testing.T) {
		c, f := newClient()
		err := c.InstallCertificate(t.Context(), "example.com", []string{"example.com", "other.example.com"})
		if err == nil || !strings.Contains(err.Error(), "other.example.com") {
			t.Errorf("err = %v", err)
		}
		if len(f.Calls()) != 0 {
			t.Error("no certificate may be requested before DNS is ready")
		}
	})
	t.Run("invalid domain", func(t *testing.T) {
		c, _ := newClient()
		if err := c.InstallCertificate(t.Context(), "bad domain", nil); err == nil {
			t.Error("want an error")
		}
	})
}

func TestEnsureLayoutAndKey(t *testing.T) {
	c, f := newClient()
	if err := c.EnsureLayout(t.Context(), "/home/u/htdocs/example.com/", "u", []string{"releases", "shared"}); err != nil {
		t.Fatal(err)
	}
	script := f.Calls()[0].Spec.Script
	if !strings.Contains(script, "mkdir -p /home/u/htdocs/example.com/releases /home/u/htdocs/example.com/shared") || !strings.Contains(script, "chown u:u") {
		t.Errorf("script = %q", script)
	}

	key := `ssh-ed25519 AAAAC3Nza comment with 'quote' and "dq"`
	if err := c.InstallAuthorizedKey(t.Context(), "u", key); err != nil {
		t.Fatal(err)
	}
	call := f.Calls()[1].Spec
	if call.Env["CP_KEY"] != key || strings.Contains(call.Script, "AAAAC3Nza") {
		t.Errorf("the key must travel in the environment: %+v", call)
	}
	for _, bad := range []string{"", "a\nb"} {
		if err := c.InstallAuthorizedKey(t.Context(), "u", bad); err == nil {
			t.Errorf("key %q must be rejected", bad)
		}
	}
	assertValidShell(t, f)
}

func TestEnablePM2Boot(t *testing.T) {
	const printed = "sudo env PATH=$PATH:/usr/local/bin /usr/local/lib/node_modules/pm2/bin/pm2 startup systemd -u site1 --hp /home/site1"

	t.Run("runs the printed command as root without sudo", func(t *testing.T) {
		c, root := newClient()
		site := transport.NewFake("site1@srv").On("pm2 startup", transport.Result{Stdout: printed + "\n"})
		changed, err := c.EnablePM2Boot(t.Context(), site, "site1")
		if err != nil || !changed {
			t.Fatalf("changed %v, err %v", changed, err)
		}
		got := root.Calls()[0].Spec.Script
		if strings.Contains(got, "sudo") || !strings.HasPrefix(got, "env PATH=$PATH:/usr/local/bin ") {
			t.Errorf("root script = %q", got)
		}
	})
	t.Run("nothing printed: already set up", func(t *testing.T) {
		c, root := newClient()
		site := transport.NewFake("site1@srv")
		if changed, err := c.EnablePM2Boot(t.Context(), site, "site1"); err != nil || changed {
			t.Errorf("changed %v, err %v", changed, err)
		}
		if len(root.Calls()) != 0 {
			t.Error("root must not run anything")
		}
	})
	t.Run("unexpected shape is refused", func(t *testing.T) {
		c, root := newClient()
		site := transport.NewFake("site1@srv").On("pm2 startup", transport.Result{Stdout: "sudo rm -rf / ; pm2 startup\n"})
		if _, err := c.EnablePM2Boot(t.Context(), site, "site1"); err == nil {
			t.Error("want an error")
		}
		if len(root.Calls()) != 0 {
			t.Error("root must not run an unexpected command")
		}
	})
}

func TestValidators(t *testing.T) {
	for s, want := range map[string]bool{"example.com": true, "a.b.example.co.ma": true, "localhost": false, "a b.com": false, "x.com;y": false, "": false} {
		if ValidDomain(s) != want {
			t.Errorf("ValidDomain(%q) != %v", s, want)
		}
	}
	for s, want := range map[string]bool{"site1": true, "integral-surveys": true, "Root": false, "a b": false, "": false, "1abc": false} {
		if ValidSiteUser(s) != want {
			t.Errorf("ValidSiteUser(%q) != %v", s, want)
		}
	}
}

func TestPHPTemplateFollowsTheCurrentLink(t *testing.T) {
	base, err := os.ReadFile("testdata/symfony7.tpl")
	if err != nil {
		t.Fatal(err)
	}
	got, err := PHPTemplate(string(base), "current/public", "8.2")
	if err != nil {
		t.Fatal(err)
	}
	if first, _, _ := strings.Cut(got, "\n"); first != `#{"phpVersion":"8.2","rootDirectory":"current/public"}` {
		t.Errorf("header = %q", first)
	}
	for _, want := range []string{"fastcgi_param SCRIPT_FILENAME $realpath_root$fastcgi_script_name;", "fastcgi_param DOCUMENT_ROOT $realpath_root;", "{{php_fpm_port}}", "{{varnish_proxy_pass}}", "{{root}}"} {
		if !strings.Contains(got, want) {
			t.Errorf("the template lacks %q", want)
		}
	}
	if strings.Contains(got, "$document_root$fastcgi_script_name") {
		t.Error("the old script path must be gone")
	}
	// Only the PHP location changes: every other line of the base is kept.
	if strings.Count(got, "\n") != strings.Count(string(base), "\n")+2 {
		t.Errorf("expected one header line and one added parameter, got %d lines for %d", strings.Count(got, "\n"), strings.Count(string(base), "\n"))
	}
	if _, err := PHPTemplate("server { }", "current/public", "8.2"); err == nil {
		t.Error("a base without the PHP location must be refused")
	}
}

func TestEnsureVhostTemplateCreatesItOnceFromTheBase(t *testing.T) {
	base, _ := os.ReadFile("testdata/symfony7.tpl")
	root := transport.NewFake("root@srv")
	root.On("clpctl vhost-templates:list", transport.Result{Stdout: "| Generic | | System |\n| Symfony 7 | public | System |\n"})
	root.On("vhost-template:view", transport.Result{Stdout: string(base)})
	c := &Client{Root: root}
	created, err := c.EnsureVhostTemplate(t.Context(), TemplateSpec{Name: "deploykit-site", Base: "Symfony 7", RootDirectory: "current/public", PHPVersion: "8.2"})
	if err != nil || !created {
		t.Fatalf("created = %v, err = %v", created, err)
	}
	trace := root.Trace()
	for _, want := range []string{"CP_BASE=Symfony 7", "vhost-template:add", "CP_NAME=deploykit-site", `#{"phpVersion":"8.2","rootDirectory":"current/public"}`, "$realpath_root"} {
		if !strings.Contains(trace, want) {
			t.Errorf("trace lacks %q:\n%s", want, trace)
		}
	}

	again := transport.NewFake("root@srv")
	again.On("clpctl vhost-templates:list", transport.Result{Stdout: "| deploykit-site | current/public | Custom |\n"})
	created, err = (&Client{Root: again}).EnsureVhostTemplate(t.Context(), TemplateSpec{Name: "deploykit-site", Base: "Symfony 7", RootDirectory: "current/public", PHPVersion: "8.2"})
	if err != nil || created {
		t.Errorf("an existing template is left alone: created = %v, err = %v", created, err)
	}
	if strings.Contains(again.Trace(), "vhost-template:add") {
		t.Error("nothing may be created the second time")
	}
}
