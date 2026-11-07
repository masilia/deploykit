package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/masilia/deploykit/internal/transport"
)

// twoTargets is a project whose inventory holds demo and other, an app that can be asked questions (line prompts, so a test can answer),
// and a server that answers `status`.
func twoTargets(t *testing.T, answers string) *harness {
	t.Helper()
	dir := newProjectDir(t, cliManifest)
	other := "HOST=other.example\nSITE_USER=othersite\nROOT_USER=root\nDOMAIN=other.example.org\nPATH_ON_SERVER=/home/othersite/htdocs/other.example\nAPP_PORT=3002\n"
	if err := os.WriteFile(filepath.Join(dir, "targets", "other.env"), []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(dir)
	h.site.OnFunc(func(s transport.Spec) bool { return strings.Contains(s.Script, "for d in *") }, transport.Result{Stdout: "20261026000000\tlive\t\n"})
	h.app.interactive = func() bool { return true }
	h.app.accessible = true
	h.app.in = strings.NewReader(answers)
	return h
}

// ran says which server the command talked to: the path of the target is in what was sent to it.
func ran(h *harness) (demo, other bool) {
	trace := h.site.Trace()
	return strings.Contains(trace, "demo.example"), strings.Contains(trace, "other.example")
}

func TestStatusAsksWhichTargetWhenThereAreSeveral(t *testing.T) {
	h := twoTargets(t, "2\n")
	if err := h.run("status"); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	out := h.out.String()
	for _, want := range []string{
		"Which target for 'deploykit status'?",
		"Pass --target <id> to skip this question.",
		"1. demo   demo.example  srv.example",
		"2. other  other.example.org  other.example",
		"Choose 1-2 [1]:",
		"(next time: deploykit status --target other)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if demo, other := ran(h); demo || !other {
		t.Errorf("the command must run on the chosen target (other), demo=%v other=%v:\n%s", demo, other, h.site.Trace())
	}
}

func TestPickerEnterTakesTheFirstAndBadAnswersAreAskedAgain(t *testing.T) {
	h := twoTargets(t, "\n")
	if err := h.run("status"); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	if demo, other := ran(h); !demo || other {
		t.Errorf("Enter alone chooses the first target (demo), demo=%v other=%v", demo, other)
	}

	h = twoTargets(t, "9\nabc\n2\n")
	if err := h.run("status"); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	if strings.Count(h.out.String(), "a number between 1 and 2") != 2 {
		t.Errorf("each bad answer is refused with the same message:\n%s", h.out)
	}
	if _, other := ran(h); !other {
		t.Error("the valid answer after the bad ones chooses other")
	}
}

func TestNoQuestionWhenNobodyCanAnswer(t *testing.T) {
	h := twoTargets(t, "2\n")
	h.app.interactive = func() bool { return false } // a script, a pipe, CI
	err := h.run("status")
	if err == nil || !strings.Contains(err.Error(), "choose a target with --target") || !strings.Contains(err.Error(), "[demo other]") {
		t.Fatalf("without a terminal the error that lists the targets is kept, got %v", err)
	}
	if strings.Contains(h.out.String(), "Which target") || len(h.site.Calls()) != 0 {
		t.Errorf("nothing is asked and nothing runs:\n%s", h.out)
	}
	// The unread answer shows that nobody waited for one.
	if rest, _ := h.app.in.(*strings.Reader); rest.Len() != len("2\n") {
		t.Error("the input must not be read")
	}
}

func TestNoQuestionWhenTheTargetIsNamedOrAloneOrAnInventoryOfOne(t *testing.T) {
	h := twoTargets(t, "2\n")
	if err := h.run("status", "--target", "demo"); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	if strings.Contains(h.out.String(), "Which target") {
		t.Errorf("--target skips the question:\n%s", h.out)
	}
	if demo, other := ran(h); !demo || other {
		t.Errorf("--target demo runs on demo, demo=%v other=%v", demo, other)
	}

	// An inventory of one is never a question.
	dir := newProjectDir(t, cliManifest)
	one := newHarness(dir)
	one.site.OnFunc(func(s transport.Spec) bool { return strings.Contains(s.Script, "for d in *") }, transport.Result{Stdout: "20261026000000\tlive\t\n"})
	one.app.interactive = func() bool { return true }
	one.app.accessible = true
	one.app.in = strings.NewReader("")
	if err := one.run("status"); err != nil {
		t.Fatalf("%v\n%s", err, one.out)
	}
	if strings.Contains(one.out.String(), "Which target") {
		t.Errorf("one target is the target:\n%s", one.out)
	}
}

func TestLeavingThePickerStopsTheCommand(t *testing.T) {
	h := twoTargets(t, "") // the input ends: Ctrl-D
	err := h.run("status")
	if err == nil || !strings.Contains(err.Error(), "no target chosen") {
		t.Fatalf("leaving the question stops the command, got %v", err)
	}
	if len(h.site.Calls()) != 0 {
		t.Error("nothing runs on a server")
	}
}

func TestAnEmptyInventoryPointsAtTargetAdd(t *testing.T) {
	h := twoTargets(t, "1\n")
	for _, id := range []string{"demo", "other"} {
		if err := os.Remove(filepath.Join(filepath.Dir(h.app.manifestPath), "targets", id+".env")); err != nil {
			t.Fatal(err)
		}
	}
	err := h.run("status")
	if err == nil || !strings.Contains(err.Error(), "holds no target") || !strings.Contains(err.Error(), "deploykit target add") {
		t.Fatalf("an empty inventory says how to add a target, got %v", err)
	}
	if strings.Contains(h.out.String(), "Which target") {
		t.Error("there is nothing to choose from")
	}
}

// The question names the command it is for, so that a person choosing a target for a rollback or a deploy knows it.
func TestThePickerNamesTheCommand(t *testing.T) {
	h := twoTargets(t, "1\n")
	_ = h.run("rollback") // it may fail afterwards (one release only): the question is what is under test
	if !strings.Contains(h.out.String(), "Which target for 'deploykit rollback'?") {
		t.Errorf("the title names the command:\n%s", h.out)
	}
	if !strings.Contains(h.out.String(), "(next time: deploykit rollback --target demo)") {
		t.Errorf("the hint repeats the command:\n%s", h.out)
	}
}
