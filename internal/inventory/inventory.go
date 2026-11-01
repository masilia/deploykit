// Package inventory reads deployment targets: one secret-free targets/<id>.env file per destination (docs/targets.md).
//
// It ports the legacy client_ids, client_open and cfg_get, and adds the canonical key names so both script families
// files both work: a manifest asks for target.host and the file may say HOST or DEPLOY_HOST.
package inventory

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/masilia/deploykit/internal/envfile"
	"github.com/masilia/deploykit/internal/tmpl"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidID reports whether id may name a target (legacy client_open: A-Za-z0-9._-).
func ValidID(id string) bool { return idPattern.MatchString(id) }

// field describes one canonical key.
type field struct {
	name    string
	aliases []string // accepted env keys, first is the one written back
	def     string
}

// fields is the table of docs/targets.md.
var fields = []field{
	{"host", []string{"HOST", "DEPLOY_HOST"}, ""},
	{"ssh_port", []string{"SSH_PORT", "DEPLOY_PORT", "DEPLOY_SSH_PORT"}, "22"},
	{"ssh_user", []string{"SSH_USER", "DEPLOY_USER"}, ""}, // falls back to site_user, see Get
	{"root_user", []string{"ROOT_USER", "DEPLOY_ROOT_USER"}, ""},
	{"site_user", []string{"SITE_USER"}, ""},
	{"path", []string{"PATH_ON_SERVER", "DEPLOY_PATH"}, ""},
	{"domain", []string{"DOMAIN", "DEPLOY_DOMAIN"}, ""},
	{"site_url", []string{"SITE_URL"}, ""},
	{"app_port", []string{"APP_PORT"}, ""},
	{"proxy_port", []string{"PROXY_PORT"}, ""},
	{"tls_mode", []string{"TLS_MODE"}, ""},
	{"hosting", []string{"HOSTING"}, ""},
	{"contract", []string{"CONTRAT", "CONTRACT"}, "ondemand"},
	{"rollout_order", []string{"ROLLOUT_ORDER"}, "50"},
	{"project", []string{"COMPOSE_PROJECT", "PROJECT"}, ""}, // overrides the manifest's project name for this target
}

var projectPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Project is the compose project name of a target: its own `project` key when set, otherwise the manifest's. Two
// instances of one application on the same host must have different projects, or they would share containers and
// volumes. The name must be a valid compose project name.
func (t *Target) Project(manifestProject string) (string, error) {
	name := t.Get("project")
	if name == "" {
		name = manifestProject
	}
	if !projectPattern.MatchString(name) {
		return "", fmt.Errorf("target %s: project %q must be lowercase letters, digits, - or _", t.ID, name)
	}
	return name, nil
}

func lookupField(name string) (field, bool) {
	for _, f := range fields {
		if f.name == name {
			return f, true
		}
	}
	return field{}, false
}

// Target is one destination: its file layered over the optional global configuration.
type Target struct {
	ID   string
	Path string
	Doc  *envfile.Doc
	cfg  envfile.Layers
}

// List returns the ids of dir/*.env, sorted (client_ids). A missing directory is an empty inventory.
func List(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".env") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(e.Name(), ".env"))
	}
	sort.Strings(ids)
	return ids, nil
}

// Open loads dir/<id>.env (client_select: the file is required). global, when not nil, is the editor-side fallback
// (deploy.env).
func Open(dir, id string, global *envfile.Doc) (*Target, error) {
	t, err := OpenOrNew(dir, id, global)
	if err != nil {
		return nil, err
	}
	if _, statErr := os.Stat(t.Path); statErr != nil {
		ids, _ := List(dir)
		return nil, fmt.Errorf("target %q not found (expected %s); inventory: %s", id, t.Path, strings.Join(ids, " "))
	}
	return t, nil
}

// OpenOrNew is Open but accepts a file that does not exist yet (client_open: the setup wizard creates it step by step).
func OpenOrNew(dir, id string, global *envfile.Doc) (*Target, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("invalid target id %q (allowed: A-Za-z0-9._-)", id)
	}
	path := filepath.Join(dir, id+".env")
	doc, err := envfile.LoadOptional(path)
	if err != nil {
		return nil, err
	}
	t := &Target{ID: id, Path: path, Doc: doc, cfg: envfile.Layers{doc}}
	if global != nil {
		t.cfg = append(t.cfg, global)
	}
	return t, nil
}

// Get returns a canonical value (target.host…), a raw env key, or def. Canonical names resolve through their aliases,
// target file first, then the global file; the table default applies when nothing is set. ssh_user falls back to
// site_user, because on a CloudPanel server the site user is the deploy login.
func (t *Target) Get(name string) string {
	f, ok := lookupField(name)
	if !ok {
		return t.cfg.Get(name, "")
	}
	for _, key := range f.aliases {
		if v := t.cfg.Get(key, ""); v != "" {
			return v
		}
	}
	if name == "ssh_user" {
		if v := t.Get("site_user"); v != "" {
			return v
		}
	}
	return f.def
}

// Set stores a canonical value, reusing the alias already present in the file so a legacy file keeps its DEPLOY_* style.
// Call Save to write it.
func (t *Target) Set(name, value string) error {
	f, ok := lookupField(name)
	if !ok {
		return t.Doc.Set(name, value)
	}
	key := f.aliases[0]
	for _, alias := range f.aliases {
		if t.Doc.Has(alias) {
			key = alias
			break
		}
	}
	return t.Doc.Set(key, value)
}

// Save writes the target file, creating the directory if needed. Targets are inventory, not secrets, but the file
// keeps whatever mode it already has (new files are 0600).
func (t *Target) Save() error {
	if err := os.MkdirAll(filepath.Dir(t.Path), 0o750); err != nil {
		return err
	}
	return t.Doc.Save(t.Path)
}

// Vars returns the template variables: {"target": {...}} with every canonical field (defaults applied), the id, and
// every other key of the file under its lower-cased name.
func (t *Target) Vars() tmpl.Vars {
	m := map[string]string{"id": t.ID}
	known := map[string]bool{}
	for _, f := range fields {
		m[f.name] = t.Get(f.name)
		for _, a := range f.aliases {
			known[a] = true
		}
	}
	for _, key := range t.Doc.Keys() {
		if !known[key] {
			m[strings.ToLower(key)] = t.cfg.Get(key, "")
		}
	}
	return tmpl.Vars{"target": m}
}

// RolloutOrder is the fleet deploy order: ascending, default 50 (an unparsable value also counts as 50).
func (t *Target) RolloutOrder() int {
	n, err := strconv.Atoi(t.Get("rollout_order"))
	if err != nil {
		return 50
	}
	return n
}

// Select resolves the targets a fleet command acts on (fleet.sh): explicit ids, or all of them; with supervised, only
// those whose contract is "supervised". The result is validated, so a typo fails before anything runs.
func Select(dir string, ids []string, all, supervised bool, global *envfile.Doc) ([]*Target, error) {
	if len(ids) == 0 && all {
		var err error
		if ids, err = List(dir); err != nil {
			return nil, err
		}
	}
	var out []*Target
	for _, id := range ids {
		t, err := Open(dir, id, global)
		if err != nil {
			return nil, err
		}
		if supervised && t.Get("contract") != "supervised" {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// SortByRollout orders targets for a fleet deploy: rollout order ascending, then id.
func SortByRollout(ts []*Target) {
	sort.SliceStable(ts, func(i, j int) bool {
		if a, b := ts[i].RolloutOrder(), ts[j].RolloutOrder(); a != b {
			return a < b
		}
		return ts[i].ID < ts[j].ID
	})
}
