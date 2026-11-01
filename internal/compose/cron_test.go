package compose

import (
	"strings"
	"testing"

	"github.com/masilia/deploykit/internal/transport"
)

func TestMergeCrontab(t *testing.T) {
	entries := []CronEntry{
		{Name: "backup", Schedule: "30 2 * * *", Command: "deploykit server backup"},
		{Name: "monitor", Schedule: "*/5 * * * *", Command: "deploykit server monitor"},
	}
	existing := strings.Join([]string{
		"MAILTO=ops@example.com",
		"0 5 * * * /usr/local/bin/my-own-job",
		// Legacy lines of the old scripts for THIS root, and for another client on the same server.
		"30 2 * * * cd /opt/app && sh deployment/backup.sh >> /opt/app/log/acme-backup.log 2>&1",
		"*/5 * * * * cd /opt/app && sh deployment/monitor.sh >> /opt/app/log/monitor.log 2>&1",
		"30 2 * * * cd /opt/other && sh deployment/backup.sh >> /opt/other/log/acme-backup.log 2>&1",
		"",
	}, "\n")

	got := MergeCrontab(existing, "/opt/app", entries)
	want := `MAILTO=ops@example.com
0 5 * * * /usr/local/bin/my-own-job
30 2 * * * cd /opt/other && sh deployment/backup.sh >> /opt/other/log/acme-backup.log 2>&1
# BEGIN deploykit /opt/app
30 2 * * * cd /opt/app && deploykit server backup >> /opt/app/log/backup.log 2>&1
*/5 * * * * cd /opt/app && deploykit server monitor >> /opt/app/log/monitor.log 2>&1
# END deploykit /opt/app
`
	if got != want {
		t.Errorf("crontab:\n%s\nwant:\n%s", got, want)
	}

	// Idempotent: merging the result again changes nothing.
	if again := MergeCrontab(got, "/opt/app", entries); again != got {
		t.Errorf("not idempotent:\n%s", again)
	}
	// Another root's block is independent.
	both := MergeCrontab(got, "/opt/other", []CronEntry{{Name: "backup", Schedule: "0 3 * * *", Command: "deploykit server backup"}})
	if !strings.Contains(both, "# BEGIN deploykit /opt/app") || !strings.Contains(both, "# BEGIN deploykit /opt/other") || strings.Contains(both, "deployment/backup.sh") {
		t.Errorf("two roots:\n%s", both)
	}
	// Removing every entry removes the block.
	if gone := MergeCrontab(got, "/opt/app", nil); strings.Contains(gone, "BEGIN deploykit /opt/app") || strings.Contains(gone, "deploykit server") {
		t.Errorf("empty merge:\n%s", gone)
	}
	// An empty crontab works too.
	if fresh := MergeCrontab("", "/opt/app", entries[:1]); !strings.HasPrefix(fresh, "# BEGIN deploykit /opt/app\n30 2 * * *") {
		t.Errorf("fresh:\n%s", fresh)
	}
}

func TestInstallCrons(t *testing.T) {
	s, f := newStack(t, "certbot")
	f.On("crontab -l", transport.Result{Stdout: "0 5 * * * mine\n"})
	err := s.InstallCrons(t.Context(), []CronEntry{{Name: "backup", Schedule: "30 2 * * *", Command: "deploykit server backup"}})
	if err != nil {
		t.Fatal(err)
	}
	calls := f.Calls()
	last := calls[len(calls)-1]
	if strings.Join(last.Spec.Argv, " ") != "crontab -" || !strings.Contains(last.Stdin, "0 5 * * * mine\n# BEGIN deploykit /opt/app") {
		t.Errorf("install call = %v / %q", last.Spec.Argv, last.Stdin)
	}
	for _, bad := range []CronEntry{{Name: "x", Schedule: "* * *", Command: "c"}, {Name: "a b", Schedule: "* * * * *", Command: "c"}, {Name: "", Schedule: "* * * * *", Command: "c"}} {
		if err := s.InstallCrons(t.Context(), []CronEntry{bad}); err == nil {
			t.Errorf("%+v must be rejected", bad)
		}
	}
}
