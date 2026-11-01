package ui

import (
	"bytes"
	"strings"
	"testing"
)

// A writer that is not a terminal gets the same layout without any escape sequence.
func TestStylesArePlainOnANonTerminal(t *testing.T) {
	var buf bytes.Buffer
	s := NewStyles(&buf)
	out := s.Banner("deploykit init", "Create the configuration") +
		s.Panel("Review", s.Rows([][2]string{{"Project", "acme"}, {"Runtime", "pm2"}})) + "\n" +
		s.OK("wrote %s", "deploykit.yaml") + "\n" + s.Fail("nope") + "\n" + s.Caution("careful") + "\n" + s.Info("next") + "\n" + s.Step(2, 5, "Ingress")
	if strings.Contains(out, "\x1b[") {
		t.Errorf("no colour expected for a buffer:\n%q", out)
	}
	for _, want := range []string{"◆ deploykit init", "Create the configuration", "Review", "╭", "│", "Project  acme", "Runtime  pm2", "✔ wrote deploykit.yaml", "✖ nope", "! careful", "› next", "2/5  Ingress"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestFormThemeBuilds(t *testing.T) {
	th := FormTheme()
	if th == nil || th.Focused.Title.GetBold() != true {
		t.Error("the focused title should be bold")
	}
}

func TestDarkBackgroundDoesNotAskTheTerminal(t *testing.T) {
	for _, c := range []struct {
		theme, fgbg string
		want        bool
	}{
		{"", "", true}, {"light", "", false}, {"DARK", "15;0", true},
		{"", "0;15", false}, {"", "15;0", true}, {"", "0;default;15", false}, {"", "garbage;x", true},
	} {
		t.Setenv("DEPLOYKIT_THEME", c.theme)
		t.Setenv("COLORFGBG", c.fgbg)
		if got := darkBackground(); got != c.want {
			t.Errorf("theme %q COLORFGBG %q: dark = %v, want %v", c.theme, c.fgbg, got, c.want)
		}
	}
}
