package compose

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/runner"
	"github.com/masilia/deploykit/internal/transport"
)

func TestRollbackDecision(t *testing.T) {
	for _, tc := range []struct {
		cur, prev string
		auto      bool
		want      bool
	}{
		{"1.1.0", "1.0.0", true, true},
		{"1.1.0", "1.0.0", false, false}, // AUTO_ROLLBACK=0
		{"1.1.0", "", true, false},       // nothing to go back to
		{"1.1.0", "unknown", true, false},
		{"1.1.0", "1.1.0", true, false}, // a redeploy of the same version
		{"", "1.0.0", true, true},
	} {
		if got := RollbackDecision(tc.cur, tc.prev, tc.auto); got != tc.want {
			t.Errorf("RollbackDecision(%q, %q, %v) = %v", tc.cur, tc.prev, tc.auto, got)
		}
	}
}

type updateFixture struct {
	u        *Update
	fake     *transport.Fake
	messages []string
	upCalls  int
	mu       sync.Mutex
}

const serverEnv = "REGISTRY=ghcr.io/acme\nAPP_VERSION=1.1.0\nAPP_VERSION_PREV=1.0.0\nDOMAIN=app.example\nBACKUP_PASSPHRASE=pw\nPOSTGRES_USER=acme\nADMIN_EMAIL=a@b.c\nAPP_SECRET=already-set\n"

// newUpdate builds an Update over a Fake server where everything succeeds.
func newUpdate(t *testing.T, env string) *updateFixture { return newUpdateOpts(t, env, nil) }

// newUpdateOpts is newUpdate with failure injection: pre registers rules that must win over the success rules (the
// first matching rule wins), so it runs before them.
func newUpdateOpts(t *testing.T, env string, pre func(fx *updateFixture)) *updateFixture {
	t.Helper()
	s, f := newStack(t, "certbot")
	fx := &updateFixture{fake: f}
	// Rules are checked in order; the first match wins.
	f.OnFunc(func(sp transport.Spec) bool { // count `up -d` calls without answering
		if strings.HasSuffix(strings.Join(sp.Argv, " "), "up -d") {
			fx.mu.Lock()
			fx.upCalls++
			fx.mu.Unlock()
		}
		return false
	}, transport.Result{})
	if pre != nil {
		pre(fx)
	}
	f.OnFunc(func(sp transport.Spec) bool { return strings.Join(sp.Argv, " ") == "cat /opt/app/.env" }, transport.Result{Stdout: env})
	f.OnFunc(func(sp transport.Spec) bool { return strings.Join(sp.Argv, " ") == "mktemp -d" }, transport.Result{Stdout: "/tmp/tmp.upd\n"})
	f.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "df -Pm") }, transport.Result{Stdout: "9000\n"})
	f.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "docker info") }, transport.Result{Stdout: "/var/lib/docker\n"})
	f.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "docker logs --since") }, transport.Result{})
	f.OnFunc(func(sp transport.Spec) bool { return sp.Env["DK_SVC"] != "" }, transport.Result{Stdout: "true healthy\n"})
	f.OnFunc(func(sp transport.Spec) bool { return len(sp.Argv) > 0 && sp.Argv[0] == "curl" }, transport.Result{Stdout: `{"status":"ok"}`})

	m, err := manifest.Load("../../examples/compose-app/deploykit.yaml")
	if err != nil {
		t.Fatal(err)
	}
	envSpec, _ := m.EnvConfig()
	data, backup, _ := m.BackupConfig()
	fx.u = &Update{
		Stack: s, Env: envSpec, BaseURL: "https://app.example", PID: 4242,
		Images: []string{"frontend", "backend"},
		Now:    func() time.Time { return time.Date(2026, 10, 27, 14, 0, 0, 0, time.UTC) },
		Notify: func(_ context.Context, text string) { fx.messages = append(fx.messages, text) },
		Crons:  []CronEntry{{Name: "backup", Schedule: "30 2 * * *", Command: "deploykit server backup"}},
	}
	// Backup works on the same Fake, with the server .env as its environment.
	envDoc, _ := (RemoteEnv{T: f, Path: "/opt/app/.env"}).Load(context.Background())
	fx.u.Backup = &Backup{Stack: s, Data: data, Cfg: backup, Env: envDoc, Now: fx.u.Now}
	return fx
}

func (fx *updateFixture) run(t *testing.T) error {
	t.Helper()
	return fx.u.Run(t.Context(), &runner.Runner{})
}

// inOrderTrace checks that each fragment appears after the previous one.
func inOrderTrace(t *testing.T, trace string, fragments ...string) {
	t.Helper()
	pos := 0
	for _, f := range fragments {
		i := strings.Index(trace[pos:], f)
		if i < 0 {
			t.Fatalf("expected %q after offset %d in:\n%s", f, pos, trace)
		}
		pos += i + len(f)
	}
}

func TestUpdateSucceeds(t *testing.T) {
	fx := newUpdate(t, serverEnv)
	if err := fx.run(t); err != nil {
		t.Fatal(err)
	}
	trace := fx.fake.Trace()
	inOrderTrace(t, trace,
		"$ test -f .installed",
		"env DK_LOCK=.lock-ops", // lock
		"docker info",           // disk check
		"pg_dump",               // backup
		"$ docker compose -p acme -f docker-compose.prod.yml pull",
		"doctrine:migrations:migrate -n", // migrations
		"app:data:backfill",              // post-migration hook
		"$ docker compose -p acme -f docker-compose.prod.yml up -d",
		"exec -T proxy nginx -s reload",
		"doctrine:schema:validate --skip-sync", // ORM mapping check after the probes
		"cat >> \"$1\"",                        // journal
		"docker image prune -f",
		"crontab -",
		"$ rm -rf .lock-ops", // the lock is released last
	)
	// New secrets are generated, existing ones are kept, and the .env is saved through stdin.
	var saved string
	for _, c := range fx.fake.Calls() {
		if c.Spec != nil && len(c.Spec.Argv) > 0 && c.Spec.Argv[0] == "sh" && strings.Contains(strings.Join(c.Spec.Argv, " "), `mv -f "$tmp" "$1"`) {
			saved = c.Stdin
		}
	}
	if !strings.Contains(saved, "APP_SECRET=already-set") || !strings.Contains(saved, "HEALTH_TOKEN=") || !strings.Contains(saved, "JWT_PASSPHRASE=") {
		t.Errorf("saved .env = %q", saved)
	}
	if strings.Contains(fx.fake.Trace(), "JWT_PASSPHRASE=") && !strings.Contains(fx.fake.Trace(), "stdin <") {
		t.Error("the secrets must travel on stdin")
	}
	if len(fx.messages) != 1 || !strings.Contains(fx.messages[0], "1.1.0") || !strings.Contains(fx.messages[0], "1.0.0") {
		t.Errorf("messages = %v", fx.messages)
	}
	journal := ""
	for _, c := range fx.fake.Calls() {
		if c.Stdin != "" && strings.Contains(c.Stdin, "\tOK   ") {
			journal = c.Stdin
		}
	}
	if journal != "2026-10-27 14:00:00\tOK   1.1.0 (replaces 1.0.0)\n" {
		t.Errorf("journal = %q", journal)
	}
}

// failHealthUntil makes the HTTP probes fail until the stack has been restarted upCalls times.
func (fx *updateFixture) failHealthUntil(upCalls int) {
	fx.fake.OnFunc(func(sp transport.Spec) bool {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		return len(sp.Argv) > 0 && sp.Argv[0] == "curl" && fx.upCalls < upCalls
	}, transport.Result{ExitCode: 22, Stderr: "HTTP 502"})
}

func newUpdateWithFailingHealth(t *testing.T, env string, upCalls int) *updateFixture {
	return newUpdateOpts(t, env, func(fx *updateFixture) { fx.failHealthUntil(upCalls) })
}

func TestUpdateRollsBackAutomatically(t *testing.T) {
	fx := newUpdateWithFailingHealth(t, serverEnv, 2)
	err := fx.run(t)
	if err == nil || !strings.Contains(err.Error(), "automatic return to 1.0.0 done and verified") {
		t.Fatalf("err = %v", err)
	}
	trace := fx.fake.Trace()
	inOrderTrace(t, trace, "up -d", "curl", "up -d") // restart, failing probe, restart again (the rollback)
	if got := strings.Join(fx.journal(), "|"); got != "2026-10-27 14:00:00\tKO   1.1.0 (after 1.0.0)|2026-10-27 14:00:00\tROLLBACK 1.0.0 (failure of 1.1.0)" {
		t.Errorf("journal = %q", got)
	}
	if saved := fx.savedEnvs(); len(saved) == 0 || !strings.Contains(saved[len(saved)-1], "APP_VERSION=1.0.0\n") {
		t.Errorf("the rollback must write APP_VERSION=1.0.0 to .env, saved: %q", saved)
	}
	if fx.upCalls != 2 {
		t.Errorf("up -d ran %d times, want 2 (update, rollback)", fx.upCalls)
	}
	if len(fx.messages) != 1 || !strings.Contains(fx.messages[0], "automatic return to 1.0.0") {
		t.Errorf("messages = %v", fx.messages)
	}
	if strings.Contains(trace, "docker image prune") {
		t.Error("a failed update must not clean images (the previous ones are needed)")
	}
	if !strings.Contains(trace, "$ rm -rf .lock-ops") {
		t.Error("the lock must be released after a failed update")
	}
}

func TestUpdateReportsAFailedRollback(t *testing.T) {
	fx := newUpdateWithFailingHealth(t, serverEnv, 99) // never healthy again
	err := fx.run(t)
	if err == nil || !strings.Contains(err.Error(), "AND the return to 1.0.0 failed") {
		t.Fatalf("err = %v", err)
	}
	if len(fx.messages) != 1 || !strings.Contains(fx.messages[0], "manual intervention") {
		t.Errorf("messages = %v", fx.messages)
	}
}

func TestUpdateWithAutoRollbackDisabled(t *testing.T) {
	fx := newUpdateWithFailingHealth(t, serverEnv+"AUTO_ROLLBACK=0\n", 99)
	err := fx.run(t)
	if err == nil || !strings.Contains(err.Error(), "manual rollback: set APP_VERSION=1.0.0") {
		t.Fatalf("err = %v", err)
	}
	if fx.upCalls != 1 {
		t.Errorf("up -d ran %d times: with AUTO_ROLLBACK=0 nothing may be restarted again", fx.upCalls)
	}
	if len(fx.messages) != 1 || !strings.Contains(fx.messages[0], "disabled or impossible") {
		t.Errorf("messages = %v", fx.messages)
	}
}

func TestUpdateWithNoPreviousVersionCannotRollBack(t *testing.T) {
	env := strings.Replace(serverEnv, "APP_VERSION_PREV=1.0.0\n", "", 1)
	fx := newUpdateWithFailingHealth(t, env, 99)
	err := fx.run(t)
	if err == nil || !strings.Contains(err.Error(), "manual rollback") || fx.upCalls != 1 {
		t.Fatalf("err = %v, up calls = %d", err, fx.upCalls)
	}
}

func TestUpdateStopsWhenMigrationsFail(t *testing.T) {
	fx := newUpdateOpts(t, serverEnv, func(fx *updateFixture) {
		fx.fake.Fail("doctrine:migrations:migrate", 1, "SQLSTATE[42P07] relation exists")
	})
	err := fx.run(t)
	if err == nil || !strings.Contains(err.Error(), "migrations failed") || !strings.Contains(err.Error(), "set APP_VERSION=1.0.0") {
		t.Fatalf("err = %v", err)
	}
	trace := fx.fake.Trace()
	if strings.Contains(trace, "up -d") || strings.Contains(trace, "app:data:backfill") {
		t.Errorf("nothing may start or run after a failed migration:\n%s", trace)
	}
	if !strings.Contains(trace, "$ rm -rf .lock-ops") {
		t.Error("the lock must be released")
	}
}

func TestUpdateRefusesWithoutAnInstallation(t *testing.T) {
	fx := newUpdateOpts(t, serverEnv, func(fx *updateFixture) { fx.fake.Fail("test -f .installed", 1, "") })
	err := fx.run(t)
	if err == nil || !strings.Contains(err.Error(), "no installation found") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(fx.fake.Trace(), "DK_LOCK") {
		t.Error("no lock may be taken for an uninstalled stack")
	}
}

func TestUpdateRefusesWhileAnotherOperationRuns(t *testing.T) {
	fx := newUpdateOpts(t, serverEnv, func(fx *updateFixture) {
		fx.fake.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, `mkdir "$DK_LOCK"`) }, transport.Result{Stdout: "777\n", ExitCode: 3})
	})
	err := fx.run(t)
	if err == nil || !strings.Contains(err.Error(), "pid 777") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(fx.fake.Trace(), "pg_dump") || strings.Contains(fx.fake.Trace(), "pull") {
		t.Error("nothing may run while the lock is held")
	}
	if strings.Contains(fx.fake.Trace(), "rm -rf .lock-ops") {
		t.Error("the lock of the other operation must not be removed")
	}
}

func TestUpdateRejectsAnUnknownStep(t *testing.T) {
	fx := newUpdate(t, serverEnv)
	fx.u.Stack.Cfg.Update.Steps = []string{"lock", "teleport"}
	if err := fx.run(t); err == nil || !strings.Contains(err.Error(), "unknown update step") {
		t.Errorf("err = %v", err)
	}
}

func TestUpdateDryRunChangesNothing(t *testing.T) {
	fx := newUpdate(t, serverEnv)
	if err := fx.u.Run(t.Context(), &runner.Runner{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	trace := fx.fake.Trace()
	for _, forbidden := range []string{"pull", "up -d", "pg_dump", "DK_LOCK", "migrate"} {
		if strings.Contains(trace, forbidden) {
			t.Errorf("a dry run must not run %q:\n%s", forbidden, trace)
		}
	}
}

func TestManifestStepNamesAreValidated(t *testing.T) {
	ok, err := manifest.Parse([]byte(`version: 1
project: p
artifact: { type: image, registry: ghcr.io/o, images: [{ name: a, dockerfile: D }] }
runtime: { type: compose, files: [c.yml], services: [{ name: a, role: app }], update: { steps: [lock, verify] } }
ingress: { type: standalone }
`))
	if err != nil || ok == nil {
		t.Fatalf("valid steps: %v", err)
	}
	_, err = manifest.Parse([]byte(`version: 1
project: p
artifact: { type: image, registry: ghcr.io/o, images: [{ name: a, dockerfile: D }] }
runtime: { type: compose, files: [c.yml], services: [{ name: a, role: app }], update: { steps: [lock, teleport] } }
ingress: { type: standalone }
`))
	if err == nil || !strings.Contains(err.Error(), "teleport") {
		t.Errorf("err = %v", err)
	}
}

// journal returns the lines appended to versions.log, in order.
func (fx *updateFixture) journal() []string {
	var out []string
	for _, c := range fx.fake.Calls() {
		if c.Spec != nil && strings.Contains(strings.Join(c.Spec.Argv, " "), `cat >> "$1"`) {
			out = append(out, strings.TrimRight(c.Stdin, "\n"))
		}
	}
	return out
}

// savedEnvs returns every .env content written to the server, in order.
func (fx *updateFixture) savedEnvs() []string {
	var out []string
	for _, c := range fx.fake.Calls() {
		if c.Spec != nil && strings.Contains(strings.Join(c.Spec.Argv, " "), `mv -f "$tmp" "$1"`) {
			out = append(out, c.Stdin)
		}
	}
	return out
}

// A release may re-encrypt stored secrets after the health check. It must run after a verified update, never on the
// rollback path, and its failure must not fail the update.
func TestUpdateRunsPostUpdateCommandsAfterVerification(t *testing.T) {
	fx := newUpdate(t, serverEnv)
	if err := fx.run(t); err != nil {
		t.Fatal(err)
	}
	inOrderTrace(t, fx.fake.Trace(), "doctrine:schema:validate --skip-sync", "app:secrets:rotate")
	if !strings.Contains(fx.fake.Trace(), "docker image prune -f") {
		t.Error("images are still cleaned after a successful update")
	}

	failing := newUpdateOpts(t, serverEnv, func(fx *updateFixture) { fx.fake.Fail("app:secrets:rotate", 1, "unreadable=2") })
	if err := failing.run(t); err != nil {
		t.Fatalf("a failing post-update command must not fail the update: %v", err)
	}
	if len(failing.journal()) != 1 || !strings.Contains(failing.journal()[0], "OK   1.1.0") {
		t.Errorf("the update must still be journaled OK: %v", failing.journal())
	}

	rolled := newUpdateWithFailingHealth(t, serverEnv, 2)
	if err := rolled.run(t); err == nil {
		t.Fatal("want the update to fail")
	}
	if strings.Contains(rolled.fake.Trace(), "app:secrets:rotate") {
		t.Error("secrets must not be re-encrypted when the update failed and was rolled back")
	}
}
