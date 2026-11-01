package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/masilia/deploykit/internal/inventory"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/transport"
)

const cliManifest = `version: 1
project: demo
inventory: targets
artifact:
  type: release-dir
  build:
    command: "mkdir -p out && printf ok > out/index.txt"
  assemble: [{ from: "out/.", to: "." }]
runtime:
  type: pm2
  entry: server.js
  env: { PORT: "{{ target.app_port }}" }
  health: { path: /, expect: 200, tries: 2, interval: 1s }
ingress:
  type: cloudpanel
  site: { type: nodejs }
`

// project creates a git repository holding the manifest and one target.
func newProjectDir(t *testing.T, manifestYAML string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("deploykit.yaml", manifestYAML)
	write("targets/demo.env", "HOST=srv.example\nSITE_USER=demosite\nROOT_USER=root\nDOMAIN=demo.example\nPATH_ON_SERVER=/home/demosite/htdocs/demo.example\nAPP_PORT=3001\n")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"}, {"add", "."},
		{"-c", "user.email=t@example.com", "-c", "user.name=T", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "init"},
	} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

type harness struct {
	app  *app
	site *transport.Fake
	root *transport.Fake
	out  *bytes.Buffer
}

func newHarness(dir string) *harness {
	out := &bytes.Buffer{}
	a := newApp(out)
	h := &harness{app: a, site: transport.NewFake("demosite@srv"), root: transport.NewFake("root@srv"), out: out}
	a.manifestPath = filepath.Join(dir, "deploykit.yaml")
	a.now = func() time.Time { return time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC) }
	a.ssh = func(_ *inventory.Target, _ *manifest.Manifest, r role) (transport.Transport, error) {
		if r == roleRoot {
			return h.root, nil
		}
		return h.site, nil
	}
	h.site.OnFunc(func(s transport.Spec) bool { return strings.Contains(s.Script, "readlink -f") }, transport.Result{Stdout: "20261026000000\n"})
	h.site.On("DK_TRIES", transport.Result{Stdout: "200\n"})
	return h
}

func (h *harness) run(args ...string) error {
	cmd := newRootCmdTo(h.out, h.app)
	cmd.SetArgs(args)
	return cmd.Execute()
}

func TestValidate(t *testing.T) {
	dir := newProjectDir(t, cliManifest)
	h := newHarness(dir)
	if err := h.run("validate"); err != nil {
		t.Fatal(err)
	}
	if got := h.out.String(); !strings.Contains(got, "project demo, artifact release-dir, runtime pm2") || !strings.Contains(got, "1 target(s)") {
		t.Errorf("output = %q", got)
	}
}

func TestValidateReportsABadManifest(t *testing.T) {
	dir := newProjectDir(t, cliManifest+"surprise: true\n")
	h := newHarness(dir)
	err := h.run("validate")
	if err == nil || !strings.Contains(err.Error(), "surprise") {
		t.Errorf("err = %v", err)
	}
}

func TestDeployEndToEndWithAFakeServer(t *testing.T) {
	dir := newProjectDir(t, cliManifest)
	h := newHarness(dir)
	if err := h.run("deploy", "--target", "demo"); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	out := h.out.String()
	for _, want := range []string{"› source", "✔ build", "✔ assemble", "✔ upload", "✔ activate", "✔ finalize"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(h.site.Trace(), "pm2 startOrReload") {
		t.Errorf("the release was never activated:\n%s", h.site.Trace())
	}
}

func TestDeployDryRunSendsNothing(t *testing.T) {
	dir := newProjectDir(t, cliManifest)
	h := newHarness(dir)
	if err := h.run("deploy", "--dry-run"); err != nil { // the only target is picked
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "◌ source: would") {
		t.Errorf("output:\n%s", h.out)
	}
	if len(h.site.Calls()) != 0 {
		t.Errorf("a dry run must not contact the server:\n%s", h.site.Trace())
	}
}

func TestDeployBuildOnly(t *testing.T) {
	dir := newProjectDir(t, cliManifest)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	h := newHarness(dir)
	if err := h.run("deploy", "--build-only"); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	if len(h.site.Calls()) != 0 {
		t.Error("build-only must not contact the server")
	}
	// Kept for inspection, but outside the repository: Next.js derives its tracing root from the lockfiles above the build.
	kept, _ := filepath.Glob(filepath.Join(tmp, "deploykit-*", "release", "index.txt"))
	if len(kept) != 1 {
		t.Errorf("the assembled release should be kept under the temp dir, found %v", kept)
	}
	if _, err := os.Stat(filepath.Join(dir, ".deploykit")); err == nil {
		t.Error("nothing may be built inside the repository")
	}
}

func TestStatus(t *testing.T) {
	dir := newProjectDir(t, cliManifest)
	h := newHarness(dir)
	h.site.OnFunc(func(s transport.Spec) bool { return strings.Contains(s.Script, "for d in *") }, transport.Result{Stdout: "" +
		"20261026000000\tlive\t{\"stamp\":\"20261026000000\",\"commit\":\"abc1234\",\"ref\":\"HEAD\",\"date\":\"2026-10-26T00:00:00Z\"}\n" +
		"20261025000000\tsuperseded\t\n"})
	if err := h.run("status", "-t", "demo"); err != nil {
		t.Fatal(err)
	}
	out := h.out.String()
	if !strings.Contains(out, "current 20261026000000") || !strings.Contains(out, "* 20261026000000  live       abc1234") || !strings.Contains(out, "  20261025000000  superseded -") {
		t.Errorf("status output:\n%s", out)
	}
}

func TestRollback(t *testing.T) {
	dir := newProjectDir(t, cliManifest)
	h := newHarness(dir)
	h.site.OnFunc(func(s transport.Spec) bool { return strings.Contains(s.Script, "for d in *") }, transport.Result{Stdout: "20261026000000\tlive\t\n20261025000000\tsuperseded\t\n"})
	if err := h.run("rollback"); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	if !strings.Contains(h.site.Trace(), "env DK_STAMP=20261025000000") {
		t.Errorf("trace:\n%s", h.site.Trace())
	}
}

func TestBootstrapUsesTheRootIdentityForProvisioning(t *testing.T) {
	dir := newProjectDir(t, strings.Replace(cliManifest, "site: { type: nodejs }", "site: { type: nodejs, node_version: \"22\" }", 1))
	h := newHarness(dir)
	pub := filepath.Join(t.TempDir(), "key.pub")
	if err := os.WriteFile(pub, []byte("ssh-ed25519 AAAA me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.run("bootstrap", "--pubkey", pub); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	if !strings.Contains(h.root.Trace(), "CP_KEY=ssh-ed25519 AAAA me") {
		t.Errorf("provisioning must go through the root transport:\n%s", h.root.Trace())
	}
	if strings.Contains(h.site.Trace(), "clpctl") {
		t.Error("clpctl must never run as the site user")
	}
}

func TestPM2OnlyCommandsExplainThemselvesOnAComposeManifest(t *testing.T) {
	h := newHarness(".")
	h.app.manifestPath = filepath.Join("..", "..", "examples", "compose-app", "deploykit.yaml")
	for _, cmd := range []string{"rollback"} {
		err := h.run(cmd, "-t", "example")
		if err == nil || !strings.Contains(err.Error(), "pm2 targets") || !strings.Contains(err.Error(), "compose") {
			t.Errorf("%s: err = %v", cmd, err)
		}
	}
}

func TestMissingManifest(t *testing.T) {
	h := newHarness(t.TempDir())
	if err := h.run("validate"); err == nil {
		t.Error("a missing deploykit.yaml must be an error")
	}
}

func TestTargetMustBeChosenWhenAmbiguous(t *testing.T) {
	dir := newProjectDir(t, cliManifest)
	if err := os.WriteFile(filepath.Join(dir, "targets", "second.env"), []byte("HOST=h2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(dir)
	if err := h.run("status"); err == nil || !strings.Contains(err.Error(), "--target") {
		t.Errorf("err = %v", err)
	}
}

func TestDoctor(t *testing.T) {
	dir := newProjectDir(t, cliManifest)
	h := newHarness(dir)
	h.root.Fail("command -v clpctl", 1, "")
	err := h.run("doctor", "-t", "demo")
	if err == nil || !strings.Contains(err.Error(), "required tools are missing") {
		t.Fatalf("a missing clpctl must fail the doctor: %v\n%s", err, h.out)
	}
	out := h.out.String()
	for _, want := range []string{"git", "clpctl", "FAIL", "pm2"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

func TestDoctorLocalOnlyNeverContactsTheServer(t *testing.T) {
	dir := newProjectDir(t, cliManifest)
	h := newHarness(dir)
	_ = h.run("doctor", "--local")
	if len(h.site.Calls())+len(h.root.Calls()) != 0 {
		t.Error("--local must not contact the server")
	}
}

func TestDoctorExplainsAMissingSiteUserBeforeBootstrap(t *testing.T) {
	dir := newProjectDir(t, cliManifest)
	h := newHarness(dir)
	for _, tool := range []string{"curl --version", "node -v", "pm2 -v", "rsync --version"} {
		h.site.Fail(tool, 255, "Permission denied (publickey)")
	}
	if err := h.run("doctor", "-t", "demo"); err != nil {
		t.Fatalf("a site user that does not exist yet must not fail the doctor: %v\n%s", err, h.out)
	}
	out := h.out.String()
	if !strings.Contains(out, "site user") || !strings.Contains(out, "run `deploykit bootstrap`") || strings.Contains(out, "curl") && strings.Contains(out, "not found") {
		t.Errorf("output:\n%s", out)
	}
}
