package compose

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/masilia/deploykit/internal/envfile"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/runner"
	"github.com/masilia/deploykit/internal/tmpl"
	"github.com/masilia/deploykit/internal/transport"
)

func restoreSpec(t *testing.T) *manifest.RestoreTestSpec {
	t.Helper()
	m, err := manifest.Load("../../examples/compose-app/deploykit.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.RestoreTestConfig()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestEvaluateRestore(t *testing.T) {
	spec := restoreSpec(t) // 0.9 of the tables, doctrine_migration_versions with at least one row
	for _, tc := range []struct {
		live, restored, migrations int
		ok                         bool
	}{
		{100, 100, 5, true},
		{100, 90, 5, true},
		{100, 89, 5, false},
		{11, 9, 1, true}, // 11*9/10 = 9 in integer arithmetic, as the shell did
		{11, 8, 1, false},
		{10, 9, 1, true},
		{0, 0, 1, true},
		{100, 100, 0, false}, // no migration recorded
	} {
		err := EvaluateRestore(tc.live, tc.restored, tc.migrations, spec)
		if (err == nil) != tc.ok {
			t.Errorf("live %d restored %d migrations %d: err = %v, want ok=%v", tc.live, tc.restored, tc.migrations, err, tc.ok)
		}
	}
}

func TestRestoreTestConfigValidation(t *testing.T) {
	base := func(extra string) string {
		return `version: 1
project: p
artifact: { type: image, registry: ghcr.io/o, images: [{ name: a, dockerfile: D }] }
runtime: { type: compose, files: [c.yml], services: [{ name: a, role: app }] }
ingress: { type: standalone }
restore_test: { ` + extra + ` }
`
	}
	for extra, want := range map[string]string{
		"throwaway_database: 'x; drop database prod'": "plain identifier",
		"checks: { min_tables_ratio: 1.5 }":           "between 0 and 1",
		"checks: { migrations_table: 'a b' }":         "plain identifier",
		"checks: { surprise: true }":                  "surprise",
	} {
		if _, err := manifest.Parse([]byte(base(extra))); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", extra, err, want)
		}
	}
	if _, err := manifest.Parse([]byte(base("schedule: '0 4 2 * *'"))); err != nil {
		t.Errorf("defaults: %v", err)
	}
}

func newRestoreTest(t *testing.T, pre func(*transport.Fake)) (*RestoreTest, *transport.Fake, *[]string) {
	t.Helper()
	s, f := newStack(t, "certbot")
	if pre != nil {
		pre(f)
	}
	f.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "ls -t") }, transport.Result{Stdout: "/opt/app/backups/acme-backup-20261026-023000.tar.gpg\n"})
	f.OnFunc(func(sp transport.Spec) bool { return strings.Join(sp.Argv, " ") == "mktemp -d" }, transport.Result{Stdout: "/tmp/tmp.rt\n"})
	f.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "psql") }, transport.Result{Stdout: "40\n"})
	data, backup, _ := mustBackup(t)
	var messages []string
	return &RestoreTest{
		Stack: s, Data: data, Backup: backup, Spec: restoreSpec(t), Env: envfile.Parse([]byte("BACKUP_PASSPHRASE=restore-pass\n")),
		Domain: "app.example", PID: 99,
		Notify: func(_ context.Context, text string) { messages = append(messages, text) },
	}, f, &messages
}

func mustBackup(t *testing.T) (*manifest.DataSpec, *manifest.BackupSpec, error) {
	t.Helper()
	m, _ := manifest.Load("../../examples/compose-app/deploykit.yaml")
	return m.BackupConfig()
}

func TestRestoreTestSucceeds(t *testing.T) {
	rt, f, messages := newRestoreTest(t, nil)
	if err := rt.Run(t.Context(), &runner.Runner{}); err != nil {
		t.Fatal(err)
	}
	trace := f.Trace()
	inOrderTrace(t, trace,
		"env DK_LOCK=.lock-ops",
		"ls -t",
		"sha256sum -c",
		"secret DK_PASS=***",
		"tar -tzf \"$DK_TMP/documents.tar.gz\"",
		"createdb -U",
		"pg_restore -U",
		"psql -U",
		"dropdb -U", // the throwaway database is dropped
		"$ rm -rf /tmp/tmp.rt",
		"$ rm -rf .lock-ops",
	)
	if strings.Contains(trace, "restore-pass") {
		t.Error("the passphrase leaked into the trace")
	}
	if len(*messages) != 1 || !strings.Contains((*messages)[0], "Restore test passed") {
		t.Errorf("messages = %v", *messages)
	}
	// The live database is read, never written: the only DROP is of the throwaway database.
	for _, c := range f.Calls() {
		if c.Spec != nil && strings.Contains(c.Spec.Script, "dropdb") && c.Spec.Env["DK_NAME"] != "restore_check" {
			t.Errorf("dropdb must only target the throwaway database, got %q", c.Spec.Env["DK_NAME"])
		}
	}
}

func TestRestoreTestFailureStillCleansUpAndAlerts(t *testing.T) {
	rt, f, messages := newRestoreTest(t, func(f *transport.Fake) {
		f.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "pg_restore") }, transport.Result{ExitCode: 1, Stderr: "pg_restore: error: could not execute query"})
	})
	err := rt.Run(t.Context(), &runner.Runner{})
	if err == nil || !strings.Contains(err.Error(), "pg_restore failed") {
		t.Fatalf("err = %v", err)
	}
	trace := f.Trace()
	if !strings.Contains(trace, "dropdb -U") || !strings.Contains(trace, "$ rm -rf /tmp/tmp.rt") || !strings.Contains(trace, "$ rm -rf .lock-ops") {
		t.Errorf("cleanup must run after a failure:\n%s", trace)
	}
	if len(*messages) != 1 || !strings.Contains((*messages)[0], "FAILED") || !strings.Contains((*messages)[0], "acme-backup-20261026-023000") {
		t.Errorf("messages = %v", *messages)
	}
}

func TestRestoreTestWithNoBackup(t *testing.T) {
	rt, _, messages := newRestoreTest(t, func(f *transport.Fake) {
		f.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "ls -t") }, transport.Result{})
	})
	if err := rt.Run(t.Context(), &runner.Runner{}); err == nil || !strings.Contains(err.Error(), "no backup found") {
		t.Errorf("err = %v", err)
	}
	if len(*messages) != 1 || !strings.Contains((*messages)[0], "FAILED") {
		t.Errorf("messages = %v", *messages)
	}
}

func TestRestoreTestDetectsAnIncompleteRestore(t *testing.T) {
	rt, _, _ := newRestoreTest(t, nil)
	calls := 0
	// First psql (live) says 100 tables, the restored database only 50.
	fake := transport.NewFake("srv")
	fake.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "ls -t") }, transport.Result{Stdout: "/opt/app/backups/b.tar.gpg\n"})
	fake.OnFunc(func(sp transport.Spec) bool { return strings.Join(sp.Argv, " ") == "mktemp -d" }, transport.Result{Stdout: "/tmp/x\n"})
	fake.OnFunc(func(sp transport.Spec) bool {
		if strings.Contains(sp.Script, "psql") {
			calls++
			return calls == 1
		}
		return false
	}, transport.Result{Stdout: "100\n"})
	fake.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "psql") }, transport.Result{Stdout: "50\n"})
	rt.Stack.T = fake
	err := rt.Run(t.Context(), &runner.Runner{})
	if err == nil || !strings.Contains(err.Error(), "incomplete schema: 50 tables restored for 100") {
		t.Errorf("err = %v", err)
	}
}

func TestRestoreTestNeedsPostgres(t *testing.T) {
	rt, f, _ := newRestoreTest(t, nil)
	rt.Data.Database.Type = "mysql"
	if err := rt.Run(t.Context(), &runner.Runner{}); err == nil || !strings.Contains(err.Error(), "postgres") {
		t.Errorf("err = %v", err)
	}
	if len(f.Calls()) != 0 {
		t.Error("nothing may run for an unsupported database")
	}
}

// End to end with a real backup made by Backup (real gpg), restored through the fake docker + fake psql tools.
func TestRestoreTestEndToEnd(t *testing.T) {
	for _, tool := range []string{"gpg", "tar", "sha256sum", "find"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	bk, root := realBackup(t, "echo FAKE-DUMP", "grep -q FAKE-DUMP")
	if err := bk.Run(t.Context(), &runner.Runner{}); err != nil {
		t.Fatal(err)
	}

	// Fake database tools: psql prints fixed counts, pg_restore checks it is fed the dump, the others succeed.
	bin := t.TempDir()
	for name, body := range map[string]string{
		"psql":       "echo 12\n",
		"pg_restore": "grep -q FAKE-DUMP\n",
		"createdb":   "exit 0\n",
		"dropdb":     "echo \"$@\" >> \"$DK_DROPLOG\"\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o700); err != nil { //nolint:gosec // test doubles
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dropLog := filepath.Join(t.TempDir(), "drops")
	t.Setenv("DK_DROPLOG", dropLog)

	rt := &RestoreTest{
		Stack: bk.Stack, Data: bk.Data, Backup: bk.Cfg, Spec: restoreSpec(t), Env: bk.Env, Domain: "app.example", PID: os.Getpid(),
	}
	bk.Stack.Vars = tmpl.Vars{"target": map[string]string{"tls_mode": "certbot"}}
	rt.Data.Database.Type = "postgres"
	rt.Stack.Sleep = func(time.Duration) {}
	if err := rt.Run(t.Context(), &runner.Runner{}); err != nil {
		t.Fatalf("restore test: %v", err)
	}
	if got, _ := os.ReadFile(dropLog); !strings.Contains(string(got), "restore_check") {
		t.Errorf("the throwaway database must be dropped, drop log = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, ".lock-ops")); err == nil {
		t.Error("the lock must be released")
	}

	// A corrupted backup is caught by the checksum.
	file := bk.Path()
	if err := os.WriteFile(file, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt2 := &RestoreTest{Stack: bk.Stack, Data: bk.Data, Backup: bk.Cfg, Spec: restoreSpec(t), Env: bk.Env, PID: os.Getpid()}
	if err := rt2.Run(t.Context(), &runner.Runner{}); err == nil || !strings.Contains(err.Error(), "corrupted") {
		t.Errorf("a tampered backup must fail the checksum: %v", err)
	}
}
