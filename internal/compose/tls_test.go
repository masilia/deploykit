package compose

import (
	"bytes"
	"strings"
	"testing"

	"github.com/masilia/deploykit/internal/envfile"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/transport"
)

func standaloneWith(extra ...manifest.ExtraDomain) *manifest.Standalone {
	st := &manifest.Standalone{}
	st.TLS.Volumes = []string{"letsencrypt", "certbot-www"}
	st.TLS.DomainConf.Template = "docker/proxy/domain.conf.example"
	st.TLS.DomainConf.Target = "docker/proxy/conf.d/{{ target.domain }}.conf"
	st.TLS.ExtraDomains = extra
	return st
}

var adminDomain = manifest.ExtraDomain{Domain: "{{ env.ADMIN_HOST | default: '' }}", Template: "docker/proxy/admin.conf.example", Target: "docker/proxy/conf.d/admin.conf"}

// tlsRuns returns the environment of every TLS script, in order.
func tlsRuns(f *transport.Fake) []map[string]string {
	var runs []map[string]string
	for _, c := range f.Calls() {
		if c.Spec != nil && strings.Contains(c.Spec.Script, "certbot/certbot") {
			runs = append(runs, c.Spec.Env)
		}
	}
	return runs
}

func TestEnsureTLSSecuresEveryDomainWithItsOwnFile(t *testing.T) {
	s, f := newStack(t, "certbot")
	doc := envfile.Parse([]byte("DOMAIN=app.example.com\nADMIN_HOST=admin.example.com\nLETSENCRYPT_EMAIL=ops@example.com\n"))
	if err := EnsureTLS(t.Context(), &bytes.Buffer{}, s, standaloneWith(adminDomain), doc); err != nil {
		t.Fatal(err)
	}
	runs := tlsRuns(f)
	if len(runs) != 2 {
		t.Fatalf("want one script per name, got %d: %v", len(runs), runs)
	}
	want := []struct{ domain, conf, tpl string }{
		{"app.example.com", "docker/proxy/conf.d/app.example.com.conf", "docker/proxy/domain.conf.example"},
		{"admin.example.com", "docker/proxy/conf.d/admin.conf", "docker/proxy/admin.conf.example"},
	}
	for i, w := range want {
		r := runs[i]
		if r["DK_DOMAIN"] != w.domain || r["DK_CONF"] != w.conf || r["DK_TEMPLATE"] != w.tpl || r["DK_EMAIL"] != "ops@example.com" || r["DK_LE"] != "acme_letsencrypt" {
			t.Errorf("run %d = %v, want %+v", i, r, w)
		}
	}
}

func TestEnsureTLSSkipsAnExtraDomainThatIsNotSet(t *testing.T) {
	s, f := newStack(t, "certbot")
	doc := envfile.Parse([]byte("DOMAIN=app.example.com\n")) // ADMIN_HOST absent
	var out bytes.Buffer
	if err := EnsureTLS(t.Context(), &out, s, standaloneWith(adminDomain), doc); err != nil {
		t.Fatal(err)
	}
	if runs := tlsRuns(f); len(runs) != 1 || runs[0]["DK_DOMAIN"] != "app.example.com" {
		t.Errorf("only the main domain is secured, got %v", runs)
	}
	if !strings.Contains(out.String(), "skipped") {
		t.Errorf("the skip must be said, got %q", out.String())
	}
}

func TestEnsureTLSRefusesAnInvalidDomainBeforeRunningAnything(t *testing.T) {
	for _, bad := range []string{"admin.example.com; rm -rf /", "not a domain", "-x.example.com", "admin"} {
		s, f := newStack(t, "certbot")
		doc := envfile.Parse([]byte("DOMAIN=app.example.com\nADMIN_HOST=" + strconvQuote(bad) + "\n"))
		err := EnsureTLS(t.Context(), &bytes.Buffer{}, s, standaloneWith(adminDomain), doc)
		if err == nil || !strings.Contains(err.Error(), "not a valid domain") {
			t.Errorf("%q: want a domain error, got %v", bad, err)
		}
		if n := len(tlsRuns(f)); n != 0 {
			t.Errorf("%q: %d scripts ran before the check", bad, n)
		}
	}
}

// strconvQuote keeps a value with spaces or a semicolon on one .env line.
func strconvQuote(v string) string { return `"` + v + `"` }

func TestEnsureTLSKeepsGoingWhenOneNameFails(t *testing.T) {
	s, f := newStack(t, "certbot")
	f.OnFunc(func(sp transport.Spec) bool { return sp.Env["DK_DOMAIN"] == "app.example.com" }, transport.Result{ExitCode: 1, Stderr: "dns"})
	doc := envfile.Parse([]byte("DOMAIN=app.example.com\nADMIN_HOST=admin.example.com\n"))
	err := EnsureTLS(t.Context(), &bytes.Buffer{}, s, standaloneWith(adminDomain), doc)
	if err == nil || !strings.Contains(err.Error(), "app.example.com") || strings.Contains(err.Error(), "admin.example.com") {
		t.Fatalf("the error must name the name that failed, and only it: %v", err)
	}
	if runs := tlsRuns(f); len(runs) != 2 {
		t.Errorf("the second name must still be tried, got %d runs", len(runs))
	}
}

func TestEnsureTLSNeedsBothVolumes(t *testing.T) {
	s, _ := newStack(t, "certbot")
	st := standaloneWith()
	st.TLS.Volumes = []string{"letsencrypt"}
	if err := EnsureTLS(t.Context(), &bytes.Buffer{}, s, st, envfile.Parse([]byte("DOMAIN=app.example.com\n"))); err == nil {
		t.Error("one volume is not enough")
	}
}

func TestUpdateTLSStepIsOptionalAndReusesTheInstallLogic(t *testing.T) {
	s, f := newStack(t, "certbot")
	u := &Update{Stack: s, Standalone: standaloneWith(adminDomain), doc: envfile.Parse([]byte("DOMAIN=app.example.com\nADMIN_HOST=admin.example.com\n"))}
	steps, err := u.steps("tls")
	if err != nil || len(steps) != 1 || !steps[0].Optional {
		t.Fatalf("the tls update step must exist and be optional (a failure may not roll a release back): %+v, %v", steps, err)
	}
	if err := steps[0].Run(t.Context(), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if runs := tlsRuns(f); len(runs) != 2 {
		t.Errorf("update secures the same names as install, got %d runs", len(runs))
	}
	// Behind CloudPanel there is nothing to do.
	u = &Update{Stack: s}
	steps, _ = u.steps("tls")
	f2Before := len(f.Calls())
	if err := steps[0].Run(t.Context(), &bytes.Buffer{}); err != nil || len(f.Calls()) != f2Before {
		t.Errorf("no standalone ingress: no call expected (err %v)", err)
	}
}
