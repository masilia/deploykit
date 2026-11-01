package releasedir

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/tmpl"
	"github.com/masilia/deploykit/internal/transport"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "user.email=t@example.com", "-c", "user.name=T", "-c", "commit.gpgsign=false"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func repoWithCommit(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	tree(t, dir, map[string]string{"app.txt": "v1", "sub/b.txt": "b"})
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "first commit")
	return dir
}

func TestExportIgnoresUncommittedChanges(t *testing.T) {
	repo := repoWithCommit(t)
	tree(t, repo, map[string]string{"app.txt": "v2-dirty", "untracked.txt": "u"})

	dest := filepath.Join(t.TempDir(), "src")
	src, err := Export(t.Context(), transport.NewLocal(), repo, "HEAD", dest)
	if err != nil {
		t.Fatal(err)
	}
	if got := listing(t, dest); got != "app.txt,sub/b.txt" {
		t.Errorf("export = %s (untracked files must not ship)", got)
	}
	data, _ := os.ReadFile(filepath.Join(dest, "app.txt"))
	if string(data) != "v1" {
		t.Errorf("app.txt = %q, want the committed v1", data)
	}
	if len(src.Commit) != 7 || src.Subject != "first commit" || !src.Dirty {
		t.Errorf("source = %+v, want a 7-char commit, the subject and Dirty (tracked file modified)", src)
	}
}

func TestExportOfAMissingRefFails(t *testing.T) {
	repo := repoWithCommit(t)
	if _, err := Export(t.Context(), transport.NewLocal(), repo, "v9.9.9", filepath.Join(t.TempDir(), "x")); err == nil {
		t.Error("an unknown ref must fail")
	}
}

func TestBuildPlanAndOptions(t *testing.T) {
	rd := &manifest.ReleaseDir{}
	rd.Build.Install = "npm ci"
	rd.Build.Checks = []string{"npm run lint", "npx tsc --noEmit"}
	rd.Build.Command = "npm run build"

	names := func(steps []BuildStep) string {
		var n []string
		for _, s := range steps {
			n = append(n, s.Name)
		}
		return strings.Join(n, "|")
	}
	if got := names(BuildPlan(rd, BuildOptions{})); got != "install|check: npm run lint|check: npx tsc --noEmit|build" {
		t.Errorf("full plan = %s", got)
	}
	if got := names(BuildPlan(rd, BuildOptions{SkipInstall: true, SkipChecks: true})); got != "build" {
		t.Errorf("skipped plan = %s", got)
	}
}

func TestBuildEnvRendersTemplates(t *testing.T) {
	rd := &manifest.ReleaseDir{}
	rd.Build.Env = map[string]string{"NEXT_PUBLIC_SITE_URL": "{{ target.site_url }}", "PLAIN": "x"}
	env, err := BuildEnv(rd, tmpl.Vars{"target": map[string]string{"site_url": "https://example.com"}})
	if err != nil || env["NEXT_PUBLIC_SITE_URL"] != "https://example.com" || env["PLAIN"] != "x" {
		t.Errorf("env = %v, %v", env, err)
	}
	if _, err := BuildEnv(rd, tmpl.Vars{}); err == nil {
		t.Error("a missing variable must fail the build before it starts")
	}
}

func TestRunBuildStepStreamsOutputAndPropagatesFailure(t *testing.T) {
	var out bytes.Buffer
	err := RunBuildStep(t.Context(), transport.NewLocal(), t.TempDir(), BuildStep{Name: "build", Command: `echo "site=$SITE"`}, map[string]string{"SITE": "https://x"}, &out)
	if err != nil || strings.TrimSpace(out.String()) != "site=https://x" {
		t.Errorf("out %q, err %v", out.String(), err)
	}
	if err := RunBuildStep(t.Context(), transport.NewLocal(), t.TempDir(), BuildStep{Command: "exit 4"}, nil, &out); err == nil {
		t.Error("a failing build command must fail")
	}
}
