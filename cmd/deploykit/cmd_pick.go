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
		// One reader for every question of the run: a reader of its own per question would swallow, with its buffer, the answers that
		// the next question is waiting for.
		if a.askReader == nil {
			a.askReader = bufio.NewReader(in)
		}
		in = a.askReader
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

// The two options after the targets in the list of pickTargets. Target ids match ^[A-Za-z0-9._-]+$, so these cannot be one.
const (
	pickAllTargets     = "\x00all"
	pickSeveralTargets = "\x00several"
)

// pickTargets asks which targets a command that works on several of them is for. The first question is the one of pickTarget: Enter
// takes the target that is highlighted, which is what most people mean; two more lines at the end of the list say "all of them" and "several",
// and "several" opens a list where Space marks the targets and an empty validation is refused.
func (a *app) pickTargets(dir string, ids []string, global *envfile.Doc) (chosen []string, ok bool, err error) {
	if !a.canPick() || len(ids) < 2 {
		return nil, false, nil
	}
	cmdPath := a.commandPath()
	labels := append(targetLabels(dir, ids, global),
		fmt.Sprintf("all %d targets", len(ids)),
		"several: mark the ones you want, on the next screen")
	values := append(append([]string{}, ids...), pickAllTargets, pickSeveralTargets)
	first, err := scaffold.Pick(a.askOptions(), fmt.Sprintf("Which targets for '%s'?", cmdPath),
		"Enter takes the highlighted one. Pass --all or --target <id> to skip this question.", labels, values)
	if err == nil {
		switch first {
		case pickAllTargets:
			chosen = ids
		case pickSeveralTargets:
			chosen, err = scaffold.PickMany(a.askOptions(), fmt.Sprintf("Several targets for '%s'", cmdPath),
				"Space (or x) marks a target, Enter validates.", targetLabels(dir, ids, global), ids)
		default:
			chosen = []string{first}
		}
	}
	if err != nil {
		if errors.Is(err, scaffold.ErrAborted) {
			return nil, true, errors.New("no target chosen")
		}
		return nil, true, err
	}
	hint := cmdPath
	if len(chosen) == len(ids) {
		hint += " --all"
	} else {
		for _, id := range chosen {
			hint += " --target " + id
		}
	}
	_, _ = fmt.Fprintln(a.out, a.styles().Muted.Render("targets "+strings.Join(chosen, ", ")+"  (next time: "+hint+")"))
	return chosen, true, nil
}

// oneLine keeps an error message on one line of a list.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
