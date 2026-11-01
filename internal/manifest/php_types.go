package manifest

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// SharedPath is a file or folder kept in shared/ and linked into every release: what must survive a deploy (uploads,
// logs, sessions, the environment file with the secrets).
type SharedPath struct {
	Path string `yaml:"path"`
	Kind string `yaml:"kind"` // file or dir; dir by default
	// Required marks a file that the operator must have put in shared/ (the .env.local with the secrets): a deploy
	// without it stops before anything is uploaded.
	Required bool `yaml:"required"`
}

// PHPHealth is the check after the switch: the new release must answer through the site's backend (127.0.0.1:8080 on
// CloudPanel, past Varnish) with the site's Host header.
type PHPHealth struct {
	// Backend is where the site's own web server listens; on a CloudPanel PHP site 127.0.0.1:8080 (past Varnish).
	Backend  string        `yaml:"backend"`
	Path     string        `yaml:"path"`
	Expect   []int         `yaml:"expect"`
	Tries    int           `yaml:"tries"`
	Interval time.Duration `yaml:"interval"`
}

// PHPFPM configures the php-fpm runtime: a release folder served by the PHP-FPM pool of a CloudPanel PHP site.
type PHPFPM struct {
	Type       string `yaml:"type"`
	PHPVersion string `yaml:"php_version"`
	// WebRoot is the folder of a release that the web server serves; the CloudPanel root is current/<web_root>.
	WebRoot  string            `yaml:"web_root"`
	Shared   []SharedPath      `yaml:"shared"`
	Writable []string          `yaml:"writable"`
	Env      map[string]string `yaml:"env"`
	// Commands are named commands run in a release folder as the site user. {{ php }} is php<php_version>.
	Commands map[string]string `yaml:"commands"`
	// PHPExtensions is auto (the ext-* of composer.json are checked on the server), or none.
	PHPExtensions string `yaml:"php_extensions"`
	// Prepare runs in the new release, before the switch; a failure stops the deploy. AfterSwitch runs after the switch
	// and after a rollback; a failure is only a warning. Both list names of Commands.
	Prepare     []string  `yaml:"prepare"`
	AfterSwitch []string  `yaml:"after_switch"`
	Reload      string    `yaml:"reload"` // none, or root-systemctl (root reloads php<version>-fpm)
	Health      PHPHealth `yaml:"health"`
}

var (
	phpVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
	commandName       = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)
	envNamePattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	backendPattern    = regexp.MustCompile(`^[A-Za-z0-9.-]+:[0-9]{1,5}$`)
	imagePattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)
)

// PHP returns the typed php-fpm runtime with defaults applied (web_root: public, health: / answers 200).
func (m *Manifest) PHP() (*PHPFPM, error) {
	var p PHPFPM
	if err := m.Runtime.Decode(&p); err != nil {
		return nil, err
	}
	if !phpVersionPattern.MatchString(p.PHPVersion) {
		return nil, errorf("php_version: %q must look like 8.2", p.PHPVersion)
	}
	if p.WebRoot == "" {
		p.WebRoot = "public"
	}
	if err := relativePath("web_root", p.WebRoot); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i := range p.Shared {
		s := &p.Shared[i]
		if err := relativePath(fmt.Sprintf("shared[%d].path", i), s.Path); err != nil {
			return nil, err
		}
		switch s.Kind {
		case "":
			s.Kind = "dir"
		case "dir", "file":
		default:
			return nil, errorf("shared[%d] %s: kind must be file or dir, got %q", i, s.Path, s.Kind)
		}
		if s.Required && s.Kind != "file" {
			return nil, errorf("shared[%d] %s: only a file can be required", i, s.Path)
		}
		clean := path.Clean(s.Path)
		if seen[clean] {
			return nil, errorf("shared: %s is listed twice", s.Path)
		}
		seen[clean] = true
	}
	for i, w := range p.Writable {
		if err := relativePath(fmt.Sprintf("writable[%d]", i), w); err != nil {
			return nil, err
		}
	}
	for k := range p.Env {
		if !envNamePattern.MatchString(k) {
			return nil, errorf("env: %q is not a valid variable name", k)
		}
	}
	for name, cmd := range p.Commands {
		if !commandName.MatchString(name) {
			return nil, errorf("commands: %q must be letters, digits, '-' or '_'", name)
		}
		if strings.TrimSpace(cmd) == "" {
			return nil, errorf("commands.%s: empty", name)
		}
	}
	for _, list := range []struct {
		name  string
		names []string
	}{{"prepare", p.Prepare}, {"after_switch", p.AfterSwitch}} {
		for _, n := range list.names {
			if _, ok := p.Commands[n]; !ok {
				return nil, errorf("%s: %q is not a declared command (declared: %s)", list.name, n, strings.Join(p.CommandNames(), ", "))
			}
		}
	}
	switch p.PHPExtensions {
	case "":
		p.PHPExtensions = "auto"
	case "auto", "none":
	default:
		return nil, errorf("php_extensions: auto or none, got %q", p.PHPExtensions)
	}
	switch p.Reload {
	case "":
		p.Reload = "none"
	case "none", "root-systemctl":
	default:
		return nil, errorf("reload: none or root-systemctl, got %q", p.Reload)
	}
	h := &p.Health
	if h.Backend == "" {
		h.Backend = "127.0.0.1:8080"
	}
	if !backendPattern.MatchString(h.Backend) {
		return nil, errorf("health.backend: %q must be host:port", h.Backend)
	}
	if h.Path == "" {
		h.Path = "/"
	}
	if !strings.HasPrefix(h.Path, "/") {
		return nil, errorf("health.path: %q must start with /", h.Path)
	}
	if len(h.Expect) == 0 {
		h.Expect = []int{200}
	}
	if h.Tries == 0 {
		h.Tries = 20
	}
	if h.Interval == 0 {
		h.Interval = 2 * time.Second
	}
	return &p, nil
}

// CommandNames lists the declared commands, sorted.
func (p *PHPFPM) CommandNames() []string {
	names := make([]string, 0, len(p.Commands))
	for n := range p.Commands {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// relativePath checks a path that must stay inside a release folder.
func relativePath(field, p string) error {
	clean := path.Clean(p)
	if p == "" || strings.HasPrefix(p, "/") || clean == ".." || strings.HasPrefix(clean, "../") || clean == "." {
		return errorf("%s: %q must be a relative path inside the release", field, p)
	}
	return nil
}

// VhostTemplateRef names the CloudPanel vhost template of a site. A plain string is the name of an existing template; a
// mapping {name, base} asks deploykit to create the template from a base one, with the root directory of the layout.
type VhostTemplateRef struct {
	Name string `yaml:"name"`
	Base string `yaml:"base"`
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (v *VhostTemplateRef) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		v.Name = n.Value
		return nil
	}
	var full struct {
		Name string `yaml:"name"`
		Base string `yaml:"base"`
	}
	if err := strictDecode(n, &full); err != nil {
		return err
	}
	v.Name, v.Base = full.Name, full.Base
	return nil
}
