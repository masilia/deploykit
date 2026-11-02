package manifest

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Image configures the image artifact: Docker images built from a git tag and pushed to a registry.
type Image struct {
	Type          string `yaml:"type"`
	Registry      string `yaml:"registry"`
	RegistryLogin struct {
		Host  string `yaml:"host"`
		User  string `yaml:"user"`
		Token string `yaml:"token"`
	} `yaml:"registry_login"`
	VersionPattern string `yaml:"version_pattern"`
	Source         struct {
		Ref                string `yaml:"ref"`
		CreateTagIfMissing bool   `yaml:"create_tag_if_missing"`
	} `yaml:"source"`
	Images []struct {
		Name       string `yaml:"name"`
		Dockerfile string `yaml:"dockerfile"`
		Context    string `yaml:"context"`
		Target     string `yaml:"target"` // multi-stage Dockerfile: the stage to build (docker build --target)
	} `yaml:"images"`
	Tags  []string `yaml:"tags"`
	Gates struct {
		Command string `yaml:"command"`
		OptIn   bool   `yaml:"opt_in"`
	} `yaml:"gates"`
	EnsureRelease bool `yaml:"ensure_release"`
}

// Image returns the typed image artifact.
func (m *Manifest) Image() (*Image, error) {
	var i Image
	if err := m.Artifact.Decode(&i); err != nil {
		return nil, err
	}
	if i.Registry == "" {
		return nil, errorf("registry: required")
	}
	if len(i.Images) == 0 {
		return nil, errorf("images: at least one image is required")
	}
	for n, img := range i.Images {
		if img.Name == "" || img.Dockerfile == "" {
			return nil, errorf("images[%d]: name and dockerfile are required", n)
		}
		if img.Target != "" && !stageNamePattern.MatchString(img.Target) {
			return nil, errorf("images[%d]: target %q is not a Dockerfile stage name", n, img.Target)
		}
	}
	if len(i.Tags) == 0 {
		i.Tags = []string{"{{ version }}", "sha-{{ commit7 }}"}
	}
	if i.VersionPattern == "" {
		i.VersionPattern = `^[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.-]+)?$`
	}
	if i.Source.Ref == "" {
		i.Source.Ref = "v{{ version }}"
	}
	return &i, nil
}

// AppCommand is a command run in a one-off container: a plain string, or a mapping with environment indirection.
type AppCommand struct {
	Run string
	// EnvFrom maps a variable of the command to the server .env key whose value it receives, so a secret reaches the
	// command through the environment and never through its arguments.
	EnvFrom map[string]string
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (c *AppCommand) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		c.Run = n.Value
		return nil
	}
	var full struct {
		Run     string            `yaml:"run"`
		EnvFrom map[string]string `yaml:"env_from"`
	}
	if err := strictDecode(n, &full); err != nil {
		return err
	}
	c.Run, c.EnvFrom = full.Run, full.EnvFrom
	return nil
}

// Probe is one health probe of a compose stack.
type Probe struct {
	Name        string `yaml:"name"`
	HTTP        string `yaml:"http"`
	Contains    string `yaml:"contains"`
	Method      string `yaml:"method"`
	Tries       int    `yaml:"tries"`
	Delay       string `yaml:"delay"`
	Service     string `yaml:"service"`
	LogContains string `yaml:"log_contains"`
	Since       string `yaml:"since"`
	Wait        bool   `yaml:"wait"`
}

// Service is one service of the stack.
type Service struct {
	Name string `yaml:"name"`
	Role string `yaml:"role"`
}

// Compose configures the Docker Compose runtime.
type Compose struct {
	Type       string   `yaml:"type"`
	MinCompose string   `yaml:"min_compose"`
	Files      []string `yaml:"files"`
	// ProjectDirectory is passed as `docker compose --project-directory`: where the server .env is read and what relative
	// paths in the compose files mean. Needed when the files live in a subfolder (docker/) but the .env sits in the root.
	ProjectDirectory string `yaml:"project_directory"`
	Overrides        []struct {
		When string `yaml:"when"`
		File string `yaml:"file"`
	} `yaml:"overrides"`
	VersionState struct {
		File        string `yaml:"file"`
		CurrentKey  string `yaml:"current_key"`
		PreviousKey string `yaml:"previous_key"`
	} `yaml:"version_state"`
	InstalledMarker string    `yaml:"installed_marker"`
	Journal         string    `yaml:"journal"`
	Lock            string    `yaml:"lock"`
	Services        []Service `yaml:"services"`
	App             struct {
		Service  string                `yaml:"service"`
		Commands map[string]AppCommand `yaml:"commands"`
		Hooks    struct {
			AfterMigrate []string `yaml:"after_migrate"`
		} `yaml:"hooks"`
	} `yaml:"app"`
	Health struct {
		Wait struct {
			Timeout  time.Duration `yaml:"timeout"`
			Interval time.Duration `yaml:"interval"`
		} `yaml:"wait"`
		Probes []Probe  `yaml:"probes"`
		After  []string `yaml:"after"`
	} `yaml:"health"`
	Update struct {
		Steps []string `yaml:"steps"`
		Disk  struct {
			Path      string `yaml:"path"`
			MinFreeMB string `yaml:"min_free_mb"`
		} `yaml:"disk"`
		OnFailure struct {
			Rollback string `yaml:"rollback"`
		} `yaml:"on_failure"`
		Cleanup struct {
			KeepVersions  []string `yaml:"keep_versions"`
			PruneDangling bool     `yaml:"prune_dangling"`
		} `yaml:"cleanup"`
		After []string `yaml:"after"`
	} `yaml:"update"`
	Install struct {
		Steps     []string `yaml:"steps"`
		Reminders []string `yaml:"reminders"`
	} `yaml:"install"`
}

// Compose returns the typed compose runtime with defaults applied.
func (m *Manifest) Compose() (*Compose, error) {
	var c Compose
	if err := m.Runtime.Decode(&c); err != nil {
		return nil, err
	}
	if d := c.ProjectDirectory; d != "" && (strings.HasPrefix(d, "/") || d == ".." || strings.HasPrefix(d, "../") || strings.Contains(d, "/../")) {
		return nil, errorf("project_directory %q must be a path inside the deployment root", d)
	}
	if len(c.Files) == 0 {
		return nil, errorf("files: at least one compose file is required")
	}
	if c.MinCompose == "" {
		c.MinCompose = "2.24"
	}
	vs := &c.VersionState
	if vs.File == "" {
		vs.File = ".env"
	}
	if vs.CurrentKey == "" {
		vs.CurrentKey = "APP_VERSION"
	}
	if vs.PreviousKey == "" {
		vs.PreviousKey = "APP_VERSION_PREV"
	}
	if c.InstalledMarker == "" {
		c.InstalledMarker = ".installed"
	}
	if c.Journal == "" {
		c.Journal = "versions.log"
	}
	if c.Lock == "" {
		c.Lock = ".lock-ops"
	}
	if c.Health.Wait.Timeout == 0 {
		c.Health.Wait.Timeout = 180 * time.Second
	}
	if c.Health.Wait.Interval == 0 {
		c.Health.Wait.Interval = 5 * time.Second
	}
	if len(c.Services) == 0 {
		return nil, errorf("services: at least one service is required")
	}
	seen := map[string]bool{}
	for _, s := range c.Services {
		if s.Name == "" || seen[s.Name] {
			return nil, errorf("services: %q is empty or duplicated", s.Name)
		}
		seen[s.Name] = true
	}
	if c.App.Service != "" && !seen[c.App.Service] {
		return nil, errorf("app.service %q is not a declared service", c.App.Service)
	}
	for _, name := range c.Update.Steps {
		if !oneOf(name, UpdateSteps...) {
			return nil, errorf("update.steps: unknown step %q (known: %s)", name, strings.Join(UpdateSteps, ", "))
		}
	}
	for _, name := range c.Install.Steps {
		if !oneOf(name, InstallSteps...) {
			return nil, errorf("install.steps: unknown step %q (known: %s)", name, strings.Join(InstallSteps, ", "))
		}
	}
	for _, name := range append(append([]string(nil), c.Health.After...), c.Update.After...) {
		if _, ok := c.App.Commands[name]; !ok && name != "monitoring_cron" {
			return nil, errorf("%q is referenced but is neither an app command nor a known step", name)
		}
	}
	return &c, nil
}

// EnvSpec describes the server .env.
type EnvSpec struct {
	File     string   `yaml:"file"`
	Template string   `yaml:"template"`
	Prompts  []string `yaml:"prompts"`
	Secrets  map[string]struct {
		HexBytes int `yaml:"hex_bytes"`
	} `yaml:"secrets"`
	FromTargetOnDeploy []string `yaml:"from_target_on_deploy"`
}

// EnvConfig returns the typed env section; absent means no managed secrets.
func (m *Manifest) EnvConfig() (*EnvSpec, error) {
	var e EnvSpec
	if m.Env.Kind != 0 {
		if err := strictDecode(&m.Env, &e); err != nil {
			return nil, err
		}
	}
	if e.File == "" {
		e.File = ".env"
	}
	for k, s := range e.Secrets {
		if s.HexBytes < 1 {
			return nil, errorf("secrets.%s: hex_bytes must be at least 1", k)
		}
	}
	return &e, nil
}

// DataSpec describes what a backup contains.
type DataSpec struct {
	Database struct {
		Type    string `yaml:"type"`
		Service string `yaml:"service"`
		Dump    string `yaml:"dump"`
		Verify  string `yaml:"verify"`
	} `yaml:"database"`
	Volumes      []string `yaml:"volumes"`
	IncludeFiles []string `yaml:"include_files"`
}

// BackupSpec describes how backups are made, kept and copied.
type BackupSpec struct {
	NamePrefix string `yaml:"name_prefix"`
	Dir        string `yaml:"dir"`
	Encryption struct {
		Type          string `yaml:"type"`
		Cipher        string `yaml:"cipher"`
		PassphraseKey string `yaml:"passphrase_key"`
	} `yaml:"encryption"`
	Verify    []string `yaml:"verify"`
	Retention struct {
		DailyDays    int `yaml:"daily_days"`
		MonthlyDays  int `yaml:"monthly_days"`
		MonthlyOnDay int `yaml:"monthly_on_day"`
	} `yaml:"retention"`
	MinFreeMB string `yaml:"min_free_mb"`
	Offsite   struct {
		RsyncTarget string `yaml:"rsync_target"`
		Optional    bool   `yaml:"optional"`
		OnFailure   string `yaml:"on_failure"`
	} `yaml:"offsite"`
	Schedule string `yaml:"schedule"`
}

// BackupConfig returns the typed data and backup sections together; both are required for a backup.
func (m *Manifest) BackupConfig() (*DataSpec, *BackupSpec, error) {
	if m.Data.Kind == 0 || m.Backup.Kind == 0 {
		return nil, nil, errorf("data and backup sections are required")
	}
	var d DataSpec
	var b BackupSpec
	if err := strictDecode(&m.Data, &d); err != nil {
		return nil, nil, fmt.Errorf("data: %w", err)
	}
	if err := strictDecode(&m.Backup, &b); err != nil {
		return nil, nil, fmt.Errorf("backup: %w", err)
	}
	if b.NamePrefix == "" {
		return nil, nil, errorf("backup.name_prefix: required")
	}
	if b.Dir == "" {
		b.Dir = "./backups"
	}
	if b.Encryption.Type != "" && b.Encryption.Type != "gpg" {
		return nil, nil, errorf("backup.encryption.type: only gpg exists, got %q", b.Encryption.Type)
	}
	if b.Encryption.Type == "gpg" && b.Encryption.PassphraseKey == "" {
		return nil, nil, errorf("backup.encryption.passphrase_key: required with gpg")
	}
	r := &b.Retention
	if r.DailyDays == 0 {
		r.DailyDays = 7
	}
	if r.MonthlyDays == 0 {
		r.MonthlyDays = 30
	}
	if r.MonthlyOnDay == 0 {
		r.MonthlyOnDay = 1
	}
	if r.MonthlyOnDay < 1 || r.MonthlyOnDay > 28 {
		return nil, nil, errorf("backup.retention.monthly_on_day must be between 1 and 28")
	}
	return &d, &b, nil
}

// MonitorCheck is one check of the supervision cron: the string "services", or a mapping.
type MonitorCheck struct {
	Kind     string // services | http | disk
	HTTP     string
	Contains string
	Header   map[string]string
	// JSONStatus is the dotted path of a field whose value is ok, warn or fail.
	JSONStatus string
	DiskWarnMB string
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (c *MonitorCheck) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		if n.Value != "services" {
			return fmt.Errorf("line %d: unknown monitor check %q", n.Line, n.Value)
		}
		c.Kind = "services"
		return nil
	}
	var full struct {
		HTTP       string            `yaml:"http"`
		Contains   string            `yaml:"contains"`
		Header     map[string]string `yaml:"header"`
		JSONStatus string            `yaml:"json_status"`
		DiskWarnMB string            `yaml:"disk_free_mb_warn"`
	}
	if err := strictDecode(n, &full); err != nil {
		return err
	}
	switch {
	case full.HTTP != "":
		c.Kind = "http"
	case full.DiskWarnMB != "":
		c.Kind = "disk"
	default:
		return fmt.Errorf("line %d: a monitor check needs http or disk_free_mb_warn", n.Line)
	}
	c.HTTP, c.Contains, c.Header, c.JSONStatus, c.DiskWarnMB = full.HTTP, full.Contains, full.Header, full.JSONStatus, full.DiskWarnMB
	return nil
}

// MonitorSpec configures the supervision cron.
type MonitorSpec struct {
	Schedule        string         `yaml:"schedule"`
	StateFile       string         `yaml:"state_file"`
	FailThreshold   string         `yaml:"fail_threshold"`
	RemindHours     string         `yaml:"remind_hours"`
	SkipWhileLocked bool           `yaml:"skip_while_locked"`
	Checks          []MonitorCheck `yaml:"checks"`
	BaseURLWhenCP   string         `yaml:"base_url_when_cloudpanel"`
}

// MonitorConfig returns the typed monitor section.
func (m *Manifest) MonitorConfig() (*MonitorSpec, error) {
	if m.Monitor.Kind == 0 {
		return nil, errorf("monitor section is required")
	}
	var s MonitorSpec
	if err := strictDecode(&m.Monitor, &s); err != nil {
		return nil, err
	}
	if s.StateFile == "" {
		s.StateFile = "log/monitor.state"
	}
	if s.Schedule == "" {
		s.Schedule = "*/5 * * * *"
	}
	return &s, nil
}

// FleetSpec configures multi-target operations.
type FleetSpec struct {
	RolloutOrderKey  string   `yaml:"rollout_order_key"`
	ContractKey      string   `yaml:"contract_key"`
	BackupStaleHours int      `yaml:"backup_stale_hours"`
	StatusColumns    []string `yaml:"status_columns"`
}

// FleetConfig returns the typed fleet section with defaults (26 hours, as fleet.sh).
func (m *Manifest) FleetConfig() (*FleetSpec, error) {
	var f FleetSpec
	if m.Fleet.Kind != 0 {
		if err := strictDecode(&m.Fleet, &f); err != nil {
			return nil, err
		}
	}
	if f.BackupStaleHours == 0 {
		f.BackupStaleHours = 26
	}
	return &f, nil
}

// Standalone configures the standalone ingress (own nginx container and certbot).
type Standalone struct {
	Type string `yaml:"type"`
	TLS  struct {
		Type       string   `yaml:"type"`
		Email      string   `yaml:"email"`
		Volumes    []string `yaml:"volumes"`
		DomainConf struct {
			Template string `yaml:"template"`
			Target   string `yaml:"target"`
		} `yaml:"domain_conf"`
		Renew struct {
			Schedule string `yaml:"schedule"`
		} `yaml:"renew"`
	} `yaml:"tls"`
	Preflight struct {
		Port80Free       bool `yaml:"port_80_free"`
		DNSMatchesServer bool `yaml:"dns_matches_server"`
	} `yaml:"preflight"`
}

// UpdateSteps and InstallSteps are the step names the manifest may list, in the order the tool knows them.
var (
	UpdateSteps  = []string{"lock", "disk_check", "backup", "pull", "migrate", "secrets", "up", "proxy_reload", "verify"}
	InstallSteps = []string{"secrets", "images_pull", "up_data", "jwt_keypair", "migrate", "seed_roles", "create_admin", "up_all", "tls", "crons", "monitoring_cron", "health"}
)

// RestoreTestSpec configures the monthly proof that the latest backup can be restored.
type RestoreTestSpec struct {
	Schedule          string `yaml:"schedule"`
	ThrowawayDatabase string `yaml:"throwaway_database"`
	Checks            struct {
		MinTablesRatio    float64 `yaml:"min_tables_ratio"`
		MigrationsTable   string  `yaml:"migrations_table"`
		MigrationsMinRows int     `yaml:"migrations_min_rows"`
	} `yaml:"checks"`
}

// RestoreTestConfig returns the typed restore_test section with defaults (90 % of the tables, one migration).
func (m *Manifest) RestoreTestConfig() (*RestoreTestSpec, error) {
	if m.RestoreTest.Kind == 0 {
		return nil, errorf("restore_test section is required")
	}
	var r RestoreTestSpec
	if err := strictDecode(&m.RestoreTest, &r); err != nil {
		return nil, err
	}
	if r.ThrowawayDatabase == "" {
		r.ThrowawayDatabase = "restore_check"
	}
	if !restoreDBPattern.MatchString(r.ThrowawayDatabase) {
		return nil, errorf("throwaway_database %q must be a plain identifier", r.ThrowawayDatabase)
	}
	if r.Checks.MinTablesRatio == 0 {
		r.Checks.MinTablesRatio = 0.9
	}
	if r.Checks.MinTablesRatio < 0 || r.Checks.MinTablesRatio > 1 {
		return nil, errorf("checks.min_tables_ratio must be between 0 and 1")
	}
	if r.Checks.MigrationsTable != "" && !restoreDBPattern.MatchString(r.Checks.MigrationsTable) {
		return nil, errorf("checks.migrations_table %q must be a plain identifier", r.Checks.MigrationsTable)
	}
	return &r, nil
}

var restoreDBPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// CronJob is a project-specific scheduled command. Run executes in the deployment root with `compose` available as a
// shell function.
type CronJob struct {
	Name     string `yaml:"name"`
	Schedule string `yaml:"schedule"`
	Run      string `yaml:"run"`
}

// stageNamePattern is what `FROM … AS <name>` accepts, so a target never carries a flag or a path.
var stageNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

var cronNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// CronJobs returns the crons section, validated.
func (m *Manifest) CronJobs() ([]CronJob, error) {
	if m.Crons.Kind == 0 {
		return nil, nil
	}
	var jobs []CronJob
	if err := strictDecode(&m.Crons, &jobs); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i, j := range jobs {
		switch {
		case !cronNamePattern.MatchString(j.Name):
			return nil, errorf("crons[%d]: invalid name %q", i, j.Name)
		case seen[j.Name]:
			return nil, errorf("crons[%d]: duplicate name %q", i, j.Name)
		case len(strings.Fields(j.Schedule)) != 5:
			return nil, errorf("crons[%d] %s: schedule %q needs five fields", i, j.Name, j.Schedule)
		case strings.TrimSpace(j.Run) == "":
			return nil, errorf("crons[%d] %s: run is required", i, j.Name)
		}
		seen[j.Name] = true
	}
	return jobs, nil
}

// SyncSpec lists what `deploy` uploads to the server next to the compose files. An entry ending in "/" is a directory,
// mirrored with --delete; the others are single files. Never lists patterns that must not reach a server (the editor's
// credentials, the inventory of other targets).
type SyncSpec struct {
	Files []string `yaml:"files"`
	Never []string `yaml:"never"`
}

// SyncConfig returns the typed sync section.
func (m *Manifest) SyncConfig() (*SyncSpec, error) {
	var s SyncSpec
	if m.Sync.Kind != 0 {
		if err := strictDecode(&m.Sync, &s); err != nil {
			return nil, err
		}
	}
	for _, f := range s.Files {
		if f == "" || strings.HasPrefix(f, "/") || strings.Contains(f, "..") {
			return nil, errorf("sync.files: %q must be a relative path inside the project", f)
		}
	}
	return &s, nil
}
