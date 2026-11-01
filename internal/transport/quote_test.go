package transport

import (
	"os/exec"
	"testing"
)

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		"":                "''",
		"plain":           "plain",
		"/opt/app-1.2/x":  "/opt/app-1.2/x",
		"a b":             "'a b'",
		"it's":            `'it'\''s'`,
		"$HOME":           "'$HOME'",
		"a;b":             "'a;b'",
		"a\nb":            "'a\nb'",
		"--pw=a&b|c":      "'--pw=a&b|c'",
		"name=value":      "name=value",
		`"dq"`:            `'"dq"'`,
		"back\\slash":     `'back\slash'`,
		"`id`":            "'`id`'",
		"é":               "'é'",
		"user@host:/path": "user@host:/path",
	} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}

// Quote must round-trip through a real shell for hostile input.
func TestQuoteRoundTripsThroughShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	for _, in := range []string{
		"", " ", "plain", "it's", `"`, "$(id)", "`id`", "a;b&&c|d", "*", "a\nb", "\\", "-n", "~", "#c", "!x", "é ü",
		"''", `'"'"'`, "a\tb",
	} {
		out, err := exec.CommandContext(t.Context(), "sh", "-c", "printf %s "+Quote(in)).Output()
		if err != nil {
			t.Fatalf("sh: %v", err)
		}
		if string(out) != in {
			t.Errorf("round trip of %q gave %q", in, out)
		}
	}
}

func TestExportLines(t *testing.T) {
	got, err := exportLines(map[string]string{"B": "it's", "A": "1"}, map[string]string{"C": "x y"})
	if err != nil {
		t.Fatal(err)
	}
	want := "export A=1\nexport B='it'\\''s'\nexport C='x y'\n"
	if got != want {
		t.Errorf("exportLines = %q, want %q", got, want)
	}
	for _, bad := range []string{"", "1A", "A-B", "A B", "A;rm"} {
		if _, err := exportLines(map[string]string{bad: "v"}); err == nil {
			t.Errorf("exportLines accepted the name %q", bad)
		}
	}
}
