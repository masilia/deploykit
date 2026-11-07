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

// pickTarget asks which of the targets to use, when the inventory has several and none was named with --target. ok is false when
// nobody can be asked (see canPick): the caller then reports the error it always reported.
func (a *app) pickTarget(dir string, ids []string, global *envfile.Doc) (id string, ok bool, err error) {
	if !a.canPick() {
		return "", false, nil
	}
	width := 0
	for _, id := range ids {
		width = max(width, utf8.RuneCountInString(id))
	}
	labels := make([]string, len(ids))
	for i, id := range ids {
		labels[i] = id + strings.Repeat(" ", width-utf8.RuneCountInString(id))
		t, oerr := inventory.Open(dir, id, global)
		if oerr != nil {
			labels[i] += "  (cannot be read: " + oneLine(oerr.Error()) + ")"
			continue
		}
		if d := t.Get("domain"); d != "" {
			labels[i] += "  " + d
		}
		if h := t.Get("host"); h != "" {
			labels[i] += "  " + h
		}
	}
	cmdPath := a.cmdPath
	if cmdPath == "" {
		cmdPath = "deploykit"
	}
	in := a.in
	if in == nil {
		in = os.Stdin
	}
	accessible := a.plain || os.Getenv("TERM") == "dumb" || a.accessible || !hasScreenSize()
	if accessible {
		in = bufio.NewReader(in)
	}
	opts := scaffold.FormOptions{In: in, Out: a.out, Accessible: accessible}
	chosen, err := scaffold.Pick(opts, fmt.Sprintf("Which target for '%s'?", cmdPath), "Pass --target <id> to skip this question.", labels, ids)
	if err != nil {
		if errors.Is(err, scaffold.ErrAborted) {
			return "", true, errors.New("no target chosen")
		}
		return "", true, err
	}
	_, _ = fmt.Fprintln(a.out, a.styles().Muted.Render(fmt.Sprintf("target %s  (next time: %s --target %s)", chosen, cmdPath, chosen)))
	return chosen, true, nil
}

// oneLine keeps an error message on one line of a list.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
