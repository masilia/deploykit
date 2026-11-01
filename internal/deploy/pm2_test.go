package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/tmpl"
	"github.com/masilia/deploykit/internal/transport"
)

func pm2Config() *manifest.PM2 {
	return &manifest.PM2{
		Name:      "{{ project }}",
		Entry:     "server.js",
		Env:       map[string]string{"NODE_ENV": "production", "PORT": "{{ target.app_port }}", "HOSTNAME": "0.0.0.0"},
		MaxMemory: "700M",
	}
}

var testVars = tmpl.Vars{"project": "acme-site", "target": map[string]string{"app_port": "3001"}}

func TestEcosystem(t *testing.T) {
	got, err := Ecosystem(pm2Config(), Layout{Root: "/home/u/htdocs/x.ma"}, testVars)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"name": "acme-site"`, `"cwd": "/home/u/htdocs/x.ma/current"`, `"script": "server.js"`,
		`"PORT": "3001"`, `"HOSTNAME": "0.0.0.0"`, `"max_memory_restart": "700M"`, `"time": true`, "module.exports = {",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ecosystem lacks %s:\n%s", want, got)
		}
	}
}

// The generated file must be valid JavaScript that node can load.
func TestEcosystemIsLoadableByNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	cfg := pm2Config()
	cfg.Env["TRICKY"] = `quote " backslash \ newline ` + "\n" + ` ${x} ` + "`" + ` '`
	content, err := Ecosystem(cfg, Layout{Root: "/srv/site"}, testVars)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "ecosystem.config.cjs")
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(t.Context(), node, "-e", `const c=require(process.argv[1]); process.stdout.write(c.apps[0].env.TRICKY)`, file).Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	if string(out) != cfg.Env["TRICKY"] {
		t.Errorf("node read %q, want %q", out, cfg.Env["TRICKY"])
	}
}

func TestEcosystemRejectsBadInput(t *testing.T) {
	bad := pm2Config()
	bad.Name = "x; rm -rf /"
	if _, err := Ecosystem(bad, Layout{Root: "/s"}, testVars); err == nil {
		t.Error("an unsafe pm2 name must be rejected")
	}
	if _, err := Ecosystem(pm2Config(), Layout{Root: "/s"}, tmpl.Vars{"project": "p"}); err == nil {
		t.Error("an undefined template variable must be an error")
	}
}

func TestPM2Commands(t *testing.T) {
	f := transport.NewFake("site@srv").On("pm2 pid", transport.Result{Stdout: "4242\n"})
	p := PM2{T: f, Name: "app", L: Layout{Root: "/srv/site"}}

	if ok, err := p.Managed(t.Context()); err != nil || !ok {
		t.Errorf("Managed = %v, %v", ok, err)
	}
	if err := p.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Logs(t.Context(), 30); err != nil {
		t.Fatal(err)
	}
	if err := p.UploadEcosystem(t.Context(), "module.exports = {};\n"); err != nil {
		t.Fatal(err)
	}
	calls := f.Calls()
	if !strings.Contains(calls[1].Spec.Script, "pm2 startOrReload ecosystem.config.cjs --update-env && pm2 save") || calls[1].Spec.Env["DK_ROOT"] != "/srv/site" {
		t.Errorf("reload call = %+v", calls[1].Spec)
	}
	if calls[2].Spec.Env["DK_LINES"] != "30" || !strings.Contains(calls[2].Spec.Script, "--nostream") {
		t.Errorf("logs call = %+v", calls[2].Spec)
	}
	if calls[3].Stdin != "module.exports = {};\n" || calls[3].Spec.Argv[len(calls[3].Spec.Argv)-1] != "/srv/site/ecosystem.config.cjs" {
		t.Errorf("ecosystem upload = %+v / %q", calls[3].Spec, calls[3].Stdin)
	}
}

func TestManagedTreatsPidZeroAsNotRunning(t *testing.T) {
	for out, want := range map[string]bool{"": false, "0\n": false, "123\n": true} {
		p := PM2{T: transport.NewFake("x").On("pm2 pid", transport.Result{Stdout: out}), Name: "a"}
		if got, _ := p.Managed(t.Context()); got != want {
			t.Errorf("pid output %q: Managed = %v, want %v", out, got, want)
		}
	}
}

func TestHealthy(t *testing.T) {
	h := manifest.Health{Path: "/fr", Expect: 200, Tries: 30, Interval: time.Second}

	f := transport.NewFake("srv").On("DK_TRIES", transport.Result{Stdout: "200\n"})
	if code, ok, err := Healthy(t.Context(), f, "3001", h); err != nil || !ok || code != "200" {
		t.Errorf("healthy: %q %v %v", code, ok, err)
	}
	env := f.Calls()[0].Spec.Env
	if env["DK_PORT"] != "3001" || env["DK_PATH"] != "/fr" || env["DK_TRIES"] != "30" || env["DK_SLEEP"] != "1" {
		t.Errorf("env = %v", env)
	}

	f = transport.NewFake("srv").On("DK_TRIES", transport.Result{Stdout: "502\n", ExitCode: 1})
	if code, ok, err := Healthy(t.Context(), f, "3001", h); err != nil || ok || code != "502" {
		t.Errorf("unhealthy must be (502, false, nil): %q %v %v", code, ok, err)
	}

	f = transport.NewFake("srv").On("DK_TRIES", transport.Result{ExitCode: 255, Stderr: "ssh: refused"})
	if _, ok, err := Healthy(t.Context(), f, "3001", h); err == nil || ok {
		t.Error("a transport failure is an error, not 'unhealthy'")
	}
	for _, bad := range []struct{ port, path string }{{"3001; x", "/"}, {"3001", "fr"}} {
		hh := h
		hh.Path = bad.path
		if _, _, err := Healthy(t.Context(), transport.NewFake("x"), bad.port, hh); err == nil {
			t.Errorf("%+v must be rejected", bad)
		}
	}
}

// Run the health script for real against a local HTTP server.
func TestHealthyScriptAgainstARealServer(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}
	srv := startHTTP(t, 200)
	h := manifest.Health{Path: "/fr", Expect: 200, Tries: 3, Interval: 10 * time.Millisecond}
	if code, ok, err := Healthy(t.Context(), transport.NewLocal(), srv.port, h); err != nil || !ok || code != "200" {
		t.Errorf("200 server: %q %v %v", code, ok, err)
	}
	srv500 := startHTTP(t, 500)
	if code, ok, err := Healthy(t.Context(), transport.NewLocal(), srv500.port, h); err != nil || ok || code != "500" {
		t.Errorf("500 server: %q %v %v", code, ok, err)
	}
}
