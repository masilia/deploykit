package envfile

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run the original shell functions from the legacy scripts and require the Go port to agree with them.

// refDir holds the legacy scripts (lib.sh, env-set.sh); DEPLOYKIT_REFERENCE_DIR points at them, the tests skip without.
var refDir = os.Getenv("DEPLOYKIT_REFERENCE_DIR")

func requireShell(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	if _, err := os.Stat(filepath.Join(refDir, "lib.sh")); err != nil {
		t.Skipf("reference scripts not found: %v", err)
	}
}

func shellFileGet(t *testing.T, file, key, def string) string {
	t.Helper()
	lib, err := filepath.Abs(filepath.Join(refDir, "lib.sh"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(t.Context(), "sh", "-c", `. "$1"; file_get "$2" "$3" "$4"`, "sh", lib, file, key, def).Output()
	if err != nil {
		t.Fatalf("file_get: %v", err)
	}
	return string(out)
}

func TestGetParityWithFileGet(t *testing.T) {
	requireShell(t)
	contents := map[string]string{
		"basic":       "A=1\nB=two\n",
		"duplicates":  "A=1\nA=2\nA=3\n",
		"quotes":      "D=\"dq\"\nS='sq'\nL=\"lead\nT=trail\"\nM=a\"b\nBOTH=\"'x'\"\n",
		"prefix":      "APP_VERSION_PREV=0.9\nAPP_VERSION=1.0\n",
		"comments":    "# A=commented\n  A=indented\nB=1\n",
		"equals":      "URL=https://x/?a=b&c=d\n",
		"empty":       "A=\nB=\"\"\n",
		"spaces":      "A = 1\nB= 2\nC =3\n",
		"nofinalnl":   "A=1\nB=2",
		"metachars":   "A=$(echo x) `y` & | \\ ; *\n",
		"unicode":     "A=héllo wörld\n",
		"blank lines": "\n\nA=1\n\n",
	}
	keys := []string{"A", "B", "C", "D", "S", "L", "T", "M", "BOTH", "APP_VERSION", "APP_VERSION_PREV", "URL", "ABSENT"}

	dir := t.TempDir()
	for name, content := range contents {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".env")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		doc := Parse([]byte(content))
		for _, key := range keys {
			for _, def := range []string{"", "dflt"} {
				// file_get prints through printf '%s', so compare exact strings.
				want := shellFileGet(t, path, key, def)
				if got := doc.Get(key, def); got != want {
					t.Errorf("%s: Get(%q, %q) = %q, shell file_get = %q", name, key, def, got, want)
				}
			}
		}
	}

	if want, got := shellFileGet(t, filepath.Join(dir, "does-not-exist.env"), "A", "dflt"), Parse(nil).Get("A", "dflt"); got != want {
		t.Errorf("missing file: Get = %q, shell = %q", got, want)
	}
}

func TestSetParityWithEnvSet(t *testing.T) {
	requireShell(t)
	script, err := filepath.Abs(filepath.Join(refDir, "env-set.sh"))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, initial, input string
	}{
		{"append and replace", "A=1\nB=2\n", "A=10\nC=30\n"},
		{"duplicates all replaced", "A=1\nX=0\nA=2\n", "A=9\n"},
		{"comments in the input and in the file", "# head\nA=1\n", "# note\n\nA=2\n"},
		{"sed-special characters", "A=1\n", "A=a&b|c\\d\nB=x/y\\z&\n"},
		{"shell metacharacters", "A=1\n", "A=$(id) `id` ; * \"q\" 'q'\n"},
		{"value with equals", "A=1\n", "A=k=v=w\n"},
		{"empty value", "A=1\n", "A=\n"},
		{"similar key names", "A_PREV=1\nA=2\n", "A=3\nA_PREV=4\n"},
		{"input without final newline", "A=1\n", "A=2\nB=3"},
		{"empty target", "", "A=1\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "target.env")
			if err := os.WriteFile(path, []byte(tc.initial), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), "sh", script, path)
			cmd.Stdin = strings.NewReader(tc.input)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("env-set.sh: %v\n%s", err, out)
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			as, err := ParseAssignments(strings.NewReader(tc.input))
			if err != nil {
				t.Fatal(err)
			}
			doc := Parse([]byte(tc.initial))
			if err := doc.Apply(as); err != nil {
				t.Fatal(err)
			}
			if got := string(doc.Bytes()); got != string(want) {
				t.Errorf("Go result = %q, env-set.sh result = %q", got, want)
			}
		})
	}
}

func TestEnvSetRejectsWhatParseAssignmentsRejects(t *testing.T) {
	requireShell(t)
	script, err := filepath.Abs(filepath.Join(refDir, "env-set.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"not an assignment\n", "1BAD=x\n", "A-B=x\n", " A=1\n"} {
		path := filepath.Join(t.TempDir(), "target.env")
		if err := os.WriteFile(path, []byte("Z=1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), "sh", script, path)
		cmd.Stdin = strings.NewReader(input)
		shellErr := cmd.Run()
		_, goErr := ParseAssignments(strings.NewReader(input))
		if (shellErr != nil) != (goErr != nil) {
			t.Errorf("input %q: env-set.sh failed=%v, ParseAssignments failed=%v", input, shellErr != nil, goErr != nil)
		}
	}
}
