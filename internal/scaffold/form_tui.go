package scaffold

import (
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/masilia/deploykit/internal/ui"
)

// runFullScreen draws the screens with huh: one group per screen, hidden screens skipped, options refreshed when the
// answer they depend on changes.
func runFullScreen(o FormOptions, screens []screen) error {
	groups := make([]*huh.Group, 0, len(screens))
	for _, sc := range screens {
		fields := make([]huh.Field, 0, len(sc.qs))
		for _, q := range sc.qs {
			fields = append(fields, huhField(q))
		}
		groups = append(groups, huh.NewGroup(fields...).Title(sc.title).WithHideFunc(sc.hidden))
	}
	return runForm(huh.NewForm(groups...), o)
}

func huhOptions(cs []choice, selected func(string) bool) []huh.Option[string] {
	out := make([]huh.Option[string], len(cs))
	for i, c := range cs {
		out[i] = huh.NewOption(c.Label, c.Value)
		if selected != nil && selected(c.Value) {
			out[i] = out[i].Selected(true)
		}
	}
	return out
}

func huhField(q question) huh.Field {
	switch q.kind {
	case qInput:
		in := huh.NewInput().Title(q.title).Description(q.desc).Value(q.str)
		if q.validate != nil {
			in = in.Validate(q.validate)
		}
		if q.suggest != nil {
			in = in.PlaceholderFunc(q.suggest, q.watch)
		}
		return in
	case qSelect:
		return huh.NewSelect[string]().Title(q.title).Description(q.desc).Value(q.str).Options(huhOptions(q.options(), nil)...)
	case qMulti:
		ms := huh.NewMultiSelect[string]().Title(q.title).Description(q.desc).Value(q.list)
		selected := func(v string) bool { return contains(*q.list, v) }
		if q.watch != nil {
			return ms.OptionsFunc(func() []huh.Option[string] { return huhOptions(q.options(), selected) }, q.watch)
		}
		return ms.Options(huhOptions(q.options(), selected)...)
	}
	return huh.NewNote().Title(q.title).Description(strings.TrimRight(q.desc, "\n"))
}

func runForm(form *huh.Form, o FormOptions) error {
	form = form.WithTheme(ui.FormTheme())
	if o.In != nil {
		form = form.WithInput(o.In)
	}
	if o.Out != nil {
		form = form.WithOutput(o.Out)
	}
	if err := runProtected(form); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return ErrAborted
		}
		return err
	}
	return nil
}

// runProtected turns a panic of the form library into an error: huh v1.0.0 panics instead of returning the error of
// a program that could not start (a terminal with no size, for example).
func runProtected(form *huh.Form) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("the full-screen form could not start (%v); use --plain for line prompts", r)
		}
	}()
	return form.Run()
}

// Confirm asks a yes/no question.
func Confirm(o FormOptions, title, yes, no string) (bool, error) {
	if o.Accessible {
		return newLiner(o).confirm(title)
	}
	ok := true
	form := huh.NewForm(huh.NewGroup(huh.NewConfirm().Title(title).Affirmative(yes).Negative(no).Value(&ok)))
	if err := runForm(form, o); err != nil {
		return false, err
	}
	return ok, nil
}

// ChooseConflict asks what to do with files that exist with other content. The bool is false when the user stops.
func ChooseConflict(o FormOptions, paths []string) (Conflict, bool, error) {
	picked := "stop"
	cs := []choice{
		{"Stop, write nothing", "stop"},
		{"Write the new files next to them as <name>.new", "new"},
		{"Replace them, keeping the old ones as <name>.bak", "overwrite"},
	}
	q := question{kind: qSelect, title: "Already there with other content: " + strings.Join(paths, ", "), str: &picked,
		options: func() []choice { return cs }}
	var err error
	if o.Accessible {
		err = newLiner(o).askSelect(q)
	} else {
		err = runForm(huh.NewForm(huh.NewGroup(huhField(q))), o)
	}
	if err != nil {
		return ConflictStop, false, err
	}
	switch picked {
	case "new":
		return ConflictSideBySide, true, nil
	case "overwrite":
		return ConflictOverwrite, true, nil
	}
	return ConflictStop, false, nil
}
