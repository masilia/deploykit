package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/masilia/deploykit/internal/transport"
)

const serverEnvText = "APP_SECRET=s3cretvalue\nDOMAIN=app.acme.fr\nDATABASE_URL=postgresql://u:dbpw@db/x\nAPP_VERSION=1.0.0\nPROXY_PORT=8081\n"

// newEnvHarness is a compose target whose server .env holds serverEnvText.
func newEnvHarness(t *testing.T) (*composeHarness, *transport.Fake) {
	t.Helper()
	ch := newComposeHarness(t, map[string]string{"acme": acmeTarget})
	site := ch.site("acme")
	site.Fail("test -e /opt/app/.lock-ops", 1, "") // no operation holds the lock, unless a test registers a rule before this one
	site.OnFunc(func(s transport.Spec) bool { return strings.Join(s.Argv, " ") == "cat /opt/app/.env" }, transport.Result{Stdout: serverEnvText})
	return ch, site
}

// savedEnv is what the last Save wrote to the server.
func savedEnv(f *transport.Fake) (string, bool) {
	var out string
	var saved bool
	for _, c := range f.Calls() {
		if c.Spec != nil && strings.Contains(strings.Join(c.Spec.Argv, " "), `mv -f "$tmp" "$1"`) {
			out, saved = c.Stdin, true
		}
	}
	return out, saved
}

func TestEnvListMasksTheSecrets(t *testing.T) {
	ch, _ := newEnvHarness(t)
	if err := ch.run("env", "list", "-t", "acme"); err != nil {
		t.Fatalf("%v\n%s", err, ch.out)
	}
	out := ch.out.String()
	for _, want := range []string{"DOMAIN", "app.acme.fr", "PROXY_PORT", "8081", "<secret, 11 chars>", "--reveal"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing lacks %q:\n%s", want, out)
		}
	}
	for _, leak := range []string{"s3cretvalue", "dbpw"} {
		if strings.Contains(out, leak) {
			t.Errorf("the listing leaks %q:\n%s", leak, out)
		}
	}
}

func TestEnvGetNeedsRevealForASecret(t *testing.T) {
	ch, _ := newEnvHarness(t)
	err := ch.run("env", "get", "-t", "acme", "APP_SECRET")
	if err == nil || !strings.Contains(err.Error(), "--reveal") || strings.Contains(ch.out.String(), "s3cretvalue") {
		t.Fatalf("a secret must not print without --reveal: err=%v out=%q", err, ch.out)
	}
	if err := ch.run("env", "get", "-t", "acme", "APP_SECRET", "--reveal"); err != nil || strings.TrimSpace(ch.out.String()) != "s3cretvalue" {
		t.Errorf("--reveal prints the value alone: err=%v out=%q", err, ch.out)
	}
	ch.out.Reset()
	if err := ch.run("env", "get", "-t", "acme", "DOMAIN"); err != nil || strings.TrimSpace(ch.out.String()) != "app.acme.fr" {
		t.Errorf("a plain value prints: err=%v out=%q", err, ch.out)
	}
	if err := ch.run("env", "get", "-t", "acme", "NOPE"); err == nil || !strings.Contains(err.Error(), "not set") {
		t.Errorf("a missing key is an error: %v", err)
	}
}

func TestEnvSetReadsTheValueFromStdinAndNeverEchoesIt(t *testing.T) {
	ch, site := newEnvHarness(t)
	ch.app.in = strings.NewReader("smtp://u:topsecret@mail\n")
	if err := ch.run("env", "set", "-t", "acme", "MAILER_DSN"); err != nil {
		t.Fatalf("%v\n%s", err, ch.out)
	}
	env, saved := savedEnv(site)
	if !saved || !strings.Contains(env, "MAILER_DSN=smtp://u:topsecret@mail\n") || !strings.Contains(env, "DOMAIN=app.acme.fr") {
		t.Errorf("the key is added and the rest kept:\n%s", env)
	}
	out := ch.out.String()
	if strings.Contains(out, "topsecret") || strings.Contains(site.Trace(), "topsecret") {
		t.Errorf("the value must never be printed nor appear in a call:\n%s\n%s", out, site.Trace())
	}
	if !strings.Contains(out, "set MAILER_DSN (new)") || !strings.Contains(out, "--apply") {
		t.Errorf("the output names the key and the next step:\n%s", out)
	}
	if strings.Contains(site.Trace(), "up -d") {
		t.Errorf("without --apply nothing is restarted:\n%s", site.Trace())
	}
}

func TestEnvSetReportsUnchangedAndWritesNothing(t *testing.T) {
	ch, site := newEnvHarness(t)
	ch.app.in = strings.NewReader("app.acme.fr\n")
	if err := ch.run("env", "set", "-t", "acme", "DOMAIN"); err != nil {
		t.Fatal(err)
	}
	if _, saved := savedEnv(site); saved || !strings.Contains(ch.out.String(), "unchanged") {
		t.Errorf("an identical value is not written:\n%s", ch.out)
	}
}

func TestEnvSetRefusesWhatItShouldNot(t *testing.T) {
	ch, site := newEnvHarness(t)
	ch.app.in = strings.NewReader("9.9.9\n")
	if err := ch.run("env", "set", "-t", "acme", "APP_VERSION"); err == nil || !strings.Contains(err.Error(), "deploy") {
		t.Errorf("the version is managed by deploy: %v", err)
	}
	ch.app.in = strings.NewReader("\n")
	if err := ch.run("env", "set", "-t", "acme", "DOMAIN"); err == nil || !strings.Contains(err.Error(), "no value") {
		t.Errorf("an empty value is refused: %v", err)
	}
	ch.app.in = strings.NewReader("x\n")
	if err := ch.run("env", "set", "-t", "acme", "bad key"); err == nil {
		t.Error("an invalid name is refused")
	}
	if err := ch.run("env", "set", "-t", "acme"); err == nil {
		t.Error("a KEY or --from-file is required")
	}
	if _, saved := savedEnv(site); saved {
		t.Error("nothing was written")
	}
}

func TestEnvSetFromFileNeedsAPrivateFile(t *testing.T) {
	ch, site := newEnvHarness(t)
	file := filepath.Join(t.TempDir(), "values.env")
	if err := os.WriteFile(file, []byte("DOMAIN=new.acme.fr\nMAILER_PASSWORD=pw123\n"), 0o644); err != nil { //nolint:gosec // the point of the test
		t.Fatal(err)
	}
	if err := ch.run("env", "set", "-t", "acme", "--from-file", file); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("a file readable by others is refused: %v", err)
	}
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ch.run("env", "set", "-t", "acme", "--from-file", file); err != nil {
		t.Fatalf("%v\n%s", err, ch.out)
	}
	env, _ := savedEnv(site)
	if !strings.Contains(env, "DOMAIN=new.acme.fr") || !strings.Contains(env, "MAILER_PASSWORD=pw123") || strings.Contains(ch.out.String(), "pw123") {
		t.Errorf("both keys are set and no value is printed:\n%s\n--\n%s", env, ch.out)
	}
}

func TestEnvSetApplyRecreatesTheServicesUnlessAnOperationHoldsTheLock(t *testing.T) {
	ch, site := newEnvHarness(t)
	ch.app.in = strings.NewReader("new.acme.fr\n")
	if err := ch.run("env", "set", "-t", "acme", "DOMAIN", "--apply"); err != nil {
		t.Fatalf("%v\n%s", err, ch.out)
	}
	if trace := site.Trace(); !strings.Contains(trace, "docker compose -p acme") || !strings.Contains(trace, "up -d") {
		t.Errorf("--apply runs compose up -d for the project:\n%s", trace)
	}

	// A lock held by an update: the .env is still saved, but nothing is restarted.
	ch2 := newComposeHarness(t, map[string]string{"acme": acmeTarget})
	locked := ch2.site("acme")
	locked.OnFunc(func(s transport.Spec) bool { return strings.Join(s.Argv, " ") == "cat /opt/app/.env" }, transport.Result{Stdout: serverEnvText})
	ch2.app.in = strings.NewReader("new.acme.fr\n")
	err := ch2.run("env", "set", "-t", "acme", "DOMAIN", "--apply")
	if err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("an operation holds the lock: %v", err)
	}
	if _, saved := savedEnv(locked); !saved || strings.Contains(locked.Trace(), "up -d") {
		t.Errorf("saved, not applied:\n%s", locked.Trace())
	}
}

func TestEnvSetDryRunWritesNothing(t *testing.T) {
	ch, site := newEnvHarness(t)
	ch.app.in = strings.NewReader("new.acme.fr\n")
	if err := ch.run("--dry-run", "env", "set", "-t", "acme", "DOMAIN", "--apply"); err != nil {
		t.Fatal(err)
	}
	if _, saved := savedEnv(site); saved || !strings.Contains(ch.out.String(), "would set DOMAIN (changed)") || strings.Contains(site.Trace(), "up -d") {
		t.Errorf("a dry run changes nothing:\n%s\n%s", ch.out, site.Trace())
	}
}

func TestTargetSecretsFileIsLoadedOnlyWhenPrivate(t *testing.T) {
	ch := newComposeHarness(t, map[string]string{"acme": acmeTarget})
	p, err := ch.app.load()
	if err != nil {
		t.Fatal(err)
	}
	if as, path, err := loadTargetSecrets(p, "acme"); err != nil || as != nil || path != "" {
		t.Errorf("no file, no assignments: %v %q %v", as, path, err)
	}
	file := targetSecretsPath(p, "acme")
	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("MAILER_PASSWORD=pw\nDOMAIN=x.acme.fr\n"), 0o640); err != nil { //nolint:gosec // the point of the test
		t.Fatal(err)
	}
	if _, _, err := loadTargetSecrets(p, "acme"); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("a group-readable secrets file is refused: %v", err)
	}
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	as, path, err := loadTargetSecrets(p, "acme")
	if err != nil || len(as) != 2 || as[0].Key != "MAILER_PASSWORD" || path != file {
		t.Errorf("as=%v path=%q err=%v", as, path, err)
	}
}

func TestComposeDeployInjectsTheTargetSecretsFileAndAnExplicitEnvWins(t *testing.T) {
	ch := newComposeHarness(t, map[string]string{"acme": acmeTarget})
	site := ch.site("acme")
	site.OnFunc(func(sp transport.Spec) bool { return strings.Join(sp.Argv, " ") == "cat /opt/app/.env" }, transport.Result{Stdout: "APP_VERSION=1.0.0\nDOMAIN=app.acme.fr\n"})
	shas := 0
	site.OnFunc(func(sp transport.Spec) bool {
		if !strings.Contains(sp.Script, "sha256sum") {
			return false
		}
		shas++
		return shas == 1
	}, transport.Result{})
	bin := filepath.Join(ch.repo, "server-binary")
	site.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "sha256sum") }, transport.Result{Stdout: sha(t, bin) + "\n"})

	secrets := filepath.Join(ch.repo, "secrets", "acme.env")
	if err := os.MkdirAll(filepath.Dir(secrets), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secrets, []byte("MAILER_PASSWORD=pw-from-file\nDOMAIN=from-file.acme.fr\n"), 0o644); err != nil { //nolint:gosec // the point of the test
		t.Fatal(err)
	}
	if err := ch.run("deploy", "--version", "1.1.0", "--server-binary", bin); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("a secrets file readable by others stops the deploy: %v", err)
	}
	if err := os.Chmod(secrets, 0o600); err != nil {
		t.Fatal(err)
	}
	flagFile := filepath.Join(t.TempDir(), "explicit.env")
	if err := os.WriteFile(flagFile, []byte("DOMAIN=from-flag.acme.fr\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ch.run("deploy", "--version", "1.1.0", "--server-binary", bin, "--env", flagFile); err != nil {
		t.Fatalf("%v\n%s", err, ch.out)
	}
	env, saved := savedEnv(site)
	if !saved || !strings.Contains(env, "MAILER_PASSWORD=pw-from-file") || !strings.Contains(env, "DOMAIN=from-flag.acme.fr") || strings.Contains(env, "from-file.acme.fr") {
		t.Errorf("the file's keys are injected and --env wins over it:\n%s", env)
	}
	if strings.Contains(site.Trace(), "pw-from-file") || strings.Contains(ch.out.String(), "pw-from-file") {
		t.Error("the values travel on stdin and are never printed")
	}
	if !strings.Contains(ch.out.String(), "2 variable(s) from "+secrets) {
		t.Errorf("the deploy says where the variables came from:\n%s", ch.out)
	}
}
