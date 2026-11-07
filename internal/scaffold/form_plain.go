package scaffold

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/masilia/deploykit/internal/ui"
)

// liner reads answers a line at a time: the renderer for --plain, a dumb terminal, a pipe and the tests.
type liner struct {
	r  *bufio.Reader
	w  io.Writer
	st *ui.Styles
}

func newLiner(o FormOptions) *liner {
	in := o.In
	if in == nil {
		in = strings.NewReader("")
	}
	out := o.Out
	if out == nil {
		out = io.Discard
	}
	// One buffered reader for the whole session: a second one over the same input would find it already drained.
	r, ok := in.(*bufio.Reader)
	if !ok {
		r = bufio.NewReader(in)
	}
	return &liner{r: r, w: out, st: ui.NewStyles(out)}
}

// line reads one answer. The end of the input is an abort: a script that ran out of answers must not write files.
func (l *liner) line() (string, error) {
	s, err := l.r.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || s == "") {
		return "", ErrAborted
	}
	return strings.TrimSpace(s), nil
}

func (l *liner) say(format string, a ...any) { _, _ = fmt.Fprintf(l.w, format+"\n", a...) }

func runPlain(o FormOptions, screens []screen) error {
	l := newLiner(o)
	for _, sc := range screens {
		if sc.hidden() {
			continue
		}
		l.say("\n%s", l.st.Bold.Render(sc.title))
		for _, q := range sc.qs {
			var err error
			switch q.kind {
			case qNote:
				for _, line := range strings.Split(strings.TrimRight(q.desc, "\n"), "\n") {
					l.say("%s", line)
				}
			case qInput:
				err = l.askInput(q)
			case qSelect:
				err = l.askSelect(q)
			case qMulti:
				err = l.askMulti(q)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (l *liner) askInput(q question) error {
	if q.desc != "" {
		l.say("%s", l.st.Muted.Render(q.desc))
	}
	for {
		def := *q.str
		if def == "" && q.suggest != nil {
			def = q.suggest()
		}
		prompt := q.title
		if def != "" {
			prompt += " [" + def + "]"
		}
		_, _ = fmt.Fprint(l.w, prompt+": ")
		line, err := l.line()
		if err != nil {
			return err
		}
		if line == "" {
			line = def
		}
		if q.validate != nil {
			if verr := q.validate(line); verr != nil {
				l.say("  %s", l.st.Fail("%v", verr))
				continue
			}
		}
		*q.str = line
		return nil
	}
}

func (l *liner) askSelect(q question) error {
	opts := q.options()
	def := 0
	for i, o := range opts {
		if o.Value == *q.str {
			def = i
		}
	}
	l.say("%s", q.title)
	if q.desc != "" {
		l.say("%s", l.st.Muted.Render(q.desc))
	}
	for i, o := range opts {
		mark := ""
		if i == def {
			mark = l.st.Muted.Render("  (default)")
		}
		l.say("  %d. %s%s", i+1, o.Label, mark)
	}
	for {
		_, _ = fmt.Fprintf(l.w, "Choose 1-%d [%d]: ", len(opts), def+1)
		line, err := l.line()
		if err != nil {
			return err
		}
		n := def + 1
		if line != "" {
			var perr error
			if n, perr = strconv.Atoi(line); perr != nil || n < 1 || n > len(opts) {
				l.say("  %s", l.st.Fail("a number between 1 and %d", len(opts)))
				continue
			}
		}
		*q.str = opts[n-1].Value
		return nil
	}
}

func (l *liner) askMulti(q question) error {
	opts := q.options()
	chosen := map[string]bool{}
	for _, v := range *q.list {
		chosen[v] = true
	}
	l.say("%s", q.title)
	if q.desc != "" {
		l.say("%s", l.st.Muted.Render(q.desc))
	}
	for i, o := range opts {
		box := "[ ]"
		if chosen[o.Value] {
			box = l.st.Success.Render("[x]")
		}
		l.say("  %s %d. %s", box, i+1, o.Label)
	}
	for {
		_, _ = fmt.Fprint(l.w, "Numbers separated by commas, Enter to keep, - for none: ")
		line, err := l.line()
		if err != nil {
			return err
		}
		next := map[string]bool{}
		switch line {
		case "":
			for _, o := range opts {
				next[o.Value] = chosen[o.Value]
			}
		case "-":
		default:
			bad := false
			for _, f := range strings.Split(line, ",") {
				n, perr := strconv.Atoi(strings.TrimSpace(f))
				if perr != nil || n < 1 || n > len(opts) {
					l.say("  %s", l.st.Fail("%q is not a number between 1 and %d", strings.TrimSpace(f), len(opts)))
					bad = true
					break
				}
				next[opts[n-1].Value] = true
			}
			if bad {
				continue
			}
		}
		var out []string
		for _, o := range opts {
			if next[o.Value] {
				out = append(out, o.Value)
			}
		}
		if q.validateList != nil {
			if verr := q.validateList(out); verr != nil {
				l.say("  %s", l.st.Fail("%v", verr))
				continue
			}
		}
		*q.list = out
		return nil
	}
}

func (l *liner) confirm(title string) (bool, error) {
	for {
		_, _ = fmt.Fprint(l.w, title+" [Y/n]: ")
		line, err := l.line()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(line) {
		case "", "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
	}
}
