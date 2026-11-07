package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/masilia/deploykit/internal/transport"
)

// remoteHarness is a compose project with two targets (acme on srv1, beta on srv2, both in /opt/app), a terminal that answers with
// line prompts, and a fake server per target.
func remoteHarness(t *testing.T, answers string) *composeHarness {
	t.Helper()
	ch := newComposeHarness(t, map[string]string{"acme": acmeTarget, "beta": betaTarget})
	ch.app.interactive = func() bool { return true }
	ch.app.accessible = true
	ch.app.in = strings.NewReader(answers)
	return ch
}

// serverCalls are the commands sent to a target's server, one per line, with their folder.
func serverCalls(f *transport.Fake) []string {
	var out []string
	for _, c := range f.Calls() {
		if c.Spec != nil && len(c.Spec.Argv) > 0 {
			out = append(out, c.Spec.Dir+"$ "+strings.Join(c.Spec.Argv, " "))
		}
	}
	return out
}

const monitorOnBeta = "/opt/app$ /opt/app/.deploykit/deploykit server monitor --manifest /opt/app/deploykit.yaml"

func TestServerCommandRunsOnTheNamedTarget(t *testing.T) {
	ch := remoteHarness(t, "")
	ch.site("beta").On(".deploykit/deploykit server monitor", transport.Result{Stdout: "monitor: all checks passed\n"})
	if err := ch.run("server", "monitor", "--target", "beta"); err != nil {
		t.Fatalf("%v\n%s", err, ch.out)
	}
	calls := serverCalls(ch.sites["beta"])
	if len(calls) != 2 || calls[0] != "$ test -x /opt/app/.deploykit/deploykit" || calls[1] != monitorOnBeta {
		t.Errorf("beta received:\n%s", strings.Join(calls, "\n"))
	}
	if acme, ok := ch.sites["acme"]; ok && len(acme.Calls()) != 0 {
		t.Error("the other target must not be touched")
	}
	out := ch.out.String()
	if !strings.Contains(out, "on beta (beta@srv): deploykit server monitor") || !strings.Contains(out, "monitor: all checks passed") {
		t.Errorf("the output of the server comes back, under a line that says where it ran:\n%s", out)
	}
}

func TestServerCommandAsksWhichTargetWhenThereAreSeveral(t *testing.T) {
	ch := remoteHarness(t, "2\n") // acme, beta: 2 is beta
	if err := ch.run("server", "monitor"); err != nil {
		t.Fatalf("%v\n%s", err, ch.out)
	}
	for _, want := range []string{"Which target for 'deploykit server monitor'?", "1. acme  app.acme.fr  srv1", "2. beta  app.beta.fr  srv2", "(next time: deploykit server monitor --target beta)"} {
		if !strings.Contains(ch.out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, ch.out)
		}
	}
	if calls := serverCalls(ch.sites["beta"]); len(calls) != 2 || calls[1] != monitorOnBeta {
		t.Errorf("the chosen target runs the command: %v", calls)
	}
}

func TestServerCommandAsksNothingWithoutATerminal(t *testing.T) {
	ch := remoteHarness(t, "2\n")
	ch.app.interactive = func() bool { return false }
	err := ch.run("server", "monitor")
	if err == nil || !strings.Contains(err.Error(), "choose a target with --target") {
		t.Fatalf("a script gets the error that lists the targets, got %v", err)
	}
	if len(ch.sites) != 0 {
		t.Error("no server is contacted")
	}
}

func TestServerCommandPassesItsFlagsOn(t *testing.T) {
	ch := remoteHarness(t, "")
	if err := ch.run("--dry-run", "server", "restore-test", "--target", "acme", "--file", "backups/app.tar.gpg"); err != nil {
		t.Fatalf("%v\n%s", err, ch.out)
	}
	got := serverCalls(ch.sites["acme"])
	want := "/opt/app$ /opt/app/.deploykit/deploykit server restore-test --dry-run=true --file backups/app.tar.gpg --manifest /opt/app/deploykit.yaml"
	if len(got) != 2 || got[1] != want {
		t.Errorf("flags set by the user are sent, the ones about the workstation are not:\n got %v\nwant %s", got, want)
	}
	for _, banned := range []string{"--target", "--plain", "--repo", "workspace"} {
		if strings.Contains(got[1], banned) {
			t.Errorf("%q must stay on the workstation: %s", banned, got[1])
		}
	}
}

func TestServerUpdateAndInstallBelongToDeploy(t *testing.T) {
	for sub, want := range map[string]string{"update": "deploykit deploy --version X.Y.Z'", "install": "deploykit deploy --version X.Y.Z --install'"} {
		ch := remoteHarness(t, "")
		err := ch.run("server", sub, "--target", "acme")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("server %s from a workstation points at deploy, got %v", sub, err)
		}
		if len(ch.sites) != 0 {
			t.Errorf("server %s: nothing is sent to a server", sub)
		}
	}
}

func TestServerCommandSaysWhenTheServerHasNoDeploykitOrCannotBeReached(t *testing.T) {
	ch := remoteHarness(t, "")
	ch.site("beta").Fail("test -x", 1, "")
	err := ch.run("server", "backup", "--target", "beta")
	if err == nil || !strings.Contains(err.Error(), "has no deploykit at /opt/app/.deploykit/deploykit") || !strings.Contains(err.Error(), "deploykit deploy -t beta") {
		t.Errorf("a server where deploy never ran says what to do, got %v", err)
	}
	if calls := serverCalls(ch.sites["beta"]); len(calls) != 1 {
		t.Errorf("the command is not run when the binary is missing: %v", calls)
	}

	ch = remoteHarness(t, "")
	ch.site("beta").Fail("test -x", 255, "ssh: connect to host srv2 port 22: Connection timed out")
	err = ch.run("server", "backup", "--target", "beta")
	if err == nil || strings.Contains(err.Error(), "has no deploykit") || !strings.Contains(err.Error(), "255") {
		t.Errorf("an unreachable server is not reported as a missing binary, got %v", err)
	}
}

func TestServerCommandReportsAFailureOnTheServer(t *testing.T) {
	ch := remoteHarness(t, "")
	ch.site("acme").Fail(".deploykit/deploykit server renew-certs", 1, "certbot: rate limit")
	err := ch.run("server", "renew-certs", "--target", "acme")
	if err == nil || !strings.Contains(err.Error(), "deploykit server renew-certs failed on acme") {
		t.Errorf("the failure names the command and the target, got %v", err)
	}
}

// On the server itself, where `deploy` uploaded the target file, nothing changes: the command runs there and never goes through SSH.
func TestServerCommandOnTheServerStaysLocal(t *testing.T) {
	ch := remoteHarness(t, "")
	if err := os.MkdirAll(filepath.Join(ch.repo, ".deploykit"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ch.repo, ".deploykit", "target.env"), []byte(acmeTarget), 0o600); err != nil {
		t.Fatal(err)
	}
	// --target is for a workstation: on the server it changes nothing. If it were obeyed here, acme would be contacted over SSH.
	_ = ch.run("server", "monitor", "--target", "acme") // it runs against the local fake: what it does is the business of server_test.go
	if len(ch.sites) != 0 {
		t.Errorf("with a deployment root here the command stays here, but SSH was used for %d target(s)", len(ch.sites))
	}
}

func TestServerCommandRefusesAnotherRuntime(t *testing.T) {
	dir := newProjectDir(t, cliManifest) // pm2
	h := newHarness(dir)
	err := h.run("server", "monitor")
	if err == nil || !strings.Contains(err.Error(), "manage compose stacks") {
		t.Errorf("server commands are for compose stacks, got %v", err)
	}
}

// ---- fleet ------------------------------------------------------------------------------------------------------------------

func fleetStatusHarness(t *testing.T, answers string) *composeHarness {
	t.Helper()
	ch := remoteHarness(t, answers)
	ch.site("acme").On("DK_ROOT", transport.Result{Stdout: "2.0.0|1.9.0|yes|3"})
	ch.site("beta").On("DK_ROOT", transport.Result{Stdout: "2.1.0|2.0.0|yes|5"})
	return ch
}

func TestFleetAsksWhichTargetsWhenNoneIsGiven(t *testing.T) {
	ch := fleetStatusHarness(t, "2\n")
	if err := ch.run("fleet", "status"); err != nil {
		t.Fatalf("%v\n%s", err, ch.out)
	}
	out := ch.out.String()
	for _, want := range []string{"Which targets for 'deploykit fleet status'?", "Space marks a target", "1. acme", "2. beta", "(next time: deploykit fleet status --target beta)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// The list of the question names both targets; what comes after the reminder is the report.
	_, table, _ := strings.Cut(out, "(next time: deploykit fleet status --target beta)")
	if !strings.Contains(table, "app.beta.fr") || !strings.Contains(table, "2.1.0") || strings.Contains(table, "acme") {
		t.Errorf("only beta is reported:\n%s", table)
	}

	ch = fleetStatusHarness(t, "1,2\n")
	if err := ch.run("fleet", "status"); err != nil {
		t.Fatalf("%v\n%s", err, ch.out)
	}
	if o := ch.out.String(); !strings.Contains(o, "app.acme.fr") || !strings.Contains(o, "app.beta.fr") || !strings.Contains(o, "--target acme --target beta") {
		t.Errorf("both targets are reported and the hint repeats them:\n%s", o)
	}
}

func TestFleetKeepsItsErrorWhenNothingIsChosenOrNobodyCanAnswer(t *testing.T) {
	for name, setup := range map[string]func(*composeHarness){
		"no one at a terminal": func(ch *composeHarness) { ch.app.interactive = func() bool { return false } },
		"none marked":          func(*composeHarness) {},
	} {
		ch := fleetStatusHarness(t, "-\n")
		setup(ch)
		err := ch.run("fleet", "status")
		if err == nil || !strings.Contains(err.Error(), "no target selected: use --all or --target ID") {
			t.Errorf("%s: want the usual error, got %v", name, err)
		}
	}
}

func TestFleetAsksNothingWhenTheSelectionIsGiven(t *testing.T) {
	for _, args := range [][]string{{"fleet", "status", "--all"}, {"fleet", "status", "--target", "acme"}} {
		ch := fleetStatusHarness(t, "2\n")
		if err := ch.run(args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, ch.out)
		}
		if strings.Contains(ch.out.String(), "Which targets") {
			t.Errorf("%v: the selection is given, nothing is asked:\n%s", args, ch.out)
		}
	}
}
