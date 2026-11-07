package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// CheckEvery is how often the latest release is looked up, and how often a newer one is mentioned.
const CheckEvery = 24 * time.Hour

// State is what is remembered between two runs of the tool.
type State struct {
	CheckedAt   time.Time `json:"checked_at"`   // the last look at the releases, successful or not
	Latest      string    `json:"latest"`       // the newest stable version that look found
	NotifiedAt  time.Time `json:"notified_at"`  // the last time a newer version was mentioned
	NotifiedFor string    `json:"notified_for"` // and which one
}

// Cache is the file that holds the State.
type Cache struct{ Path string }

// DefaultCache is ~/.cache/deploykit/update.json (or where the platform keeps caches).
func DefaultCache() (Cache, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return Cache{}, err
	}
	return Cache{Path: filepath.Join(dir, "deploykit", "update.json")}, nil
}

// Load reads the state; a missing or unreadable file is an empty state, which means "look again".
func (c Cache) Load() State {
	var s State
	data, err := os.ReadFile(c.Path)
	if err != nil || json.Unmarshal(data, &s) != nil {
		return State{}
	}
	return s
}

// Save writes the state through a temporary file and a rename, so that a run reading it never sees half of it.
func (c Cache) Save(s State) error {
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.Path), ".update-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), c.Path)
}

// Due reports whether the releases should be looked up again. A date in the future (a clock that moved) counts as due.
func (s State) Due(now time.Time) bool {
	return s.CheckedAt.IsZero() || s.CheckedAt.After(now) || now.Sub(s.CheckedAt) >= CheckEvery
}

// Notice returns the sentence to show when the latest known version is newer than current, and records that it was shown. A given
// version is mentioned once a day, not on every command.
func (s *State) Notice(current string, now time.Time) (string, bool) {
	latest, err := Parse(s.Latest)
	if err != nil {
		return "", false
	}
	cur, err := Parse(current)
	if err != nil || latest.Compare(cur) <= 0 {
		return "", false
	}
	if s.NotifiedFor == latest.String() && now.Sub(s.NotifiedAt) < CheckEvery && !s.NotifiedAt.After(now) {
		return "", false
	}
	s.NotifiedAt, s.NotifiedFor = now, latest.String()
	return fmt.Sprintf("deploykit %s is available (this is %s): run 'deploykit self-update'", latest, cur), true
}

// Refresh looks up the latest stable release and remembers it. A failed look is remembered too, as a look: an offline machine tries
// again tomorrow instead of on every command. It returns the error for whoever wants it; the callers in the background ignore it.
func Refresh(ctx context.Context, c *Client, cache Cache, now time.Time) error {
	s := cache.Load()
	s.CheckedAt = now
	rel, err := c.Latest(ctx, false)
	if err == nil {
		s.Latest = rel.Version.String()
	}
	if serr := cache.Save(s); serr != nil && err == nil {
		err = serr
	}
	return err
}
