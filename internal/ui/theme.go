package ui

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

// The palette adapts to light and dark terminals. Colour is dropped automatically for a writer that is not a terminal,
// for NO_COLOR and for TERM=dumb, because every style is built from a renderer bound to the writer (see NewStyles).
var (
	colAccent  = lipgloss.AdaptiveColor{Light: "#5B45D6", Dark: "#A594FF"}
	colSuccess = lipgloss.AdaptiveColor{Light: "#1A7F4B", Dark: "#4ADE80"}
	colDanger  = lipgloss.AdaptiveColor{Light: "#C62828", Dark: "#FF6B6B"}
	colWarn    = lipgloss.AdaptiveColor{Light: "#9A6700", Dark: "#F5C451"}
	colMuted   = lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#8B93A1"}
)

// Icons shared with the plain renderer.
const (
	IconOK    = "✔"
	IconFail  = "✖"
	IconStep  = "›"
	IconPlan  = "◌"
	IconBadge = "◆"
)

// Styles renders titles, panels and status lines for one output.
type Styles struct {
	r *lipgloss.Renderer

	Title   lipgloss.Style
	Muted   lipgloss.Style
	Accent  lipgloss.Style
	Success lipgloss.Style
	Danger  lipgloss.Style
	Warn    lipgloss.Style
	Bold    lipgloss.Style
	panel   lipgloss.Style
}

// NewStyles binds the styles to w: a terminal gets colour and rounded borders, anything else gets the same layout in
// plain characters.
func NewStyles(w io.Writer) *Styles {
	r := lipgloss.NewRenderer(w)
	s := &Styles{r: r}
	s.Title = r.NewStyle().Bold(true).Foreground(colAccent)
	s.Muted = r.NewStyle().Foreground(colMuted)
	s.Accent = r.NewStyle().Foreground(colAccent)
	s.Success = r.NewStyle().Foreground(colSuccess)
	s.Danger = r.NewStyle().Foreground(colDanger)
	s.Warn = r.NewStyle().Foreground(colWarn)
	s.Bold = r.NewStyle().Bold(true)
	s.panel = r.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colAccent).Padding(0, 2)
	return s
}

// Banner is the header of a command: a badge, the title and a muted line under it.
func (s *Styles) Banner(title, subtitle string) string {
	head := s.Accent.Render(IconBadge) + " " + s.Title.Render(title)
	if subtitle == "" {
		return head + "\n"
	}
	return head + "\n" + s.Muted.Render(subtitle) + "\n"
}

// Panel draws body in a rounded box under a title.
func (s *Styles) Panel(title, body string) string {
	box := s.panel.Render(strings.TrimRight(body, "\n"))
	if title == "" {
		return box
	}
	return s.Bold.Render(title) + "\n" + box
}

// OK, Fail, Warn and Info are one-line status messages.
func (s *Styles) OK(format string, a ...any) string {
	return s.Success.Render(IconOK) + " " + fmt.Sprintf(format, a...)
}

// Fail is a failure line.
func (s *Styles) Fail(format string, a ...any) string {
	return s.Danger.Render(IconFail) + " " + fmt.Sprintf(format, a...)
}

// Caution is a warning line.
func (s *Styles) Caution(format string, a ...any) string {
	return s.Warn.Render("!") + " " + fmt.Sprintf(format, a...)
}

// Info is a neutral line introduced by the step arrow.
func (s *Styles) Info(format string, a ...any) string {
	return s.Accent.Render(IconStep) + " " + fmt.Sprintf(format, a...)
}

// Step is the progress label shown above a screen of questions: "2/5  Ingress".
func (s *Styles) Step(i, n int, name string) string {
	return s.Muted.Render(fmt.Sprintf("%d/%d", i, n)) + "  " + s.Bold.Render(name)
}

// Rows aligns label/value pairs for a panel: the labels are muted, the values plain.
func (s *Styles) Rows(pairs [][2]string) string {
	width := 0
	for _, p := range pairs {
		width = max(width, lipgloss.Width(p[0]))
	}
	var b strings.Builder
	for _, p := range pairs {
		b.WriteString(s.Muted.Render(p[0]) + strings.Repeat(" ", width-lipgloss.Width(p[0])+2) + p[1] + "\n")
	}
	return b.String()
}

// FormTheme is the huh theme of the interactive forms, in the same palette.
func FormTheme() *huh.Theme {
	t := huh.ThemeCharm()
	f := &t.Focused
	f.Base = f.Base.BorderForeground(colAccent)
	f.Title = f.Title.Foreground(colAccent).Bold(true)
	f.NoteTitle = f.NoteTitle.Foreground(colAccent).Bold(true)
	f.Description = f.Description.Foreground(colMuted)
	f.ErrorIndicator = f.ErrorIndicator.Foreground(colDanger)
	f.ErrorMessage = f.ErrorMessage.Foreground(colDanger)
	f.SelectSelector = f.SelectSelector.Foreground(colAccent)
	f.NextIndicator = f.NextIndicator.Foreground(colAccent)
	f.PrevIndicator = f.PrevIndicator.Foreground(colAccent)
	f.MultiSelectSelector = f.MultiSelectSelector.Foreground(colAccent)
	f.SelectedOption = f.SelectedOption.Foreground(colSuccess)
	f.SelectedPrefix = f.SelectedPrefix.Foreground(colSuccess)
	f.FocusedButton = f.FocusedButton.Background(colAccent).Foreground(lipgloss.AdaptiveColor{Light: "#FFFFFF", Dark: "#1B1B1F"})
	f.TextInput.Cursor = f.TextInput.Cursor.Foreground(colAccent)
	f.TextInput.Prompt = f.TextInput.Prompt.Foreground(colAccent)
	t.Blurred = *f
	t.Blurred.Base = t.Blurred.Base.BorderStyle(lipgloss.HiddenBorder())
	t.Blurred.Title = t.Blurred.Title.Foreground(colMuted).Bold(false)
	t.Blurred.NoteTitle = t.Blurred.NoteTitle.Foreground(colMuted).Bold(false)
	return t
}
