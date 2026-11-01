package fleet

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/masilia/deploykit/internal/inventory"
	"github.com/masilia/deploykit/internal/transport"
)

func TestParseRow(t *testing.T) {
	r, err := ParseRow("1.2.3|1.2.2|yes|5")
	if err != nil || r.Version != "1.2.3" || r.Previous != "1.2.2" || !r.Installed || r.BackupAge != 5 || !r.Reachable {
		t.Errorf("row = %+v, %v", r, err)
	}
	r, _ = ParseRow("-|-|no|-1")
	if r.Version != "" || r.Previous != "" || r.Installed || r.BackupAge != -1 {
		t.Errorf("empty row = %+v", r)
	}
	for _, bad := range []string{"", "a|b|c", "a|b|c|d|e", "a|b|yes|x"} {
		if _, err := ParseRow(bad); err == nil {
			t.Errorf("ParseRow(%q) must fail", bad)
		}
	}
}

func TestState(t *testing.T) {
	for _, tc := range []struct {
		row  Row
		want string
	}{
		{Row{Reachable: true, Installed: true, Version: "1.0"}, "OK"},
		{Row{Reachable: true, Installed: false}, "to install"},
		{Row{Reachable: true, Installed: true}, "version ?"},
		{Row{}, "unreachable"},
	} {
		if got := tc.row.State(); got != tc.want {
			t.Errorf("State(%+v) = %q, want %q", tc.row, got, tc.want)
		}
	}
	for age, want := range map[int]string{-1: "NONE", 0: "OK", 26: "OK", 27: "STALE (>26h)"} {
		if got, _ := BackupState(age, 26); got != want {
			t.Errorf("BackupState(%d) = %q, want %q", age, got, want)
		}
	}
	if _, ok := BackupState(26, 26); !ok {
		t.Error("exactly 26 hours is still fresh (fleet.sh: -gt 26)")
	}
}

// The status script, run for real against a directory laid out like a compose server.
func TestFetchAgainstARealDirectory(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(".env", "OTHER=1\nAPP_VERSION=2.0.0\nAPP_VERSION_PREV=1.9.0\n")
	write(".installed", "")
	write("backups/old.tar.gpg", "x")
	write("backups/new.tar.gpg", "x")
	if err := os.Chtimes(filepath.Join(root, "backups/old.tar.gpg"), time.Now().Add(-100*time.Hour), time.Now().Add(-100*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(root, "backups/new.tar.gpg"), time.Now().Add(-5*time.Hour-time.Minute), time.Now().Add(-5*time.Hour-time.Minute)); err != nil {
		t.Fatal(err)
	}
	q := Query{Root: root, EnvFile: ".env", CurrentKey: "APP_VERSION", PreviousKey: "APP_VERSION_PREV", Marker: ".installed", BackupDir: "backups"}
	r, err := q.Fetch(t.Context(), transport.NewLocal())
	if err != nil {
		t.Fatal(err)
	}
	if r.Version != "2.0.0" || r.Previous != "1.9.0" || !r.Installed || r.BackupAge != 5 {
		t.Errorf("row = %+v (the newest backup is 5 hours old)", r)
	}

	// An empty server: nothing installed, no backup.
	empty := Query{Root: t.TempDir(), EnvFile: ".env", CurrentKey: "APP_VERSION", PreviousKey: "APP_VERSION_PREV", Marker: ".installed", BackupDir: "backups"}
	r, err = empty.Fetch(t.Context(), transport.NewLocal())
	if err != nil || r.Installed || r.Version != "" || r.BackupAge != -1 {
		t.Errorf("empty = %+v, %v", r, err)
	}
	// The key prefix APP_VERSION must not match APP_VERSION_PREV.
	if _, err := exec.LookPath("sh"); err == nil && r.Previous != "" {
		t.Error("previous must be empty")
	}
	bad := q
	bad.CurrentKey = "A;rm -rf /"
	if _, err := bad.Fetch(t.Context(), transport.NewLocal()); err == nil {
		t.Error("an unsafe key name must be rejected before anything runs")
	}
}

func TestFormatTable(t *testing.T) {
	rows := []Row{
		{Target: "acme", Domain: "app.acme.fr", Host: "deploy@srv1", Contract: "supervised", Version: "2.0.0", Previous: "1.9.0", Installed: true, BackupAge: 3, Reachable: true},
		{Target: "down", Host: "deploy@srv2"},
		{Target: "fresh", Domain: "f.example", Host: "deploy@srv3", Contract: "ondemand", Reachable: true, BackupAge: -1},
	}
	got := FormatTable(rows)
	for _, want := range []string{"TARGET", "acme", "app.acme.fr", "2.0.0", "1.9.0", "3h", "OK", "down           — (unreachable: deploy@srv2)", "to install", "none"} {
		if !strings.Contains(got, want) {
			t.Errorf("table lacks %q:\n%s", want, got)
		}
	}
}

func targets(t *testing.T, specs map[string]string) []*inventory.Target {
	t.Helper()
	dir := t.TempDir()
	var out []*inventory.Target
	for id, content := range specs {
		if err := os.WriteFile(filepath.Join(dir, id+".env"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for id := range specs {
		tg, err := inventory.Open(dir, id, nil)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, tg)
	}
	return out
}

func TestDeployOrderAndStopOnFirstFailure(t *testing.T) {
	ts := targets(t, map[string]string{"c": "HOST=c\nROLLOUT_ORDER=30\n", "a": "HOST=a\nROLLOUT_ORDER=10\n", "b": "HOST=b\nROLLOUT_ORDER=20\n"})
	var ran, logs []string
	failed, err := Deploy(context.Background(), ts, false, func(s string) { logs = append(logs, s) }, func(_ context.Context, tg *inventory.Target) error {
		ran = append(ran, tg.ID)
		if tg.ID == "b" {
			return errors.New("boom")
		}
		return nil
	})
	if err == nil || strings.Join(ran, ",") != "a,b" || strings.Join(failed, ",") != "b" {
		t.Errorf("ran %v, failed %v, err %v: must stop at b and never touch c", ran, failed, err)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "b: FAILED, stopping") {
		t.Errorf("logs = %v", logs)
	}
}

func TestDeployContinueOnError(t *testing.T) {
	ts := targets(t, map[string]string{"a": "HOST=a\nROLLOUT_ORDER=1\n", "b": "HOST=b\nROLLOUT_ORDER=2\n", "c": "HOST=c\nROLLOUT_ORDER=3\n"})
	var ran []string
	failed, err := Deploy(context.Background(), ts, true, func(string) {}, func(_ context.Context, tg *inventory.Target) error {
		ran = append(ran, tg.ID)
		if tg.ID != "c" {
			return errors.New("boom")
		}
		return nil
	})
	if err == nil || strings.Join(ran, ",") != "a,b,c" || strings.Join(failed, ",") != "a,b" {
		t.Errorf("ran %v, failed %v, err %v", ran, failed, err)
	}
}

func TestDeployAllGood(t *testing.T) {
	ts := targets(t, map[string]string{"a": "HOST=a\n"})
	if failed, err := Deploy(context.Background(), ts, false, func(string) {}, func(context.Context, *inventory.Target) error { return nil }); err != nil || len(failed) != 0 {
		t.Errorf("failed %v, err %v", failed, err)
	}
}

func TestDescribe(t *testing.T) {
	ts := targets(t, map[string]string{"a": "DEPLOY_HOST=srv\nDEPLOY_USER=deploy\nDOMAIN=a.example\nCONTRAT=supervised\n"})
	r := Describe(ts[0])
	if r.Host != "deploy@srv" || r.Domain != "a.example" || r.Contract != "supervised" || r.BackupAge != -1 {
		t.Errorf("row = %+v", r)
	}
}

func TestNodeTable(t *testing.T) {
	rows := []NodeRow{
		{Target: "a", Domain: "a.example", Host: "u@h", Live: "20261026000000", Commit: "abc1234", Previous: "20261025000000", Contract: "supervised", Reachable: true},
		{Target: "b", Host: "u@h2", Reachable: true},
		{Target: "c", Host: "u@h3"},
	}
	out := FormatNodeTable(rows)
	for _, want := range []string{"LIVE", "abc1234", "supervised", "to deploy", "c              — (unreachable: u@h3)"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
	if rows[0].State() != "OK" || rows[1].State() != "to deploy" || rows[2].State() != "unreachable" {
		t.Errorf("states = %s, %s, %s", rows[0].State(), rows[1].State(), rows[2].State())
	}
}
