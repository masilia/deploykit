// Package releasedir builds the release-dir artifact on the workstation: a clean source export, the build commands, the
// assembly of the release folder and the local smoke test that must pass before anything is uploaded (ADR 0005).
package releasedir

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/masilia/deploykit/internal/manifest"
)

// Assemble builds the release folder dest from buildDir by applying the steps in order. Each step copies From (relative
// to buildDir) to To (relative to dest). A From ending in "/." copies the directory contents. Replace deletes To first,
// which matters for Next.js: the standalone folder holds a partial public/ that a plain copy would nest as public/public.
// Symlinks are copied as symlinks; nothing may escape buildDir or dest.
func Assemble(buildDir, dest string, steps []manifest.AssembleStep) error {
	if err := os.MkdirAll(dest, 0o750); err != nil {
		return err
	}
	for i, s := range steps {
		src, err := inside(buildDir, strings.TrimSuffix(s.From, "/."))
		if err != nil {
			return fmt.Errorf("assemble step %d: from: %w", i+1, err)
		}
		dst, err := inside(dest, s.To)
		if err != nil {
			return fmt.Errorf("assemble step %d: to: %w", i+1, err)
		}
		if _, err := os.Lstat(src); err != nil {
			return fmt.Errorf("assemble step %d: %w", i+1, err)
		}
		if s.Replace && dst != filepath.Clean(dest) {
			if err := os.RemoveAll(dst); err != nil {
				return err
			}
		}
		if err := copyTree(src, dst, s.Exclude); err != nil {
			return fmt.Errorf("assemble step %d: %w", i+1, err)
		}
	}
	return nil
}

// inside joins rel to root and refuses a result outside root.
func inside(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%q must be relative", rel)
	}
	p := filepath.Join(root, rel)
	r, err := filepath.Rel(filepath.Clean(root), p)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q escapes %s", rel, root)
	}
	return p, nil
}

func excluded(rel string, patterns []string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, filepath.ToSlash(rel)); ok {
			return true
		}
		if ok, _ := path.Match(p, path.Base(filepath.ToSlash(rel))); ok {
			return true
		}
	}
	return false
}

func copyTree(src, dst string, exclude []string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel != "." && excluded(rel, exclude) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			_ = os.Remove(target)
			return os.Symlink(link, target) //nolint:gosec // copying our own build output on the local machine
		case info.Mode().IsRegular():
			return copyFile(p, target, info.Mode().Perm())
		default:
			return nil // sockets, devices: never part of a release
		}
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	in, err := os.Open(src) //nolint:gosec // paths are confined by inside()
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode) //nolint:gosec // paths are confined by inside()
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// ReleaseEpoch is the modification time given to every file of a release: 2000-01-01 00:00 UTC.
var ReleaseEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// NormalizeTimes gives every regular file of dir the same modification time. A `git archive` export stamps all files
// with the commit time, so without this rsync would see every file as changed on every commit and re-upload (and never
// hard-link) the whole release. Combined with rsync --checksum, identical content then links to the previous release
// and changed content is always sent, even at the same size.
func NormalizeTimes(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		return os.Chtimes(p, ReleaseEpoch, ReleaseEpoch) //nolint:gosec // our own freshly assembled release, on the local machine
	})
}
