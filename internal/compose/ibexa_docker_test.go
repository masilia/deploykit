package compose

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/runner"
	"github.com/masilia/deploykit/internal/tmpl"
	"github.com/masilia/deploykit/internal/transport"
)

// The Ibexa Docker profile keeps its compose file in docker/ and the .env in the deployment root.
func ibexaStack(t *testing.T) (*Stack, *transport.Fake) {
	t.Helper()
	m, err := manifest.Load("../../examples/php-ibexa-docker/deploykit.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := m.Compose()
	if err != nil {
		t.Fatal(err)
	}
	f := transport.NewFake("srv")
	return &Stack{T: f, Root: "/home/acme/ibexa", Project: "ibexa", Cfg: cfg, Vars: tmpl.Vars{}, Sleep: func(time.Duration) {}}, f
}

func TestIbexaDockerComposeFilesLiveInDockerButTheEnvInTheRoot(t *testing.T) {
	s, f := ibexaStack(t)
	if _, err := s.Run(t.Context(), nil, "ps"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.Calls()[0].Spec.Argv, " "); got != "docker compose -p ibexa --project-directory . -f docker/docker-compose.prod.yml ps" {
		t.Errorf("argv = %s", got)
	}
}

func TestIbexaDockerInstallStartsSolrWithTheDataServices(t *testing.T) {
	s, f := ibexaStack(t)
	in := &Install{Stack: s}
	steps, err := in.steps("up_data")
	if err != nil {
		t.Fatal(err)
	}
	if err := (&runner.Runner{}).Do(t.Context(), steps...); err != nil {
		t.Fatal(err)
	}
	// db, redis and solr are data; varnish, web and php wait for the app commands.
	if want := "up -d db redis solr"; !strings.Contains(f.Trace(), want) {
		t.Errorf("trace lacks %q:\n%s", want, f.Trace())
	}
}

// The standalone variant of the Ibexa profile: no CloudPanel, an nginx container of the stack owns ports 80 and 443.
func TestIbexaDockerStandaloneExample(t *testing.T) {
	m, err := manifest.Load("../../examples/php-ibexa-docker/deploykit.standalone.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := m.Compose()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.Files, ",") != "docker/docker-compose.prod.yml,docker/docker-compose.standalone.yml" || cfg.ProjectDirectory != "." {
		t.Errorf("project dir %q files %v", cfg.ProjectDirectory, cfg.Files)
	}
	roles := map[string]string{}
	for _, s := range cfg.Services {
		roles[s.Name] = s.Role
	}
	// The proxy is the service the install reloads and the update reloads: it must be the one with that role, and the only one.
	if roles["proxy"] != "proxy" || roles["varnish"] != "edge" || roles["web"] == "proxy" {
		t.Errorf("roles = %v", roles)
	}
	var st manifest.Standalone
	if err := m.Ingress.Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.Type != "standalone" || len(st.TLS.Volumes) != 2 || !st.Preflight.Port80Free || !st.Preflight.DNSMatchesServer ||
		st.TLS.DomainConf.Target != "docker/proxy/conf.d/{{ target.domain }}.conf" {
		t.Errorf("standalone ingress = %+v", st)
	}
	env, err := m.EnvConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env.Prompts, "LETSENCRYPT_EMAIL") {
		t.Errorf("the certificate needs a contact: prompts = %v", env.Prompts)
	}
}
