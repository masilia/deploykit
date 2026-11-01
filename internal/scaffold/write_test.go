package scaffold

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sample() []File {
	return []File{
		{Path: "deploykit.yaml", Content: []byte("version: 1\n"), Mode: 0o644},
		{Path: "targets/prod.env", Content: []byte("HOST=h\n"), Mode: 0o600},
	}
}

func TestScalarIsBareOnlyWhenUnambiguous(t *testing.T) {
	for in, want := range map[string]string{
		"api": "api", "docker/Dockerfile.api": "docker/Dockerfile.api", "/": "/", "22": `"22"`, "true": `"true"`, "No": `"No"`,
		"a b": `"a b"`, "": `""`, "x: y": `"x: y"`, "-lead": `"-lead"`, "npm run lint": `"npm run lint"`,
	} {
		if got := scalar(in); got != want {
			t.Errorf("scalar(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestApplyCreatesThenReportsUnchanged(t *testing.T) {
	dir := t.TempDir()
	p, err := NewPlan(dir, sample(), nil)
	if err != nil {
		t.Fatal(err)
	}
	done, err := p.Apply(ConflictStop)
	if err != nil || len(done) != 2 || done[0].Action != "created" {
		t.Fatalf("first apply: %v, %v", done, err)
	}
	if info, _ := os.Stat(filepath.Join(dir, "targets/prod.env")); info.Mode().Perm() != 0o600 {
		t.Errorf("the target file mode is %v", info.Mode().Perm())
	}
	p, _ = NewPlan(dir, sample(), nil)
	if done, err = p.Apply(ConflictStop); err != nil || done[0].Action != "unchanged" || done[1].Action != "unchanged" {
		t.Errorf("second apply: %v, %v", done, err)
	}
}

func TestAnExistingFileWithOtherContentIsNeverOverwrittenSilently(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "deploykit.yaml"), []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, _ := NewPlan(dir, sample(), nil)
	_, err := p.Apply(ConflictStop)
	var conflict *ErrConflict
	if !errors.As(err, &conflict) || conflict.Paths[0] != "deploykit.yaml" {
		t.Fatalf("err = %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "targets/prod.env")); serr == nil {
		t.Error("with a conflict nothing at all may be written, not even the files that are free")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "deploykit.yaml")); string(got) != "mine\n" {
		t.Errorf("the file was touched: %q", got)
	}

	if _, err = p.Apply(ConflictSideBySide); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "deploykit.yaml.new")); string(got) != "version: 1\n" {
		t.Errorf(".new = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "deploykit.yaml")); string(got) != "mine\n" {
		t.Errorf("side by side must leave the original alone: %q", got)
	}
}

func TestOverwriteKeepsTheOldFileAsBak(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deploykit.yaml")
	for i, old := range []string{"one\n", "two\n"} {
		if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
			t.Fatal(err)
		}
		p, _ := NewPlan(dir, sample(), nil)
		if _, err := p.Apply(ConflictOverwrite); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
	}
	first, _ := os.ReadFile(path + ".bak")
	second, _ := os.ReadFile(path + ".bak.1")
	if string(first) != "one\n" || string(second) != "two\n" {
		t.Errorf("backups: %q, %q", first, second)
	}
}

func TestGitignoreIsCompletedOnlyInARepository(t *testing.T) {
	plain := t.TempDir()
	if p, _ := NewPlan(plain, sample(), []string{"deploy.env"}); len(p.Gitignore) != 0 {
		t.Errorf("no .gitignore is invented outside a git repository: %v", p.Gitignore)
	}

	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("node_modules\n/deploy.env"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, _ := NewPlan(repo, sample(), []string{"deploy.env", ".deploykit/"})
	if len(p.Gitignore) != 1 || p.Gitignore[0] != ".deploykit/" {
		t.Fatalf("only the missing line is proposed (a leading slash counts): %v", p.Gitignore)
	}
	if _, err := p.Apply(ConflictStop); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(repo, ".gitignore"))
	if !strings.Contains(string(got), "node_modules\n/deploy.env\n\n# deploykit\n.deploykit/\n") {
		t.Errorf(".gitignore = %q", got)
	}
	if p, _ = NewPlan(repo, sample(), []string{"deploy.env", ".deploykit/"}); len(p.Gitignore) != 0 {
		t.Errorf("a second run proposes nothing: %v", p.Gitignore)
	}
}

func TestPathsCannotLeaveTheProject(t *testing.T) {
	for _, bad := range []string{"../escape.yaml", "/etc/passwd", "a/../../b"} {
		if _, err := NewPlan(t.TempDir(), []File{{Path: bad, Content: []byte("x"), Mode: 0o644}}, nil); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}
