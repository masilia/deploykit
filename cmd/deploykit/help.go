package main

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/masilia/deploykit/internal/ui"
	"github.com/masilia/deploykit/internal/version"
)

// logo is the name in a three-row box-drawing font, shown above the help of the root command only.
var logo = []string{
	"╔╦╗╔═╗╔═╗╦  ╔═╗╦ ╦╦╔═╦╔╦╗",
	" ║║║╣ ╠═╝║  ║ ║╚╦╝╠╩╗║ ║ ",
	"═╩╝╚═╝╩  ╩═╝╚═╝ ╩ ╩ ╩╩ ╩ ",
}

// helpGroup is a heading of the command list of the root help.
type helpGroup struct {
	Title string
	Names []string
}

// helpGroups orders the commands of the root help by what a person is doing. A command that is in no group still shows,
// under "More"; TestEveryCommandIsInAHelpGroup makes adding one a decision.
var helpGroups = []helpGroup{
	{"Ship", []string{"release", "deploy", "rollback"}},
	{"Operate", []string{"status", "run", "env", "healthcheck", "fleet"}},
	{"Set up", []string{"init", "target", "bootstrap", "validate", "doctor"}},
	{"Other", []string{"self-update", "version", "help", "completion", "server"}},
}

// Widths of the help: the terminal's when known, capped so lines stay readable, and what is used when it is not known.
const (
	helpMaxWidth     = 110
	helpDefaultWidth = 100
	helpMinLogoWidth = 50
)

// installHelp makes the help of every command the styled layout at a terminal. Anywhere else, and with --plain or
// TERM=dumb, it is cobra's own text, unchanged: scripts and logs have always read that.
func (a *app) installHelp(root *cobra.Command) {
	plain := root.HelpFunc()
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		if !a.styled() {
			plain(cmd, args)
			return
		}
		a.printHelp(cmd)
	})
}

// helpWidth is the number of columns the help may use.
func (a *app) helpWidth() int {
	if a.helpCols > 0 {
		return a.helpCols
	}
	if f, ok := a.out.(*os.File); ok {
		if w, _, err := term.GetSize(f.Fd()); err == nil && w > 0 {
			return min(w, helpMaxWidth)
		}
	}
	return helpDefaultWidth
}

// helpRow is one line of a table: a name (a command or a flag), a short note after it, and a description.
type helpRow struct {
	name string
	note string // muted, after the name: the type of a flag
	desc string
}

// printHelp writes the styled help of cmd.
func (a *app) printHelp(cmd *cobra.Command) {
	cmd.InitDefaultHelpFlag()
	w := cmd.OutOrStdout()
	st := a.styles()
	width := a.helpWidth()
	var b strings.Builder
	head := func(title string) { b.WriteString("\n" + st.Warn.Bold(true).Render(title) + "\n") }

	if cmd.Parent() == nil {
		if width >= helpMinLogoWidth {
			for _, l := range logo {
				b.WriteString(st.Title.Render(l) + "\n")
			}
			b.WriteString("\n")
		}
		b.WriteString(wrap(cmd.Short, width) + "\n")
		b.WriteString(st.Muted.Render("version") + " " + st.Success.Render(version.String()) + "\n")
	} else {
		b.WriteString(st.Title.Render(cmd.CommandPath()) + "\n")
		text := cmd.Long
		if text == "" {
			text = cmd.Short
		}
		b.WriteString(wrapParagraphs(strings.TrimSpace(text), width) + "\n")
	}

	head("Usage:")
	for _, l := range usageLines(cmd) {
		b.WriteString("  " + l + "\n")
	}
	if len(cmd.Aliases) > 0 {
		head("Aliases:")
		b.WriteString("  " + strings.Join(cmd.Aliases, ", ") + "\n")
	}
	if ex := strings.TrimRight(cmd.Example, "\n "); ex != "" {
		head("Examples:")
		b.WriteString(ex + "\n")
	}

	if rows := flagRows(cmd.NonInheritedFlags()); len(rows) > 0 {
		head("Options:")
		a.writeTable(&b, st, rows, width)
	}
	if rows := flagRows(cmd.InheritedFlags()); len(rows) > 0 {
		head("Global options:")
		a.writeTable(&b, st, rows, width)
	}

	if subs := visibleCommands(cmd); len(subs) > 0 {
		if cmd.Parent() == nil {
			for _, g := range groupCommands(subs) {
				head(g.Title + ":")
				a.writeTable(&b, st, commandRows(g.cmds), width)
			}
		} else {
			head("Commands:")
			a.writeTable(&b, st, commandRows(subs), width)
		}
		b.WriteString("\n" + st.Muted.Render(fmt.Sprintf("Run '%s <command> --help' for details on a command.", cmd.CommandPath())) + "\n")
	}
	_, _ = fmt.Fprint(w, b.String())
	if cmd.Parent() == nil {
		a.printUpdateNotice(w, cmd) // from what is remembered: the help asks the network for nothing
	}
}

// usageLines are the ways to run cmd: its own line when it runs by itself, and one for its subcommands. "flags" is called
// "options" here, the way the table below calls them.
func usageLines(cmd *cobra.Command) []string {
	var lines []string
	if cmd.Runnable() {
		lines = append(lines, strings.Replace(cmd.UseLine(), "[flags]", "[options]", 1))
	}
	if cmd.HasAvailableSubCommands() {
		lines = append(lines, cmd.CommandPath()+" <command> [options]")
	}
	if len(lines) == 0 {
		lines = append(lines, strings.Replace(cmd.UseLine(), "[flags]", "[options]", 1))
	}
	return lines
}

// visibleCommands are the subcommands a person can run.
func visibleCommands(cmd *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	for _, c := range cmd.Commands() {
		if c.IsAvailableCommand() || c.Name() == "help" {
			out = append(out, c)
		}
	}
	return out
}

type commandGroup struct {
	Title string
	cmds  []*cobra.Command
}

// groupCommands files the commands under the headings of helpGroups, in that order; the ones nobody placed come last.
func groupCommands(cmds []*cobra.Command) []commandGroup {
	byName := map[string]*cobra.Command{}
	for _, c := range cmds {
		byName[c.Name()] = c
	}
	placed := map[string]bool{}
	var out []commandGroup
	for _, g := range helpGroups {
		var in []*cobra.Command
		for _, n := range g.Names {
			if c, ok := byName[n]; ok {
				in = append(in, c)
				placed[n] = true
			}
		}
		if len(in) > 0 {
			out = append(out, commandGroup{g.Title, in})
		}
	}
	var rest []*cobra.Command
	for _, c := range cmds {
		if !placed[c.Name()] {
			rest = append(rest, c)
		}
	}
	if len(rest) > 0 {
		out = append(out, commandGroup{"More", rest})
	}
	return out
}

func commandRows(cmds []*cobra.Command) []helpRow {
	rows := make([]helpRow, 0, len(cmds))
	for _, c := range cmds {
		rows = append(rows, helpRow{name: c.Name(), desc: c.Short})
	}
	return rows
}

// flagRows turns a flag set into rows: "-f, --manifest" with its type after it, and the usage with its default.
func flagRows(fs *pflag.FlagSet) []helpRow {
	var rows []helpRow
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Deprecated != "" {
			return
		}
		name := "    --" + f.Name
		if f.Shorthand != "" {
			name = "-" + f.Shorthand + ", --" + f.Name
		}
		typ, usage := pflag.UnquoteUsage(f)
		if d := flagDefault(f); d != "" && !strings.Contains(usage, "default") {
			usage += " (default " + d + ")"
		}
		rows = append(rows, helpRow{name: name, note: typ, desc: usage})
	})
	return rows
}

// flagDefault is the default of a flag worth showing: not the zero value of its type.
func flagDefault(f *pflag.Flag) string {
	switch f.DefValue {
	case "", "0", "false", "[]", "0s", "<nil>":
		return ""
	}
	if f.Value.Type() == "string" {
		return fmt.Sprintf("%q", f.DefValue)
	}
	return f.DefValue
}

// writeTable prints rows with the names in a column and the descriptions wrapped, hanging under their first line.
func (a *app) writeTable(b *strings.Builder, s *ui.Styles, rows []helpRow, width int) {
	const indent = 2
	col := 0
	for _, r := range rows {
		if n := utf8.RuneCountInString(r.name) + noteWidth(r.note); n > col {
			col = n
		}
	}
	col += 2
	descWidth := max(width-indent-col, 24)
	pad := strings.Repeat(" ", indent+col)
	for _, r := range rows {
		left := s.Success.Render(r.name)
		used := utf8.RuneCountInString(r.name)
		if r.note != "" {
			left += " " + s.Muted.Render(r.note)
			used += noteWidth(r.note)
		}
		lines := strings.Split(wrap(r.desc, descWidth), "\n")
		b.WriteString(strings.Repeat(" ", indent) + left + strings.Repeat(" ", col-used) + lines[0] + "\n")
		for _, l := range lines[1:] {
			b.WriteString(pad + l + "\n")
		}
	}
}

// noteWidth is the width of " note", the space included, or 0.
func noteWidth(note string) int {
	if note == "" {
		return 0
	}
	return 1 + utf8.RuneCountInString(note)
}

// wrap breaks text at spaces so that no line is longer than width (a word longer than that stays whole).
func wrap(text string, width int) string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = word
		case utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) <= width:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	return strings.Join(append(lines, line), "\n")
}

// wrapParagraphs wraps each paragraph (separated by a blank line) and keeps single line breaks of a paragraph as breaks.
func wrapParagraphs(text string, width int) string {
	paras := strings.Split(text, "\n\n")
	for i, p := range paras {
		lines := strings.Split(p, "\n")
		for j, l := range lines {
			lines[j] = wrap(l, width)
		}
		paras[i] = strings.Join(lines, "\n")
	}
	return strings.Join(paras, "\n\n")
}
