// Package fleet runs the same operation across the targets of an inventory: status table, backup freshness, and
// ordered deploys that stop at the first failure unless told otherwise (legacy fleet.sh).
package fleet

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/masilia/deploykit/internal/envfile"
	"github.com/masilia/deploykit/internal/inventory"
	"github.com/masilia/deploykit/internal/transport"
)

// Query says where a stack keeps its state on a server.
type Query struct {
	Root        string
	EnvFile     string // relative to Root
	CurrentKey  string
	PreviousKey string
	Marker      string // installation marker file
	BackupDir   string // relative to Root
}

// Row is the state of one target.
type Row struct {
	Target    string
	Domain    string
	Host      string
	Contract  string
	Version   string
	Previous  string
	Installed bool
	// BackupAge is the age of the newest backup in hours, -1 when there is none.
	BackupAge int
	// Reachable is false when the server could not be queried.
	Reachable bool
}

const statusScript = `cd "$DK_ROOT" && {
  v=$(sed -n "s/^$DK_CUR=//p" "$DK_ENV" 2>/dev/null | head -1); [ -n "$v" ] || v=-
  p=$(sed -n "s/^$DK_PREV=//p" "$DK_ENV" 2>/dev/null | head -1); [ -n "$p" ] || p=-
  test -f "$DK_MARK" && i=yes || i=no
  b=$(ls -t "$DK_BACKUPS"/*.tar.gpg 2>/dev/null | head -1)
  if [ -n "$b" ]; then a=$(( ( $(date +%s) - $(stat -c %Y "$b") ) / 3600 )); else a=-1; fi
  printf '%s|%s|%s|%s' "$v" "$p" "$i" "$a"
}
`

// Fetch reads the state of one stack.
func (q Query) Fetch(ctx context.Context, t transport.Transport) (Row, error) {
	for _, k := range []string{q.CurrentKey, q.PreviousKey} {
		if !envfile.ValidKey(k) {
			return Row{}, fmt.Errorf("invalid version key %q", k)
		}
	}
	res, err := t.Run(ctx, transport.Spec{Script: statusScript, Env: map[string]string{
		"DK_ROOT": q.Root, "DK_ENV": q.EnvFile, "DK_CUR": q.CurrentKey, "DK_PREV": q.PreviousKey, "DK_MARK": q.Marker, "DK_BACKUPS": q.BackupDir,
	}})
	if err != nil {
		return Row{}, err
	}
	return ParseRow(res.Stdout)
}

// ParseRow reads `version|previous|installed|age`.
func ParseRow(out string) (Row, error) {
	parts := strings.Split(strings.TrimSpace(out), "|")
	if len(parts) != 4 {
		return Row{}, fmt.Errorf("unexpected status output %q", out)
	}
	age, err := strconv.Atoi(parts[3])
	if err != nil {
		return Row{}, fmt.Errorf("unreadable backup age %q", parts[3])
	}
	dash := func(s string) string {
		if s == "-" {
			return ""
		}
		return s
	}
	return Row{Version: dash(parts[0]), Previous: dash(parts[1]), Installed: parts[2] == "yes", BackupAge: age, Reachable: true}, nil
}

// State is the one-word summary of a row.
func (r Row) State() string {
	switch {
	case !r.Reachable:
		return "unreachable"
	case !r.Installed:
		return "to install"
	case r.Version == "":
		return "version ?"
	}
	return "OK"
}

// BackupState describes backup freshness: ok is false when there is no backup or it is older than staleHours.
func BackupState(ageHours, staleHours int) (label string, ok bool) {
	switch {
	case ageHours < 0:
		return "NONE", false
	case ageHours > staleHours:
		return fmt.Sprintf("STALE (>%dh)", staleHours), false
	}
	return "OK", true
}

// Describe fills the identity columns of a row from the inventory.
func Describe(t *inventory.Target) Row {
	return Row{Target: t.ID, Domain: t.Get("domain"), Host: t.Get("ssh_user") + "@" + t.Get("host"), Contract: t.Get("contract"), BackupAge: -1}
}

// FormatTable renders the status table.
func FormatTable(rows []Row) string {
	var b strings.Builder
	line := func(cols ...string) {
		fmt.Fprintf(&b, "%-14s %-30s %-24s %-10s %-10s %-10s %-8s %s\n", cols[0], cols[1], cols[2], cols[3], cols[4], cols[5], cols[6], cols[7])
	}
	line("TARGET", "DOMAIN", "HOST", "VERSION", "PREV", "CONTRACT", "BACKUP", "STATE")
	for _, r := range rows {
		if !r.Reachable {
			fmt.Fprintf(&b, "%-14s — (unreachable: %s)\n", r.Target, r.Host)
			continue
		}
		backup := "none"
		if r.BackupAge >= 0 {
			backup = strconv.Itoa(r.BackupAge) + "h"
		}
		line(r.Target, orDash(r.Domain), r.Host, orDash(r.Version), orDash(r.Previous), r.Contract, backup, r.State())
	}
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Deploy runs fn on each target in order. It stops at the first failure unless continueOnError is set, and returns the
// ids that failed. A fleet deploy is never implicit: the caller passes explicit targets.
func Deploy(ctx context.Context, targets []*inventory.Target, continueOnError bool, log func(string), fn func(context.Context, *inventory.Target) error) (failed []string, err error) {
	inventory.SortByRollout(targets)
	for _, t := range targets {
		log(fmt.Sprintf("== deploying %s (order %d)", t.ID, t.RolloutOrder()))
		if runErr := fn(ctx, t); runErr != nil {
			failed = append(failed, t.ID)
			if continueOnError {
				log(fmt.Sprintf("%s: FAILED, continuing (--continue-on-error): %v", t.ID, runErr))
				continue
			}
			log(fmt.Sprintf("%s: FAILED, stopping: %v", t.ID, runErr))
			return failed, fmt.Errorf("deploy failed on %s", strings.Join(failed, ", "))
		}
		log(t.ID + ": OK")
		if ctx.Err() != nil {
			return failed, ctx.Err()
		}
	}
	if len(failed) > 0 {
		return failed, fmt.Errorf("deploy failed on %s", strings.Join(failed, ", "))
	}
	return nil, nil
}
