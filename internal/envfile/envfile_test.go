package envfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGet(t *testing.T) {
	cases := []struct {
		name, content, key, def, want string
	}{
		{"plain", "A=1\n", "A", "", "1"},
		{"missing key gives default", "A=1\n", "B", "dflt", "dflt"},
		{"empty value gives default", "A=\n", "A", "dflt", "dflt"},
		{"last occurrence wins", "A=1\nA=2\n", "A", "", "2"},
		{"longer key name does not match", "APP_VERSION_PREV=0.9\n", "APP_VERSION", "none", "none"},
		{"prefix of the key does not match", "APP_VERSION=1\n", "APP_VERSION_PREV", "none", "none"},
		{"commented line is inert", "# A=1\n", "A", "dflt", "dflt"},
		{"indented line is inert", " A=1\n", "A", "dflt", "dflt"},
		{"spaces around = do not match", "A = 1\n", "A", "dflt", "dflt"},
		{"value keeps later equals signs", "A=b=c\n", "A", "", "b=c"},
		{"double quotes removed", "A=\"x y\"\n", "A", "", "x y"},
		{"single quotes removed", "A='x y'\n", "A", "", "x y"},
		{"lone leading quote removed", "A=\"abc\n", "A", "", "abc"},
		{"lone trailing quote removed", "A=abc\"\n", "A", "", "abc"},
		{"inner quotes kept", "A=a\"b\n", "A", "", "a\"b"},
		{"double then single quotes both stripped", "A=\"'x'\"\n", "A", "", "x"},
		{"quotes only gives default", "A=\"\"\n", "A", "dflt", "dflt"},
		{"no final newline", "A=1", "A", "", "1"},
		{"shell metacharacters stay literal", "A=$(id) `id` & | \\\n", "A", "", "$(id) `id` & | \\"},
		{"empty file", "", "A", "dflt", "dflt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Parse([]byte(tc.content)).Get(tc.key, tc.def); got != tc.want {
				t.Errorf("Get(%q, %q) = %q, want %q", tc.key, tc.def, got, tc.want)
			}
		})
	}
}

func TestHas(t *testing.T) {
	d := Parse([]byte("# C=1\nA=\nAB=2\n"))
	for key, want := range map[string]bool{"A": true, "AB": true, "C": false, "B": false} {
		if got := d.Has(key); got != want {
			t.Errorf("Has(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestSet(t *testing.T) {
	cases := []struct {
		name, content, key, value, want string
	}{
		{"append", "A=1\n", "B", "2", "A=1\nB=2\n"},
		{"replace in place", "A=1\nB=2\n", "A", "9", "A=9\nB=2\n"},
		{"replace every duplicate", "A=1\nX=0\nA=2\n", "A", "9", "A=9\nX=0\nA=9\n"},
		{"comments and blank lines preserved", "# head\n\nA=1\n", "A", "2", "# head\n\nA=2\n"},
		{"longer key untouched", "A_PREV=1\n", "A", "2", "A_PREV=1\nA=2\n"},
		{"special characters stored literally", "A=1\n", "A", `a&b|c\d$e'f"g`, "A=a&b|c\\d$e'f\"g\n"},
		{"value with equals sign", "A=1\n", "A", "x=y", "A=x=y\n"},
		{"empty value", "A=1\n", "A", "", "A=\n"},
		{"append to a file without final newline", "A=1", "B", "2", "A=1\nB=2\n"},
		{"replace in a file without final newline keeps it", "A=1", "A", "2", "A=2"},
		{"append to an empty file", "", "A", "1", "A=1\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Parse([]byte(tc.content))
			if err := d.Set(tc.key, tc.value); err != nil {
				t.Fatalf("Set: %v", err)
			}
			if got := string(d.Bytes()); got != tc.want {
				t.Errorf("contents = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSetRejectsBadInput(t *testing.T) {
	d := Parse(nil)
	for _, key := range []string{"", "1A", "A-B", "A B", "A=B", "é"} {
		if err := d.Set(key, "v"); err == nil {
			t.Errorf("Set(%q): want an error", key)
		}
	}
	for _, value := range []string{"a\nb", "a\rb"} {
		if err := d.Set("A", value); err == nil {
			t.Errorf("Set with value %q: want an error", value)
		}
	}
	if len(d.Bytes()) != 0 {
		t.Errorf("rejected Set modified the document: %q", d.Bytes())
	}
}

func TestBytesRoundTrip(t *testing.T) {
	for _, content := range []string{"", "\n", "A=1\n", "A=1", "# c\n\nA=1\n\n", "\n\n"} {
		if got := string(Parse([]byte(content)).Bytes()); got != content {
			t.Errorf("round trip of %q gave %q", content, got)
		}
	}
}

func TestLoadAndLoadOptional(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent.env")

	if _, err := Load(missing); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Load of a missing file: err = %v, want ErrNotExist", err)
	}
	d, err := LoadOptional(missing)
	if err != nil {
		t.Fatalf("LoadOptional of a missing file: %v", err)
	}
	if got := d.Get("A", "dflt"); got != "dflt" {
		t.Errorf("empty Doc Get = %q, want the default", got)
	}

	present := filepath.Join(dir, "present.env")
	if err := os.WriteFile(present, []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err = LoadOptional(present)
	if err != nil || d.Get("A", "") != "1" {
		t.Errorf("LoadOptional of a present file: value %q, err %v", d.Get("A", ""), err)
	}
}

func TestSavePermissions(t *testing.T) {
	dir := t.TempDir()

	fresh := filepath.Join(dir, "fresh.env")
	if err := Parse([]byte("A=1\n")).Save(fresh); err != nil {
		t.Fatal(err)
	}
	assertMode(t, fresh, 0o600)

	existing := filepath.Join(dir, "existing.env")
	if err := os.WriteFile(existing, []byte("A=1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(existing, 0o640); err != nil {
		t.Fatal(err)
	}
	d, err := Load(existing)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Set("A", "2"); err != nil {
		t.Fatal(err)
	}
	if err := d.Save(existing); err != nil {
		t.Fatal(err)
	}
	assertMode(t, existing, 0o640)

	data, err := os.ReadFile(existing)
	if err != nil || string(data) != "A=2\n" {
		t.Errorf("saved contents = %q (err %v), want %q", data, err, "A=2\n")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != want {
		t.Errorf("%s mode = %o, want %o", filepath.Base(path), got, want)
	}
}

func TestParseAssignments(t *testing.T) {
	in := "# comment\n\nA=1\nB=x=y\nC=\nD='q'\n"
	got, err := ParseAssignments(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []Assignment{{"A", "1"}, {"B", "x=y"}, {"C", ""}, {"D", "'q'"}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("assignment %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestParseAssignmentsEmptyInput(t *testing.T) {
	got, err := ParseAssignments(strings.NewReader("# only a comment\n\n"))
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, err %v; want no assignments and no error", got, err)
	}
}

func TestParseAssignmentsErrorsDoNotLeakSecrets(t *testing.T) {
	const secret = "s3cr3t-value"
	for _, in := range []string{secret + "\n", "1BAD=" + secret + "\n", "A B=" + secret + "\n", " A=" + secret + "\n"} {
		_, err := ParseAssignments(strings.NewReader(in))
		if err == nil {
			t.Errorf("input %q: want an error", in)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("input %q: error leaks the value: %v", in, err)
		}
		if !strings.Contains(err.Error(), "line 1") {
			t.Errorf("input %q: error should name the line: %v", in, err)
		}
	}
}

func TestApply(t *testing.T) {
	d := Parse([]byte("A=1\n"))
	err := d.Apply([]Assignment{{"A", "2"}, {"B", "3"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(d.Bytes()); got != "A=2\nB=3\n" {
		t.Errorf("contents = %q", got)
	}
	if err := d.Apply([]Assignment{{"bad key", "x"}}); err == nil {
		t.Error("Apply with an invalid key: want an error")
	}
}

func TestLayers(t *testing.T) {
	target := Parse([]byte("HOST=client\nEMPTY=\n"))
	global := Parse([]byte("HOST=global\nEMPTY=fallback\nREGISTRY=ghcr.io/org\n"))
	l := Layers{target, nil, global}

	for _, tc := range []struct{ key, def, want string }{
		{"HOST", "", "client"},
		{"EMPTY", "", "fallback"},
		{"REGISTRY", "", "ghcr.io/org"},
		{"MISSING", "dflt", "dflt"},
	} {
		if got := l.Get(tc.key, tc.def); got != tc.want {
			t.Errorf("Get(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
	if got := (Layers{}).Get("A", "d"); got != "d" {
		t.Errorf("no layers: Get = %q, want the default", got)
	}
}

func TestKeys(t *testing.T) {
	d := Parse([]byte("# C=1\nB=1\nA=2\nB=3\n bad=1\n1X=2\nnoequals\nZ_9=\n"))
	got := strings.Join(d.Keys(), ",")
	if got != "B,A,Z_9" {
		t.Errorf("Keys = %q, want B,A,Z_9", got)
	}
}
