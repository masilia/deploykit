package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// initRun runs a deploykit command in dir, as a user would, with answers typed on stdin when script is not empty.
func initRun(t *testing.T, dir, script string, args ...string) (string, error) {
	t.Helper()
	out := &bytes.Buffer{}
	a := newApp(out)
	a.manifestPath = filepath.Join(dir, "deploykit.yaml")
	if script != "" {
		a.in = strings.NewReader(script)
		a.interactive = func() bool { return true }
		a.accessible = true
	} else {
		a.interactive = func() bool { return false }
	}
	cmd := newRootCmdTo(out, a)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func repoWith(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const nextPkg = `{"name":"shop-site","scripts":{"lint":"eslint","build":"next build","start":"next start"},"dependencies":{"next":"^16.4.2"},"engines":{"node":">=22"}}`

const composeProd = `name: shop
services:
  proxy: {image: "nginx:1.30"}
  api:   {image: "${REGISTRY}/backend:${APP_VERSION}"}
  worker: {image: "${REGISTRY}/backend:${APP_VERSION}"}
  db:    {image: "postgres:16"}
volumes:
  pgdata: {}
  documents: {}
`

func exists(t *testing.T, dir, rel string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, rel))
	return err == nil
}

func TestInitWritesAValidPM2ProjectWithoutAsking(t *testing.T) {
	dir := repoWith(t, map[string]string{"package.json": nextPkg})
	out, err := initRun(t, dir, "", "init", "--yes", "--host", "srv.example.com", "--domain", "www.shop.example")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"Next.js on pm2", "shop-site", "deploykit validate: ok", "bootstrap -t prod"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !exists(t, dir, "deploykit.yaml") || !exists(t, dir, "targets/prod.env") || exists(t, dir, "deploy.env.example") {
		t.Errorf("files: %v", out)
	}
	if out, err = initRun(t, dir, "", "validate"); err != nil || !strings.Contains(out, "project shop-site, artifact release-dir, runtime pm2") {
		t.Errorf("validate: %v\n%s", err, out)
	}
}

func TestInitWritesAValidComposeProjectFromTheComposeFile(t *testing.T) {
	dir := repoWith(t, map[string]string{
		"docker-compose.prod.yml": composeProd, "docker/Dockerfile.api": "FROM php\n", "deployment/.env.example": "A=1\n", ".git/config": "",
	})
	out, err := initRun(t, dir, "", "init", "--yes", "--host", "srv.example.com", "--domain", "app.shop.example", "--migrate", "bin/migrate", "--app-service", "api")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !exists(t, dir, "deploy.env.example") {
		t.Error("a compose project gets deploy.env.example")
	}
	if gi, _ := os.ReadFile(filepath.Join(dir, ".gitignore")); !strings.Contains(string(gi), "deploy.env") {
		t.Errorf(".gitignore = %q", gi)
	}
	if out, err = initRun(t, dir, "", "validate"); err != nil || !strings.Contains(out, "project shop, artifact image, runtime compose") {
		t.Errorf("validate: %v\n%s", err, out)
	}
}

func TestInitCannotAskWithoutATerminalAndSaysWhatIsMissing(t *testing.T) {
	dir := repoWith(t, map[string]string{"package.json": nextPkg})
	_, err := initRun(t, dir, "", "init")
	if err == nil || !strings.Contains(err.Error(), "--host and --domain") {
		t.Errorf("err = %v", err)
	}
	if exists(t, dir, "deploykit.yaml") {
		t.Error("nothing may be written")
	}
}

func TestInitDryRunPrintsTheFilesAndWritesNothing(t *testing.T) {
	dir := repoWith(t, map[string]string{"package.json": nextPkg})
	out, err := initRun(t, dir, "", "init", "--yes", "--dry-run", "--host", "srv.example.com", "--domain", "www.shop.example")
	if err != nil || !strings.Contains(out, "── deploykit.yaml (new)") || !strings.Contains(out, "HOST=srv.example.com") || !strings.Contains(out, "dry run: nothing was written") {
		t.Errorf("%v\n%s", err, out)
	}
	if exists(t, dir, "deploykit.yaml") {
		t.Error("a dry run must not write")
	}
}

func TestInitNeverOverwritesWithoutForce(t *testing.T) {
	dir := repoWith(t, map[string]string{"package.json": nextPkg, "deploykit.yaml": "# mine\n"})
	_, err := initRun(t, dir, "", "init", "--yes", "--host", "srv.example.com", "--domain", "www.shop.example")
	if err == nil || !strings.Contains(err.Error(), "already exist") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "deploykit.yaml")); string(got) != "# mine\n" || exists(t, dir, "targets/prod.env") {
		t.Errorf("nothing may be touched: %q", got)
	}
	if _, err = initRun(t, dir, "", "init", "--yes", "--force", "--host", "srv.example.com", "--domain", "www.shop.example"); err != nil {
		t.Fatal(err)
	}
	if bak, _ := os.ReadFile(filepath.Join(dir, "deploykit.yaml.bak")); string(bak) != "# mine\n" {
		t.Errorf("the old file must be kept as .bak, got %q", bak)
	}
}

func TestInitAsksQuestionsLineByLine(t *testing.T) {
	dir := repoWith(t, map[string]string{"package.json": nextPkg})
	script := strings.Join([]string{
		"",                 // project name: keep shop-site
		"",                 // runtime: pm2
		"staging",          // target id
		"srv.example.com",  // host
		"",                 // ssh port
		"www.shop.example", // domain
		"",                 // site user
		"",                 // path
		"4000",             // app port
		"",                 // features: keep
		"/health",          // health page
		"/, /about",        // smoke pages
		"",                 // write these files? yes
	}, "\n") + "\n"
	out, err := initRun(t, dir, script, "init", "--plain")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	target, _ := os.ReadFile(filepath.Join(dir, "targets/staging.env"))
	for _, want := range []string{"HOST=srv.example.com", "SITE_USER=shop", "APP_PORT=4000", "PATH_ON_SERVER=/home/shop/htdocs/www.shop.example"} {
		if !strings.Contains(string(target), want) {
			t.Errorf("target lacks %q:\n%s", want, target)
		}
	}
	manifest, _ := os.ReadFile(filepath.Join(dir, "deploykit.yaml"))
	if !strings.Contains(string(manifest), "path: /health") || !strings.Contains(string(manifest), `paths: [/, /about]`) {
		t.Errorf("the typed answers must reach the manifest:\n%s", manifest)
	}
	if !strings.Contains(out, "1/6  Project") || !strings.Contains(out, "deploykit validate: ok") {
		t.Errorf("output:\n%s", out)
	}
}

func TestInitAsksAgainOnAWrongAnswerAndStopsWhenTheInputEnds(t *testing.T) {
	dir := repoWith(t, map[string]string{"package.json": nextPkg})
	script := "\n\n\nnot a host!\n" // project, runtime, id, a bad host, then nothing more
	out, err := initRun(t, dir, script, "init", "--plain")
	if err != nil {
		t.Fatalf("an aborted form is not an error: %v", err)
	}
	if !strings.Contains(out, "a host name or an address") || !strings.Contains(out, "cancelled, nothing written") {
		t.Errorf("output:\n%s", out)
	}
	if exists(t, dir, "deploykit.yaml") {
		t.Error("nothing may be written after an aborted form")
	}
}

func TestInitDeclinedAtTheReview(t *testing.T) {
	dir := repoWith(t, map[string]string{"package.json": nextPkg})
	script := strings.Join([]string{"", "", "", "srv.example.com", "", "www.shop.example", "", "", "", "", "", "", "n"}, "\n") + "\n"
	out, err := initRun(t, dir, script, "init", "--plain")
	if err != nil || !strings.Contains(out, "cancelled, nothing written") || exists(t, dir, "deploykit.yaml") {
		t.Errorf("%v\n%s", err, out)
	}
}

func TestTargetAddCreatesTheNextServer(t *testing.T) {
	dir := repoWith(t, map[string]string{"package.json": nextPkg})
	if out, err := initRun(t, dir, "", "init", "--yes", "--host", "srv.example.com", "--domain", "www.shop.example"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	out, err := initRun(t, dir, "", "target", "add", "--yes", "--id", "second", "--host", "srv2.example.com", "--domain", "www.other.example")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "targets/second.env"))
	if !strings.Contains(string(body), "HOST=srv2.example.com") || !strings.Contains(string(body), "SITE_USER=other") {
		t.Errorf("target:\n%s", body)
	}
	if out, err = initRun(t, dir, "", "validate"); err != nil || !strings.Contains(out, "2 target(s)") {
		t.Errorf("validate: %v\n%s", err, out)
	}
	if _, err = initRun(t, dir, "", "target", "add", "--yes", "--id", "second", "--host", "h.example.com", "--domain", "x.example.com"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("an existing id must be refused: %v", err)
	}
}

func TestTargetAddAsksOnlyAboutTheServer(t *testing.T) {
	dir := repoWith(t, map[string]string{"docker-compose.prod.yml": composeProd, "docker/Dockerfile.api": "FROM php\n"})
	if out, err := initRun(t, dir, "", "init", "--yes", "--ingress", "standalone", "--host", "srv.example.com", "--domain", "app.shop.example"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	script := strings.Join([]string{"client-b", "srv-b.example.com", "", "app.b.example", "", "", "y"}, "\n") + "\n"
	out, err := initRun(t, dir, script, "target", "add", "--plain")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "targets/client-b.env"))
	if !strings.Contains(string(body), "SSH_USER=deploy") || strings.Contains(string(body), "SITE_USER") || !strings.Contains(string(body), "PATH_ON_SERVER=/opt/shop") {
		t.Errorf("a standalone target needs no CloudPanel keys:\n%s", body)
	}
}
