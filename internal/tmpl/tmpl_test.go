package tmpl

import (
	"os/exec"
	"strings"
	"testing"
)

var vars = Vars{
	"project": "app",
	"target":  map[string]string{"domain": "example.com", "app_port": "3001", "evil": "x'; rm -rf / #"},
	"env":     Vars{"EMPTY": "", "MIN": "500"},
	"n":       42,
}

func TestRender(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain", "plain"},
		{"{{ project }}", "app"},
		{"{{project}}", "app"},
		{"https://{{ target.domain }}/x", "https://example.com/x"},
		{"{{ target.app_port }}:{{ n }}", "3001:42"},
		{"{{ env.MIN | default: 2000 }}", "500"},
		{"{{ env.EMPTY | default: 2000 }}", "2000"},
		{"{{ env.MISSING | default: 'fallback' }}", "fallback"},
		{"{{ env.MISSING | default: \"a b\" }}", "a b"},
		{"{{ project }}-{{ project }}", "app-app"},
	} {
		got, err := Render(tc.in, vars, false)
		if err != nil || got != tc.want {
			t.Errorf("Render(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestRenderErrors(t *testing.T) {
	for _, in := range []string{
		"{{ nope }}", "{{ target.missing }}", "{{ target }}", "{{ }}", "{{ project | upper }}", "{{ project | default }}",
	} {
		if _, err := Render(in, vars, false); err == nil {
			t.Errorf("Render(%q): want an error", in)
		}
	}
}

func TestRenderShellQuotesEverything(t *testing.T) {
	got, err := Render("echo {{ target.evil }} {{ target.domain }}", vars, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, lookErr := exec.LookPath("sh"); lookErr != nil {
		t.Skip("sh not available")
	}
	out, err := exec.CommandContext(t.Context(), "sh", "-c", got).Output()
	if err != nil {
		t.Fatalf("sh: %v (%s)", err, got)
	}
	if want := "x'; rm -rf / # example.com\n"; string(out) != want {
		t.Errorf("shell saw %q, want %q", out, want)
	}
	if !strings.Contains(got, `'x'\''; rm -rf / #'`) {
		t.Errorf("rendered command = %s", got)
	}
}

func TestLookup(t *testing.T) {
	for path, want := range map[string]string{"project": "app", "target.domain": "example.com", "env.MIN": "500", "n": "42"} {
		if got, ok := Lookup(vars, path); !ok || got != want {
			t.Errorf("Lookup(%q) = %q, %v", path, got, ok)
		}
	}
	for _, path := range []string{"", "target", "target.domain.x", "x.y"} {
		if _, ok := Lookup(vars, path); ok {
			t.Errorf("Lookup(%q) should fail", path)
		}
	}
}

func TestCond(t *testing.T) {
	v := Vars{"target": map[string]string{"tls_mode": "cloudpanel", "empty": ""}}
	for expr, want := range map[string]bool{
		"target.tls_mode == 'cloudpanel'":       true,
		`target.tls_mode == "cloudpanel"`:       true,
		"target.tls_mode == 'certbot'":          false,
		"target.tls_mode != 'certbot'":          true,
		"target.tls_mode != 'cloudpanel'":       false,
		"target.missing == ''":                  true,
		"target.missing != 'x'":                 true,
		"  target.tls_mode  ==  'cloudpanel'  ": true,
	} {
		got, err := Cond(expr, v)
		if err != nil || got != want {
			t.Errorf("Cond(%q) = %v, %v; want %v", expr, got, err, want)
		}
	}
	for _, bad := range []string{"", "target.tls_mode", "== 'x'", "a > 'b'"} {
		if _, err := Cond(bad, v); err == nil {
			t.Errorf("Cond(%q) must fail", bad)
		}
	}
}
