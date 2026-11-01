package releasedir

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/masilia/deploykit/internal/manifest"
)

func tree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func listing(t *testing.T, root string) string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return strings.Join(out, ",")
}

// A Next.js standalone build: server.js, a partial public/, no .next/static.
func nextBuild(t *testing.T) string {
	dir := t.TempDir()
	tree(t, dir, map[string]string{
		".next/standalone/server.js":           "server",
		".next/standalone/node_modules/x/i.js": "dep",
		".next/standalone/public/partial.txt":  "partial",
		".next/static/chunks/a.css":            "css",
		"public/logo.png":                      "logo",
		"public/video/hero.mp4":                "video",
	})
	return dir
}

var nextSteps = []manifest.AssembleStep{
	{From: ".next/standalone/.", To: "."},
	{From: ".next/static", To: ".next/static"},
	{From: "public", To: "public", Replace: true},
}

func TestAssembleNextStandalone(t *testing.T) {
	build, release := nextBuild(t), filepath.Join(t.TempDir(), "release")
	if err := Assemble(build, release, nextSteps); err != nil {
		t.Fatal(err)
	}
	want := ".next/static/chunks/a.css,node_modules/x/i.js,public/logo.png,public/video/hero.mp4,server.js"
	if got := listing(t, release); got != want {
		t.Errorf("release = %s\nwant      %s", got, want)
	}
}

func TestAssembleWithoutReplaceNestsPublicLikeThePitfall(t *testing.T) {
	build, release := nextBuild(t), filepath.Join(t.TempDir(), "release")
	steps := []manifest.AssembleStep{{From: ".next/standalone/.", To: "."}, {From: "public", To: "public"}}
	if err := Assemble(build, release, steps); err != nil {
		t.Fatal(err)
	}
	// Without replace the partial file from the standalone folder survives next to the real files.
	if !strings.Contains(listing(t, release), "public/partial.txt") {
		t.Error("expected the stale partial public/ file to remain without replace")
	}
}

func TestAssembleExcludeAndSymlinksAndModes(t *testing.T) {
	build := t.TempDir()
	tree(t, build, map[string]string{"app/index.php": "<?php", "app/.git/HEAD": "ref", "app/var/cache/x": "c", "app/.env.local": "S=1", "app/run.sh": "#!/bin/sh"})
	if err := os.Chmod(filepath.Join(build, "app/run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("index.php", filepath.Join(build, "app/link.php")); err != nil {
		t.Fatal(err)
	}
	release := filepath.Join(t.TempDir(), "r")
	err := Assemble(build, release, []manifest.AssembleStep{{From: "app/.", To: ".", Exclude: []string{".git", "var/cache", ".env.local"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := listing(t, release); got != "index.php,link.php,run.sh" {
		t.Errorf("release = %s", got)
	}
	if target, err := os.Readlink(filepath.Join(release, "link.php")); err != nil || target != "index.php" {
		t.Errorf("symlink = %q, %v", target, err)
	}
	if st, _ := os.Stat(filepath.Join(release, "run.sh")); st.Mode().Perm() != 0o755 {
		t.Errorf("mode = %o, want 755", st.Mode().Perm())
	}
}

func TestAssembleRefusesPathsOutsideTheTrees(t *testing.T) {
	build := t.TempDir()
	tree(t, build, map[string]string{"a": "1"})
	for _, s := range []manifest.AssembleStep{
		{From: "../etc", To: "."},
		{From: "a", To: "../escape"},
		{From: "/etc/passwd", To: "x"},
		{From: "a", To: "/abs"},
	} {
		if err := Assemble(build, filepath.Join(t.TempDir(), "r"), []manifest.AssembleStep{s}); err == nil {
			t.Errorf("step %+v must be refused", s)
		}
	}
	if err := Assemble(build, filepath.Join(t.TempDir(), "r"), []manifest.AssembleStep{{From: "missing", To: "x"}}); err == nil {
		t.Error("a missing source must fail")
	}
}

func TestNormalizeTimes(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, map[string]string{"a.txt": "a", "sub/b.txt": "b"})
	old := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(dir, "a.txt"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := NormalizeTimes(dir); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"a.txt", "sub/b.txt"} {
		st, err := os.Stat(filepath.Join(dir, f))
		if err != nil || !st.ModTime().Equal(ReleaseEpoch) {
			t.Errorf("%s: mtime %v, %v", f, st.ModTime(), err)
		}
	}
}

// A leading slash anchors an exclusion at the root of the copied folder: the application's tests/ go, the tests/ of a library
// that needs its fixtures at run time (Ibexa reads vendor/ibexa/core/tests/…) stay.
func TestAnchoredExclusionKeepsNestedFoldersOfTheSameName(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	for _, f := range []string{"tests/a.php", "vendor/lib/tests/fixture.txt", "var/cache/x", "vendor/lib/var/keep", "public/var/img", ".env.local", "config/.env.local"} {
		p := filepath.Join(src, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	steps := []manifest.AssembleStep{{From: ".", To: ".", Exclude: []string{"/tests", "/var", "/public/var", ".env.local"}}}
	if err := Assemble(src, dst, steps); err != nil {
		t.Fatal(err)
	}
	for f, want := range map[string]bool{
		"tests/a.php": false, "vendor/lib/tests/fixture.txt": true, "var/cache/x": false, "vendor/lib/var/keep": true,
		"public/var/img": false, ".env.local": false, "config/.env.local": false,
	} {
		_, err := os.Stat(filepath.Join(dst, f))
		if (err == nil) != want {
			t.Errorf("%s: present = %v, want %v", f, err == nil, want)
		}
	}
}
