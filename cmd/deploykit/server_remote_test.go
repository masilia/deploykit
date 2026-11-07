package main

import (
	"crypto/sha256"
	"encoding/hex"
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

// ---- upgrading the binary of the servers ----------------------------------------------------------------------------------

// upgradeHarness is remoteHarness plus a binary to send and servers that behave: no binary at first, then the one that was sent.
func upgradeHarness(t *testing.T, answers string) (*composeHarness, string) {
	t.Helper()
	ch := remoteHarness(t, answers)
	bin := filepath.Join(t.TempDir(), "deploykit-linux-amd64")
	if err := os.WriteFile(bin, []byte("a deploykit binary"), 0o700); err != nil { //nolint:gosec // test double
		t.Fatal(err)
	}
	sum := sha256Hex(t, bin)
	for _, id := range []string{"acme", "beta"} {
		f := ch.site(id)
		calls := 0
		f.OnFunc(func(s transport.Spec) bool { // the first look at the server finds no binary
			if !strings.Contains(s.Script, "sha256sum") {
				return false
			}
			calls++
			return calls == 1
		}, transport.Result{})
		f.OnFunc(func(s transport.Spec) bool { return strings.Contains(s.Script, "sha256sum") }, transport.Result{Stdout: sum + "\n"})
	}
	return ch, bin
}

func synced(f *transport.Fake) bool {
	return strings.Contains(f.Trace(), "/.deploykit/deploykit") && strings.Contains(f.Trace(), "sync ")
}

func TestDeployBinaryOnlyPutsTheBinaryAndNothingElse(t *testing.T) {
	ch, bin := upgradeHarness(t, "")
	if err := ch.run("deploy", "--binary-only", "--target", "acme", "--server-binary", bin); err != nil {
		t.Fatalf("%v\n%s", err, ch.out)
	}
	trace := ch.sites["acme"].Trace()
	if !strings.Contains(trace, "sync "+bin+" -> /opt/app/.deploykit/deploykit") {
		t.Errorf("the binary is uploaded:\n%s", trace)
	}
	for _, banned := range []string{"server update", "server install", "/.env", "docker", "rsync", "deploykit.yaml", "mkdir"} {
		if strings.Contains(trace, banned) {
			t.Errorf("only the binary goes: %q in\n%s", banned, trace)
		}
	}
	if !strings.Contains(ch.out.String(), "server binary") {
		t.Errorf("the step is reported:\n%s", ch.out)
	}
}

func TestDeployBinaryOnlyDoesNotGoWithTheOptionsOfADeploy(t *testing.T) {
	for _, extra := range [][]string{{"--version", "1.0.0"}, {"--install"}, {"--env", "x.env"}, {"--rollback", "1.0.0"}, {"--gates"}} {
		ch, bin := upgradeHarness(t, "")
		args := append([]string{"deploy", "--binary-only", "--target", "acme", "--server-binary", bin}, extra...)
		err := ch.run(args...)
		if err == nil || !strings.Contains(err.Error(), "--binary-only puts the binary on the server and nothing else") || !strings.Contains(err.Error(), extra[0]) {
			t.Errorf("%v: want a clear refusal naming %s, got %v", extra, extra[0], err)
		}
		if len(ch.sites["acme"].Calls()) != 0 {
			t.Errorf("%v: no server is contacted", extra)
		}
	}
	h := newHarness(newProjectDir(t, cliManifest)) // pm2: no deploykit on the server
	if err := h.run("deploy", "--binary-only", "-t", "demo"); err == nil || !strings.Contains(err.Error(), "--binary-only is for compose stacks") {
		t.Errorf("a pm2 site keeps no deploykit on its server, got %v", err)
	}
}

func TestDeployBinaryOnlyDryRunSendsNothing(t *testing.T) {
	ch, bin := upgradeHarness(t, "")
	if err := ch.run("--dry-run", "deploy", "--binary-only", "--target", "acme", "--server-binary", bin); err != nil {
		t.Fatalf("%v\n%s", err, ch.out)
	}
	if synced(ch.sites["acme"]) {
		t.Errorf("a dry run uploads nothing:\n%s", ch.sites["acme"].Trace())
	}
	if !strings.Contains(ch.out.String(), "would upload "+bin) {
		t.Errorf("the dry run says what it would do:\n%s", ch.out)
	}
}

func TestFleetUpgradeIsInRolloutOrder(t *testing.T) {
	ch, bin := upgradeHarness(t, "")
	if err := ch.run("fleet", "upgrade", "--all", "--server-binary", bin); err != nil {
		t.Fatalf("%v\n%s", err, ch.out)
	}
	if !synced(ch.sites["acme"]) || !synced(ch.sites["beta"]) {
		t.Fatalf("both servers get the binary:\n%s", ch.out)
	}
	out := ch.out.String()
	// beta has ROLLOUT_ORDER=10 and acme 20: beta goes first.
	if b, a := strings.Index(out, "== upgrading the binary of beta"), strings.Index(out, "== upgrading the binary of acme"); b < 0 || a < 0 || b > a {
		t.Errorf("targets are done in rollout order (beta, then acme):\n%s", out)
	}
}

func TestFleetUpgradeAsksWhichTargetsAtATerminal(t *testing.T) {
	ch, bin := upgradeHarness(t, "2\n")
	if err := ch.run("fleet", "upgrade", "--server-binary", bin); err != nil {
		t.Fatalf("%v\n%s", err, ch.out)
	}
	if !strings.Contains(ch.out.String(), "Which targets for 'deploykit fleet upgrade'?") {
		t.Errorf("the question names the command:\n%s", ch.out)
	}
	if synced(ch.sites["acme"]) || !synced(ch.sites["beta"]) {
		t.Errorf("only the marked target (beta) is upgraded:\nacme %s\nbeta %s", ch.sites["acme"].Trace(), ch.sites["beta"].Trace())
	}

	ch, bin = upgradeHarness(t, "1\n")
	ch.app.interactive = func() bool { return false } // a script
	if err := ch.run("fleet", "upgrade", "--server-binary", bin); err == nil || !strings.Contains(err.Error(), "no target selected") {
		t.Errorf("a script must name its targets, got %v", err)
	}
	if synced(ch.sites["acme"]) || synced(ch.sites["beta"]) {
		t.Error("nothing is sent when no target is selected")
	}
}

func TestFleetUpgradeStopsAtTheFirstFailureUnlessToldNotTo(t *testing.T) {
	ch, bin := upgradeHarness(t, "")
	ch.sites["beta"].Fail("test -d", 1, "") // beta is first in rollout order and was never installed
	err := ch.run("fleet", "upgrade", "--all", "--server-binary", bin)
	if err == nil || !strings.Contains(err.Error(), "upgrade failed on beta") {
		t.Fatalf("err = %v", err)
	}
	if synced(ch.sites["acme"]) {
		t.Error("it stops at the first failure: acme is not touched")
	}

	ch, bin = upgradeHarness(t, "")
	ch.sites["beta"].Fail("test -d", 1, "")
	err = ch.run("fleet", "upgrade", "--all", "--continue-on-error", "--server-binary", bin)
	if err == nil || !strings.Contains(err.Error(), "upgrade failed on beta") {
		t.Fatalf("the failure is still the result of the command, got %v", err)
	}
	if !synced(ch.sites["acme"]) {
		t.Error("with --continue-on-error the other target is upgraded")
	}
	if !strings.Contains(ch.out.String(), "beta: FAILED, continuing") {
		t.Errorf("the failure is said as it happens:\n%s", ch.out)
	}
}

func TestFleetUpgradeDryRunSendsNothing(t *testing.T) {
	ch, bin := upgradeHarness(t, "")
	if err := ch.run("--dry-run", "fleet", "upgrade", "--all", "--server-binary", bin); err != nil {
		t.Fatalf("%v\n%s", err, ch.out)
	}
	if synced(ch.sites["acme"]) || synced(ch.sites["beta"]) {
		t.Error("a dry run uploads nothing")
	}
}

// A fleet deploy changes what runs on several servers: it is never implicit, and never a question either.
func TestFleetDeployNeverAsksWhichTargets(t *testing.T) {
	ch := remoteHarness(t, "1,2\n")
	err := ch.run("fleet", "deploy", "--version", "1.0.0")
	if err == nil || !strings.Contains(err.Error(), "a fleet deploy is never implicit") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(ch.out.String(), "Which targets") || len(ch.sites) != 0 {
		t.Errorf("no question and no server contacted:\n%s", ch.out)
	}
}

func sha256Hex(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // a test file
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
