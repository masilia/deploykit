// Package manifest loads and validates deploykit.yaml (ADR 0002).
//
// A manifest composes three axes: artifact, runtime and ingress. Parsing is strict: an unknown key is an error, and
// only the documented combinations of axes load. Sections whose consumers are not built yet (backup, monitor, fleet…)
// are kept as raw YAML and promoted to typed structs when their phase lands.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Axis type names.
const (
	ArtifactImage      = "image"
	ArtifactReleaseDir = "release-dir"

	RuntimeCompose = "compose"
	RuntimePM2     = "pm2"
	RuntimePHPFPM  = "php-fpm"

	IngressCloudPanel = "cloudpanel"
	IngressStandalone = "standalone"
)

var projectPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Manifest is a parsed deploykit.yaml.
type Manifest struct {
	Version      int    `yaml:"version"`
	Project      string `yaml:"project"`
	Inventory    string `yaml:"inventory"`
	GlobalConfig string `yaml:"global_config"`

	Artifact Axis    `yaml:"artifact"`
	Runtime  Axis    `yaml:"runtime"`
	Ingress  Ingress `yaml:"ingress"`

	SSH   SSH   `yaml:"ssh"`
	Hooks Hooks `yaml:"hooks"`
	Smoke Axis  `yaml:"smoke"` // typed on demand through Smoke()

	// Sections kept raw until their phase lands.
	Env         yaml.Node `yaml:"env"`
	Data        yaml.Node `yaml:"data"`
	Backup      yaml.Node `yaml:"backup"`
	RestoreTest yaml.Node `yaml:"restore_test"`
	Monitor     yaml.Node `yaml:"monitor"`
	Notify      yaml.Node `yaml:"notify"`
	Crons       yaml.Node `yaml:"crons"`
	Sync        yaml.Node `yaml:"sync"`
	Fleet       yaml.Node `yaml:"fleet"`

	path string
}

// SSH holds the access settings shared by every axis.
type SSH struct {
	CommandsOnStdin       bool   `yaml:"commands_on_stdin"`
	SiteUserPrelude       string `yaml:"site_user_prelude"`
	ProvisionUser         string `yaml:"provision_user"`
	ConnectTimeout        string `yaml:"connect_timeout"`
	Retries               *int   `yaml:"retries"` // retries of a connection that cannot be established; 0 disables
	BatchModeForReads     bool   `yaml:"batch_mode_for_reads"`
	InstallKeyForSiteUser struct {
		PublicKey string `yaml:"public_key"`
	} `yaml:"install_key_for_site_user"`
}

// Hook is one user command run at a hook point.
type Hook struct {
	Run string `yaml:"run"`
}

// Hooks are the user commands run around the main steps. Hook output is informational; a failing hook aborts.
type Hooks struct {
	BeforeUpload []Hook `yaml:"before_upload"`
	BeforeSwitch []Hook `yaml:"before_switch"`
	AfterSwitch  []Hook `yaml:"after_switch"`
}

// Axis is one axis of the manifest: a type discriminator plus the remaining configuration, decoded strictly on demand.
type Axis struct {
	Type string
	node yaml.Node
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (a *Axis) UnmarshalYAML(n *yaml.Node) error {
	var head struct {
		Type string `yaml:"type"`
	}
	if err := n.Decode(&head); err != nil {
		return err
	}
	a.Type, a.node = head.Type, *n
	return nil
}

// IsZero reports whether the axis was absent from the file.
func (a Axis) IsZero() bool { return a.node.Kind == 0 }

// Decode fills out from the axis, rejecting any key out does not declare.
func (a Axis) Decode(out any) error { return strictDecode(&a.node, out) }

// Ingress is the ingress axis. It is either a single Axis value or, when clients differ (the TLS_MODE key), a selector
// plus one complete variant per value.
type Ingress struct {
	Axis
	Select   string
	Variants map[string]Axis
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (i *Ingress) UnmarshalYAML(n *yaml.Node) error {
	var head struct {
		Select   string          `yaml:"select"`
		Variants map[string]Axis `yaml:"variants"`
	}
	if err := n.Decode(&head); err != nil {
		return err
	}
	if head.Select == "" && len(head.Variants) == 0 {
		return i.Axis.UnmarshalYAML(n)
	}
	for key := range mappingKeys(n) {
		if key != "select" && key != "variants" {
			return fmt.Errorf("line %d: ingress with select/variants cannot also set %q", n.Line, key)
		}
	}
	i.Select, i.Variants = head.Select, head.Variants
	i.node = *n
	return nil
}

// Variant returns the ingress axis for a target. selectValue is the already rendered Select expression; it is ignored
// when the manifest has a single ingress.
func (i Ingress) Variant(selectValue string) (Axis, error) {
	if i.Select == "" {
		return i.Axis, nil
	}
	v, ok := i.Variants[selectValue]
	if !ok {
		return Axis{}, fmt.Errorf("ingress has no variant %q (have %s)", selectValue, strings.Join(sortedKeys(i.Variants), ", "))
	}
	return v, nil
}

// Load reads and validates a manifest file.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is chosen by the operator
	if err != nil {
		return nil, err
	}
	m, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	m.path = path
	return m, nil
}

// Parse reads and validates manifest bytes.
func Parse(data []byte) (*Manifest, error) {
	root, err := rootNode(data)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := strictDecode(root, &m); err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// ReleaseBased reports a runtime served from release folders behind a CloudPanel site (pm2, php-fpm): they share the
// deploy, rollback and status flows. compose has its own.
func (m *Manifest) ReleaseBased() bool {
	return m.Runtime.Type == RuntimePM2 || m.Runtime.Type == RuntimePHPFPM
}

// Path is the file the manifest was loaded from, if any.
func (m *Manifest) Path() string { return m.path }

// Validate checks the manifest and returns every problem found.
func (m *Manifest) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if m.Version != 1 {
		add("version: must be 1, got %d", m.Version)
	}
	if !projectPattern.MatchString(m.Project) {
		add("project: %q must match %s", m.Project, projectPattern)
	}
	for name, ax := range map[string]Axis{"artifact": m.Artifact, "runtime": m.Runtime} {
		if ax.IsZero() {
			add("%s: required", name)
		}
	}
	if m.Ingress.IsZero() && m.Ingress.Select == "" {
		add("ingress: required")
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	if !oneOf(m.Artifact.Type, ArtifactImage, ArtifactReleaseDir) {
		add("artifact.type: %q is not one of image, release-dir", m.Artifact.Type)
	}
	if !oneOf(m.Runtime.Type, RuntimeCompose, RuntimePM2, RuntimePHPFPM) {
		add("runtime.type: %q is not one of compose, pm2, php-fpm", m.Runtime.Type)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	switch {
	case m.Artifact.Type == ArtifactImage && m.Runtime.Type != RuntimeCompose:
		add("artifact image requires runtime compose, not %s", m.Runtime.Type)
	case m.Artifact.Type == ArtifactReleaseDir && m.Runtime.Type == RuntimeCompose:
		add("artifact release-dir cannot run on runtime compose (use artifact image)")
	}

	for name, ing := range m.ingressAxes() {
		if !oneOf(ing.Type, IngressCloudPanel, IngressStandalone) {
			add("ingress%s: type %q is not one of cloudpanel, standalone", name, ing.Type)
			continue
		}
		if ing.Type == IngressStandalone && m.Runtime.Type != RuntimeCompose {
			add("ingress%s: standalone is only available for runtime compose", name)
		}
		if ing.Type == IngressCloudPanel {
			var cp CloudPanel
			if err := ing.Decode(&cp); err != nil {
				add("ingress%s: %v", name, err)
			} else if want := siteTypeFor(m.Runtime.Type); cp.Site.Type != want {
				add("ingress%s: site.type must be %s for runtime %s, got %q", name, want, m.Runtime.Type, cp.Site.Type)
			}
		}
	}

	if m.Artifact.Type == ArtifactReleaseDir {
		if _, err := m.ReleaseDir(); err != nil {
			add("artifact: %v", err)
		}
	}
	if m.Runtime.Type == RuntimePM2 {
		if _, err := m.PM2(); err != nil {
			add("runtime: %v", err)
		}
	}
	if m.Runtime.Type == RuntimePHPFPM {
		if m.Artifact.Type != ArtifactReleaseDir {
			add("runtime php-fpm needs artifact release-dir, got %s", m.Artifact.Type)
		}
		if _, err := m.PHP(); err != nil {
			add("runtime: %v", err)
		}
	}
	if !m.Smoke.IsZero() {
		if _, err := m.SmokeConfig(); err != nil {
			add("smoke: %v", err)
		}
	}
	errs = append(errs, m.validateCompose()...)
	return errors.Join(errs...)
}

// ingressAxes returns the ingress axes to validate, keyed by a path suffix for messages.
func (m *Manifest) ingressAxes() map[string]Axis {
	if m.Ingress.Select == "" {
		return map[string]Axis{"": m.Ingress.Axis}
	}
	out := make(map[string]Axis, len(m.Ingress.Variants))
	for k, v := range m.Ingress.Variants {
		out[".variants."+k] = v
	}
	return out
}

func siteTypeFor(runtime string) string {
	switch runtime {
	case RuntimePM2:
		return "nodejs"
	case RuntimePHPFPM:
		return "php"
	default:
		return "reverse-proxy"
	}
}

func oneOf(s string, options ...string) bool {
	for _, o := range options {
		if s == o {
			return true
		}
	}
	return false
}

func rootNode(data []byte) (*yaml.Node, error) {
	var n yaml.Node
	if err := yaml.Unmarshal(data, &n); err != nil {
		return nil, fmt.Errorf("%s", strings.TrimPrefix(err.Error(), "yaml: "))
	}
	if len(n.Content) == 0 { // an empty file: let validation report what is missing
		return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}, nil
	}
	return &n, nil
}

// strictDecode decodes n into out and fails on any key out does not declare.
func strictDecode(n *yaml.Node, out any) error {
	data, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%s", strings.TrimPrefix(err.Error(), "yaml: "))
	}
	return nil
}

func mappingKeys(n *yaml.Node) map[string]bool {
	keys := map[string]bool{}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			keys[n.Content[i].Value] = true
		}
	}
	return keys
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// validateCompose checks the sections that belong to the image / compose pair, strictly.
func (m *Manifest) validateCompose() []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if m.Artifact.Type == ArtifactImage {
		if _, err := m.Image(); err != nil {
			add("artifact: %v", err)
		}
	}
	if m.Runtime.Type != RuntimeCompose {
		return errs
	}
	if _, err := m.Compose(); err != nil {
		add("runtime: %v", err)
	}
	if _, err := m.EnvConfig(); err != nil {
		add("env: %v", err)
	}
	if m.Data.Kind != 0 || m.Backup.Kind != 0 {
		if _, _, err := m.BackupConfig(); err != nil {
			add("%v", err)
		}
	}
	if m.Monitor.Kind != 0 {
		if _, err := m.MonitorConfig(); err != nil {
			add("monitor: %v", err)
		}
	}
	if _, err := m.CronJobs(); err != nil {
		add("crons: %v", err)
	}
	if m.RestoreTest.Kind != 0 {
		if _, err := m.RestoreTestConfig(); err != nil {
			add("restore_test: %v", err)
		}
	}
	if _, err := m.FleetConfig(); err != nil {
		add("fleet: %v", err)
	}
	if _, err := m.SyncConfig(); err != nil {
		add("sync: %v", err)
	}
	for name, ing := range m.ingressAxes() {
		if ing.Type == IngressStandalone {
			var st Standalone
			if err := ing.Decode(&st); err != nil {
				add("ingress%s: %v", name, err)
			}
		}
	}
	return errs
}
