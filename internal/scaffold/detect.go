// Package scaffold creates the configuration of a project: it looks at the repository, turns the answers of a form (or
// the flags of a script) into a deploykit.yaml and a target file, and writes them without overwriting anything silently.
package scaffold

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Kind is the sort of project found in a repository.
type Kind string

// The kinds Detect can report.
const (
	KindNone    Kind = ""
	KindNode    Kind = "pm2"     // a Node.js application run by pm2 (Next.js standalone)
	KindCompose Kind = "compose" // a Docker Compose stack
)

// Service is one service of a compose file with the role deploykit would give it.
type Service struct {
	Name  string
	Role  string
	Image string
}

// Findings is what Detect learned. Every value is only a default: the form lets the user change it.
type Findings struct {
	Dir         string
	Name        string // suggested project name, already a valid one
	Kind        Kind
	Also        Kind   // the other kind, when both were found
	GitRemote   string // origin URL, empty when unknown
	NodeVersion string // major version, default "22"

	Next        bool
	NextVersion string
	Scripts     map[string]string // the scripts of package.json that matter: lint, typecheck, build

	ComposeFiles []string // relative paths; the production file first
	Services     []Service
	Volumes      []string
	Database     string // postgres, mysql or empty
	Dockerfiles  []string
	EnvTemplate  string // an example .env found in the repository

	pkgName, composeName string // name hints, used by suggestName
}

var (
	projectName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	nonName     = regexp.MustCompile(`[^a-z0-9._-]+`)
	majorOnly   = regexp.MustCompile(`\d+`)
)

// Slug turns any text into a valid project name (lower case letters, digits, '.', '_' and '-').
func Slug(s string) string {
	s = nonName.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-")
	s = strings.Trim(s, "-._")
	if !projectName.MatchString(s) {
		return ""
	}
	return s
}

// Detect inspects dir. It never fails: what cannot be read is simply not reported.
func Detect(dir string) Findings {
	f := Findings{Dir: dir, NodeVersion: "22", Scripts: map[string]string{}}
	f.GitRemote = gitRemote(dir)
	node := detectNode(dir, &f)
	compose := detectCompose(dir, &f)
	f.Dockerfiles = dockerfiles(dir)
	for _, c := range []string{"deployment/.env.example", ".env.example", ".env.prod.example", ".env.production.example"} {
		if _, err := os.Stat(filepath.Join(dir, c)); err == nil {
			f.EnvTemplate = c
			break
		}
	}

	switch {
	case compose:
		f.Kind = KindCompose
		if node {
			f.Also = KindNode
		}
	case node:
		f.Kind = KindNode
	}
	f.Name = suggestName(dir, f)
	return f
}

func suggestName(dir string, f Findings) string {
	for _, c := range []string{f.composeName, f.pkgName, repoName(f.GitRemote), filepath.Base(dir)} {
		if s := Slug(c); s != "" {
			return s
		}
	}
	return "app"
}

// gitRemote reads the origin URL from .git/config without running git.
func gitRemote(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, ".git", "config")) //nolint:gosec // a file of the project being scaffolded
	if err != nil {
		return ""
	}
	inOrigin := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "["):
			inOrigin = line == `[remote "origin"]`
		case inOrigin && strings.HasPrefix(line, "url"):
			if _, v, ok := strings.Cut(line, "="); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

// repoName is the last path element of a git URL, without .git.
func repoName(url string) string {
	url = strings.TrimSuffix(strings.TrimSpace(url), ".git")
	if i := strings.LastIndexAny(url, "/:"); i >= 0 {
		return url[i+1:]
	}
	return url
}

func detectNode(dir string, f *Findings) bool {
	data, err := os.ReadFile(filepath.Join(dir, "package.json")) //nolint:gosec // a file of the project being scaffolded
	if err != nil {
		return false
	}
	var pkg struct {
		Name            string            `json:"name"`
		Scripts         map[string]string `json:"scripts"`
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
		Engines         map[string]string `json:"engines"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return false
	}
	f.pkgName = pkg.Name[strings.LastIndex(pkg.Name, "/")+1:]
	if v, ok := pkg.Dependencies["next"]; ok {
		f.Next, f.NextVersion = true, major(v)
	} else if v, ok := pkg.DevDependencies["next"]; ok {
		f.Next, f.NextVersion = true, major(v)
	}
	for _, name := range []string{"lint", "typecheck", "type-check", "build"} {
		if cmd, ok := pkg.Scripts[name]; ok {
			f.Scripts[name] = cmd
		}
	}
	for _, src := range []string{readTrim(filepath.Join(dir, ".nvmrc")), readTrim(filepath.Join(dir, ".node-version")), pkg.Engines["node"]} {
		if m := major(src); m != "" {
			f.NodeVersion = m
			break
		}
	}
	return f.Next || pkg.Scripts["build"] != "" && pkg.Scripts["start"] != ""
}

func readTrim(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // a file of the project being scaffolded
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// major returns the first number of a version or range ("^16.4.2" → "16", ">=22" → "22", "v20.1.0" → "20").
func major(v string) string { return majorOnly.FindString(v) }

type composeFile struct {
	Name     string                    `yaml:"name"`
	Services map[string]composeService `yaml:"services"`
	Volumes  map[string]any            `yaml:"volumes"`
}

type composeService struct {
	Image string `yaml:"image"`
}

// detectCompose finds the compose files at the top level and in docker/, puts the production one first, and reads the
// services and named volumes of that file.
func detectCompose(dir string, f *Findings) bool {
	var found []string
	for _, pattern := range []string{"docker-compose*.yml", "docker-compose*.yaml", "compose*.yml", "compose*.yaml"} {
		matches, _ := filepath.Glob(filepath.Join(dir, pattern))
		for _, m := range matches {
			found = append(found, filepath.Base(m))
		}
	}
	if len(found) == 0 {
		return false
	}
	sort.SliceStable(found, func(i, j int) bool { return composeRank(found[i]) < composeRank(found[j]) })
	found = dedupe(found)
	f.ComposeFiles = found

	var cf composeFile
	data, err := os.ReadFile(filepath.Join(dir, found[0])) //nolint:gosec // a file of the project being scaffolded
	if err != nil || yaml.Unmarshal(data, &cf) != nil {
		return true
	}
	f.composeName = cf.Name
	names := make([]string, 0, len(cf.Services))
	for n := range cf.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		svc := Service{Name: n, Image: cf.Services[n].Image}
		svc.Role = GuessRole(n, svc.Image)
		if svc.Role == "db" && f.Database == "" {
			f.Database = databaseOf(svc.Image + " " + n)
		}
		f.Services = append(f.Services, svc)
	}
	for v := range cf.Volumes {
		f.Volumes = append(f.Volumes, v)
	}
	sort.Strings(f.Volumes)
	return true
}

// composeRank orders compose files: production first, then the base file, then overrides.
func composeRank(name string) int {
	switch {
	case strings.Contains(name, "prod"):
		return 0
	case strings.Contains(name, "override"), strings.Contains(name, "cloudpanel"), strings.Contains(name, "dev"), strings.Contains(name, "local"):
		return 2
	}
	return 1
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// GuessRole gives a service the role deploykit uses to pick health and backup behaviour: proxy, app, worker, db, cache
// or realtime. The name wins for workers because they share the application's image.
func GuessRole(name, image string) string {
	n, img := strings.ToLower(name), strings.ToLower(image)
	has := func(s string, words ...string) bool {
		for _, w := range words {
			if strings.Contains(s, w) {
				return true
			}
		}
		return false
	}
	switch {
	case has(n, "worker", "scheduler", "cron", "messenger", "queue"):
		return "worker"
	case has(img, "postgres", "mysql", "mariadb") || n == "db" || n == "database":
		return "db"
	case has(img, "redis", "valkey", "memcached") || n == "redis" || n == "cache":
		return "cache"
	case has(img, "mercure") || n == "hub" || n == "mercure":
		return "realtime"
	case has(img, "nginx", "caddy", "traefik", "haproxy") || n == "proxy" || n == "nginx" || n == "web":
		return "proxy"
	}
	return "app"
}

func databaseOf(s string) string {
	switch {
	case strings.Contains(strings.ToLower(s), "postgres"):
		return "postgres"
	case strings.Contains(strings.ToLower(s), "mysql"), strings.Contains(strings.ToLower(s), "mariadb"):
		return "mysql"
	}
	return ""
}

// dockerfiles lists Dockerfile, Dockerfile.* at the top level and under docker/ (two levels deep).
func dockerfiles(dir string) []string {
	var out []string
	for _, pattern := range []string{"Dockerfile*", "docker/Dockerfile*", "docker/*/Dockerfile*"} {
		matches, _ := filepath.Glob(filepath.Join(dir, pattern))
		for _, m := range matches {
			if rel, err := filepath.Rel(dir, m); err == nil {
				out = append(out, filepath.ToSlash(rel))
			}
		}
	}
	sort.Strings(out)
	return dedupe(out)
}
