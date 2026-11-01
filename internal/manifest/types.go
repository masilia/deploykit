package manifest

import "time"

// ReleaseDir configures the release-dir artifact: built locally, assembled into a folder, uploaded with rsync.
type ReleaseDir struct {
	Type   string `yaml:"type"`
	Source struct {
		Ref string `yaml:"ref"`
	} `yaml:"source"`
	Build struct {
		Node string `yaml:"node"`
		// Workdir is the folder of the export the build commands run in (a project whose application lives in a
		// subfolder). Assemble paths stay relative to the export root.
		Workdir string `yaml:"workdir"`
		// Image runs the build commands in a throwaway container of this Docker image instead of on this machine: the
		// export is mounted at /work, so the build needs no PHP, composer or node installed here, only Docker.
		Image   string            `yaml:"image"`
		Checks  []string          `yaml:"checks"`
		Install string            `yaml:"install"`
		Command string            `yaml:"command"`
		Env     map[string]string `yaml:"env"`
	} `yaml:"build"`
	Assemble []AssembleStep `yaml:"assemble"`
	Keep     int            `yaml:"keep"`
}

// AssembleStep copies From (relative to the build directory) to To (relative to the release folder).
type AssembleStep struct {
	From    string   `yaml:"from"`
	To      string   `yaml:"to"`
	Replace bool     `yaml:"replace"`
	Exclude []string `yaml:"exclude"`
}

// ReleaseDir returns the typed release-dir artifact with defaults applied (keep: 3, ref: HEAD).
func (m *Manifest) ReleaseDir() (*ReleaseDir, error) {
	var r ReleaseDir
	if err := m.Artifact.Decode(&r); err != nil {
		return nil, err
	}
	if r.Keep == 0 {
		r.Keep = 3
	}
	if r.Keep < 1 {
		return nil, errorf("keep must be at least 1")
	}
	if r.Source.Ref == "" {
		r.Source.Ref = "HEAD"
	}
	if len(r.Assemble) == 0 {
		return nil, errorf("assemble: at least one step is required")
	}
	if r.Build.Workdir != "" {
		if err := relativePath("build.workdir", r.Build.Workdir); err != nil {
			return nil, err
		}
	}
	if r.Build.Image != "" && !imagePattern.MatchString(r.Build.Image) {
		return nil, errorf("build.image: %q is not a Docker image reference", r.Build.Image)
	}
	return &r, nil
}

// Health describes the post-switch health probe.
type Health struct {
	Where    string        `yaml:"where"`
	Path     string        `yaml:"path"`
	Expect   int           `yaml:"expect"`
	Tries    int           `yaml:"tries"`
	Interval time.Duration `yaml:"interval"`
}

// PM2 configures the pm2 runtime.
type PM2 struct {
	Type      string            `yaml:"type"`
	Name      string            `yaml:"name"`
	Entry     string            `yaml:"entry"`
	Cwd       string            `yaml:"cwd"`
	Env       map[string]string `yaml:"env"`
	MaxMemory string            `yaml:"max_memory"`
	OnBoot    struct {
		SystemdUnit string `yaml:"systemd_unit"`
	} `yaml:"on_boot"`
	Switch    string `yaml:"switch"`
	Preflight struct {
		SameArch              bool `yaml:"same_arch"`
		PortFreeOnFirstDeploy bool `yaml:"port_free_on_first_deploy"`
	} `yaml:"preflight"`
	Health    Health `yaml:"health"`
	OnFailure struct {
		Rollback string `yaml:"rollback"`
		ShowLogs string `yaml:"show_logs"`
	} `yaml:"on_failure"`
}

// PM2 returns the typed pm2 runtime with defaults applied.
func (m *Manifest) PM2() (*PM2, error) {
	var p PM2
	if err := m.Runtime.Decode(&p); err != nil {
		return nil, err
	}
	if p.Entry == "" {
		return nil, errorf("entry: required")
	}
	if p.Name == "" {
		p.Name = "{{ project }}"
	}
	if p.Cwd == "" {
		p.Cwd = "current"
	}
	if p.Switch == "" {
		p.Switch = "atomic_symlink"
	}
	if p.Switch != "atomic_symlink" {
		return nil, errorf("switch: only atomic_symlink exists, got %q", p.Switch)
	}
	h := &p.Health
	if h.Where == "" {
		h.Where = "server"
	}
	if h.Path == "" {
		h.Path = "/"
	}
	if h.Expect == 0 {
		h.Expect = 200
	}
	if h.Tries == 0 {
		h.Tries = 30
	}
	if h.Interval == 0 {
		h.Interval = time.Second
	}
	if p.OnFailure.Rollback == "" {
		p.OnFailure.Rollback = "previous_release"
	}
	return &p, nil
}

// PasswordSpec asks for a generated password.
type PasswordSpec struct {
	Generate int    `yaml:"generate"`
	ShowOnce bool   `yaml:"show_once"`
	Store    string `yaml:"store"`
}

// CloudPanel configures the CloudPanel ingress.
type CloudPanel struct {
	Type string `yaml:"type"`
	Site struct {
		Type          string           `yaml:"type"`
		NodeVersion   string           `yaml:"node_version"`
		PHPVersion    string           `yaml:"php_version"`
		AppPort       string           `yaml:"app_port"`
		URL           string           `yaml:"url"`
		User          string           `yaml:"user"`
		UserPassword  PasswordSpec     `yaml:"user_password"`
		VhostTemplate VhostTemplateRef `yaml:"vhost_template"`
		WebRoot       string           `yaml:"web_root"`
	} `yaml:"site"`
	PortDiscovery struct {
		Listening       string `yaml:"listening"`
		Declared        string `yaml:"declared"`
		Pick            string `yaml:"pick"`
		Requested       string `yaml:"requested"`
		PersistInTarget bool   `yaml:"persist_in_target"`
	} `yaml:"port_discovery"`
	TLS struct {
		Command    string   `yaml:"command"`
		RequireDNS []string `yaml:"require_dns"`
		SanWWW     string   `yaml:"san_www"`
		OnFailure  string   `yaml:"on_failure"`
	} `yaml:"tls"`
	VhostTemplates []struct {
		Name string `yaml:"name"`
		File string `yaml:"file"`
	} `yaml:"vhost_templates"`
	VhostExtra struct {
		Status string `yaml:"status"`
	} `yaml:"vhost_extra"`
	Database struct {
		Create struct {
			Name     string       `yaml:"name"`
			User     string       `yaml:"user"`
			Password PasswordSpec `yaml:"password"`
		} `yaml:"create"`
	} `yaml:"database"`
}

// Smoke configures the local check of a release before anything is uploaded.
type Smoke struct {
	Start struct {
		Command string            `yaml:"command"`
		Env     map[string]string `yaml:"env"`
		Port    struct {
			Range []int `yaml:"range"`
		} `yaml:"port"`
		RemoveEnv []string `yaml:"remove_env"`
	} `yaml:"start"`
	FollowRedirects int      `yaml:"follow_redirects"`
	Expect          []int    `yaml:"expect"`
	Paths           []string `yaml:"paths"`
	Discover        []struct {
		From  string `yaml:"from"`
		Match string `yaml:"match"`
		First bool   `yaml:"first"`
	} `yaml:"discover"`
	Ranges []string `yaml:"ranges"`
}

// SmokeConfig returns the typed smoke section.
func (m *Manifest) SmokeConfig() (*Smoke, error) {
	var s Smoke
	if err := m.Smoke.Decode(&s); err != nil {
		return nil, err
	}
	if s.Start.Command == "" {
		return nil, errorf("start.command: required")
	}
	if len(s.Start.Port.Range) != 2 || s.Start.Port.Range[0] > s.Start.Port.Range[1] {
		return nil, errorf("start.port.range: want [low, high]")
	}
	if len(s.Expect) == 0 {
		s.Expect = []int{200}
	}
	return &s, nil
}
