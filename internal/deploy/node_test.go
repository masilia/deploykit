package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/masilia/deploykit/internal/inventory"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/runner"
	"github.com/masilia/deploykit/internal/transport"
)

const nodeManifest = `
version: 1
project: demo
artifact:
  type: release-dir
  build:
    checks: ["test -f app.txt"]
    command: "mkdir -p out && printf '%s' \"$SITE\" > out/index.txt && cp app.txt out/app.txt"
    env: { SITE: "{{ target.site_url }}" }
  assemble: [{ from: "out/.", to: "." }]
  keep: 2
runtime:
  type: pm2
  name: "{{ project }}"
  entry: server.js
  env: { PORT: "{{ target.app_port }}", HOSTNAME: "0.0.0.0" }
  preflight: { same_arch: true, port_free_on_first_deploy: true }
  health: { path: /fr, expect: 200, tries: 5, interval: 1s }
ingress:
  type: cloudpanel
  site: { type: nodejs }
hooks:
  before_upload: [{ run: "echo reminder-from-hook" }]
`

const (
	prevStamp = "20261026000000"
	newStamp  = "20261027120000"
)

type fixture struct {
	node *Node
	site *transport.Fake
	out  *strings.Builder
}

func newFixture(t *testing.T, manifestYAML string) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}} {
		runIn(t, repo, "git", args...)
	}
	if err := os.WriteFile(filepath.Join(repo, "app.txt"), []byte("app"), 0o600); err != nil {
		t.Fatal(err)
	}
	runIn(t, repo, "git", "add", ".")
	runIn(t, repo, "git", "-c", "user.email=t@example.com", "-c", "user.name=T", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "initial commit")

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "demo.env"), []byte("PATH_ON_SERVER=/home/demo/htdocs/demo.example\nAPP_PORT=3001\nSITE_URL=https://demo.example\nHOST=h\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target, err := inventory.Open(dir, "demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse([]byte(manifestYAML))
	if err != nil {
		t.Fatal(err)
	}

	site := transport.NewFake("demo@srv")
	site.OnFunc(func(s transport.Spec) bool { return strings.Contains(s.Script, "readlink -f") }, transport.Result{Stdout: prevStamp + "\n"})
	site.OnFunc(func(s transport.Spec) bool {
		return strings.Contains(s.Script, "uname") || strings.Join(s.Argv, " ") == "uname -sm"
	}, transport.Result{Stdout: "Linux x86_64\n"})

	n, err := NewNode(Node{
		Manifest: m, Target: target,
		Local: transport.NewLocal(), Site: site,
		Repo: repo, WorkDir: t.TempDir(), Author: "tester",
		Now: func() time.Time { return time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{node: n, site: site, out: &strings.Builder{}}
}

func runIn(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

type events struct{ lines []string }

func (e *events) Handle(ev runner.Event) {
	switch ev.Kind {
	case runner.StepFailed:
		e.lines = append(e.lines, "FAILED "+ev.Step+": "+ev.Err.Error())
	case runner.Output:
		e.lines = append(e.lines, "  "+ev.Line)
	case runner.StepStart:
		e.lines = append(e.lines, "step "+ev.Step)
	}
}

func (e *events) text() string { return strings.Join(e.lines, "\n") }

// inOrder checks that each fragment appears after the previous one.
func inOrder(t *testing.T, haystack string, fragments ...string) {
	t.Helper()
	pos := 0
	for _, f := range fragments {
		i := strings.Index(haystack[pos:], f)
		if i < 0 {
			t.Fatalf("expected %q after offset %d in:\n%s", f, pos, haystack)
		}
		pos += i + len(f)
	}
}

func TestDeploySuccess(t *testing.T) {
	fx := newFixture(t, nodeManifest)
	fx.site.On("DK_TRIES", transport.Result{Stdout: "200\n"})
	ev := &events{}
	if err := fx.node.Deploy(t.Context(), &runner.Runner{Reporter: ev}); err != nil {
		t.Fatalf("%v\n%s", err, ev.text())
	}

	trace := fx.site.Trace()
	inOrder(t, trace,
		"mkdir \"$DK_LOCK\"", // lock
		"DK_RELEASES",        // preflight
		"mkdir -p /home/demo/htdocs/demo.example/releases/"+newStamp,
		"/releases/"+newStamp+"/.release.json", // metadata written before the upload
		"sync "+fx.node.releaseDir+"/ -> /home/demo/htdocs/demo.example/releases/"+newStamp+"/ --delete --link-dest=/home/demo/htdocs/demo.example/releases/"+prevStamp+" --exclude=.status --exclude=.release.json",
		"/ecosystem.config.cjs",
		`ln -sfn "releases/$DK_STAMP" current.tmp`, // switch
		"pm2 startOrReload",
		"DK_TRIES", // health
		"/releases/"+newStamp+"/.status",
		"rm -rf -- \"$DK_LOCK\"",
	)
	if !strings.Contains(trace, "env DK_STAMP="+newStamp) {
		t.Errorf("switch must target the new stamp:\n%s", trace)
	}

	// The release that was uploaded holds the build output, rendered from the target.
	got, err := os.ReadFile(filepath.Join(fx.node.releaseDir, "index.txt"))
	if err != nil || string(got) != "https://demo.example" {
		t.Errorf("index.txt = %q, %v", got, err)
	}
	if !strings.Contains(ev.text(), "reminder-from-hook") {
		t.Errorf("before_upload hook did not run:\n%s", ev.text())
	}
	// The new release is marked live ("live\n" is 5 bytes on stdin).
	if !strings.Contains(trace, "stdin <5 bytes>") { // "live\n"
		t.Errorf("expected a live status write:\n%s", trace)
	}
}

func TestDeployRollsBackWhenHealthFails(t *testing.T) {
	fx := newFixture(t, nodeManifest)
	fx.site.On("DK_TRIES", transport.Result{Stdout: "502\n", ExitCode: 1})
	fx.site.On("pm2 logs", transport.Result{Stdout: "Error: cannot find module\n"})
	ev := &events{}
	err := fx.node.Deploy(t.Context(), &runner.Runner{Reporter: ev})
	if err == nil || !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "rolled back to "+prevStamp) {
		t.Fatalf("err = %v", err)
	}
	trace := fx.site.Trace()
	inOrder(t, trace,
		"env DK_STAMP="+newStamp, // switch to the new release
		"DK_TRIES",
		"env DK_STAMP="+prevStamp, // and back
		"pm2 startOrReload",       // reload of the previous release
		"pm2 logs",
		"rm -rf -- \"$DK_LOCK\"", // the lock is released even on failure
	)
	if !strings.Contains(ev.text(), "Error: cannot find module") || !strings.Contains(ev.text(), "going back to "+prevStamp) {
		t.Errorf("operator output:\n%s", ev.text())
	}
	if strings.Contains(trace, "rm -rf -- "+newStamp) {
		t.Error("the failed release is pruned by a later deploy, not during the failure")
	}
}

func TestDeployFirstTimeHasNoLinkDestAndNoRollbackTarget(t *testing.T) {
	fx := newFixture(t, nodeManifest)
	fx.site = transport.NewFake("demo@srv") // no current release on the server
	fx.node.Site = fx.site
	fx.site.On("uname", transport.Result{Stdout: "Linux x86_64\n"})
	fx.site.On("DK_TRIES", transport.Result{Stdout: "000\n", ExitCode: 1})
	err := fx.node.Deploy(t.Context(), &runner.Runner{})
	if err == nil || strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("err = %v (there is no previous release to go back to)", err)
	}
	if strings.Contains(fx.site.Trace(), "--link-dest") {
		t.Error("--link-dest must not be used when there is no previous release")
	}
}

func TestSmokeFailureSendsNothing(t *testing.T) {
	yaml := nodeManifest + `
smoke:
  start: { command: "echo broken >&2; exit 1", port: { range: [35000, 35100] } }
  paths: [/fr]
`
	fx := newFixture(t, yaml)
	err := fx.node.Deploy(t.Context(), &runner.Runner{})
	if err == nil || !strings.Contains(err.Error(), "nothing was sent") {
		t.Fatalf("err = %v", err)
	}
	if n := len(fx.site.Calls()); n != 0 {
		t.Errorf("the server was contacted %d times before the smoke test passed:\n%s", n, fx.site.Trace())
	}
}

func TestBuildOnlyNeverContactsTheServer(t *testing.T) {
	fx := newFixture(t, nodeManifest)
	fx.node.Opts.BuildOnly = true
	ev := &events{}
	if err := fx.node.Deploy(t.Context(), &runner.Runner{Reporter: ev}); err != nil {
		t.Fatal(err)
	}
	if n := len(fx.site.Calls()); n != 0 {
		t.Errorf("build-only contacted the server:\n%s", fx.site.Trace())
	}
	if _, err := os.Stat(filepath.Join(fx.node.releaseDir, "app.txt")); err != nil {
		t.Errorf("the release must be assembled: %v", err)
	}
}

func TestDeployRefusesWhenLocked(t *testing.T) {
	fx := newFixture(t, nodeManifest)
	fx.site.OnFunc(func(s transport.Spec) bool { return strings.Contains(s.Script, "mkdir \"$DK_LOCK\"") },
		transport.Result{Stdout: "alice since 2026-10-27T11:59:00Z\n", ExitCode: 3})
	err := fx.node.Deploy(t.Context(), &runner.Runner{})
	if err == nil || !strings.Contains(err.Error(), "alice") || !strings.Contains(err.Error(), ".deploy.lock") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(fx.site.Trace(), "startOrReload") || strings.Contains(fx.site.Trace(), "sync ") {
		t.Error("nothing may be uploaded or switched while another deploy holds the lock")
	}
	// A deploy that never got the lock must not remove the lock of the other one.
	if strings.Contains(fx.site.Trace(), "rm -rf -- \"$DK_LOCK\"") {
		t.Error("the lock belongs to someone else and must not be removed")
	}
}

func TestPreflightRejectsADifferentArchitecture(t *testing.T) {
	fx := newFixture(t, nodeManifest)
	fx.site = transport.NewFake("demo@srv")
	fx.node.Site = fx.site
	fx.site.OnFunc(func(s transport.Spec) bool { return strings.Join(s.Argv, " ") == "uname -sm" }, transport.Result{Stdout: "Darwin arm64\n"})
	err := fx.node.Deploy(t.Context(), &runner.Runner{})
	if err == nil || !strings.Contains(err.Error(), "sharp") {
		t.Fatalf("err = %v", err)
	}
}

func TestRollback(t *testing.T) {
	fx := newFixture(t, nodeManifest)
	fx.site.OnFunc(func(s transport.Spec) bool { return strings.Contains(s.Script, "for d in *") }, transport.Result{Stdout: "" +
		"20261027000000\tfailed\t\n" +
		"20261026000000\tlive\t\n" + // current (readlink rule above)
		"20261025000000\tsuperseded\t\n"})
	fx.site.On("DK_TRIES", transport.Result{Stdout: "200\n"})

	if err := fx.node.Rollback(t.Context(), &runner.Runner{}, ""); err != nil {
		t.Fatal(err)
	}
	trace := fx.site.Trace()
	inOrder(t, trace, "env DK_STAMP=20261025000000", "pm2 startOrReload", "DK_TRIES", "/releases/"+prevStamp+"/.status", "/releases/20261025000000/.status", "rm -rf -- \"$DK_LOCK\"")

	fx2 := newFixture(t, nodeManifest)
	fx2.site.OnFunc(func(s transport.Spec) bool { return strings.Contains(s.Script, "for d in *") }, transport.Result{Stdout: "20261026000000\tlive\t\n"})
	if err := fx2.node.Rollback(t.Context(), &runner.Runner{}, ""); err == nil || !strings.Contains(err.Error(), "no older release") {
		t.Errorf("err = %v", err)
	}
	if err := fx2.node.Rollback(t.Context(), &runner.Runner{}, "20250101000000"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("explicit unknown release: err = %v", err)
	}
}

func TestNewNodeValidation(t *testing.T) {
	m, err := manifest.Parse([]byte(nodeManifest))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name, content string) *inventory.Target {
		if err := os.WriteFile(filepath.Join(dir, name+".env"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		tg, _ := inventory.Open(dir, name, nil)
		return tg
	}
	for name, tg := range map[string]*inventory.Target{
		"no path":     write("a", "APP_PORT=3000\n"),
		"relative":    write("b", "PATH_ON_SERVER=htdocs/x\nAPP_PORT=3000\n"),
		"no app port": write("c", "PATH_ON_SERVER=/srv/x\n"),
	} {
		if _, err := NewNode(Node{Manifest: m, Target: tg}); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
