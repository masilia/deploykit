// Package envfile reads and edits KEY=VALUE environment files without ever evaluating them.
//
// It ports the behaviour of the legacy lib.sh (file_get, cfg_get) and env-set.sh, because live servers keep their
// configuration in files written and read by those scripts. Files may hold secrets, so values are never executed,
// expanded or logged by this package.
package envfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var keyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidKey reports whether key is a legal variable name (env-set.sh: ^[A-Za-z_][A-Za-z0-9_]*$).
func ValidKey(key string) bool { return keyPattern.MatchString(key) }

// Doc is an env file held as lines. Comments, blank lines and ordering are preserved by edits.
type Doc struct {
	lines        []string
	finalNewline bool
}

// Parse builds a Doc from file contents. It never fails: any line that does not start with KEY= is simply inert.
func Parse(data []byte) *Doc {
	s := string(data)
	if s == "" {
		return &Doc{finalNewline: true}
	}
	d := &Doc{finalNewline: strings.HasSuffix(s, "\n")}
	d.lines = strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	return d
}

// Load reads path. A missing file is an error (env-set.sh: "$target absent").
func Load(path string) (*Doc, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is chosen by the operator
	if err != nil {
		return nil, err
	}
	return Parse(data), nil
}

// LoadOptional reads path and returns an empty Doc when it does not exist (file_get: a missing file yields the default).
func LoadOptional(path string) (*Doc, error) {
	d, err := Load(path)
	if errors.Is(err, os.ErrNotExist) {
		return Parse(nil), nil
	}
	return d, err
}

// Bytes returns the file contents, byte for byte when nothing was edited.
func (d *Doc) Bytes() []byte {
	if len(d.lines) == 0 {
		return nil
	}
	s := strings.Join(d.lines, "\n")
	if d.finalNewline {
		s += "\n"
	}
	return []byte(s)
}

// Has reports whether a line starts with KEY= (grep -q "^KEY=").
func (d *Doc) Has(key string) bool {
	prefix := key + "="
	for _, l := range d.lines {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

// Get returns the value of key, or def when the key is absent or its value is empty.
//
// Same rules as file_get: the last KEY= line wins, "# KEY=x" comments and longer names such as KEY_PREV do not
// match, everything after the first '=' is the value, and the quotes are removed with the same four independent
// steps (leading ", trailing ", leading ', trailing ') as the original sed pipeline, so a value such as "abc
// loses its lone quote.
func (d *Doc) Get(key, def string) string {
	prefix := key + "="
	value, found := "", false
	for _, l := range d.lines {
		if strings.HasPrefix(l, prefix) {
			value, found = strings.TrimPrefix(l, prefix), true
		}
	}
	if !found {
		return def
	}
	value = strings.TrimPrefix(value, `"`)
	value = strings.TrimSuffix(value, `"`)
	value = strings.TrimPrefix(value, `'`)
	value = strings.TrimSuffix(value, `'`)
	if value == "" {
		return def
	}
	return value
}

// Set upserts key (env-set.sh): every existing KEY= line is replaced, in place, by KEY=value; if there is none
// the line is appended. The value is stored literally: no quoting and no escaping, so characters such as & | \ $
// survive. Unlike the shell script, appending to a file without a final newline starts a new line instead of
// gluing the entry onto the last one.
func (d *Doc) Set(key, value string) error {
	if !ValidKey(key) {
		return fmt.Errorf("invalid key %q", key)
	}
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("value of %s contains a line break", key)
	}
	line, prefix, replaced := key+"="+value, key+"=", false
	for i, l := range d.lines {
		if strings.HasPrefix(l, prefix) {
			d.lines[i] = line
			replaced = true
		}
	}
	if !replaced {
		d.lines = append(d.lines, line)
		d.finalNewline = true
	}
	return nil
}

// Apply sets every assignment in order.
func (d *Doc) Apply(as []Assignment) error {
	for _, a := range as {
		if err := d.Set(a.Key, a.Value); err != nil {
			return err
		}
	}
	return nil
}

// Save writes the Doc to path atomically (temporary file in the same directory, then rename). An existing file keeps
// its permissions; a new file is created with mode 0600 because env files hold secrets.
func (d *Doc) Save(path string) error {
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(d.Bytes()); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// Keys returns the names of the KEY= lines in order of first appearance, without duplicates. Lines that are not a
// valid assignment (comments, blanks, malformed names) are skipped.
func (d *Doc) Keys() []string {
	seen := map[string]bool{}
	var keys []string
	for _, l := range d.lines {
		key, _, ok := strings.Cut(l, "=")
		if ok && ValidKey(key) && !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	return keys
}
