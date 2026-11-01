package scaffold

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Status says what writing a file would do.
type Status string

// What a planned file looks like next to what is already on disk.
const (
	StatusNew       Status = "new"       // no such file yet
	StatusUnchanged Status = "unchanged" // the file already has exactly this content
	StatusDiffers   Status = "differs"   // the file exists with other content
)

// Conflict is what to do with a file that exists with other content.
type Conflict int

// The three answers to a conflict. Nothing is ever overwritten without one of them being chosen.
const (
	ConflictStop       Conflict = iota // write nothing and report the files
	ConflictSideBySide                 // write the new content next to the old file as <name>.new
	ConflictOverwrite                  // replace the file, keeping the old one as <name>.bak
)

// Planned is a file together with its status.
type Planned struct {
	File
	Status Status
}

// Plan is the set of changes a scaffold would make to a directory.
type Plan struct {
	Dir       string
	Files     []Planned
	Gitignore []string // lines missing from .gitignore
}

// ErrConflict is returned by Apply with ConflictStop when files differ.
type ErrConflict struct{ Paths []string }

func (e *ErrConflict) Error() string {
	return "already exist with other content: " + strings.Join(e.Paths, ", ") + " (use --force to overwrite, keeping .bak copies)"
}

// NewPlan compares files with what is in dir. ignore lists the lines .gitignore should have; they are only proposed when
// the directory is a git repository or already has a .gitignore.
func NewPlan(dir string, files []File, ignore []string) (*Plan, error) {
	p := &Plan{Dir: dir}
	for _, f := range files {
		abs, err := p.resolve(f.Path)
		if err != nil {
			return nil, err
		}
		pf := Planned{File: f, Status: StatusNew}
		switch existing, err := os.ReadFile(abs); { //nolint:gosec // paths are resolved inside the project directory
		case err == nil && bytes.Equal(existing, f.Content):
			pf.Status = StatusUnchanged
		case err == nil:
			pf.Status = StatusDiffers
		case !errors.Is(err, os.ErrNotExist):
			return nil, err
		}
		p.Files = append(p.Files, pf)
	}
	p.Gitignore = p.missingIgnores(ignore)
	return p, nil
}

// resolve returns the absolute path of a relative file, refusing anything that would leave the directory.
func (p *Plan) resolve(rel string) (string, error) {
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing to write %q outside the project", rel)
	}
	return filepath.Join(p.Dir, clean), nil
}

func (p *Plan) missingIgnores(ignore []string) []string {
	path := filepath.Join(p.Dir, ".gitignore")
	data, err := os.ReadFile(path) //nolint:gosec // the project's own .gitignore
	if err != nil {
		if _, gerr := os.Stat(filepath.Join(p.Dir, ".git")); gerr != nil {
			return nil
		}
	}
	have := map[string]bool{}
	for _, l := range strings.Split(string(data), "\n") {
		have[strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "/"))] = true
	}
	var missing []string
	for _, l := range ignore {
		if !have[l] && !have[strings.TrimSuffix(l, "/")] {
			missing = append(missing, l)
		}
	}
	return missing
}

// Conflicts lists the files that exist with other content.
func (p *Plan) Conflicts() []string {
	var out []string
	for _, f := range p.Files {
		if f.Status == StatusDiffers {
			out = append(out, f.Path)
		}
	}
	return out
}

// Written is one thing Apply did.
type Written struct {
	Path   string
	Action string
}

// Apply writes the plan. With ConflictStop it checks first and writes nothing at all when a file would be replaced.
func (p *Plan) Apply(policy Conflict) ([]Written, error) {
	if c := p.Conflicts(); len(c) > 0 && policy == ConflictStop {
		return nil, &ErrConflict{Paths: c}
	}
	var done []Written
	for _, f := range p.Files {
		abs, err := p.resolve(f.Path)
		if err != nil {
			return done, err
		}
		switch f.Status {
		case StatusUnchanged:
			done = append(done, Written{f.Path, "unchanged"})
			continue
		case StatusNew:
			if err := writeAtomic(abs, f.Content, f.Mode); err != nil {
				return done, err
			}
			done = append(done, Written{f.Path, "created"})
		case StatusDiffers:
			if policy == ConflictSideBySide {
				if err := writeAtomic(abs+".new", f.Content, f.Mode); err != nil {
					return done, err
				}
				done = append(done, Written{f.Path + ".new", "written next to the existing file"})
				continue
			}
			backup, err := backupName(abs)
			if err != nil {
				return done, err
			}
			if err := os.Rename(abs, backup); err != nil {
				return done, err
			}
			if err := writeAtomic(abs, f.Content, f.Mode); err != nil {
				return done, err
			}
			done = append(done, Written{f.Path, "replaced, the old one is " + filepath.Base(backup)})
		}
	}
	if len(p.Gitignore) > 0 {
		if err := p.appendIgnores(); err != nil {
			return done, err
		}
		done = append(done, Written{".gitignore", "updated: " + strings.Join(p.Gitignore, ", ")})
	}
	return done, nil
}

func (p *Plan) appendIgnores() error {
	path := filepath.Join(p.Dir, ".gitignore")
	old, err := os.ReadFile(path) //nolint:gosec // the project's own .gitignore
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var b bytes.Buffer
	b.Write(old)
	if len(old) > 0 && !bytes.HasSuffix(old, []byte("\n")) {
		b.WriteByte('\n')
	}
	b.WriteString("\n# deploykit\n" + strings.Join(p.Gitignore, "\n") + "\n")
	return writeAtomic(path, b.Bytes(), 0o644)
}

// backupName finds a free name: path.bak, then path.bak.1, path.bak.2…
func backupName(path string) (string, error) {
	for i := 0; i < 1000; i++ {
		name := path + ".bak"
		if i > 0 {
			name += "." + strconv.Itoa(i)
		}
		if _, err := os.Lstat(name); errors.Is(err, os.ErrNotExist) {
			return name, nil
		}
	}
	return "", fmt.Errorf("no free backup name for %s", path)
}

// writeAtomic writes through a temporary file in the same directory, so a crash never leaves half a file.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".deploykit-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}
