package main

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

// helpOut runs the root command with args and returns what it printed. styled turns on the styled layout (a test seam: a
// buffer is not a terminal); cols is the width of the help, 0 for the default.
func helpOut(t *testing.T, styled bool, cols int, args ...string) string {
	t.Helper()
	out := &bytes.Buffer{}
	a := newApp(out)
	a.forceStyled = styled
	a.helpCols = cols
	cmd := newRootCmdTo(out, a)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return out.String()
}

// Outside a terminal the help is cobra's text, as it always was: scripts and logs read it.
func TestPlainHelpIsCobrasOwnText(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"--plain", "--help"}} {
		out := helpOut(t, false, 0, args...)
		for _, want := range []string{
			"Usage:\n  deploykit [command]",
			"Available Commands:",
			"  deploy      Deploy a release",
			"Flags:",
			"--dry-run",
			`Use "deploykit [command] --help" for more information about a command.`,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("%v: plain help lacks %q:\n%s", args, want, out)
			}
		}
		for _, banned := range []string{"╔", "Ship:", "Operate:", "<command> [options]"} {
			if strings.Contains(out, banned) {
				t.Errorf("%v: the styled layout leaked into the plain help (%q):\n%s", args, banned, out)
			}
		}
	}
}

func TestStyledRootHelp(t *testing.T) {
	out := helpOut(t, true, 100, "--help")
	for _, want := range []string{logo[0], logo[2], "Usage:", "deploykit <command> [options]", "Options:", "--manifest string", "-v, --verbose", "Ship:", "Operate:", "Set up:", "Other:", "Run 'deploykit <command> --help' for details on a command."} {
		if !strings.Contains(out, want) {
			t.Errorf("styled root help lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Available Commands:") || strings.Contains(out, "Flags:") {
		t.Errorf("cobra's headings must not appear in the styled layout:\n%s", out)
	}
	// The groups come in the order of helpGroups, and each command sits under its own heading.
	last := -1
	for _, g := range helpGroups {
		i := strings.Index(out, g.Title+":")
		if i < 0 || i < last {
			t.Errorf("group %q is missing or out of order in:\n%s", g.Title, out)
		}
		last = i
	}
	ship, op, deploy := strings.Index(out, "Ship:"), strings.Index(out, "Operate:"), strings.Index(out, "  deploy ")
	if deploy <= ship || deploy >= op {
		t.Errorf("deploy must be listed under Ship:\n%s", out)
	}
}

// A new command must be given a heading on purpose: without one it would land under "More".
func TestEveryCommandIsInAHelpGroup(t *testing.T) {
	out := &bytes.Buffer{}
	root := newRootCmdTo(out, newApp(out))
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	in := map[string]bool{}
	for _, g := range helpGroups {
		for _, n := range g.Names {
			in[n] = true
		}
	}
	for _, c := range visibleCommands(root) {
		if !in[c.Name()] {
			t.Errorf("command %q is in no helpGroups entry: add it to the group it belongs to in cmd/deploykit/help.go", c.Name())
		}
	}
	for _, g := range helpGroups {
		for _, n := range g.Names {
			if c, _, err := root.Find([]string{n}); err != nil || c == root {
				t.Errorf("helpGroups lists %q, which is not a command", n)
			}
		}
	}
}

func TestUnplacedCommandsStillShowUnderMore(t *testing.T) {
	groups := groupCommands([]*cobra.Command{{Use: "deploy"}, {Use: "zzz-new"}})
	if len(groups) != 2 || groups[0].Title != "Ship" || groups[1].Title != "More" || groups[1].cmds[0].Name() != "zzz-new" {
		t.Errorf("groups = %+v", groups)
	}
}

func TestStyledSubcommandHelp(t *testing.T) {
	out := helpOut(t, true, 100, "deploy", "--help")
	for _, want := range []string{"deploykit deploy", "Usage:", "deploykit deploy [options]", "Options:", "--version string", "-t, --target string", "Global options:", "--manifest string"} {
		if !strings.Contains(out, want) {
			t.Errorf("styled deploy help lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, logo[0]) {
		t.Errorf("only the root help carries the logo:\n%s", out)
	}
	// `deploykit help deploy` is the same page.
	if again := helpOut(t, true, 100, "help", "deploy"); again != out {
		t.Errorf("help deploy differs from deploy --help:\n%s\n---\n%s", again, out)
	}
}

func TestStyledParentHelpListsItsSubcommands(t *testing.T) {
	out := helpOut(t, true, 100, "target", "--help")
	for _, want := range []string{"deploykit target <command> [options]", "Commands:", "  add  ", "Run 'deploykit target <command> --help'"} {
		if !strings.Contains(out, want) {
			t.Errorf("styled target help lacks %q:\n%s", want, out)
		}
	}
}

func TestStyledHelpWrapsToTheWidth(t *testing.T) {
	for _, cols := range []int{60, 80, 100} {
		for _, args := range [][]string{{"--help"}, {"deploy", "--help"}, {"bootstrap", "--help"}} {
			for n, line := range strings.Split(helpOut(t, true, cols, args...), "\n") {
				// A line may exceed the width only when it is one unbreakable word (a long flag name) or the banner.
				if utf8.RuneCountInString(line) > cols && strings.Contains(strings.TrimSpace(line), " ") && !strings.Contains(line, "version") {
					t.Errorf("%v at %d columns, line %d is %d wide: %q", args, cols, n+1, utf8.RuneCountInString(line), line)
				}
			}
		}
	}
}

func TestStyledHelpDropsTheLogoWhenNarrow(t *testing.T) {
	if out := helpOut(t, true, 40, "--help"); strings.Contains(out, "╔") {
		t.Errorf("a %d-column terminal gets no logo:\n%s", 40, out)
	}
	if out := helpOut(t, true, 100, "--help"); !strings.Contains(out, "╔") {
		t.Error("a wide terminal gets the logo")
	}
}

func TestHelpFlagTable(t *testing.T) {
	out := &bytes.Buffer{}
	a := newApp(out)
	a.forceStyled = true
	a.helpCols = 100
	cmd := &cobra.Command{Use: "thing", Short: "Does a thing", Run: func(*cobra.Command, []string) {}}
	fs := cmd.Flags()
	fs.String("name", "box", "the name")
	fs.IntP("count", "c", 3, "how many")
	fs.Bool("force", false, "do it anyway")
	fs.String("secret", "x", "never shown")
	fs.String("quoted", "a", "already says (default: a) in its usage")
	if err := fs.MarkHidden("secret"); err != nil {
		t.Fatal(err)
	}
	root := &cobra.Command{Use: "deploykit"}
	root.AddCommand(cmd)
	root.SetOut(out) // the help goes where cobra is told to write
	a.printHelp(cmd)
	got := out.String()
	for _, want := range []string{`--name string`, `(default "box")`, `-c, --count int`, `(default 3)`, `--force`, "do it anyway"} {
		if !strings.Contains(got, want) {
			t.Errorf("flag table lacks %q:\n%s", want, got)
		}
	}
	for _, banned := range []string{"--secret", "never shown", "(default false)", `(default "a")`} {
		if strings.Contains(got, banned) {
			t.Errorf("flag table must not show %q:\n%s", banned, got)
		}
	}
	if strings.Count(got, "default") != 3 { // name, count, and the usage that already says it
		t.Errorf("a default is shown once per flag that has one:\n%s", got)
	}
}

func TestWrap(t *testing.T) {
	cases := []struct {
		in    string
		width int
		want  string
	}{
		{"one two three", 100, "one two three"},
		{"one two three", 7, "one two\nthree"},
		{"averyveryverylongword short", 10, "averyveryverylongword\nshort"},
		{"", 10, ""},
	}
	for _, c := range cases {
		if got := wrap(c.in, c.width); got != c.want {
			t.Errorf("wrap(%q, %d) = %q, want %q", c.in, c.width, got, c.want)
		}
	}
}
