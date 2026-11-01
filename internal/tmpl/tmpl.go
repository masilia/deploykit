// Package tmpl renders the small expression language used inside manifests: {{ path.to.value }} with an optional
// default, {{ env.MIN_FREE_MB | default: 2000 }}.
//
// It is deliberately not text/template: there are no functions, no pipelines beyond default and no control flow, so a
// manifest or a target file cannot run anything. In shell mode every interpolated value is quoted, which closes the
// injection hole of templated commands (ADR 0002, decision D6).
package tmpl

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/masilia/deploykit/internal/transport"
)

// Vars is the lookup tree: nested map[string]any (or map[string]string) down to string, int or bool leaves.
type Vars map[string]any

var expr = regexp.MustCompile(`\{\{\s*(.*?)\s*\}\}`)

// Render replaces every {{ expression }} in s. In shell mode the replacement is shell-quoted.
func Render(s string, vars Vars, shell bool) (string, error) {
	var firstErr error
	out := expr.ReplaceAllStringFunc(s, func(m string) string {
		inner := strings.TrimSpace(expr.FindStringSubmatch(m)[1])
		v, err := eval(inner, vars)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return m
		}
		if shell {
			return transport.Quote(v)
		}
		return v
	})
	return out, firstErr
}

// Lookup resolves a dotted path in vars.
func Lookup(vars Vars, path string) (string, bool) {
	var cur any = map[string]any(vars)
	for _, part := range strings.Split(path, ".") {
		switch m := cur.(type) {
		case map[string]any:
			next, ok := m[part]
			if !ok {
				return "", false
			}
			cur = next
		case Vars:
			next, ok := m[part]
			if !ok {
				return "", false
			}
			cur = next
		case map[string]string:
			next, ok := m[part]
			if !ok {
				return "", false
			}
			cur = next
		default:
			return "", false
		}
	}
	switch v := cur.(type) {
	case string:
		return v, true
	case int:
		return fmt.Sprint(v), true
	case bool:
		return fmt.Sprint(v), true
	default:
		return "", false
	}
}

func eval(inner string, vars Vars) (string, error) {
	path, filter, hasFilter := strings.Cut(inner, "|")
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("empty expression")
	}
	def, hasDefault := "", false
	if hasFilter {
		name, arg, ok := strings.Cut(strings.TrimSpace(filter), ":")
		if !ok || strings.TrimSpace(name) != "default" {
			return "", fmt.Errorf("unsupported filter in {{ %s }}: only `| default: value` exists", inner)
		}
		def, hasDefault = strings.Trim(strings.TrimSpace(arg), `"'`), true
	}
	if v, ok := Lookup(vars, path); ok && v != "" {
		return v, nil
	}
	if hasDefault {
		return def, nil
	}
	return "", fmt.Errorf("undefined variable %q", path)
}

// Cond evaluates the one condition form manifests use: `path == 'value'` or `path != 'value'`. A path that is not
// defined compares as the empty string.
func Cond(expr string, vars Vars) (bool, error) {
	for _, op := range []string{"!=", "=="} {
		left, right, ok := strings.Cut(expr, op)
		if !ok {
			continue
		}
		path, want := strings.TrimSpace(left), strings.Trim(strings.TrimSpace(right), `"'`)
		if path == "" {
			break
		}
		got, _ := Lookup(vars, path)
		if op == "==" {
			return got == want, nil
		}
		return got != want, nil
	}
	return false, fmt.Errorf("unsupported condition %q: use `path == 'value'` or `path != 'value'`", expr)
}
