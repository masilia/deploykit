package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

func info(mainVersion string, settings map[string]string) *debug.BuildInfo {
	bi := &debug.BuildInfo{Main: debug.Module{Version: mainVersion}}
	for k, v := range settings {
		bi.Settings = append(bi.Settings, debug.BuildSetting{Key: k, Value: v})
	}
	return bi
}

func TestFormat(t *testing.T) {
	vcs := map[string]string{"vcs.revision": "0123456789abcdef", "vcs.time": "2026-10-28T08:00:00Z", "vcs.modified": "false"}
	for name, tc := range map[string]struct {
		version, commit, date string
		bi                    *debug.BuildInfo
		want                  string
	}{
		"linker values win":                   {"0.1.0", "abc1234", "2026-10-28", info("(devel)", vcs), "0.1.0 (commit abc1234, built 2026-10-28)"},
		"plain go build in a checkout":        {"dev", "none", "unknown", info("(devel)", vcs), "dev (commit 0123456, built 2026-10-28T08:00:00Z)"},
		"modified tree is flagged":            {"dev", "none", "unknown", info("(devel)", map[string]string{"vcs.revision": "0123456789abcdef", "vcs.modified": "true"}), "dev (commit 0123456+dirty, built unknown)"},
		"go install pkg@v0.1.0":               {"dev", "none", "unknown", info("v0.1.0", nil), "v0.1.0 (commit none, built unknown)"},
		"pseudo-version of an untagged build": {"dev", "none", "unknown", info("v0.0.0-20261027235447-57a57ff6d2e7+dirty", nil), "dev (commit none, built unknown)"},
		"pre-release tag":                     {"dev", "none", "unknown", info("v0.1.0-rc.1", nil), "v0.1.0-rc.1 (commit none, built unknown)"},
		"no build information at all":         {"dev", "none", "unknown", nil, "dev (commit none, built unknown)"},
		"a short revision is not truncated":   {"dev", "none", "unknown", info("(devel)", map[string]string{"vcs.revision": "abc"}), "dev (commit none, built unknown)"},
		"linker version beats the module one": {"0.2.0", "none", "unknown", info("v0.1.0", nil), "0.2.0 (commit none, built unknown)"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := format(tc.version, tc.commit, tc.date, tc.bi); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestStringHasTheExpectedShape(t *testing.T) {
	if s := String(); !strings.Contains(s, "(commit ") || !strings.Contains(s, ", built ") {
		t.Errorf("String() = %q", s)
	}
}

func TestRelease(t *testing.T) {
	cases := []struct {
		name    string
		version string
		main    string
		want    string
		ok      bool
	}{
		{"goreleaser build", "0.5.1", "", "0.5.1", true},
		{"tag with its v", "v0.5.1", "", "0.5.1", true},
		{"release candidate", "0.6.0-rc.1", "", "0.6.0-rc.1", true},
		{"go install pkg@v0.1.0", "dev", "v0.1.0", "0.1.0", true},
		{"make build between two tags", "v0.5.1-8-g732186d", "", "", false},
		{"make build, modified tree", "v0.5.1-8-g732186d-dirty", "", "", false},
		{"plain go build", "dev", "(devel)", "", false},
		{"go build of an untagged checkout", "dev", "v0.0.0-20261027235447-57a57ff6d2e7", "", false},
		{"nothing", "", "", "", false},
	}
	for _, c := range cases {
		got, ok := release(c.version, &debug.BuildInfo{Main: debug.Module{Version: c.main}})
		if got != c.want || ok != c.ok {
			t.Errorf("%s: release(%q, main %q) = %q, %v; want %q, %v", c.name, c.version, c.main, got, ok, c.want, c.ok)
		}
	}
}
