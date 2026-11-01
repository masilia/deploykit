package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"

	"github.com/masilia/deploykit/internal/compose"
	"github.com/masilia/deploykit/internal/deploy"
	"github.com/masilia/deploykit/internal/doctor"
	"github.com/masilia/deploykit/internal/fleet"
	"github.com/masilia/deploykit/internal/runner"
	"github.com/masilia/deploykit/internal/ui"
)

// styled reports whether output is for a person at a terminal: layout, colour and a spinner. Pipes, files, cron, CI,
// TERM=dumb and --plain keep the timestamped plain lines that logs and the tests rely on.
func (a *app) styled() bool {
	if a.forceStyled {
		return true
	}
	if a.plain || os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := a.out.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

func (a *app) styles() *ui.Styles { return ui.NewStyles(a.out) }

// reporter is the renderer of runner events.
func (a *app) reporter() runner.Reporter {
	if a.styled() {
		return ui.NewPretty(a.out, a.verbose, !a.forceStyled)
	}
	return ui.NewPlain(a.out, a.verbose)
}

// flow frames a command that runs steps: a banner above, a one-line result below.
func (a *app) flow(title, subtitle string, fn func() error) error {
	if !a.styled() {
		return fn()
	}
	st := a.styles()
	_, _ = fmt.Fprint(a.out, st.Banner(title, subtitle)+"\n")
	start := a.now()
	err := fn()
	took := a.now().Sub(start).Round(100 * time.Millisecond)
	switch {
	case err != nil:
		_, _ = fmt.Fprintln(a.out, "\n"+st.Fail("%s failed after %s", title, took))
	case a.dryRun:
		_, _ = fmt.Fprintln(a.out, "\n"+st.Info("%s: dry run, nothing was changed", title))
	default:
		_, _ = fmt.Fprintln(a.out, "\n"+st.OK("%s done in %s", title, took))
	}
	return err
}

func (a *app) printLine(plain string, styled func(*ui.Styles) string) {
	if a.styled() {
		_, _ = fmt.Fprintln(a.out, styled(a.styles()))
		return
	}
	_, _ = fmt.Fprintln(a.out, plain)
}

// ---- validate, doctor ------------------------------------------------------------------------------------------

func (a *app) printValidated(path, project, artifact, runtime string, ids []string, dir string) {
	if !a.styled() {
		_, _ = fmt.Fprintf(a.out, "%s: project %s, artifact %s, runtime %s\n", path, project, artifact, runtime)
		_, _ = fmt.Fprintf(a.out, "%d target(s) in %s: %v\n", len(ids), dir, ids)
		return
	}
	st := a.styles()
	_, _ = fmt.Fprint(a.out, st.Banner("validate", path))
	_, _ = fmt.Fprintln(a.out, "\n  "+st.OK("%s  %s", st.Bold.Render(project), st.Muted.Render(artifact+" · "+runtime)))
	_, _ = fmt.Fprintln(a.out, "  "+st.OK("%d target(s)  %s", len(ids), st.Muted.Render(strings.Join(ids, ", "))))
}

func (a *app) printReport(report []doctor.Result) {
	if !a.styled() {
		for _, r := range report {
			_, _ = fmt.Fprintf(a.out, "%-5s %-24s %-16s %s\n", r.Level, r.Where, r.Name, r.Detail)
		}
		return
	}
	st := a.styles()
	_, _ = fmt.Fprint(a.out, st.Banner("doctor", "tools needed here and on the server")+"\n")
	where, problems, warnings := "", 0, 0
	width := 0
	for _, r := range report {
		width = max(width, lipgloss.Width(r.Name))
	}
	for _, r := range report {
		if r.Where != where {
			where = r.Where
			_, _ = fmt.Fprintln(a.out, "  "+st.Bold.Render(where))
		}
		var icon, detail string
		switch r.Level {
		case doctor.OK:
			icon, detail = st.Success.Render(ui.IconOK), st.Muted.Render(shorten(r.Detail, 72))
		case doctor.Warn:
			icon, detail, warnings = st.Warn.Render("!"), st.Warn.Render(r.Detail), warnings+1
		default:
			icon, detail, problems = st.Danger.Render(ui.IconFail), st.Danger.Render(r.Detail), problems+1
		}
		pad := strings.Repeat(" ", width-lipgloss.Width(r.Name))
		_, _ = fmt.Fprintf(a.out, "    %s %s%s  %s\n", icon, r.Name, pad, detail)
	}
	switch {
	case problems > 0:
		_, _ = fmt.Fprintln(a.out, "\n"+st.Fail("%d required tool(s) missing", problems))
	case warnings > 0:
		_, _ = fmt.Fprintln(a.out, "\n"+st.Caution("ready, with %d warning(s)", warnings))
	default:
		_, _ = fmt.Fprintln(a.out, "\n"+st.OK("everything needed is there"))
	}
}

// shorten cuts a long version banner (curl prints a whole line of library versions) to n characters.
func shorten(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// ---- tables ----------------------------------------------------------------------------------------------------

func (a *app) printFleetTable(rows []fleet.Row) {
	if !a.styled() {
		_, _ = fmt.Fprint(a.out, fleet.FormatTable(rows))
		return
	}
	st := a.styles()
	data := make([][]string, len(rows))
	for i, r := range rows {
		if !r.Reachable {
			data[i] = []string{r.Target, orDash(r.Domain), r.Host, "-", "-", r.Contract, "-", "unreachable"}
			continue
		}
		backup := "none"
		if r.BackupAge >= 0 {
			backup = strconv.Itoa(r.BackupAge) + "h"
		}
		data[i] = []string{r.Target, orDash(r.Domain), r.Host, orDash(r.Version), orDash(r.Previous), r.Contract, backup, r.State()}
	}
	_, _ = fmt.Fprint(a.out, st.Table([]string{"TARGET", "DOMAIN", "HOST", "VERSION", "PREV", "CONTRACT", "BACKUP", "STATE"}, data, func(col int, v string) lipgloss.Style {
		switch col {
		case 0:
			return st.Bold
		case 3:
			return st.Accent
		case 4, 5, 2:
			return st.Muted
		case 6, 7:
			return st.State(v)
		}
		return st.Plain()
	}))
}

func (a *app) printNodeTable(rows []fleet.NodeRow) {
	if !a.styled() {
		_, _ = fmt.Fprint(a.out, fleet.FormatNodeTable(rows))
		return
	}
	st := a.styles()
	data := make([][]string, len(rows))
	for i, r := range rows {
		if !r.Reachable {
			data[i] = []string{r.Target, orDash(r.Domain), r.Host, "-", "-", "-", r.Contract, "unreachable"}
			continue
		}
		data[i] = []string{r.Target, orDash(r.Domain), r.Host, orDash(r.Live), orDash(r.Commit), orDash(r.Previous), r.Contract, r.State()}
	}
	_, _ = fmt.Fprint(a.out, st.Table([]string{"TARGET", "DOMAIN", "HOST", "LIVE", "COMMIT", "PREV", "CONTRACT", "STATE"}, data, func(col int, v string) lipgloss.Style {
		switch col {
		case 0:
			return st.Bold
		case 3:
			return st.Accent
		case 2, 4, 5, 6:
			return st.Muted
		case 7:
			return st.State(v)
		}
		return st.Plain()
	}))
}

// printReleases is the status of one pm2 target: the live release first, then the ones kept.
func (a *app) printReleases(targetID, where, current string, list []deploy.Release) {
	if !a.styled() {
		_, _ = fmt.Fprintf(a.out, "target %s (%s)\ncurrent %s\n", targetID, where, orDash(current))
		for _, r := range list {
			mark, commit := " ", "-"
			if r.Stamp == current {
				mark = "*"
			}
			if r.Meta != nil {
				commit = r.Meta.Commit
			}
			_, _ = fmt.Fprintf(a.out, "%s %s  %-10s %s\n", mark, r.Stamp, r.Status, commit)
		}
		return
	}
	st := a.styles()
	_, _ = fmt.Fprint(a.out, st.Banner("status", targetID+"  "+where)+"\n")
	data := make([][]string, len(list))
	for i, r := range list {
		mark, commit := "", "-"
		if r.Stamp == current {
			mark = ui.IconStep
		}
		if r.Meta != nil {
			commit = r.Meta.Commit
		}
		data[i] = []string{mark, r.Stamp, string(r.Status), commit}
	}
	if len(data) == 0 {
		_, _ = fmt.Fprintln(a.out, "  "+st.Caution("nothing deployed yet"))
		return
	}
	_, _ = fmt.Fprint(a.out, st.Table([]string{"", "RELEASE", "STATUS", "COMMIT"}, data, func(col int, v string) lipgloss.Style {
		switch col {
		case 0:
			return st.Accent
		case 1:
			return st.Bold
		case 2:
			return st.State(v)
		}
		return st.Muted
	}))
}

// printBackups shows the age of the newest backup of each target.
func (a *app) printBackups(rows [][]string) {
	if !a.styled() {
		_, _ = fmt.Fprintf(a.out, "%-14s %-10s %s\n", "TARGET", "AGE", "STATE")
		for _, r := range rows {
			_, _ = fmt.Fprintf(a.out, "%-14s %-10s %s\n", r[0], r[1], r[2])
		}
		return
	}
	st := a.styles()
	_, _ = fmt.Fprint(a.out, st.Banner("backups", "age of the newest backup")+"\n")
	_, _ = fmt.Fprint(a.out, st.Table([]string{"TARGET", "AGE", "STATE"}, rows, func(col int, v string) lipgloss.Style {
		switch col {
		case 0:
			return st.Bold
		case 2:
			return st.State(v)
		}
		return st.Muted
	}))
}

// printProbes shows the result of each health probe.
func (a *app) printProbes(results []compose.ProbeResult, base string) {
	if !a.styled() {
		for _, r := range results {
			mark := "OK "
			if !r.OK {
				mark = "KO "
			}
			_, _ = fmt.Fprintf(a.out, "%s%s: %s\n", mark, r.Name, r.Detail)
		}
		return
	}
	st := a.styles()
	_, _ = fmt.Fprint(a.out, st.Banner("healthcheck", base)+"\n")
	width := 0
	for _, r := range results {
		width = max(width, lipgloss.Width(r.Name))
	}
	for _, r := range results {
		pad := strings.Repeat(" ", width-lipgloss.Width(r.Name))
		if r.OK {
			_, _ = fmt.Fprintf(a.out, "  %s %s%s  %s\n", st.Success.Render(ui.IconOK), r.Name, pad, st.Muted.Render(r.Detail))
		} else {
			_, _ = fmt.Fprintf(a.out, "  %s %s%s  %s\n", st.Danger.Render(ui.IconFail), st.Danger.Render(r.Name), pad, st.Danger.Render(r.Detail))
		}
	}
}
