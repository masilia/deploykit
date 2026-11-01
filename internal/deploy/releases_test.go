package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/masilia/deploykit/internal/transport"
)

func TestStamp(t *testing.T) {
	if got := NewStamp(time.Date(2026, 10, 26, 19, 26, 35, 0, time.UTC)); got != "20261026192635" {
		t.Errorf("NewStamp = %s", got)
	}
	for s, want := range map[string]bool{"20261026192635": true, "2026102619263": false, "20261026192635x": false, "../etc": false, "": false, "2026102619263a": false} {
		if ValidStamp(s) != want {
			t.Errorf("ValidStamp(%q) != %v", s, want)
		}
	}
}

func rel(stamp string, s Status) Release { return Release{Stamp: stamp, Status: s} }

func TestRollbackTarget(t *testing.T) {
	list := []Release{
		rel("20260105000000", StatusFailed),
		rel("20260104000000", StatusLive),
		rel("20260103000000", StatusFailed),
		rel("20260102000000", StatusSuperseded),
		rel("20260101000000", StatusUnknown),
	}
	for current, want := range map[string]string{
		"20260104000000": "20260102000000", // skips the failed 03
		"20260102000000": "20260101000000", // releases made by the old shell scripts are valid targets
	} {
		if got, err := RollbackTarget(list, current); err != nil || got != want {
			t.Errorf("RollbackTarget(%s) = %q, %v; want %q", current, got, err, want)
		}
	}
	if _, err := RollbackTarget(list, "20260101000000"); err == nil {
		t.Error("the oldest release has nowhere to go back to")
	}
	if _, err := RollbackTarget([]Release{rel("20260101000000", StatusFailed)}, "20260102000000"); err == nil {
		t.Error("a failed release is never a target")
	}
}

func TestPruneList(t *testing.T) {
	for name, tc := range map[string]struct {
		releases []Release
		current  string
		keep     int
		want     string
	}{
		"keeps the newest healthy ones": {
			[]Release{rel("5", StatusLive), rel("4", StatusSuperseded), rel("3", StatusSuperseded), rel("2", StatusSuperseded), rel("1", StatusSuperseded)},
			"5", 3, "2,1"},
		"failed releases go first and do not count": {
			[]Release{rel("6", StatusLive), rel("5", StatusFailed), rel("4", StatusFailed), rel("3", StatusSuperseded), rel("2", StatusSuperseded), rel("1", StatusSuperseded)},
			"6", 3, "5,4,1"},
		"current is always kept even when old": {
			[]Release{rel("5", StatusSuperseded), rel("4", StatusSuperseded), rel("3", StatusLive), rel("2", StatusSuperseded)},
			"3", 2, "4,2"}, // 5 and current 3 are kept: 5 consumed one slot, current the other
		"stale building releases are removed, in-flight ones are not": {
			[]Release{rel("7", StatusBuilding), rel("6", StatusLive), rel("5", StatusBuilding)},
			"6", 3, "5"},
		"nothing to delete": {[]Release{rel("2", StatusLive), rel("1", StatusSuperseded)}, "2", 3, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := strings.Join(PruneList(tc.releases, tc.current, tc.keep), ","); got != tc.want {
				t.Errorf("PruneList = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestListReleasesParsesTheServerOutput(t *testing.T) {
	f := transport.NewFake("srv").On("DK_RELEASES", transport.Result{Stdout: "" +
		"20260101000000\tunknown\t\n" +
		"20260102000000\tlive\t{\"stamp\":\"20260102000000\",\"commit\":\"abc1234\",\"ref\":\"HEAD\",\"date\":\"2026-01-02T00:00:00Z\"}\n" +
		"not-a-stamp\tlive\t\n" +
		"20260103000000\tfailed\t{broken json\n"})
	got, err := ListReleases(t.Context(), f, Layout{Root: "/srv/site"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Stamp != "20260103000000" || got[2].Stamp != "20260101000000" {
		t.Fatalf("releases = %+v (want 3, newest first, junk ignored)", got)
	}
	if got[0].Status != StatusFailed || got[0].Meta != nil {
		t.Errorf("broken metadata must be ignored, not fatal: %+v", got[0])
	}
	if got[1].Meta == nil || got[1].Meta.Commit != "abc1234" || got[1].Status != StatusLive {
		t.Errorf("release with metadata = %+v", got[1])
	}
}

// The remote scripts are POSIX shell; run them for real against a temp directory with the Local transport.
func TestRemoteScriptsAgainstARealDirectory(t *testing.T) {
	if out, err := exec.CommandContext(t.Context(), "mv", "--version").CombinedOutput(); err != nil || !strings.Contains(string(out), "GNU") {
		t.Skip("GNU mv required for mv -T")
	}
	root := t.TempDir()
	l := Layout{Root: root}
	local := transport.NewLocal()
	for _, s := range []string{"20260101000000", "20260102000000"} {
		if err := os.MkdirAll(l.Release(s), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	meta := Meta{Stamp: "20260102000000", Commit: "abc1234", Ref: "HEAD", Subject: "it's a \"quoted\" subject", Date: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)}
	if err := WriteMeta(t.Context(), local, l, meta); err != nil {
		t.Fatal(err)
	}

	if cur, err := Current(t.Context(), local, l); err != nil || cur != "" {
		t.Errorf("no link yet: current = %q, %v", cur, err)
	}
	if err := Switch(t.Context(), local, l, "20260101000000"); err != nil {
		t.Fatal(err)
	}
	if err := Switch(t.Context(), local, l, "20260102000000"); err != nil {
		t.Fatal(err)
	}
	if cur, err := Current(t.Context(), local, l); err != nil || cur != "20260102000000" {
		t.Errorf("current = %q, %v", cur, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "current.tmp")); err == nil {
		t.Error("current.tmp must not be left behind")
	}

	list, err := ListReleases(t.Context(), local, l)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if list[0].Status != StatusBuilding || list[0].Meta == nil || list[0].Meta.Subject != meta.Subject {
		t.Errorf("newest = %+v", list[0])
	}
	if list[1].Status != StatusUnknown {
		t.Errorf("a folder without .status is unknown, got %s", list[1].Status)
	}

	if err := SetStatus(t.Context(), local, l, "20260102000000", StatusLive); err != nil {
		t.Fatal(err)
	}
	if err := Remove(t.Context(), local, l, []string{"20260101000000"}); err != nil {
		t.Fatal(err)
	}
	list, _ = ListReleases(t.Context(), local, l)
	if len(list) != 1 || list[0].Status != StatusLive {
		t.Errorf("after remove: %+v", list)
	}
}

func TestRemoteHelpersRejectBadStamps(t *testing.T) {
	f := transport.NewFake("srv")
	l := Layout{Root: "/srv"}
	for _, bad := range []string{"../../etc", "1; rm -rf /", "", "2026"} {
		if Switch(t.Context(), f, l, bad) == nil || SetStatus(t.Context(), f, l, bad, StatusLive) == nil || Remove(t.Context(), f, l, []string{bad}) == nil {
			t.Errorf("stamp %q must be rejected", bad)
		}
	}
	if len(f.Calls()) != 0 {
		t.Error("nothing may reach the server for an invalid stamp")
	}
}
