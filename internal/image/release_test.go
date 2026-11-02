package image

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/runner"
	"github.com/masilia/deploykit/internal/transport"
)

func TestValidRegistryMatchesTheShellFunction(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	dir := os.Getenv("DEPLOYKIT_REFERENCE_DIR")
	if dir == "" {
		t.Skip("DEPLOYKIT_REFERENCE_DIR not set: the original lib.sh is not part of this repository")
	}
	lib, err := filepath.Abs(filepath.Join(dir, "lib.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, reg := range []string{
		"ghcr.io/my-org", "ghcr.io/my-org/sub", "my-org", "docker.io", "localhost/x", "localhost:5000/x", "registry.local:5000/team/app",
		"ghcr.io", "ghcr.io/", "/x", "", "a b/c", "ghcr.io/org name", "GHCR.IO/Org", "host:abc/x", "x.y/a;b", "ghcr.io/org/", "-.-/x",
	} {
		shErr := exec.CommandContext(t.Context(), "sh", "-c", `. "$1"; registry_valid "$2"`, "sh", lib, reg).Run()
		if want, got := shErr == nil, ValidRegistry(reg); got != want {
			t.Errorf("ValidRegistry(%q) = %v, shell registry_valid = %v", reg, got, want)
		}
	}
}

func cfg(t *testing.T) *manifest.Image {
	t.Helper()
	m, err := manifest.Load("../../examples/compose-app/deploykit.yaml")
	if err != nil {
		t.Fatal(err)
	}
	img, err := m.Image()
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "user.email=t@example.com", "-c", "user.name=T", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	for name, content := range map[string]string{"docker/Dockerfile.front": "FROM scratch\n", "docker/Dockerfile.api": "FROM scratch\n", "app.txt": "v1"} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run("add", ".")
	run("commit", "-q", "-m", "release me")
	return dir
}

func newRelease(t *testing.T, version string) (*Release, *transport.Fake) {
	t.Helper()
	docker := transport.NewFake("docker")
	return &Release{
		Cfg: cfg(t), Registry: "ghcr.io/acme", Version: version, Repo: gitRepo(t), WorkDir: t.TempDir(),
		Git: transport.NewLocal(), Docker: docker,
	}, docker
}

func TestReleaseBuildsTheRequestedStageOfAMultiStageDockerfile(t *testing.T) {
	r, docker := newRelease(t, "1.2.3")
	r.Cfg.Images[0].Target = "php"
	if err := (&runner.Runner{}).Do(t.Context(), r.Steps()...); err != nil {
		t.Fatal(err)
	}
	trace := docker.Trace()
	src := filepath.Join(r.WorkDir, "src")
	if want := "docker build -f " + src + "/docker/Dockerfile.front --target php -t ghcr.io/acme/frontend:1.2.3"; !strings.Contains(trace, want) {
		t.Errorf("trace lacks %q:\n%s", want, trace)
	}
	if strings.Contains(trace, "Dockerfile.api --target") {
		t.Errorf("an image without a target must not get one:\n%s", trace)
	}
}

func TestRefs(t *testing.T) {
	r, _ := newRelease(t, "1.2.3")
	refs, err := r.Refs("abc1234")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0].Name != "frontend" || strings.Join(refs[0].Tags, " ") != "ghcr.io/acme/frontend:1.2.3 ghcr.io/acme/frontend:sha-abc1234" {
		t.Errorf("refs = %+v", refs)
	}
}

func TestValidate(t *testing.T) {
	r, _ := newRelease(t, "1.2.3")
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"1.2", "v1.2.3", "latest", "1.2.3; rm", ""} {
		r.Version = bad
		if r.Validate() == nil {
			t.Errorf("version %q must be rejected", bad)
		}
	}
	r.Version, r.Registry = "1.2.3", "my-org"
	if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "docker.io") {
		t.Errorf("a bare namespace must be rejected with the explanation: %v", err)
	}
}

func TestReleaseBuildsFromAnExportOfTheTagAndPushesEveryTag(t *testing.T) {
	r, docker := newRelease(t, "1.2.3")
	// An uncommitted change must not ship.
	if err := os.WriteFile(filepath.Join(r.Repo, "app.txt"), []byte("v2-dirty"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&runner.Runner{}).Do(t.Context(), r.Steps()...); err != nil {
		t.Fatal(err)
	}
	// The tag was created on HEAD.
	if out, err := exec.CommandContext(t.Context(), "git", "-C", r.Repo, "tag", "-l", "v1.2.3").Output(); err != nil || strings.TrimSpace(string(out)) != "v1.2.3" {
		t.Fatalf("tag: %q, %v", out, err)
	}
	trace := docker.Trace()
	src := filepath.Join(r.WorkDir, "src")
	for _, want := range []string{
		"docker build -f " + src + "/docker/Dockerfile.front -t ghcr.io/acme/frontend:1.2.3 -t ghcr.io/acme/frontend:sha-",
		"docker build -f " + src + "/docker/Dockerfile.api -t ghcr.io/acme/backend:1.2.3",
		"docker push ghcr.io/acme/frontend:1.2.3",
		"docker push ghcr.io/acme/backend:1.2.3",
	} {
		if !strings.Contains(trace, want) {
			t.Errorf("trace lacks %q:\n%s", want, trace)
		}
	}
	if strings.Count(trace, "docker push") != 4 {
		t.Errorf("want 4 pushes (2 images x 2 tags):\n%s", trace)
	}
	if got, _ := os.ReadFile(filepath.Join(src, "app.txt")); string(got) != "v1" {
		t.Errorf("the build context holds %q: it must be the committed v1, not the dirty worktree", got)
	}
}

func TestReleaseUsesAnExistingTag(t *testing.T) {
	r, _ := newRelease(t, "2.0.0")
	if out, err := exec.CommandContext(t.Context(), "git", "-C", r.Repo, "tag", "v2.0.0").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	var lines []string
	rep := reporter(func(e runner.Event) {
		if e.Kind == runner.Output {
			lines = append(lines, e.Line)
		}
	})
	if err := (&runner.Runner{Reporter: rep}).Do(t.Context(), r.Steps()[:1]...); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "tag v2.0.0 exists") {
		t.Errorf("output = %v", lines)
	}
}

type reporter func(runner.Event)

func (f reporter) Handle(e runner.Event) { f(e) }

func TestReleaseRefusesToCreateATagWhenNotAllowed(t *testing.T) {
	r, docker := newRelease(t, "3.0.0")
	r.Cfg.Source.CreateTagIfMissing = false
	err := (&runner.Runner{}).Do(t.Context(), r.Steps()...)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("err = %v", err)
	}
	if len(docker.Calls()) != 0 {
		t.Error("nothing may be built without the tag")
	}
}

func TestGatesRunOnlyWhenAsked(t *testing.T) {
	has := func(steps []runner.Step, name string) bool {
		for _, s := range steps {
			if s.Name == name {
				return true
			}
		}
		return false
	}
	r, _ := newRelease(t, "1.0.0")
	if has(r.Steps(), "gates") {
		t.Error("gates are opt-in")
	}
	r.Gates = true
	if !has(r.Steps(), "gates") {
		t.Error("--gates must add the step")
	}
}

func TestLoginSendsTheTokenOnStdin(t *testing.T) {
	r, docker := newRelease(t, "1.0.0")
	r.Creds = Credentials{User: "bot", Token: "ghp_secrettoken"}
	steps := r.Steps()
	if err := (&runner.Runner{}).Do(t.Context(), steps[:2]...); err != nil { // tag, registry login
		t.Fatal(err)
	}
	call := docker.Calls()[0]
	if strings.Join(call.Spec.Argv, " ") != "docker login ghcr.io -u bot --password-stdin" || call.Stdin != "ghp_secrettoken" {
		t.Errorf("login = %v / %q", call.Spec.Argv, call.Stdin)
	}
	if strings.Contains(docker.Trace(), "ghp_secrettoken") {
		t.Error("the token must never be in the trace")
	}
}

func TestPresent(t *testing.T) {
	r, docker := newRelease(t, "1.2.3")
	if ok, err := r.Present(t.Context()); err != nil || !ok {
		t.Errorf("all manifests resolve: %v, %v", ok, err)
	}

	r2, docker2 := newRelease(t, "1.2.3")
	docker2.Fail("manifest inspect ghcr.io/acme/backend", 1, "no such manifest")
	docker2.Fail("image inspect", 1, "no such image")
	if ok, _ := r2.Present(t.Context()); ok {
		t.Error("one missing image means the release is not present")
	}

	// Local fallback: images built here, registry unreachable.
	r3, docker3 := newRelease(t, "1.2.3")
	docker3.Fail("manifest inspect", 1, "unauthorized")
	if ok, _ := r3.Present(t.Context()); !ok {
		t.Error("images present locally count as present")
	}
	_ = docker
}

func TestPresentRetriesAfterLogin(t *testing.T) {
	r, docker := newRelease(t, "1.2.3")
	r.Creds = Credentials{User: "bot", Token: "tok"}
	n := 0
	docker.OnFunc(func(s transport.Spec) bool {
		if len(s.Argv) > 2 && s.Argv[1] == "manifest" {
			n++
			return n <= 2 // the first probe round (2 images) is refused, the second succeeds
		}
		return false
	}, transport.Result{ExitCode: 1, Stderr: "unauthorized"})
	if ok, err := r.Present(t.Context()); err != nil || !ok {
		t.Fatalf("Present = %v, %v", ok, err)
	}
	if !strings.Contains(docker.Trace(), "docker login ghcr.io") {
		t.Errorf("a login must happen between the two probes:\n%s", docker.Trace())
	}
}

func TestIbexaDockerExampleBuildsFourImagesFromTwoStagesAndTwoSmallContexts(t *testing.T) {
	m, err := manifest.Load("../../examples/php-ibexa-docker/deploykit.yaml")
	if err != nil {
		t.Fatal(err)
	}
	img, err := m.Image()
	if err != nil {
		t.Fatal(err)
	}
	r := &Release{Cfg: img, Registry: "ghcr.io/acme", Version: "1.2.3"}
	refs, err := r.Refs("abc1234")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, b := range refs {
		got = append(got, b.Name+" "+b.Dockerfile+" "+b.Context+" "+b.Target)
	}
	want := "ibexa-php docker/Dockerfile . php|ibexa-web docker/Dockerfile . web|ibexa-varnish docker/varnish/Dockerfile docker/varnish |ibexa-solr docker/solr/Dockerfile docker/solr/conf "
	if strings.Join(got, "|") != want {
		t.Errorf("images = %q\nwant     %q", strings.Join(got, "|"), want)
	}
}
