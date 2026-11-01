package main

import (
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"github.com/masilia/deploykit/internal/cloudpanel"
	"github.com/masilia/deploykit/internal/envfile"
	"github.com/masilia/deploykit/internal/inventory"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/runner"
	"github.com/masilia/deploykit/internal/transport"
	"github.com/masilia/deploykit/internal/ui"
)

// role selects which SSH identity a transport uses on a CloudPanel server.
type role int

const (
	roleSite role = iota // the site user: every deploy
	roleRoot             // root or a sudoer: provisioning only (ADR 0004)
)

// app carries the flags shared by every command and the seams tests replace.
type app struct {
	manifestPath string
	repoPath     string // the git repository to build and upload from; default: the manifest's directory
	verbose      bool
	dryRun       bool

	out io.Writer

	// Seams: production values are set by newApp; tests inject fakes.
	local transport.Transport
	ssh   func(t *inventory.Target, m *manifest.Manifest, r role) (transport.Transport, error)
	dns   cloudpanel.Resolver
	now   func() time.Time
	// sleep waits between health polls; tests replace it so a failing check does not wait for its timeout.
	sleep func(time.Duration)
}

func newApp(out io.Writer) *app {
	return &app{out: out, local: transport.NewLocal(), ssh: defaultSSH, now: time.Now}
}

// project is a loaded manifest with the paths derived from its location.
type project struct {
	m      *manifest.Manifest
	dir    string // where the manifest lives: the inventory and the global configuration are relative to it
	repo   string // the git repository to build and upload from (--repo; default: dir)
	global *envfile.Doc
}

func (a *app) load() (*project, error) {
	path := a.manifestPath
	if path == "" {
		path = "deploykit.yaml"
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	m, err := manifest.Load(abs)
	if err != nil {
		return nil, err
	}
	p := &project{m: m, dir: filepath.Dir(abs)}
	p.repo = p.dir
	if a.repoPath != "" {
		if p.repo, err = filepath.Abs(a.repoPath); err != nil {
			return nil, err
		}
	}
	if m.GlobalConfig != "" {
		if p.global, err = envfile.LoadOptional(filepath.Join(p.dir, m.GlobalConfig)); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// inventoryDir is where the targets live.
func (p *project) inventoryDir() string {
	dir := p.m.Inventory
	if dir == "" {
		dir = "targets"
	}
	if filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(p.dir, dir)
}

// target opens the named target; with no name, the only one of the inventory.
func (p *project) target(id string) (*inventory.Target, error) {
	if id == "" {
		ids, err := inventory.List(p.inventoryDir())
		if err != nil {
			return nil, err
		}
		if len(ids) != 1 {
			return nil, fmt.Errorf("choose a target with --target (inventory %s: %v)", p.inventoryDir(), ids)
		}
		id = ids[0]
	}
	return inventory.Open(p.inventoryDir(), id, p.global)
}

func defaultSSH(t *inventory.Target, m *manifest.Manifest, r role) (transport.Transport, error) {
	host := t.Get("host")
	if host == "" {
		return nil, fmt.Errorf("target %s has no host", t.ID)
	}
	port, err := strconv.Atoi(t.Get("ssh_port"))
	if err != nil {
		return nil, fmt.Errorf("target %s: ssh_port must be a number", t.ID)
	}
	s := &transport.SSH{Host: host, Port: port}
	if s.ConnectTimeout, s.Retries, err = sshSettings(m.SSH); err != nil {
		return nil, err
	}
	switch r {
	case roleRoot:
		s.User = t.Get("root_user")
		if s.User == "" {
			s.User = m.SSH.ProvisionUser
		}
		if s.User == "" {
			s.User = "root"
		}
	default:
		s.User = t.Get("ssh_user")
		s.Prelude = m.SSH.SiteUserPrelude
	}
	return s, nil
}

// sshSettings reads ssh.connect_timeout (a duration of at least one second) and ssh.retries (0 disables the retries of
// a connection that cannot be established). The transport's own defaults apply to what is not set.
func sshSettings(c manifest.SSH) (connectTimeout, retries int, err error) {
	if c.ConnectTimeout != "" {
		d, perr := time.ParseDuration(c.ConnectTimeout)
		if perr != nil || d < time.Second {
			return 0, 0, fmt.Errorf("ssh.connect_timeout %q must be a duration of at least 1s, such as 10s", c.ConnectTimeout)
		}
		connectTimeout = int(d / time.Second)
	}
	if c.Retries != nil {
		switch {
		case *c.Retries < 0:
			return 0, 0, fmt.Errorf("ssh.retries must be 0 or more, got %d", *c.Retries)
		case *c.Retries == 0:
			retries = -1
		default:
			retries = *c.Retries
		}
	}
	return connectTimeout, retries, nil
}

func (a *app) runner(dryRun bool) *runner.Runner {
	return &runner.Runner{Reporter: ui.NewPlain(a.out, a.verbose), DryRun: dryRun}
}

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}
