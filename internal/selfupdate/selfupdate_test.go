package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
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
)

func mustParse(t *testing.T, s string) Version {
	t.Helper()
	v, err := Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestParseAndCompare(t *testing.T) {
	older := [][2]string{
		{"0.5.1", "0.6.0"},
		{"0.9.0", "0.10.0"},
		{"1.2.3", "1.2.4"},
		{"1.9.9", "2.0.0"},
		{"0.6.0-rc.1", "0.6.0"},
		{"0.6.0-rc.1", "0.6.0-rc.2"},
		{"0.6.0-rc.2", "0.6.0-rc.10"},
		{"0.6.0-alpha", "0.6.0-beta"},
		{"0.6.0-1", "0.6.0-alpha"},
		{"0.6.0-rc", "0.6.0-rc.1"},
		{"0.5.1", "v0.5.2"},
	}
	for _, p := range older {
		a, b := mustParse(t, p[0]), mustParse(t, p[1])
		if a.Compare(b) != -1 || b.Compare(a) != 1 {
			t.Errorf("%s must be older than %s", p[0], p[1])
		}
	}
	if mustParse(t, "v0.5.1").Compare(mustParse(t, "0.5.1+build.7")) != 0 {
		t.Error("the v and the build metadata do not count")
	}
	if got := mustParse(t, "v1.2.3-rc.1").String(); got != "1.2.3-rc.1" {
		t.Errorf("String = %q", got)
	}
	for _, bad := range []string{"", "dev", "1.2", "1.2.3.4", "a.b.c", "01.2.3", "1.2.3-", "1.2.3-rc..1", "-1.2.3"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q is not a version", bad)
		}
	}
}

// ---- a fake release server --------------------------------------------------------------------------------------------

const goos, goarch = "linux", "amd64"

// fakeBinary is a shell script that answers `version` the way deploykit does.
func fakeBinary(version string) []byte {
	return []byte("#!/bin/sh\necho \"deploykit " + version + " (commit abc1234, built today)\"\n")
}

func tarGz(t *testing.T, files map[string][]byte, extra ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range extra {
		h := h
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type fakeRelease struct {
	tag        string
	draft, pre bool
	archive    []byte // nil: built from fakeBinary(tag)
	badSum     bool
	noSums     bool
}

type fakeServer struct {
	*httptest.Server
	hits     atomic.Int32 // requests to the release list
	download atomic.Int32 // requests to an asset
}

func newFakeServer(t *testing.T, releases ...fakeRelease) *fakeServer {
	t.Helper()
	fs := &fakeServer{}
	mux := http.NewServeMux()
	fs.Server = httptest.NewServer(mux)
	t.Cleanup(fs.Close)
	assets := map[string][]byte{}
	var list []map[string]any
	for _, r := range releases {
		v, perr := Parse(r.tag)
		if perr != nil { // a tag that is not a version: listed as it is, with nothing to download
			list = append(list, map[string]any{"tag_name": r.tag, "assets": []map[string]string{}})
			continue
		}
		archive := r.archive
		if archive == nil {
			archive = tarGz(t, map[string][]byte{"deploykit": fakeBinary(v.String()), "README.md": []byte("readme")})
		}
		name := ArchiveName(v, goos, goarch)
		sum := sha256.Sum256(archive)
		hexsum := hex.EncodeToString(sum[:])
		if r.badSum {
			hexsum = strings.Repeat("0", 64)
		}
		prefix := "/dl/" + r.tag + "/"
		assets[prefix+name] = archive
		as := []map[string]string{{"name": name, "browser_download_url": fs.URL + prefix + name}}
		if !r.noSums {
			assets[prefix+"checksums.txt"] = []byte(fmt.Sprintf("%s  %s\n%s  other_file.tar.gz\n", hexsum, name, strings.Repeat("a", 64)))
			as = append(as, map[string]string{"name": "checksums.txt", "browser_download_url": fs.URL + prefix + "checksums.txt"})
		}
		list = append(list, map[string]any{"tag_name": r.tag, "draft": r.draft, "prerelease": r.pre, "assets": as})
	}
	mux.HandleFunc("/repos/masilia/deploykit/releases", func(w http.ResponseWriter, _ *http.Request) {
		fs.hits.Add(1)
		_ = json.NewEncoder(w).Encode(list)
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		fs.download.Add(1)
		data, ok := assets[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	})
	return fs
}

func (fs *fakeServer) client() *Client {
	return &Client{API: fs.URL, Repo: DefaultRepo, HTTP: fs.Client(), UserAgent: "test", AllowHTTP: true}
}

// ---- finding releases ------------------------------------------------------------------------------------------------------

func TestLatestIsTheNewestStableByVersionNumber(t *testing.T) {
	fs := newFakeServer(t,
		fakeRelease{tag: "v0.4.0"},
		fakeRelease{tag: "v0.10.0"}, // newest: it is not the last of the answer, and 0.10 > 0.9
		fakeRelease{tag: "v0.9.0"},
		fakeRelease{tag: "v0.11.0", draft: true},
		fakeRelease{tag: "v0.12.0-rc.1", pre: true},
		fakeRelease{tag: "not-a-version"},
	)
	got, err := fs.client().Latest(t.Context(), false)
	if err != nil || got.Version.String() != "0.10.0" {
		t.Fatalf("Latest = %v, %v; want 0.10.0 (a draft and a pre-release are not offered)", got.Version, err)
	}
	pre, err := fs.client().Latest(t.Context(), true)
	if err != nil || pre.Version.String() != "0.12.0-rc.1" {
		t.Fatalf("Latest with pre-releases = %v, %v; want 0.12.0-rc.1", pre.Version, err)
	}
}

func TestLatestWithNothingPublished(t *testing.T) {
	fs := newFakeServer(t, fakeRelease{tag: "v0.1.0", draft: true})
	if _, err := fs.client().Latest(t.Context(), false); err == nil {
		t.Error("only a draft exists: there is no release to install")
	}
}

func TestFindOneVersion(t *testing.T) {
	fs := newFakeServer(t, fakeRelease{tag: "v0.5.1"}, fakeRelease{tag: "v0.6.0"})
	for _, v := range []string{"0.5.1", "v0.5.1"} {
		if r, err := fs.client().Find(t.Context(), v); err != nil || r.Version.String() != "0.5.1" {
			t.Errorf("Find(%s) = %v, %v", v, r.Version, err)
		}
	}
	if _, err := fs.client().Find(t.Context(), "9.9.9"); err == nil {
		t.Error("an unknown version is an error")
	}
	if _, err := fs.client().Find(t.Context(), "latest"); err == nil {
		t.Error("a word is not a version")
	}
}

func TestOnlyHTTPSIsAcceptedInProduction(t *testing.T) {
	fs := newFakeServer(t, fakeRelease{tag: "v0.6.0"})
	c := fs.client()
	c.AllowHTTP = false
	if _, err := c.Latest(t.Context(), false); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("a plain-http API must be refused, got %v", err)
	}
}

func TestAnswersAreCapped(t *testing.T) {
	old := maxJSON
	maxJSON = 10
	defer func() { maxJSON = old }()
	fs := newFakeServer(t, fakeRelease{tag: "v0.6.0"})
	if _, err := fs.client().Latest(t.Context(), false); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Errorf("an oversized answer must be refused, got %v", err)
	}
}

// ---- installing -------------------------------------------------------------------------------------------------------------

func needSh(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
}

func installTarget(t *testing.T) (dir, exe string) {
	t.Helper()
	dir = t.TempDir()
	exe = filepath.Join(dir, "deploykit")
	if err := os.WriteFile(exe, []byte("OLD BINARY"), 0o755); err != nil { //nolint:gosec // an executable
		t.Fatal(err)
	}
	return dir, exe
}

func opts(exe string) InstallOptions { return InstallOptions{Exe: exe, GOOS: goos, GOARCH: goarch} }

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // a test file
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".deploykit-update-") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestInstallReplacesTheBinaryAndKeepsTheOldOne(t *testing.T) {
	needSh(t)
	fs := newFakeServer(t, fakeRelease{tag: "v0.6.0"})
	dir, exe := installTarget(t)
	rel, err := fs.client().Latest(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := fs.client().Install(t.Context(), rel, opts(exe))
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, exe); !strings.Contains(got, "deploykit 0.6.0") {
		t.Errorf("the binary was not replaced: %q", got)
	}
	if backup != exe+".old" || read(t, backup) != "OLD BINARY" {
		t.Errorf("the old binary must be kept as %s.old, got %q", exe, backup)
	}
	if st, err := os.Stat(exe); err != nil || st.Mode().Perm() != 0o755 {
		t.Errorf("the new binary must be executable: %v, %v", st, err)
	}
	if out, err := exec.CommandContext(t.Context(), exe, "version").CombinedOutput(); err != nil || !strings.Contains(string(out), "0.6.0") { //nolint:gosec // the file this test installed
		t.Errorf("the installed binary does not run: %v %s", err, out)
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Errorf("temporary files left behind: %v", l)
	}
}

func TestInstallRefusesAWrongChecksum(t *testing.T) {
	needSh(t)
	fs := newFakeServer(t, fakeRelease{tag: "v0.6.0", badSum: true})
	dir, exe := installTarget(t)
	rel, _ := fs.client().Latest(t.Context(), false)
	if _, err := fs.client().Install(t.Context(), rel, opts(exe)); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("a wrong checksum must stop the install, got %v", err)
	}
	if read(t, exe) != "OLD BINARY" {
		t.Error("the current binary must be untouched")
	}
	if _, err := os.Stat(exe + ".old"); err == nil {
		t.Error("no backup is made when nothing is replaced")
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Errorf("temporary files left behind: %v", l)
	}
}

func TestInstallRefusesWhatCannotBeChecked(t *testing.T) {
	fs := newFakeServer(t, fakeRelease{tag: "v0.6.0", noSums: true})
	_, exe := installTarget(t)
	rel, _ := fs.client().Latest(t.Context(), false)
	if _, err := fs.client().Install(t.Context(), rel, opts(exe)); err == nil || !strings.Contains(err.Error(), "checksums.txt") {
		t.Fatalf("a release without checksums.txt is refused, got %v", err)
	}
	if read(t, exe) != "OLD BINARY" {
		t.Error("the current binary must be untouched")
	}
}

func TestInstallNeedsAnArchiveForThePlatform(t *testing.T) {
	fs := newFakeServer(t, fakeRelease{tag: "v0.6.0"})
	_, exe := installTarget(t)
	rel, _ := fs.client().Latest(t.Context(), false)
	o := opts(exe)
	o.GOOS = "plan9"
	if _, err := fs.client().Install(t.Context(), rel, o); err == nil || !strings.Contains(err.Error(), "plan9/amd64") {
		t.Errorf("an unknown platform must be named in the error, got %v", err)
	}
	if n := fs.download.Load(); n != 0 {
		t.Errorf("nothing is downloaded when there is no archive for the platform (%d requests)", n)
	}
}

func TestInstallOnlyTakesTheBinaryAtTheTopOfTheArchive(t *testing.T) {
	needSh(t)
	cases := map[string]struct {
		archive []byte
		wantErr string
	}{
		"binary in a folder": {tarGz(t, map[string][]byte{"sub/deploykit": fakeBinary("0.6.0")}), "no deploykit binary"},
		"path traversal":     {tarGz(t, map[string][]byte{"../deploykit": fakeBinary("0.6.0")}), "no deploykit binary"},
		"absolute path":      {tarGz(t, map[string][]byte{"/etc/deploykit": fakeBinary("0.6.0")}), "no deploykit binary"},
		"only a symlink":     {tarGz(t, nil, tar.Header{Name: "deploykit", Typeflag: tar.TypeSymlink, Linkname: "/bin/sh", Mode: 0o755}), "no deploykit binary"},
		"only other files":   {tarGz(t, map[string][]byte{"README.md": []byte("x"), "dk": []byte("x")}), "no deploykit binary"},
		"an empty binary":    {tarGz(t, map[string][]byte{"deploykit": {}}), "unexpected size"},
		"not an archive":     {[]byte("this is not a gzip file"), "unpack"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fs := newFakeServer(t, fakeRelease{tag: "v0.6.0", archive: c.archive})
			dir, exe := installTarget(t)
			rel, _ := fs.client().Latest(t.Context(), false)
			_, err := fs.client().Install(t.Context(), rel, opts(exe))
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, c.wantErr)
			}
			if read(t, exe) != "OLD BINARY" {
				t.Error("the current binary must be untouched")
			}
			if l := leftovers(t, dir); len(l) != 0 {
				t.Errorf("temporary files left behind: %v", l)
			}
			if _, err := os.Stat(filepath.Join(dir, "..", "deploykit")); err == nil && name == "path traversal" {
				t.Error("an entry must never be written outside the folder")
			}
		})
	}
}

func TestInstallKeepsTheOldBinaryWhenTheNewOneDoesNotRun(t *testing.T) {
	needSh(t)
	// The archive's binary says 0.5.0 although the release is 0.6.0: a wrong build, or a wrong platform.
	archive := tarGz(t, map[string][]byte{"deploykit": fakeBinary("0.5.0")})
	fs := newFakeServer(t, fakeRelease{tag: "v0.6.0", archive: archive})
	dir, exe := installTarget(t)
	rel, _ := fs.client().Latest(t.Context(), false)
	if _, err := fs.client().Install(t.Context(), rel, opts(exe)); err == nil || !strings.Contains(err.Error(), "untouched") {
		t.Fatalf("a binary that does not report the release's version must be refused, got %v", err)
	}
	if read(t, exe) != "OLD BINARY" {
		t.Error("the current binary must be untouched")
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Errorf("temporary files left behind: %v", l)
	}

	// And one that does not run at all.
	bad := tarGz(t, map[string][]byte{"deploykit": []byte("not an executable")})
	fs = newFakeServer(t, fakeRelease{tag: "v0.6.0", archive: bad})
	rel, _ = fs.client().Latest(t.Context(), false)
	if _, err := fs.client().Install(t.Context(), rel, opts(exe)); err == nil {
		t.Fatal("a file that does not run must be refused")
	}
	if read(t, exe) != "OLD BINARY" {
		t.Error("the current binary must be untouched")
	}
}

func TestInstallInAFolderThatCannotBeWritten(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	fs := newFakeServer(t, fakeRelease{tag: "v0.6.0"})
	dir, exe := installTarget(t)
	if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // the point of the test
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }() //nolint:gosec // let the temp dir be removed
	rel, _ := fs.client().Latest(t.Context(), false)
	_, err := fs.client().Install(t.Context(), rel, opts(exe))
	if err == nil || !strings.Contains(err.Error(), dir) {
		t.Fatalf("the error must name the folder, got %v", err)
	}
	if n := fs.download.Load(); n != 0 {
		t.Errorf("the folder is checked before anything is downloaded (%d requests)", n)
	}
}

func TestReplaceUndoesTheBackupWhenTheSecondRenameFails(t *testing.T) {
	dir, exe := installTarget(t)
	_, err := replace(exe, filepath.Join(dir, "does-not-exist"))
	if err == nil || !strings.Contains(err.Error(), "back in place") {
		t.Fatalf("err = %v", err)
	}
	if read(t, exe) != "OLD BINARY" {
		t.Error("the old binary must be back in place")
	}
}

func TestChecksumOf(t *testing.T) {
	sums := []byte(strings.Repeat("a", 64) + "  one.tar.gz\n" + strings.ToUpper(strings.Repeat("b", 64)) + " *two.tar.gz\nnot a line\nshort  three.tar.gz\n")
	if got, err := checksumOf(sums, "one.tar.gz"); err != nil || got != strings.Repeat("a", 64) {
		t.Errorf("one = %q, %v", got, err)
	}
	if got, err := checksumOf(sums, "two.tar.gz"); err != nil || got != strings.Repeat("b", 64) {
		t.Errorf("two (upper case, binary marker) = %q, %v", got, err)
	}
	if _, err := checksumOf(sums, "three.tar.gz"); err == nil {
		t.Error("a short checksum is malformed")
	}
	if _, err := checksumOf(sums, "four.tar.gz"); err == nil {
		t.Error("a file that is not listed has no checksum")
	}
}

// ---- remembering and telling -----------------------------------------------------------------------------------------------

func TestCacheRoundTripAndBadFiles(t *testing.T) {
	c := Cache{Path: filepath.Join(t.TempDir(), "deep", "update.json")}
	if s := c.Load(); !s.CheckedAt.IsZero() || s.Latest != "" {
		t.Errorf("a missing file is an empty state: %+v", s)
	}
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	if err := c.Save(State{CheckedAt: now, Latest: "0.6.0"}); err != nil {
		t.Fatal(err)
	}
	if s := c.Load(); !s.CheckedAt.Equal(now) || s.Latest != "0.6.0" {
		t.Errorf("round trip lost data: %+v", s)
	}
	if st, err := os.Stat(c.Path); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("the state file is private: %v, %v", st, err)
	}
	if err := os.WriteFile(c.Path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s := c.Load(); !s.CheckedAt.IsZero() {
		t.Errorf("a corrupt file is an empty state: %+v", s)
	}
}

func TestDue(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	for name, c := range map[string]struct {
		checked time.Time
		want    bool
	}{
		"never":                  {time.Time{}, true},
		"an hour ago":            {now.Add(-time.Hour), false},
		"just under a day":       {now.Add(-23*time.Hour - 59*time.Minute), false},
		"a day ago":              {now.Add(-24 * time.Hour), true},
		"a clock that went back": {now.Add(time.Hour), true},
	} {
		if got := (State{CheckedAt: c.checked}).Due(now); got != c.want {
			t.Errorf("%s: Due = %v, want %v", name, got, c.want)
		}
	}
}

func TestNoticeIsGivenOnceADay(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	s := State{Latest: "0.6.0"}
	msg, ok := s.Notice("0.5.1", now)
	if !ok || !strings.Contains(msg, "0.6.0") || !strings.Contains(msg, "0.5.1") || !strings.Contains(msg, "self-update") {
		t.Fatalf("notice = %q, %v", msg, ok)
	}
	if _, ok := s.Notice("0.5.1", now.Add(time.Hour)); ok {
		t.Error("the same version is not mentioned twice in a day")
	}
	if _, ok := s.Notice("0.5.1", now.Add(25*time.Hour)); !ok {
		t.Error("it is mentioned again after a day")
	}
	s.Latest = "0.7.0"
	if _, ok := s.Notice("0.5.1", now.Add(26*time.Hour+time.Minute)); !ok {
		t.Error("a newer version is a new piece of news")
	}
	for _, cur := range []string{"0.7.0", "0.8.0", "not a version"} {
		fresh := State{Latest: "0.7.0"}
		if _, ok := fresh.Notice(cur, now); ok {
			t.Errorf("nothing to say when current is %q and latest 0.7.0", cur)
		}
	}
	if _, ok := (&State{}).Notice("0.5.1", now); ok {
		t.Error("an empty state says nothing")
	}
}

func TestRefreshRemembersTheLatestAndTheFailures(t *testing.T) {
	fs := newFakeServer(t, fakeRelease{tag: "v0.6.0"}, fakeRelease{tag: "v0.7.0-rc.1", pre: true})
	cache := Cache{Path: filepath.Join(t.TempDir(), "update.json")}
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	if err := Refresh(context.Background(), fs.client(), cache, now); err != nil {
		t.Fatal(err)
	}
	if s := cache.Load(); s.Latest != "0.6.0" || !s.CheckedAt.Equal(now) {
		t.Errorf("state = %+v (a pre-release is not the latest)", s)
	}

	// The server goes away: the previous answer stays, and the look is dated so that it is not repeated at once.
	fs.Close()
	later := now.Add(48 * time.Hour)
	if err := Refresh(context.Background(), fs.client(), cache, later); err == nil {
		t.Error("an unreachable server is an error for the caller")
	}
	if s := cache.Load(); s.Latest != "0.6.0" || !s.CheckedAt.Equal(later) || s.Due(later.Add(time.Hour)) {
		t.Errorf("state after a failed look = %+v", s)
	}
}
