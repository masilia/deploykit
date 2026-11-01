package transport

import (
	"fmt"
	"sort"
	"strings"

	"github.com/masilia/deploykit/internal/envfile"
)

// Quote returns s quoted for a POSIX shell. Plain words are left alone; anything else is single-quoted, and each
// embedded single quote is written as: close quote, backslash, quote, reopen quote.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("_@%+=:,./-", r):
		default:
			safe = false
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// QuoteArgs quotes each argument and joins them with spaces.
func QuoteArgs(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = Quote(a)
	}
	return strings.Join(q, " ")
}

// exportLines renders `export KEY='value'` lines in key order. Keys must be valid variable names.
func exportLines(vars ...map[string]string) (string, error) {
	var b strings.Builder
	for _, m := range vars {
		keys := make([]string, 0, len(m))
		for k := range m {
			if !envfile.ValidKey(k) {
				return "", fmt.Errorf("transport: invalid environment variable name %q", k)
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString("export " + k + "=" + Quote(m[k]) + "\n")
		}
	}
	return b.String(), nil
}

// guardDelimiter ends the here-document that carries a script; a longer one is used if the script contains it.
const guardDelimiter = "__DEPLOYKIT_SCRIPT_END__"

// guardStdin wraps a script so that the shell reads all of it before running any of it, then runs it with an empty
// stdin. Scripts travel on the stdin of the shell, and a command in the middle of a script that forwards its stdin
// (docker compose exec -T, cat, ssh) would otherwise swallow the lines that follow, which then silently never run.
func guardStdin(script string) string {
	delim := guardDelimiter
	for strings.Contains(script, delim) {
		delim += "_"
	}
	if !strings.HasSuffix(script, "\n") {
		script += "\n"
	}
	return "__dk_script=$(cat <<'" + delim + "'\n" + script + delim + "\n)\n" + "eval \"$__dk_script\" </dev/null\n"
}
