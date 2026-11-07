package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/term"

	"github.com/masilia/deploykit/internal/envfile"
	"github.com/masilia/deploykit/internal/inventory"
	"github.com/masilia/deploykit/internal/scaffold"
)

// canPick reports whether a person can be asked a question: both ends are a terminal. A script, a pipe and CI have nobody to answer,
// so there the command keeps failing with the list of targets instead of waiting for an answer that never comes.
func (a *app) canPick() bool {
	if a.interactive != nil {
		return a.interactive()
	}
	return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
}

// targetLabels are the lines of a list of targets: the id, then the domain and the host, so that a person can tell them apart.
func targetLabels(dir string, ids []string, global *envfile.Doc) []string {
	width := 0
	for _, id := range ids {
		width = max(width, utf8.RuneCountInString(id))
	}
	labels := make([]string, len(ids))
	for i, id := range ids {
		labels[i] = id + strings.Repeat(" ", width-utf8.RuneCountInString(id))
		t, err := inventory.Open(dir, id, global)
		if err != nil {
			labels[i] += "  (cannot be read: " + oneLine(err.Error()) + ")"
			continue
		}
		if d := t.Get("domain"); d != "" {
			labels[i] += "  " + d
		}
		if h := t.Get("host"); h != "" {
			labels[i] += "  " + h
		}
	}
	return labels
}

// askOptions says how a question is put: full screen, or line prompts for --plain, a dumb terminal and a terminal that has no size.
func (a *app) askOptions() scaffold.FormOptions {
	in := a.in
	if in == nil {
		in = os.Stdin
	}
	accessible := a.plain || os.Getenv("TERM") == "dumb" || a.accessible || !hasScreenSize()
	if accessible {
		in = bufio.NewReader(in) // shared by every prompt of the command, so none of them loses buffered answers
	}
	return scaffold.FormOptions{In: in, Out: a.out, Accessible: accessible}
}

func (a *app) commandPath() string {
	if a.cmdPath == "" {
		return "deploykit"
	}
	return a.cmdPath
}

// pickTarget asks which of the targets to use, when the inventory has several and none was named with --target. ok is false when
// nobody can be asked (see canPick): the caller then reports the error it always reported.
func (a *app) pickTarget(dir string, ids []string, global *envfile.Doc) (id string, ok bool, err error) {
	if !a.canPick() {
		return "", false, nil
	}
	cmdPath := a.commandPath()
	chosen, err := scaffold.Pick(a.askOptions(), fmt.Sprintf("Which target for '%s'?", cmdPath),
		"Pass --target <id> to skip this question.", targetLabels(dir, ids, global), ids)
	if err != nil {
		if errors.Is(err, scaffold.ErrAborted) {
			return "", true, errors.New("no target chosen")
		}
		return "", true, err
	}
	_, _ = fmt.Fprintln(a.out, a.styles().Muted.Render(fmt.Sprintf("target %s  (next time: %s --target %s)", chosen, cmdPath, chosen)))
	return chosen, true, nil
}

// pickTargets asks which targets a command that works on several of them is for. Nothing is chosen at the start; choosing none is an
// empty answer, which the caller reports like any other missing selection.
func (a *app) pickTargets(dir string, ids []string, global *envfile.Doc) (chosen []string, ok bool, err error) {
	if !a.canPick() || len(ids) < 2 {
		return nil, false, nil
	}
	cmdPath := a.commandPath()
	chosen, err = scaffold.PickMany(a.askOptions(), fmt.Sprintf("Which targets for '%s'?", cmdPath),
		"Space marks a target, Enter validates. Pass --all or --target <id> to skip this question.", targetLabels(dir, ids, global), ids)
	if err != nil {
		if errors.Is(err, scaffold.ErrAborted) {
			return nil, true, errors.New("no target chosen")
		}
		return nil, true, err
	}
	if len(chosen) > 0 {
		hint := cmdPath
		for _, id := range chosen {
			hint += " --target " + id
		}
		_, _ = fmt.Fprintln(a.out, a.styles().Muted.Render("targets "+strings.Join(chosen, ", ")+"  (next time: "+hint+")"))
	}
	return chosen, true, nil
}

// oneLine keeps an error message on one line of a list.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
