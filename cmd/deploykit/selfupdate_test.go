package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/masilia/deploykit/internal/selfupdate"
)

// ---- a fake GitHub ----------------------------------------------------------------------------------------------------------

type fakeGitHub struct {
	*httptest.Server
	lists     atomic.Int32 // requests for the release list
	downloads atomic.Int32 // requests for an archive or checksums.txt
}

// newFakeGitHub serves releases (tag names) for linux/amd64, each with a binary that answers `version`. delay slows the release list.
func newFakeGitHub(t *testing.T, delay time.Duration, badSum bool, tags ...string) *fakeGitHub {
	t.Helper()
	g := &fakeGitHub{}
	mux := http.NewServeMux()
	g.Server = httptest.NewServer(mux)
	t.Cleanup(g.Close)
	files := map[string][]byte{}
	var list []map[string]any
	for _, tag := range tags {
		v, err := selfupdate.Parse(tag)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		bin := []byte("#!/bin/sh\necho \"deploykit " + v.String() + " (commit abc1234, built today)\"\n")
		_ = tw.WriteHeader(&tar.Header{Name: "deploykit", Mode: 0o755, Size: int64(len(bin)), Typeflag: tar.TypeReg})
		_, _ = tw.Write(bin)
		_ = tw.Close()
		_ = gz.Close()
		name := selfupdate.ArchiveName(v, "linux", "amd64")
		sum := sha256.Sum256(buf.Bytes())
		hexsum := hex.EncodeToString(sum[:])
		if badSum {
			hexsum = strings.Repeat("0", 64)
		}
		prefix := "/dl/" + tag + "/"
		files[prefix+name] = buf.Bytes()
		files[prefix+"checksums.txt"] = []byte(hexsum + "  " + name + "\n")
		list = append(list, map[string]any{"tag_name": tag, "assets": []map[string]string{
			{"name": name, "browser_download_url": g.URL + prefix + name},
			{"name": "checksums.txt", "browser_download_url": g.URL + prefix + "checksums.txt"},
		}})
	}
	mux.HandleFunc("/repos/masilia/deploykit/releases", func(w http.ResponseWriter, _ *http.Request) {
		g.lists.Add(1)
		time.Sleep(delay)
		_ = json.NewEncoder(w).Encode(list)
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		g.downloads.Add(1)
		data, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	})
	return g
}

func (g *fakeGitHub) client() *selfupdate.Client {
	return &selfupdate.Client{API: g.URL, Repo: selfupdate.DefaultRepo, HTTP: g.Client(), UserAgent: "test", AllowHTTP: true}
}

// ---- running self-update ----------------------------------------------------------------------------------------------------

// suEnv is an app wired to a fake GitHub, a binary in a temp folder (with a dk link), and a version.
type suEnv struct {
	app    *app
	out    *bytes.Buffer
	errOut *bytes.Buffer
	dir    string
	exe    string // the real file
	link   string // dk -> deploykit, which is what is run
	gh     *fakeGitHub
}

// newSUEnv: current is the running version ("" for a build from a checkout).
func newSUEnv(t *testing.T, current string, gh *fakeGitHub) *suEnv {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	t.Setenv("CI", "")
	t.Setenv("DEPLOYKIT_NO_UPDATE_CHECK", "")
	dir := t.TempDir()
	exe := filepath.Join(dir, "deploykit")
	if err := os.WriteFile(exe, []byte("OLD BINARY"), 0o755); err != nil { //nolint:gosec // an executable
		t.Fatal(err)
	}
	link := filepath.Join(dir, "dk")
	if err := os.Symlink("deploykit", link); err != nil {
		t.Fatal(err)
	}
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	a := newApp(out)
	a.forceStyled = true
	a.releases = gh.client()
	a.exePath = func() (string, error) { return link, nil }
	a.currentVer = func() (string, bool) { return current, current != "" }
	a.goos, a.goarch = "linux", "amd64"
	cache := selfupdate.Cache{Path: filepath.Join(dir, "cache", "update.json")}
	a.updateCache = &cache
	return &suEnv{app: a, out: out, errOut: errOut, dir: dir, exe: exe, link: link, gh: gh}
}

func (e *suEnv) run(args ...string) error {
	e.out.Reset()
	e.errOut.Reset()
	root := newRootCmdTo(e.out, e.app)
	root.SetErr(e.errOut)
	root.SetArgs(args)
	return root.Execute()
}

func fileText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // a test file
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSelfUpdateCheck(t *testing.T) {
	gh := newFakeGitHub(t, 0, false, "v0.5.1", "v0.6.0")
	for name, c := range map[string]struct{ current, want string }{
		"newer exists":    {"0.5.1", "deploykit 0.6.0 is available (this is 0.5.1)"},
		"up to date":      {"0.6.0", "deploykit 0.6.0 is up to date"},
		"a build of mine": {"", "Latest release: 0.6.0"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newSUEnv(t, c.current, gh)
			if err := e.run("self-update", "--check"); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(e.out.String(), c.want) {
				t.Errorf("output lacks %q:\n%s", c.want, e.out)
			}
			if fileText(t, e.exe) != "OLD BINARY" || gh.downloads.Load() != 0 {
				t.Error("--check changes nothing and downloads nothing")
			}
		})
	}
}

func TestSelfUpdateInstallsThroughTheLink(t *testing.T) {
	gh := newFakeGitHub(t, 0, false, "v0.5.1", "v0.6.0", "v0.7.0-rc.1")
	e := newSUEnv(t, "0.5.1", gh)
	if err := e.run("self-update"); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	if got := fileText(t, e.exe); !strings.Contains(got, "deploykit 0.6.0") {
		t.Errorf("deploykit was not replaced by 0.6.0 (a pre-release is not offered): %q", got)
	}
	if fileText(t, e.exe+".old") != "OLD BINARY" {
		t.Error("the old binary is kept as deploykit.old")
	}
	if fi, err := os.Lstat(e.link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("dk must still be the link it was: %v", err)
	}
	for _, want := range []string{"Installing 0.6.0 over 0.5.1", "checked against checksums.txt", "deploykit.old", "next 'deploykit deploy'"} {
		if !strings.Contains(e.out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, e.out)
		}
	}
	if strings.Contains(e.out.String(), "separate copy") {
		t.Errorf("dk is a link, so there is no separate copy to warn about:\n%s", e.out)
	}
	// Run again: the installed version is the latest.
	e.app.currentVer = func() (string, bool) { return "0.6.0", true }
	if err := e.run("self-update"); err != nil || !strings.Contains(e.out.String(), "up to date") {
		t.Errorf("a second run must find nothing to do: %v\n%s", err, e.out)
	}
}

func TestSelfUpdateWithPreReleases(t *testing.T) {
	gh := newFakeGitHub(t, 0, false, "v0.6.0", "v0.7.0-rc.1")
	e := newSUEnv(t, "0.6.0", gh)
	if err := e.run("self-update", "--pre"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fileText(t, e.exe), "0.7.0-rc.1") {
		t.Errorf("--pre offers the release candidate: %q", fileText(t, e.exe))
	}
}

func TestSelfUpdateWarnsOfASeparateCopy(t *testing.T) {
	gh := newFakeGitHub(t, 0, false, "v0.6.0")
	e := newSUEnv(t, "0.5.1", gh)
	if err := os.Remove(e.link); err != nil {
		t.Fatal(err)
	}
	copyOfDK := filepath.Join(e.dir, "dk")
	if err := os.WriteFile(copyOfDK, []byte("OLD DK"), 0o755); err != nil { //nolint:gosec // an executable
		t.Fatal(err)
	}
	e.app.exePath = func() (string, error) { return e.exe, nil }
	if err := e.run("self-update"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), copyOfDK+" is a separate copy") || fileText(t, copyOfDK) != "OLD DK" {
		t.Errorf("a separate dk is reported and left alone:\n%s", e.out)
	}
}

func TestSelfUpdateLeavesABuildFromACheckoutAlone(t *testing.T) {
	gh := newFakeGitHub(t, 0, false, "v0.6.0")
	e := newSUEnv(t, "", gh)
	err := e.run("self-update")
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("a build from a checkout is not replaced without --force, got %v", err)
	}
	if fileText(t, e.exe) != "OLD BINARY" || gh.downloads.Load() != 0 {
		t.Error("nothing is downloaded or replaced")
	}
	if err := e.run("self-update", "--force"); err != nil {
		t.Fatalf("--force replaces it: %v", err)
	}
	if !strings.Contains(fileText(t, e.exe), "0.6.0") {
		t.Error("--force installs the release")
	}
}

func TestSelfUpdateVersionsOlderAndSame(t *testing.T) {
	gh := newFakeGitHub(t, 0, false, "v0.5.1", "v0.6.0")
	e := newSUEnv(t, "0.6.0", gh)
	// Asking for an older one is a downgrade: --force.
	if err := e.run("self-update", "--version", "0.5.1"); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("a downgrade needs --force, got %v", err)
	}
	if fileText(t, e.exe) != "OLD BINARY" {
		t.Error("nothing changed")
	}
	if err := e.run("self-update", "--version", "0.5.1", "--force"); err != nil || !strings.Contains(fileText(t, e.exe), "0.5.1") {
		t.Fatalf("--force goes back: %v", err)
	}
	// The same version is a reinstall: --force.
	e.app.currentVer = func() (string, bool) { return "0.6.0", true }
	before := gh.downloads.Load()
	if err := e.run("self-update", "--version", "0.6.0"); err != nil || gh.downloads.Load() != before {
		t.Errorf("the same version needs nothing (%v)", err)
	}
	if err := e.run("self-update", "--version", "0.6.0", "--force"); err != nil || gh.downloads.Load() == before {
		t.Errorf("--force reinstalls (%v)", err)
	}
	// A release candidate newer than the latest stable is not a reason to go back.
	e.app.currentVer = func() (string, bool) { return "0.7.0-rc.1", true }
	if err := e.run("self-update"); err != nil || !strings.Contains(e.out.String(), "newer than the latest release") {
		t.Errorf("no downgrade without being asked: %v\n%s", err, e.out)
	}
	if err := e.run("self-update", "--version", "9.9.9"); err == nil {
		t.Error("an unknown release is an error")
	}
}

func TestSelfUpdateDryRunChangesNothing(t *testing.T) {
	gh := newFakeGitHub(t, 0, false, "v0.6.0")
	e := newSUEnv(t, "0.5.1", gh)
	if err := e.run("--dry-run", "self-update"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "Dry run: would download deploykit_0.6.0_linux_amd64.tar.gz") {
		t.Errorf("the plan is shown:\n%s", e.out)
	}
	if fileText(t, e.exe) != "OLD BINARY" || gh.downloads.Load() != 0 {
		t.Error("a dry run downloads nothing and replaces nothing")
	}
}

func TestSelfUpdateFailureLeavesTheBinaryAlone(t *testing.T) {
	gh := newFakeGitHub(t, 0, true, "v0.6.0") // checksums.txt lists a wrong hash
	e := newSUEnv(t, "0.5.1", gh)
	if err := e.run("self-update"); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("a wrong checksum is an error, got %v", err)
	}
	if fileText(t, e.exe) != "OLD BINARY" {
		t.Error("the current binary must be untouched")
	}
	if _, err := os.Stat(e.exe + ".old"); err == nil {
		t.Error("no backup is made when nothing is replaced")
	}
}

func TestSelfUpdateRefusesGoRun(t *testing.T) {
	gh := newFakeGitHub(t, 0, false, "v0.6.0")
	e := newSUEnv(t, "0.5.1", gh)
	dir := filepath.Join(e.dir, "go-build123", "b001", "exe")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "deploykit")
	if err := os.WriteFile(p, []byte("x"), 0o755); err != nil { //nolint:gosec // an executable
		t.Fatal(err)
	}
	e.app.exePath = func() (string, error) { return p, nil }
	if err := e.run("self-update"); err == nil || !strings.Contains(err.Error(), "go run") {
		t.Errorf("go run has no installed binary to replace, got %v", err)
	}
}

// ---- the daily notice -------------------------------------------------------------------------------------------------------

// noopRun runs a command that does nothing, through the real hooks of the root command.
func (e *suEnv) noopRun(t *testing.T, args ...string) {
	t.Helper()
	e.out.Reset()
	e.errOut.Reset()
	root := newRootCmdTo(e.out, e.app)
	root.SetErr(e.errOut)
	root.AddCommand(&cobra.Command{Use: "noop", Run: func(*cobra.Command, []string) {}})
	root.SetArgs(append([]string{"noop"}, args...))
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
}

func (e *suEnv) remember(t *testing.T, s selfupdate.State) {
	t.Helper()
	if err := e.app.updateCache.Save(s); err != nil {
		t.Fatal(err)
	}
}

func TestNoticeIsToldOnceADayFromWhatIsRemembered(t *testing.T) {
	gh := newFakeGitHub(t, 0, false, "v0.6.0")
	e := newSUEnv(t, "0.5.1", gh)
	e.app.now = func() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) }
	e.remember(t, selfupdate.State{CheckedAt: e.app.now(), Latest: "0.6.0"}) // looked at an hour ago: not due
	e.noopRun(t)
	if got := e.errOut.String(); !strings.Contains(got, "deploykit 0.6.0 is available (this is 0.5.1)") || !strings.Contains(got, "DEPLOYKIT_NO_UPDATE_CHECK=1") {
		t.Fatalf("the notice goes to stderr:\n%q", got)
	}
	if e.out.Len() != 0 {
		t.Errorf("stdout is left alone: %q", e.out)
	}
	if gh.lists.Load() != 0 {
		t.Error("nothing is asked of the network while the last look is recent")
	}
	e.noopRun(t)
	if e.errOut.Len() != 0 {
		t.Errorf("the same news is not told twice in a day: %q", e.errOut)
	}
	e.app.now = func() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC).Add(25 * time.Hour) }
	e.remember(t, selfupdate.State{CheckedAt: e.app.now(), Latest: "0.6.0", NotifiedAt: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC), NotifiedFor: "0.6.0"})
	e.noopRun(t)
	if !strings.Contains(e.errOut.String(), "0.6.0 is available") {
		t.Errorf("it is told again after a day:\n%q", e.errOut)
	}
}

func TestNoticeLooksAtTheReleasesInTheBackground(t *testing.T) {
	gh := newFakeGitHub(t, 0, false, "v0.6.0")
	e := newSUEnv(t, "0.5.1", gh)
	e.app.checkWait = 5 * time.Second // the test must not depend on the speed of the machine
	e.noopRun(t)                      // nothing is remembered: the look is due
	if gh.lists.Load() != 1 {
		t.Fatalf("one look at the releases, got %d", gh.lists.Load())
	}
	if !strings.Contains(e.errOut.String(), "deploykit 0.6.0 is available") {
		t.Errorf("the answer is told at the end of the same run:\n%q", e.errOut)
	}
	if s := e.app.updateCache.Load(); s.Latest != "0.6.0" || s.CheckedAt.IsZero() {
		t.Errorf("the answer is remembered: %+v", s)
	}
	e.noopRun(t)
	if gh.lists.Load() != 1 {
		t.Errorf("a day passes between two looks, got %d", gh.lists.Load())
	}
}

func TestNoticeNeverSlowsACommandDown(t *testing.T) {
	gh := newFakeGitHub(t, 2*time.Second, false, "v0.6.0") // a release list that takes 2 seconds
	e := newSUEnv(t, "0.5.1", gh)
	e.app.checkWait = 50 * time.Millisecond
	start := time.Now()
	e.noopRun(t)
	if d := time.Since(start); d > time.Second {
		t.Errorf("the command waited %s for the look: it must give up after the wait", d)
	}
	if e.errOut.Len() != 0 {
		t.Errorf("an answer that did not come says nothing: %q", e.errOut)
	}
}

func TestNoticeSurvivesAnUnreachableServer(t *testing.T) {
	gh := newFakeGitHub(t, 0, false, "v0.6.0")
	e := newSUEnv(t, "0.5.1", gh)
	gh.Close()
	e.app.checkWait = 5 * time.Second
	e.noopRun(t) // t.Fatal inside noopRun if the command failed
	if e.errOut.Len() != 0 {
		t.Errorf("an unreachable server is no news: %q", e.errOut)
	}
	if s := e.app.updateCache.Load(); s.CheckedAt.IsZero() || s.Due(time.Now().Add(time.Hour)) {
		t.Errorf("the failed look is remembered as a look, so it is not repeated at once: %+v", s)
	}
}

func TestNoticeIsOffWhereItShouldBe(t *testing.T) {
	stale := selfupdate.State{CheckedAt: time.Now(), Latest: "0.9.0"}
	cases := map[string]func(*testing.T, *suEnv){
		"a build from a checkout": func(_ *testing.T, e *suEnv) { e.app.currentVer = func() (string, bool) { return "", false } },
		"the opt-out":             func(t *testing.T, _ *suEnv) { t.Setenv("DEPLOYKIT_NO_UPDATE_CHECK", "1") },
		"CI":                      func(t *testing.T, _ *suEnv) { t.Setenv("CI", "true") },
		"not at a terminal":       func(_ *testing.T, e *suEnv) { e.app.forceStyled = false },
		"--plain":                 func(_ *testing.T, e *suEnv) { e.app.forceStyled, e.app.plain = false, true },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			gh := newFakeGitHub(t, 0, false, "v0.9.0")
			e := newSUEnv(t, "0.5.1", gh)
			setup(t, e)
			e.remember(t, stale)
			e.noopRun(t)
			if e.errOut.Len() != 0 || gh.lists.Load() != 0 {
				t.Errorf("no notice and no look expected: %q (%d requests)", e.errOut, gh.lists.Load())
			}
			// A due look must not happen either.
			e.remember(t, selfupdate.State{})
			e.noopRun(t)
			if gh.lists.Load() != 0 {
				t.Errorf("no look at the releases expected (%d requests)", gh.lists.Load())
			}
		})
	}
}

func TestNoticeIsNotForTheCommandsOfCronAndCompletion(t *testing.T) {
	gh := newFakeGitHub(t, 0, false, "v0.9.0")
	e := newSUEnv(t, "0.5.1", gh)
	root := newRootCmdTo(e.out, e.app)
	server := &cobra.Command{Use: "server"}
	monitor := &cobra.Command{Use: "monitor"}
	server.AddCommand(monitor)
	root.AddCommand(server)
	for name, c := range map[string]*cobra.Command{
		"server monitor":   monitor,
		"server":           server,
		"self-update":      {Use: "self-update"},
		"completion":       {Use: "completion"},
		"__complete":       {Use: "__complete"},
		"__completeNoDesc": {Use: "__completeNoDesc"},
		"version":          {Use: "version"},
	} {
		if e.app.updateNoticeApplies(c) {
			t.Errorf("%s must not look at the releases or mention one", name)
		}
	}
	if !e.app.updateNoticeApplies(&cobra.Command{Use: "status"}) {
		t.Error("status is a command a person runs: the notice applies")
	}
}

func TestRootHelpMentionsANewerRelease(t *testing.T) {
	gh := newFakeGitHub(t, 0, false, "v0.6.0")
	e := newSUEnv(t, "0.5.1", gh)
	e.app.helpCols = 100
	e.remember(t, selfupdate.State{CheckedAt: time.Now(), Latest: "0.6.0"})
	if err := e.run("--help"); err != nil {
		t.Fatal(err)
	}
	help, notice := e.out.String(), "deploykit 0.6.0 is available"
	if !strings.Contains(help, "Usage:") || !strings.Contains(help, notice) || strings.Index(help, notice) < strings.Index(help, "Run 'deploykit <command> --help'") {
		t.Errorf("the notice comes after the help:\n%s", help)
	}
	if gh.lists.Load() != 0 {
		t.Error("the help asks the network for nothing")
	}
	// A subcommand's help does not carry it.
	e.remember(t, selfupdate.State{CheckedAt: time.Now(), Latest: "0.6.0"})
	if err := e.run("deploy", "--help"); err != nil || strings.Contains(e.out.String(), notice) {
		t.Errorf("only the root help mentions it (%v)", err)
	}
}

func TestSelfUpdateIsInAHelpGroup(t *testing.T) {
	out := fmt.Sprint(helpGroups)
	if !strings.Contains(out, "self-update") {
		t.Error("self-update belongs in a help group")
	}
}

// The default wait is what users get: a command that ends at once gives the look a fraction of a second, not as long as it takes.
func TestNoticeDefaultWaitIsShort(t *testing.T) {
	gh := newFakeGitHub(t, 3*time.Second, false, "v0.6.0")
	e := newSUEnv(t, "0.5.1", gh)
	if e.app.checkWait != 0 {
		t.Fatal("this test uses the default wait")
	}
	start := time.Now()
	e.noopRun(t)
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Errorf("the command waited %s for a slow server: the default wait must stay under a second", d)
	}
}
