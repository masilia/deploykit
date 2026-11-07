// Package version holds the build identity: injected at link time by the Makefile and GoReleaser, completed from the
// build information Go embeds in every binary built inside a git repository (or installed with `go install pkg@vX.Y.Z`).
package version

import (
	"fmt"
	"regexp"
	"runtime/debug"
	"strings"
)

// Set with -ldflags "-X github.com/masilia/deploykit/internal/version.Version=..." (see Makefile, .goreleaser.yaml).
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns the one-line build identity printed by `deploykit version`.
func String() string {
	bi, _ := debug.ReadBuildInfo()
	return format(Version, Commit, Date, bi)
}

// format fills in what the linker did not set from bi, so that a plain `go build` in a checkout still says which commit
// it came from (and whether the tree was modified), and `go install …@v0.1.0` reports v0.1.0.
func format(version, commit, date string, bi *debug.BuildInfo) string {
	dirty := false
	if bi != nil {
		if version == "dev" && isRelease(bi.Main.Version) {
			version = bi.Main.Version
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if commit == "none" && len(s.Value) >= 7 {
					commit = s.Value[:7]
				}
			case "vcs.time":
				if date == "unknown" {
					date = s.Value
				}
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
	}
	if dirty && commit != "none" {
		commit += "+dirty"
	}
	return fmt.Sprintf("%s (commit %s, built %s)", version, commit, date)
}

// isRelease reports whether v is a tagged module version (v0.1.0, v1.2.3-rc.1), as opposed to "(devel)" or the
// pseudo-version Go stamps on a build from an untagged checkout (v0.0.0-20261027235447-57a57ff6d2e7, possibly +dirty).
func isRelease(v string) bool {
	return v != "" && v != "(devel)" && !strings.HasPrefix(v, "v0.0.0-") && !strings.Contains(v, "+dirty")
}

// releasePattern is a tagged version without its leading v: 0.5.1, 1.2.3-rc.1.
var releasePattern = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z]+(\.[0-9A-Za-z]+)*)?$`)

// describePattern is what `git describe` appends to a build between two tags: 0.5.1-8-g732186d, with -dirty when the tree was modified.
var describePattern = regexp.MustCompile(`-\d+-g[0-9a-f]{7,}(-dirty)?$`)

// Release returns the version of this binary, without the leading v (0.5.1), when it is a tagged release: what GoReleaser builds, or
// `go install pkg@vX.Y.Z`. It is false for a build from a checkout (dev, 0.5.1-8-g732186d, a modified tree), which has no version
// anyone could compare with a release.
func Release() (string, bool) {
	bi, _ := debug.ReadBuildInfo()
	return release(Version, bi)
}

func release(v string, bi *debug.BuildInfo) (string, bool) {
	if v == "dev" && bi != nil && isRelease(bi.Main.Version) {
		v = bi.Main.Version
	}
	v = strings.TrimPrefix(v, "v")
	if !releasePattern.MatchString(v) || describePattern.MatchString(v) || strings.HasSuffix(v, "-dirty") {
		return "", false
	}
	return v, true
}
