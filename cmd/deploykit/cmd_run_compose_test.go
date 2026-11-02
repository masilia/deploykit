package main

import (
	"strings"
	"testing"

	"github.com/masilia/deploykit/internal/transport"
)

const runEnvText = "APP_VERSION=1.0.0\nADMIN_EMAIL=boss@example.com\nADMIN_FIRST_NAME=Boss\nADMIN_LAST_NAME=Man\nADMIN_PASSWORD=s3cretadminpw\n"

// newRunHarness is a compose target (the compose-app example) whose .env holds runEnvText and whose lock is free.
func newRunHarness(t *testing.T) (*composeHarness, *transport.Fake) {
	t.Helper()
	ch := newComposeHarness(t, map[string]string{"acme": acmeTarget})
	site := ch.site("acme")
	site.Fail("test -e /opt/app/.lock-ops", 1, "") // free, unless a test registers a rule before this one
	site.OnFunc(func(s transport.Spec) bool { return strings.Join(s.Argv, " ") == "cat /opt/app/.env" }, transport.Result{Stdout: runEnvText})
	return ch, site
}

func TestRunListsTheComposeCommandsWithoutAnySecret(t *testing.T) {
	ch, site := newRunHarness(t)
	if err := ch.run("run", "-t", "acme"); err != nil {
		t.Fatalf("%v\n%s", err, ch.out)
	}
	out := ch.out.String()
	for _, want := range []string{"migrate", "doctrine:migrations:migrate", "create_admin", "uses from .env: ADMIN_PASSWORD", "--exec"} {
		if !strings.Contains(out, want) {
			t.Errorf("the list lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "s3cretadminpw") || strings.Contains(site.Trace(), "docker compose") {
		t.Errorf("listing reads no value and starts nothing:\n%s\n%s", out, site.Trace())
	}
}

func TestRunStartsAOneOffContainerOfTheAppService(t *testing.T) {
	ch, site := newRunHarness(t)
	if err := ch.run("run", "migrate", "-t", "acme"); err != nil {
		t.Fatalf("%v\n%s", err, ch.out)
	}
	trace := site.Trace()
	for _, want := range []string{"docker compose -p acme", "run --rm api sh -c", "doctrine:migrations:migrate -n"} {
		if !strings.Contains(trace, want) {
			t.Errorf("trace lacks %q:\n%s", want, trace)
		}
	}
	if strings.Contains(trace, "exec -T") {
		t.Errorf("the default is a one-off container:\n%s", trace)
	}
}

func TestRunPassesASecretByEnvironmentNameOnly(t *testing.T) {
	ch, site := newRunHarness(t)
	if err := ch.run("run", "create_admin", "-t", "acme"); err != nil {
		t.Fatalf("%v\n%s", err, ch.out)
	}
	trace := site.Trace()
	if !strings.Contains(trace, "-e APP_CREATE_USER_PASSWORD") || !strings.Contains(trace, "--email=") {
		t.Errorf("the secret is named, the plain values are rendered:\n%s", trace)
	}
	if strings.Contains(trace, "s3cretadminpw") || strings.Contains(ch.out.String(), "s3cretadminpw") {
		t.Errorf("the password must never be in a call or in the output:\n%s\n%s", trace, ch.out)
	}
}

func TestRunExecUsesTheRunningContainerWithTheSameSecretHandling(t *testing.T) {
	ch, site := newRunHarness(t)
	if err := ch.run("run", "create_admin", "-t", "acme", "--exec"); err != nil {
		t.Fatalf("%v\n%s", err, ch.out)
	}
	trace := site.Trace()
	if !strings.Contains(trace, "exec -T -e APP_CREATE_USER_PASSWORD api sh -c") || strings.Contains(trace, "run --rm") || strings.Contains(trace, "s3cretadminpw") {
		t.Errorf("--exec runs in the running container, secret by name:\n%s", trace)
	}
}

func TestRunRefusesAnUnknownCommandAndANeededSecretThatIsMissing(t *testing.T) {
	ch, site := newRunHarness(t)
	err := ch.run("run", "reindex", "-t", "acme")
	if err == nil || !strings.Contains(err.Error(), "not a command of this manifest") || !strings.Contains(err.Error(), "migrate") {
		t.Errorf("an unknown command lists the known ones: %v", err)
	}
	if strings.Contains(site.Trace(), "run --rm") {
		t.Error("nothing may start")
	}
	// A secret that the command needs but the .env lacks stops before any container starts.
	ch2 := newComposeHarness(t, map[string]string{"acme": acmeTarget})
	s2 := ch2.site("acme")
	s2.Fail("test -e /opt/app/.lock-ops", 1, "")
	s2.OnFunc(func(s transport.Spec) bool { return strings.Join(s.Argv, " ") == "cat /opt/app/.env" }, transport.Result{Stdout: "ADMIN_EMAIL=a@b.c\nADMIN_FIRST_NAME=A\nADMIN_LAST_NAME=B\n"})
	if err := ch2.run("run", "create_admin", "-t", "acme"); err == nil || !strings.Contains(err.Error(), "ADMIN_PASSWORD is missing") || strings.Contains(s2.Trace(), "run --rm") {
		t.Errorf("a missing secret is reported and nothing starts: %v\n%s", err, s2.Trace())
	}
}

func TestRunRefusesWhileAnOperationHoldsTheLock(t *testing.T) {
	ch := newComposeHarness(t, map[string]string{"acme": acmeTarget})
	site := ch.site("acme") // no rule for the lock: the Fake answers success, so the lock is held
	site.OnFunc(func(s transport.Spec) bool { return strings.Join(s.Argv, " ") == "cat /opt/app/.env" }, transport.Result{Stdout: runEnvText})
	err := ch.run("run", "migrate", "-t", "acme")
	if err == nil || !strings.Contains(err.Error(), "holds the lock") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(site.Trace(), "run --rm") {
		t.Errorf("nothing may start while the lock is held:\n%s", site.Trace())
	}
}

func TestRunDryRunStartsNothing(t *testing.T) {
	ch, site := newRunHarness(t)
	if err := ch.run("--dry-run", "run", "migrate", "-t", "acme"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ch.out.String(), "would run migrate in a one-off container of api") || strings.Contains(site.Trace(), "run --rm") {
		t.Errorf("a dry run only says what it would do:\n%s\n%s", ch.out, site.Trace())
	}
}
