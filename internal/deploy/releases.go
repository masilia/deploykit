// Package deploy implements the release-dir deployment flows: the remote release layout, the pm2 runtime, deploy,
// rollback and bootstrap on a CloudPanel Node.js site (profile docs/profile-nextjs-cloudpanel.md).
package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/masilia/deploykit/internal/transport"
)

// Status is the lifecycle of a release folder. It lives in a sibling file, .status, so changing it never rewrites the
// immutable .release.json.
type Status string

// Release statuses.
const (
	StatusBuilding   Status = "building"
	StatusLive       Status = "live"
	StatusSuperseded Status = "superseded"
	StatusFailed     Status = "failed"
	StatusUnknown    Status = "unknown" // a release created by the old shell scripts
)

var stampPattern = regexp.MustCompile(`^[0-9]{14}$`)

// ValidStamp reports whether s is a release folder name (YYYYMMDDHHMMSS).
func ValidStamp(s string) bool { return stampPattern.MatchString(s) }

// NewStamp names a release after t.
func NewStamp(t time.Time) string { return t.Format("20060102150405") }

// Meta is the immutable description of a release, stored as .release.json.
type Meta struct {
	Stamp   string    `json:"stamp"`
	Commit  string    `json:"commit"`
	Ref     string    `json:"ref"`
	Subject string    `json:"subject,omitempty"`
	Author  string    `json:"author,omitempty"`
	Date    time.Time `json:"date"`
}

// Release is one folder under releases/.
type Release struct {
	Stamp  string
	Status Status
	Meta   *Meta // nil for releases without metadata
}

// Layout is the remote tree of a site (docs/profile-nextjs-cloudpanel.md §3).
type Layout struct{ Root string }

// Releases is the folder holding every release.
func (l Layout) Releases() string { return l.Root + "/releases" }

// Shared holds runtime secrets and persistent data.
func (l Layout) Shared() string { return l.Root + "/shared" }

// CurrentLink is the symlink to the live release.
func (l Layout) CurrentLink() string { return l.Root + "/current" }

// Release is the folder of one release.
func (l Layout) Release(stamp string) string { return l.Releases() + "/" + stamp }

// Ecosystem is the pm2 configuration file sent on every deploy.
func (l Layout) Ecosystem() string { return l.Root + "/ecosystem.config.cjs" }

const listScript = `cd "$DK_RELEASES" 2>/dev/null || exit 0
for d in *; do
  [ -d "$d" ] || continue
  s=$(cat "$d/.status" 2>/dev/null || echo unknown)
  printf '%s\t%s\t' "$d" "$s"
  tr -d '\n' < "$d/.release.json" 2>/dev/null
  printf '\n'
done
`

// ListReleases reads the releases on the server, newest first. Folders that are not valid stamps are ignored.
func ListReleases(ctx context.Context, t transport.Transport, l Layout) ([]Release, error) {
	res, err := t.Run(ctx, transport.Spec{Script: listScript, Env: map[string]string{"DK_RELEASES": l.Releases()}})
	if err != nil {
		return nil, err
	}
	var out []Release
	for _, line := range strings.Split(res.Stdout, "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 2 || !ValidStamp(parts[0]) {
			continue
		}
		r := Release{Stamp: parts[0], Status: Status(strings.TrimSpace(parts[1]))}
		if r.Status == "" {
			r.Status = StatusUnknown
		}
		if len(parts) == 3 && strings.TrimSpace(parts[2]) != "" {
			var m Meta
			if json.Unmarshal([]byte(parts[2]), &m) == nil {
				r.Meta = &m
			}
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Stamp > out[j].Stamp })
	return out, nil
}

// Current returns the stamp the current link points at, or "" when there is no live release yet.
func Current(ctx context.Context, t transport.Transport, l Layout) (string, error) {
	res, err := t.Run(ctx, transport.Spec{
		Script: `p=$(readlink -f "$DK_LINK" 2>/dev/null) && [ -d "$p" ] && basename "$p" || true` + "\n",
		Env:    map[string]string{"DK_LINK": l.CurrentLink()},
	})
	if err != nil {
		return "", err
	}
	stamp := strings.TrimSpace(res.Stdout)
	if !ValidStamp(stamp) {
		return "", nil
	}
	return stamp, nil
}

// WriteMeta records a new release as building.
func WriteMeta(ctx context.Context, t transport.Transport, l Layout, m Meta) error {
	if !ValidStamp(m.Stamp) {
		return fmt.Errorf("invalid release stamp %q", m.Stamp)
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := writeFile(ctx, t, l.Release(m.Stamp)+"/.release.json", string(data)+"\n"); err != nil {
		return err
	}
	return SetStatus(ctx, t, l, m.Stamp, StatusBuilding)
}

// SetStatus records the lifecycle status of a release.
func SetStatus(ctx context.Context, t transport.Transport, l Layout, stamp string, s Status) error {
	if !ValidStamp(stamp) {
		return fmt.Errorf("invalid release stamp %q", stamp)
	}
	return writeFile(ctx, t, l.Release(stamp)+"/.status", string(s)+"\n")
}

// writeFile writes content to a remote path, the content travelling on stdin.
func writeFile(ctx context.Context, t transport.Transport, path, content string) error {
	_, err := t.Run(ctx, transport.Spec{
		Argv:  []string{"sh", "-c", `cat > "$1"`, "sh", path},
		Stdin: strings.NewReader(content),
	})
	return err
}

// Switch points the current link at a release, atomically: a temporary link is renamed over the old one.
func Switch(ctx context.Context, t transport.Transport, l Layout, stamp string) error {
	if !ValidStamp(stamp) {
		return fmt.Errorf("invalid release stamp %q", stamp)
	}
	_, err := t.Run(ctx, transport.Spec{
		// CloudPanel creates `current/public/index.php` as real folders when a PHP site is made with the root directory
		// current/public; a link cannot replace a folder (`mv -T`), so the first switch sets such a folder aside, never deletes it.
		Script: `set -e
cd "$DK_ROOT"
if [ -d current ] && [ ! -L current ]; then mv current "current.replaced.$(date +%Y%m%d%H%M%S)"; fi
ln -sfn "releases/$DK_STAMP" current.tmp
mv -Tf current.tmp current
`,
		Env: map[string]string{"DK_ROOT": l.Root, "DK_STAMP": stamp},
	})
	return err
}

// Remove deletes releases. Only valid stamps are accepted, so no path can be smuggled in.
func Remove(ctx context.Context, t transport.Transport, l Layout, stamps []string) error {
	if len(stamps) == 0 {
		return nil
	}
	for _, s := range stamps {
		if !ValidStamp(s) {
			return fmt.Errorf("invalid release stamp %q", s)
		}
	}
	_, err := t.Run(ctx, transport.Spec{
		Script: `cd "$DK_RELEASES" && rm -rf -- ` + strings.Join(stamps, " ") + "\n",
		Env:    map[string]string{"DK_RELEASES": l.Releases()},
	})
	return err
}

// RollbackTarget picks the release to go back to: the newest release older than current that did not fail. releases
// must be newest first, as ListReleases returns them.
func RollbackTarget(releases []Release, current string) (string, error) {
	for _, r := range releases {
		if r.Stamp < current && r.Status != StatusFailed && r.Status != StatusBuilding {
			return r.Stamp, nil
		}
	}
	return "", fmt.Errorf("no older release to go back to")
}

// PruneList returns the releases to delete so that at most keep healthy releases remain, current included. Failed
// releases are deleted first and never count against the retention (a failed deploy used to push a good release out
// of the window). A release still "building" that is newer than current is left alone: it may be in flight.
func PruneList(releases []Release, current string, keep int) []string {
	// The current release always stays and takes one of the keep slots, wherever it sits in the list.
	slots := keep
	for _, r := range releases {
		if r.Stamp == current {
			slots--
		}
	}
	var del []string
	for _, r := range releases { // newest first
		switch {
		case r.Stamp == current:
		case r.Status == StatusFailed:
			del = append(del, r.Stamp)
		case r.Status == StatusBuilding && r.Stamp > current:
			// possibly being uploaded right now
		case r.Status == StatusBuilding:
			del = append(del, r.Stamp)
		case slots > 0:
			slots--
		default:
			del = append(del, r.Stamp)
		}
	}
	return del
}

// ErrLocked is returned by Lock when another deploy holds the site.
type ErrLocked struct{ Owner string }

func (e *ErrLocked) Error() string {
	if e.Owner == "" {
		return "another deploy is running on this site (lock held)"
	}
	return "another deploy is running on this site (" + e.Owner + ")"
}

// Lock takes the exclusive deploy lock of a site: an atomic mkdir of .deploy.lock holding the owner's description.
// It returns the function that releases it. A lock left by a crashed deploy is cleared with ForceUnlock.
func Lock(ctx context.Context, t transport.Transport, l Layout, owner string) (unlock func(context.Context) error, err error) {
	res, runErr := t.Run(ctx, transport.Spec{
		Script: `if mkdir "$DK_LOCK" 2>/dev/null; then printf '%s\n' "$DK_OWNER" > "$DK_LOCK/owner"; exit 0; fi
cat "$DK_LOCK/owner" 2>/dev/null
exit 3
`,
		Env: map[string]string{"DK_LOCK": l.Root + "/.deploy.lock", "DK_OWNER": owner},
	})
	if runErr != nil {
		if res.ExitCode == 3 {
			return nil, &ErrLocked{Owner: strings.TrimSpace(res.Stdout)}
		}
		return nil, runErr
	}
	return func(ctx context.Context) error { return ForceUnlock(ctx, t, l) }, nil
}

// ForceUnlock removes the deploy lock.
func ForceUnlock(ctx context.Context, t transport.Transport, l Layout) error {
	_, err := t.Run(ctx, transport.Spec{
		Script: `rm -rf -- "$DK_LOCK"` + "\n",
		Env:    map[string]string{"DK_LOCK": l.Root + "/.deploy.lock"},
	})
	return err
}
