package compose

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/masilia/deploykit/internal/envfile"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/runner"
	"github.com/masilia/deploykit/internal/tmpl"
	"github.com/masilia/deploykit/internal/transport"
)

func TestBackupName(t *testing.T) {
	daily := time.Date(2026, 10, 27, 2, 30, 5, 0, time.UTC)
	monthly := time.Date(2026, 11, 1, 2, 30, 5, 0, time.UTC)
	if got := BackupName("acme-backup", daily, 1); got != "acme-backup-20261027-023005" {
		t.Errorf("daily = %s", got)
	}
	if got := BackupName("acme-backup", monthly, 1); got != "acme-backup-20261101-023005-monthly" {
		t.Errorf("monthly = %s", got)
	}
}

func TestRetentionDeletes(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	day := 24 * time.Hour
	files := []BackupFile{
		{"acme-backup-A.tar.gpg", ago(6*day + 23*time.Hour)},     // keep
		{"acme-backup-B.tar.gpg", ago(7*day + 23*time.Hour)},     // 7 whole days: not > 7, keep
		{"acme-backup-C.tar.gpg", ago(8*day + time.Hour)},        // delete
		{"acme-backup-C.tar.gpg.sha256", ago(8*day + time.Hour)}, // follows its backup
		{"acme-backup-D-monthly.tar.gpg", ago(29 * day)},         // keep
		{"acme-backup-E-monthly.tar.gpg", ago(31 * day)},         // delete
		{"acme-backup-E-monthly.tar.gpg.sha256", ago(31 * day)},  // delete
		{"acme-backup-F-monthly.tar.gpg", ago(10 * day)},         // monthly outlives the daily limit
		{"other-backup-G.tar.gpg", ago(400 * day)},               // another prefix: never touched
		{"acme-backup.tar.gpg", ago(400 * day)},                  // no dash after the prefix: not ours
	}
	got := strings.Join(RetentionDeletes(files, now, "acme-backup", 7, 30), ",")
	want := "acme-backup-C.tar.gpg,acme-backup-C.tar.gpg.sha256,acme-backup-E-monthly.tar.gpg,acme-backup-E-monthly.tar.gpg.sha256"
	if got != want {
		t.Errorf("deletes = %s\nwant      %s", got, want)
	}
}

// The retention rule must match the find commands of backup.sh on real files.
func TestRetentionMatchesFind(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	day := 24 * time.Hour
	ages := map[string]time.Duration{
		"acme-backup-1.tar.gpg": 2 * day, "acme-backup-2.tar.gpg": 6*day + 20*time.Hour, "acme-backup-3.tar.gpg": 7*day + 2*time.Hour,
		"acme-backup-4.tar.gpg": 7*day + 22*time.Hour, "acme-backup-5.tar.gpg": 8*day + time.Hour, "acme-backup-5.tar.gpg.sha256": 8*day + time.Hour,
		"acme-backup-6-monthly.tar.gpg": 29*day + 20*time.Hour, "acme-backup-7-monthly.tar.gpg": 30*day + 2*time.Hour,
		"acme-backup-8-monthly.tar.gpg": 31*day + time.Hour, "acme-backup-8-monthly.tar.gpg.sha256": 31*day + time.Hour,
		"acme-backup-9-monthly.tar.gpg": 12 * day,
	}
	var files []BackupFile
	for name, age := range ages {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		mt := now.Add(-age)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		files = append(files, BackupFile{Name: name, ModTime: mt})
	}
	find := func(args ...string) []string {
		out, err := exec.CommandContext(t.Context(), "find", append([]string{dir}, args...)...).Output()
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, l := range strings.Fields(string(out)) {
			names = append(names, filepath.Base(l))
		}
		return names
	}
	want := append(find("-name", "acme-backup-*", "!", "-name", "*-monthly.*", "-mtime", "+7"), find("-name", "acme-backup-*-monthly.*", "-mtime", "+30")...)
	sort.Strings(want)
	got := RetentionDeletes(files, now, "acme-backup", 7, 30)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Go deletes %v\nfind deletes %v", got, want)
	}
}

func TestParseListing(t *testing.T) {
	got := parseListing("a.tar.gpg\t1700000000.1234567890\nbroken line\nb.sha256\t1700086400.0\n\t5\nc\tnotanumber\n")
	if len(got) != 3 || got[0].Name != "a.tar.gpg" || got[0].ModTime.Unix() != 1700000000 || got[1].Name != "b.sha256" {
		t.Errorf("listing = %+v", got)
	}
}

func backupSpecs(t *testing.T) (*manifest.DataSpec, *manifest.BackupSpec) {
	t.Helper()
	m, err := manifest.Load("../../examples/compose-app/deploykit.yaml")
	if err != nil {
		t.Fatal(err)
	}
	d, b, err := m.BackupConfig()
	if err != nil {
		t.Fatal(err)
	}
	return d, b
}

func TestBackupStepsTraceAndSecrets(t *testing.T) {
	s, f := newStack(t, "certbot")
	f.OnFunc(func(sp transport.Spec) bool { return strings.Join(sp.Argv, " ") == "mktemp -d" }, transport.Result{Stdout: "/tmp/tmp.abc123\n"})
	f.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "df -Pm") }, transport.Result{Stdout: "9000\n"})
	f.On("find /opt/app/backups", transport.Result{})
	d, b := backupSpecs(t)
	env := envfile.Parse([]byte("BACKUP_PASSPHRASE=very-secret-pass\nBACKUP_RSYNC_TARGET=backup@remote:/vault/\nPOSTGRES_USER=acme\n"))
	bk := &Backup{Stack: s, Data: d, Cfg: b, Env: env, Now: func() time.Time { return time.Date(2026, 10, 27, 2, 30, 0, 0, time.UTC) }}
	if err := bk.Run(t.Context(), &runner.Runner{}); err != nil {
		t.Fatal(err)
	}
	trace := f.Trace()
	if strings.Contains(trace, "very-secret-pass") {
		t.Errorf("the passphrase leaked into the trace:\n%s", trace)
	}
	for _, c := range f.Calls() {
		if c.Spec != nil && strings.Contains(strings.Join(c.Spec.Argv, " ")+c.Spec.Script, "very-secret-pass") {
			t.Fatal("the passphrase is in a command or script")
		}
	}
	for _, want := range []string{
		"secret DK_PASS=***",
		"env DK_VOL=acme_documents",
		"alpine:3.20 tar -C /data -czf",
		"tar -C \"$DK_TMP\" -czf \"$DK_TMP/bundle.tar.gz\" db.dump documents.tar.gz .env",
		"--cipher-algo \"$DK_CIPHER\"",
		"env DK_OUT=/opt/app/backups/acme-backup-20261027-023000.tar.gpg",
		"$ rsync -a /opt/app/backups/acme-backup-20261027-023000.tar.gpg /opt/app/backups/acme-backup-20261027-023000.tar.gpg.sha256 backup@remote:/vault/",
		"$ rm -rf /tmp/tmp.abc123", // the temp dir is always removed
	} {
		if !strings.Contains(trace, want) {
			t.Errorf("trace lacks %q:\n%s", want, trace)
		}
	}
}

func TestBackupStopsOnLowDiskAndStillCleansUp(t *testing.T) {
	s, f := newStack(t, "certbot")
	f.OnFunc(func(sp transport.Spec) bool { return strings.Join(sp.Argv, " ") == "mktemp -d" }, transport.Result{Stdout: "/tmp/tmp.x\n"})
	f.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "df -Pm") }, transport.Result{Stdout: "100\n"})
	d, b := backupSpecs(t)
	bk := &Backup{Stack: s, Data: d, Cfg: b, Env: envfile.Parse([]byte("BACKUP_PASSPHRASE=pw\n"))}
	err := bk.Run(t.Context(), &runner.Runner{})
	if err == nil || !strings.Contains(err.Error(), "not enough disk space") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(f.Trace(), "pg_dump") {
		t.Error("nothing may be dumped when the disk is nearly full")
	}
}

func TestBackupRefusesWithoutAPassphrase(t *testing.T) {
	s, f := newStack(t, "certbot")
	d, b := backupSpecs(t)
	bk := &Backup{Stack: s, Data: d, Cfg: b, Env: envfile.Parse(nil)}
	if err := bk.Run(t.Context(), &runner.Runner{}); err == nil || !strings.Contains(err.Error(), "BACKUP_PASSPHRASE") {
		t.Errorf("err = %v", err)
	}
	if len(f.Calls()) != 0 {
		t.Error("the server must not be touched without a passphrase")
	}
}

// fakeDocker puts a `docker` on PATH that runs `compose ... exec -T <svc> <cmd>` locally, so the whole backup can run
// for real with gpg, tar, sha256sum and find.
func fakeDocker(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = compose ]; then
  shift
  while [ "$1" = -f ] || [ "$1" = -p ]; do shift 2; done
  if [ "$1" = exec ]; then shift; [ "$1" = -T ] && shift; shift; exec "$@"; fi
  echo "unsupported: compose $*" >&2; exit 2
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700); err != nil { //nolint:gosec // test double
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func realBackup(t *testing.T, dump, verify string) (*Backup, string) {
	t.Helper()
	for _, tool := range []string{"gpg", "tar", "sha256sum", "find"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	fakeDocker(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("BACKUP_PASSPHRASE=real-pass-phrase\nAPP_VERSION=1.0.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := composeApp(t)
	s := &Stack{T: transport.NewLocal(), Root: root, Project: "acme", Cfg: cfg, Vars: tmpl.Vars{"target": map[string]string{"tls_mode": "certbot"}}}
	_, b := backupSpecs(t)
	b.NamePrefix, b.Offsite.RsyncTarget = "t-backup", ""
	d := &manifest.DataSpec{IncludeFiles: []string{".env"}}
	d.Database.Service, d.Database.Dump, d.Database.Verify = "db", dump, verify
	env, err := (RemoteEnv{T: transport.NewLocal(), Path: filepath.Join(root, ".env")}).Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return &Backup{Stack: s, Data: d, Cfg: b, Env: env, Now: func() time.Time { return time.Now() }}, root
}

func TestBackupEndToEndWithRealGPG(t *testing.T) {
	bk, root := realBackup(t, "echo FAKE-DUMP-CONTENT", "grep -q FAKE-DUMP-CONTENT")
	// Seed expired and kept backups.
	old := time.Now().Add(-10 * 24 * time.Hour)
	for name, mtime := range map[string]time.Time{
		"t-backup-20200101-000000.tar.gpg": old, "t-backup-20200101-000000.tar.gpg.sha256": old,
		"t-backup-20200101-000000-monthly.tar.gpg": old, "other-keep.txt": old,
	} {
		p := filepath.Join(root, "backups", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}

	if err := bk.Run(t.Context(), &runner.Runner{}); err != nil {
		t.Fatal(err)
	}
	file := bk.Path()
	// 1. The file decrypts with the passphrase and holds the dump and the .env.
	dec := exec.CommandContext(t.Context(), "sh", "-c", `printf '%s' real-pass-phrase | gpg --batch --pinentry-mode loopback --passphrase-fd 0 --decrypt "$0" 2>/dev/null | tar -xzO -f - db.dump`, file)
	out, err := dec.Output()
	if err != nil || strings.TrimSpace(string(out)) != "FAKE-DUMP-CONTENT" {
		t.Fatalf("decrypted dump = %q, %v", out, err)
	}
	// 2. The checksum companion verifies.
	chk := exec.CommandContext(t.Context(), "sha256sum", "-c", filepath.Base(file)+".sha256")
	chk.Dir = filepath.Dir(file)
	if out, err := chk.CombinedOutput(); err != nil {
		t.Errorf("sha256 check: %v\n%s", err, out)
	}
	// 3. A wrong passphrase must not decrypt (the file really is encrypted).
	bad := exec.CommandContext(t.Context(), "sh", "-c", `printf '%s' wrong | gpg --batch --pinentry-mode loopback --passphrase-fd 0 --decrypt "$0" >/dev/null 2>&1`, file)
	if bad.Run() == nil {
		t.Error("the backup decrypts with a wrong passphrase")
	}
	// 4. Retention: the expired daily pair is gone, the monthly (10 days < 30) and foreign files stay.
	for name, wantExists := range map[string]bool{
		"t-backup-20200101-000000.tar.gpg": false, "t-backup-20200101-000000.tar.gpg.sha256": false,
		"t-backup-20200101-000000-monthly.tar.gpg": true, "other-keep.txt": true, filepath.Base(file): true,
	} {
		_, err := os.Stat(filepath.Join(root, "backups", name))
		if (err == nil) != wantExists {
			t.Errorf("%s exists = %v, want %v", name, err == nil, wantExists)
		}
	}
	// 5. The temporary directory is gone.
	if _, err := os.Stat(bk.tmp); err == nil {
		t.Errorf("temporary directory %s was left behind", bk.tmp)
	}
}

func TestBackupAbandonsAnUnreadableDump(t *testing.T) {
	bk, root := realBackup(t, "echo garbage", "false")
	err := bk.Run(t.Context(), &runner.Runner{})
	if err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "backups")); len(entries) != 0 {
		t.Errorf("nothing may be kept: %v", entries)
	}
	if _, statErr := os.Stat(bk.tmp); statErr == nil {
		t.Error("the temporary directory must be removed even on failure")
	}
}

// Regression: with BACKUP_RSYNC_TARGET absent from .env the off-site step must do nothing. It used to hand the
// unrendered template text to rsync, which created a local directory with that name on a real server.
func TestOffsiteCopyIsSkippedWhenTheTargetIsNotConfigured(t *testing.T) {
	for name, env := range map[string]string{
		"key absent": "BACKUP_PASSPHRASE=pw\n",
		"key empty":  "BACKUP_PASSPHRASE=pw\nBACKUP_RSYNC_TARGET=\n",
	} {
		t.Run(name, func(t *testing.T) {
			s, f := newStack(t, "certbot")
			f.OnFunc(func(sp transport.Spec) bool { return strings.Join(sp.Argv, " ") == "mktemp -d" }, transport.Result{Stdout: "/tmp/x\n"})
			f.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "df -Pm") }, transport.Result{Stdout: "9000\n"})
			f.On("find /opt/app/backups", transport.Result{})
			d, b := backupSpecs(t)
			bk := &Backup{Stack: s, Data: d, Cfg: b, Env: envfile.Parse([]byte(env)), Now: func() time.Time { return time.Date(2026, 10, 27, 2, 30, 0, 0, time.UTC) }}
			if err := bk.Run(t.Context(), &runner.Runner{}); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(f.Trace(), "rsync") || strings.Contains(f.Trace(), "{{") {
				t.Errorf("no off-site copy expected:\n%s", f.Trace())
			}
		})
	}
}

func TestRenderOptional(t *testing.T) {
	vars := EnvVars(envfile.Parse([]byte("A=x\nEMPTY=\n")))
	for in, want := range map[string]string{"{{ env.A }}": "x", "{{ env.MISSING }}": "", "{{ env.EMPTY }}": "", "plain": "plain", "": ""} {
		if got := renderOptional(in, vars); got != want {
			t.Errorf("renderOptional(%q) = %q, want %q", in, got, want)
		}
	}
}
