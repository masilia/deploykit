package scaffold

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"text/template"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

// File is one file a scaffold writes, with its path relative to the project directory.
type File struct {
	Path    string
	Content []byte
	Mode    os.FileMode
}

// The manifests contain deploykit's own {{ }} template language, so these templates use [[ ]].
const (
	leftDelim  = "[["
	rightDelim = "]]"
)

type secret struct {
	Key   string
	Bytes int
}

// view adds the derived values the templates need to the answers.
type view struct {
	Answers
}

func (v view) RuntimeFiles() []string {
	files := slices.Clone(v.ComposeFiles)
	if v.Ingress == IngressCloudPanel && v.Override != "" {
		files = append(files, v.Override)
	}
	return files
}

// OverrideWhenCloudPanel is true when the override file only applies to the targets that use CloudPanel.
func (v view) OverrideWhenCloudPanel() bool { return v.Ingress == IngressSelect && v.Override != "" }

// CloudPanelPossible is true when at least one target can run behind CloudPanel.
func (v view) CloudPanelPossible() bool { return v.Ingress != IngressStandalone }

// MonitoringCron is true when deploykit manages cron jobs for backups, the restore test or the monitor.
func (v view) MonitoringCron() bool {
	return v.Has(FeatBackup) || v.Has(FeatRestoreTest) || v.Has(FeatMonitor)
}

func (v view) DBService() string {
	for _, s := range v.Services {
		if s.Role == "db" {
			return s.Name
		}
	}
	return ""
}

func (v view) hasRole(role string) bool {
	return slices.ContainsFunc(v.Services, func(s Service) bool { return s.Role == role })
}

func (v view) Secrets() []secret {
	var out []secret
	if v.Database == "postgres" {
		out = append(out, secret{"POSTGRES_PASSWORD", 16})
	}
	if v.Has(FeatBackup) {
		out = append(out, secret{"BACKUP_PASSPHRASE", 16})
	}
	return out
}

func (v view) UpdateSteps() []string {
	steps := []string{"lock", "disk_check"}
	if v.Has(FeatBackup) {
		steps = append(steps, "backup")
	}
	steps = append(steps, "pull")
	if v.MigrateCommand != "" {
		steps = append(steps, "migrate")
	}
	if len(v.Secrets()) > 0 {
		steps = append(steps, "secrets")
	}
	steps = append(steps, "up")
	if v.hasRole("proxy") {
		steps = append(steps, "proxy_reload")
	}
	return append(steps, "verify")
}

func (v view) InstallSteps() []string {
	var steps []string
	if len(v.Secrets()) > 0 {
		steps = append(steps, "secrets")
	}
	steps = append(steps, "images_pull", "up_data")
	if v.MigrateCommand != "" {
		steps = append(steps, "migrate")
	}
	steps = append(steps, "up_all", "tls")
	if v.MonitoringCron() || v.Has(FeatCrons) {
		steps = append(steps, "crons")
	}
	return append(steps, "health")
}

// SyncFiles is what `deploy` uploads next to the compose files.
func (v view) SyncFiles() []string {
	files := slices.Clone(v.ComposeFiles)
	if v.Override != "" && v.Ingress != IngressStandalone {
		files = append(files, v.Override)
	}
	if v.EnvTemplate != "" {
		files = append(files, v.EnvTemplate)
	}
	files = append(files, v.SyncDirs...)
	return slices.Compact(files)
}

var (
	plainScalar = regexp.MustCompile(`^[A-Za-z_./][A-Za-z0-9_./-]*$`)
	yamlWords   = []string{"true", "false", "yes", "no", "on", "off", "null", "y", "n"}
)

// scalar writes s as a YAML scalar: bare when that is unambiguous, double-quoted otherwise (JSON escapes are valid YAML).
func scalar(s string) string {
	if plainScalar.MatchString(s) && !slices.Contains(yamlWords, strings.ToLower(s)) {
		return s
	}
	b, _ := json.Marshal(s)
	return string(b)
}

var funcs = template.FuncMap{
	"q": scalar,
	// flow writes a list as a YAML flow sequence.
	"flow": func(items []string) string {
		out := make([]string, len(items))
		for i, s := range items {
			out[i] = scalar(s)
		}
		return "[" + strings.Join(out, ", ") + "]"
	},
	"indent": func(n int, s string) string {
		pad := strings.Repeat(" ", n)
		lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
		for i, l := range lines {
			if l != "" {
				lines[i] = pad + l
			}
		}
		return strings.Join(lines, "\n")
	},
}

var blankRuns = regexp.MustCompile(`\n{3,}`)

// Manifest renders deploykit.yaml.
func Manifest(a Answers) ([]byte, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	main := "compose.yaml.tmpl"
	if a.Runtime == RuntimePM2 {
		main = "pm2.yaml.tmpl"
	}
	t := template.New("manifest").Delims(leftDelim, rightDelim).Funcs(funcs)
	t.Funcs(template.FuncMap{"include": func(name string) (string, error) {
		var b bytes.Buffer
		err := t.ExecuteTemplate(&b, "ingress-"+name+".tmpl", nil)
		return b.String(), err
	}})
	if _, err := t.ParseFS(templateFS, "templates/*.tmpl"); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := t.ExecuteTemplate(&out, main, view{a}); err != nil {
		return nil, fmt.Errorf("render %s: %w", main, err)
	}
	return append(bytes.TrimRight(blankRuns.ReplaceAll(out.Bytes(), []byte("\n\n")), "\n"), '\n'), nil
}

const globalExample = `# Editor-side settings, NOT versioned: copy this file to deploy.env (mode 600).
# It is the global configuration of every target: a key is looked up in the target file first, then here.

# The registry namespace the images are pushed to and pulled from, e.g. ghcr.io/my-org
REGISTRY=
# Credentials used to push the images and, on the server, to pull them (a token with read access is enough there).
GHCR_USER=
GHCR_TOKEN=
`

// Files renders everything a project needs: the manifest, the first target and, for compose, the global example.
func Files(a Answers) ([]File, error) {
	manifest, err := Manifest(a)
	if err != nil {
		return nil, err
	}
	files := []File{
		{Path: "deploykit.yaml", Content: manifest, Mode: 0o644},
		TargetFile(a),
	}
	if a.Runtime == RuntimeCompose {
		files = append(files, File{Path: "deploy.env.example", Content: []byte(globalExample), Mode: 0o644})
	}
	return files, nil
}

// TargetFile is the file of the first target (or of the one `target add` creates).
func TargetFile(a Answers) File {
	return File{Path: "targets/" + a.Target.ID + ".env", Content: []byte(a.Target.EnvFile(a.Runtime, a.Ingress)), Mode: 0o600}
}
