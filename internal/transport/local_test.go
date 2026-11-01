package transport

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLocalScript(t *testing.T) {
	res, err := NewLocal().Run(t.Context(), Spec{Script: "echo \"it's $((1+2))\"\necho oops >&2\n"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "it's 3\n" || res.Stderr != "oops\n" {
		t.Errorf("result = %+v", res)
	}
}

func TestLocalArgvAndStdin(t *testing.T) {
	res, err := NewLocal().Run(t.Context(), Spec{Argv: []string{"cat"}, Stdin: strings.NewReader("secret-on-stdin")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "secret-on-stdin" {
		t.Errorf("stdout = %q", res.Stdout)
	}
}

func TestLocalArgvIsNotInterpretedByAShell(t *testing.T) {
	res, err := NewLocal().Run(t.Context(), Spec{Argv: []string{"echo", "$HOME", "a;b", "`id`"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "$HOME a;b `id`\n" {
		t.Errorf("stdout = %q", res.Stdout)
	}
}

func TestLocalEnvSecretsAndDir(t *testing.T) {
	dir := t.TempDir()
	res, err := NewLocal().Run(t.Context(), Spec{
		Script:  "printf '%s|%s|' \"$A\" \"$S\"; pwd",
		Env:     map[string]string{"A": "plain"},
		Secrets: map[string]string{"S": "hush"},
		Dir:     dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(dir)
	if got := strings.TrimSpace(res.Stdout); got != "plain|hush|"+real {
		t.Errorf("stdout = %q", got)
	}
}

func TestLocalExitError(t *testing.T) {
	res, err := NewLocal().Run(t.Context(), Spec{Script: "echo bad >&2; exit 7"})
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *ExitError", err)
	}
	if ee.Code != 7 || res.ExitCode != 7 || !strings.Contains(ee.Error(), "bad") {
		t.Errorf("exit error = %+v, result = %+v", ee, res)
	}
}

func TestLocalMissingProgram(t *testing.T) {
	_, err := NewLocal().Run(t.Context(), Spec{Argv: []string{"deploykit-no-such-program"}})
	var ee *ExitError
	if err == nil || errors.As(err, &ee) {
		t.Errorf("err = %v, want a start failure that is not an ExitError", err)
	}
}

func TestLocalStreamsOutput(t *testing.T) {
	var out bytes.Buffer
	_, err := NewLocal().Run(t.Context(), Spec{Script: "echo one; echo two >&2", Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "one\n") || !strings.Contains(got, "two\n") {
		t.Errorf("streamed output = %q", got)
	}
}

func TestLocalContextCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := NewLocal().Run(ctx, Spec{Argv: []string{"sleep", "10"}})
	if err == nil {
		t.Fatal("want an error from the cancelled command")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("cancel took %s", time.Since(start))
	}
}

func TestSpecValidation(t *testing.T) {
	l := NewLocal()
	for name, spec := range map[string]Spec{
		"empty":            {},
		"both":             {Script: "true", Argv: []string{"true"}},
		"stdin and script": {Script: "true", Stdin: strings.NewReader("x")},
	} {
		if _, err := l.Run(t.Context(), spec); err == nil {
			t.Errorf("%s: want a validation error", name)
		}
	}
}

func TestLocalSync(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync not available")
	}
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("A"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "skip.log"), []byte("L"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NewLocal().Sync(t.Context(), SyncSpec{Src: src + "/", Dst: dst + "/", Excludes: []string{"*.log"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "a.txt")); err != nil {
		t.Errorf("a.txt not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "skip.log")); err == nil {
		t.Error("skip.log should have been excluded")
	}
}

func inode(t *testing.T, p string) uint64 {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Sys().(*syscall.Stat_t).Ino
}

// The scenario of a deploy: the previous release is on disk, the new one comes from a fresh export whose files have
// other mtimes. With checksum comparison and equal mtimes, unchanged files are hard-linked and a changed file of the
// SAME size is still sent.
func TestSyncLinkDestWithChecksum(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync not available")
	}
	root := t.TempDir()
	prev, next, build := filepath.Join(root, "prev"), filepath.Join(root, "next"), filepath.Join(root, "build")
	for _, d := range []string{prev, build} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	write := func(dir, name, content string, mtime time.Time) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	epoch := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	write(prev, "same.bin", "unchanged content", epoch)
	write(prev, "version.json", `{"v":"v1"}`, epoch)
	// The new build: same bytes, different mtime for the unchanged file; a same-size change for the other.
	write(build, "same.bin", "unchanged content", epoch.Add(48*time.Hour))
	write(build, "version.json", `{"v":"v2"}`, epoch.Add(48*time.Hour))
	for _, f := range []string{"same.bin", "version.json"} {
		if err := os.Chtimes(filepath.Join(build, f), epoch, epoch); err != nil { // what NormalizeTimes does
			t.Fatal(err)
		}
	}

	err := NewLocal().Sync(t.Context(), SyncSpec{Src: build + "/", Dst: next + "/", Delete: true, LinkDest: prev, Checksum: true})
	if err != nil {
		t.Fatal(err)
	}
	if inode(t, filepath.Join(prev, "same.bin")) != inode(t, filepath.Join(next, "same.bin")) {
		t.Error("an unchanged file must be hard-linked to the previous release")
	}
	if inode(t, filepath.Join(prev, "version.json")) == inode(t, filepath.Join(next, "version.json")) {
		t.Error("a changed file must not be linked")
	}
	if got, _ := os.ReadFile(filepath.Join(next, "version.json")); string(got) != `{"v":"v2"}` {
		t.Errorf("version.json = %q: a same-size change must still be uploaded", got)
	}
	if got, _ := os.ReadFile(filepath.Join(prev, "version.json")); string(got) != `{"v":"v1"}` {
		t.Errorf("the previous release must stay intact, got %q", got)
	}
}

// Regression, found on a real server: a command that forwards its stdin (docker compose exec -T) in the middle of a
// script used to swallow the lines after it, because the shell reads the script from that same stdin. The remaining
// lines silently never ran, checks included.
func TestScriptLinesAfterAStdinReaderStillRun(t *testing.T) {
	res, err := NewLocal().Run(t.Context(), Spec{Script: "echo before\ncat >/dev/null\necho after\n"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "before\nafter\n" {
		t.Errorf("stdout = %q: the line after the stdin reader did not run", res.Stdout)
	}
}

func TestScriptStdinIsEmptyForTheCommandsItRuns(t *testing.T) {
	res, err := NewLocal().Run(t.Context(), Spec{Script: "cat\necho end\n"})
	if err != nil || res.Stdout != "end\n" {
		t.Errorf("stdout %q, err %v: commands in a script must see an empty stdin, not the script text", res.Stdout, err)
	}
}

func TestScriptWrapperPreservesTheScript(t *testing.T) {
	script := "set -e\nx='it'\\''s'\ncat <<'EOT'\nheredoc $x `no`\nEOT\nf() { echo \"in f: $1\"; }\nf arg\nexit 3\n"
	res, err := NewLocal().Run(t.Context(), Spec{Script: script})
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 3 {
		t.Fatalf("err = %v, want exit 3", err)
	}
	if res.Stdout != "heredoc $x `no`\nin f: arg\n" {
		t.Errorf("stdout = %q", res.Stdout)
	}
	// A script that itself contains the wrapper's delimiter still works.
	res, err = NewLocal().Run(t.Context(), Spec{Script: "echo " + guardDelimiter + "\n"})
	if err != nil || strings.TrimSpace(res.Stdout) != guardDelimiter {
		t.Errorf("delimiter in the script: %q, %v", res.Stdout, err)
	}
}
