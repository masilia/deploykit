package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Tint chooses the style of one cell from its column and text; the zero style leaves it as it is.
type Tint func(column int, value string) lipgloss.Style

// Table renders rows under a muted header with a rule, columns aligned on the widest cell. Widths are measured on the
// visible text, so styled cells line up.
func (s *Styles) Table(headers []string, rows [][]string, tint Tint) string {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = lipgloss.Width(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if i < len(widths) {
				widths[i] = max(widths[i], lipgloss.Width(c))
			}
		}
	}
	var b strings.Builder
	line := func(cells []string, style func(i int, v string) lipgloss.Style) {
		b.WriteString(" ")
		for i := range headers {
			v := ""
			if i < len(cells) {
				v = cells[i]
			}
			b.WriteString(" " + style(i, v).Render(v) + strings.Repeat(" ", widths[i]-lipgloss.Width(v)) + " ")
		}
		b.WriteString("\n")
	}
	line(headers, func(int, string) lipgloss.Style { return s.Muted.Bold(true) })
	rule := make([]string, len(headers))
	for i, w := range widths {
		rule[i] = strings.Repeat("─", w)
	}
	line(rule, func(int, string) lipgloss.Style { return s.Muted })
	for _, r := range rows {
		line(r, func(i int, v string) lipgloss.Style {
			if tint != nil {
				return tint(i, v)
			}
			return s.r.NewStyle()
		})
	}
	return b.String()
}

// State returns the style for a state word: green when fine, yellow when something is to do, red when wrong.
func (s *Styles) State(value string) lipgloss.Style {
	switch strings.ToLower(value) {
	case "ok", "live", "healthy", "yes":
		return s.Success
	case "to deploy", "to install", "version ?", "warn", "stale", "superseded", "none":
		return s.Warn
	case "unreachable", "fail", "failed", "no":
		return s.Danger
	}
	if strings.HasPrefix(strings.ToLower(value), "stale") {
		return s.Warn
	}
	return s.r.NewStyle()
}

// Plain is the zero style bound to this output.
func (s *Styles) Plain() lipgloss.Style { return s.r.NewStyle() }
