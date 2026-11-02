package scaffold

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/masilia/deploykit/internal/inventory"
)

// Runtimes and ingress modes a scaffold can produce.
const (
	RuntimePM2     = "pm2"
	RuntimeCompose = "compose"
	RuntimePHP     = "php-fpm"

	IngressCloudPanel = "cloudpanel"
	IngressStandalone = "standalone"
	IngressSelect     = "select" // chosen per target by TLS_MODE
)

// Optional sections of a manifest, chosen by feature.
const (
	FeatBackup      = "backup"       // compose: encrypted, verified backups with retention
	FeatRestoreTest = "restore-test" // compose: monthly proof that the newest backup restores
	FeatMonitor     = "monitor"      // compose: five-minute supervision with Slack alerts
	FeatCrons       = "crons"        // compose: a block for the project's own cron jobs
	FeatChecks      = "checks"       // pm2: lint and type checks before the build
	FeatSmoke       = "smoke"        // pm2: start the release locally and fetch pages before uploading
)

// Target is the first server of the inventory (or the one `target add` creates).
type Target struct {
	ID       string
	Host     string
	SSHPort  int
	SSHUser  string // compose: the deploy login (ssh_user)
	SiteUser string // CloudPanel: the site's system user; owner of the files and deploy login for pm2
	Domain   string
	Path     string // absolute path of the site or deployment root on the server
	AppPort  int    // pm2: the port nginx forwards to
	// ProxyPort is the loopback port of the compose proxy behind CloudPanel.
	ProxyPort int
	// TLSMode is certbot or cloudpanel; written only when the manifest chooses the ingress per target.
	TLSMode string
}

// NamedCommand is a command of runtime.commands.
type NamedCommand struct{ Name, Run string }

// SharedFile is an entry of runtime.shared.
type SharedFile struct {
	Path     string
	Kind     string // file or dir
	Required bool
}

// Answers is everything the generators need. The form fills it; flags fill it for scripts.
type Answers struct {
	Project  string
	Runtime  string
	Ingress  string
	Features []string
	Target   Target

	// pm2 (Next.js standalone)
	NodeVersion string
	Next        bool
	Checks      []string // commands run before the build
	SmokePaths  []string // pages fetched by the local smoke test
	Keep        int      // releases kept on the server

	// compose
	ProjectDirectory string // "." when the compose files live in a subfolder while the server .env stays in the root
	ComposeFiles     []string
	Override         string // compose file added when the target uses CloudPanel (TLS_MODE=cloudpanel)
	Services         []Service
	Images           []ImageRef
	AppService       string         // service the app commands run in
	MigrateCommand   string         // optional: run after pulling, before restarting
	AppCommands      []NamedCommand // app.commands of a framework project (migrate, graphql, reindex…), migrate first
	Database         string         // postgres, mysql or empty
	DBImage          string         // the database image: tells MariaDB from MySQL
	EnvKeys          []string       // variables of the env template that still say CHANGE_ME
	Notes            []string       // things detection could not settle, shown to the user
	Volumes          []string
	EnvTemplate      string // optional .env template in the repository, copied on first install
	SyncDirs         []string

	// php-fpm
	PHPVersion string
	WebRoot    string
	Workdir    string // the folder of the repository holding the application ("" when it is the root)
	BuildImage string // optional: build in a container of this image
	Framework  string // ibexa, symfony or php
	Encore     bool
	Commands   []NamedCommand // runtime.commands, in order
	Prepare    []string       // names run before the switch
	Shared     []SharedFile
	// Full and FullReasons say that the project uses Redis, Solr or Varnish: shown to the user, not written to the manifest.
	Full        bool
	FullReasons []string

	// HealthPath is the page checked after a deploy (pm2: on the server; compose: through the proxy).
	HealthPath string
}

// Has reports whether a feature was chosen.
func (a Answers) Has(feature string) bool { return slices.Contains(a.Features, feature) }

// DefaultAnswers turns detection into the answers the form starts from.
func DefaultAnswers(f Findings) Answers {
	a := Answers{
		Project:     f.Name,
		NodeVersion: f.NodeVersion,
		Next:        f.Next,
		Keep:        3,
		HealthPath:  "/",
		Database:    f.Database,
	}
	switch f.Kind {
	case KindCompose:
		a.Runtime = RuntimeCompose
		a.Ingress = IngressSelect
		a.ComposeFiles = f.ComposeFiles[:1]
		for _, extra := range f.ComposeFiles[1:] {
			if strings.Contains(extra, "cloudpanel") {
				a.Override = extra
			}
		}
		if len(f.Dockerfiles) > 0 {
			a.SyncDirs = []string{"docker/"}
		}
		a.Services = f.Services
		a.Images = slices.Clone(f.Images)
		if len(a.Images) == 0 {
			for _, df := range f.Dockerfiles {
				a.Images = append(a.Images, ImageRef{Name: imageName(df), Dockerfile: df})
			}
		} else {
			// Images taken from the registry: only the folders the compose file mounts reach the server, not the build files.
			a.SyncDirs = f.BindDirs
			a.Notes = append([]string{"the images were matched to Dockerfiles by name: check the context and target of each one in artifact.images"}, f.ImageNotes...)
		}
		if len(f.ComposeFiles) > 0 && path.Dir(f.ComposeFiles[0]) != "." {
			a.ProjectDirectory = "."
		}
		a.DBImage, a.EnvKeys = f.DBImage, f.EnvKeys
		for _, s := range f.Services {
			if s.Role == "app" {
				a.AppService = s.Name
				break
			}
		}
		a.Volumes = f.Volumes
		a.EnvTemplate = f.EnvTemplate
		a.Features = []string{FeatMonitor}
		switch f.Database {
		case "postgres":
			a.Features = []string{FeatBackup, FeatRestoreTest, FeatMonitor}
		case "mysql": // the monthly restore proof supports PostgreSQL only
			a.Features = []string{FeatBackup, FeatMonitor}
		}
		if f.Framework != "" {
			a.AppCommands, a.MigrateCommand = composeAppCommands(f, a.Services)
		}
		a.Target = Target{SSHPort: 22, SSHUser: "deploy", ProxyPort: 8081}
	case KindPHP:
		a.Runtime = RuntimePHP
		a.Ingress = IngressCloudPanel
		a.PHPVersion, a.WebRoot, a.Workdir = f.PHPVersion, "public", f.PHPDir
		if a.PHPVersion == "" {
			a.PHPVersion = "8.3"
		}
		a.Framework, a.Encore, a.Full, a.FullReasons = f.Framework, f.Encore, f.Full, f.FullReasons
		a.Shared = []SharedFile{{".env.local", "file", true}, {"var/log", "dir", false}, {"var/sessions", "dir", false}}
		if f.Framework == "ibexa" {
			a.Shared = append(a.Shared, SharedFile{"public/var", "dir", false}, SharedFile{"config/jwt", "dir", false})
		}
		a.Commands = []NamedCommand{{"warmup", "{{ php }} bin/console cache:warmup"}}
		a.Prepare = []string{"warmup"}
		for _, c := range f.Console {
			name, run := consoleCommand(c)
			a.Commands = append(a.Commands, NamedCommand{name, run})
			a.Prepare = append(a.Prepare, name)
		}
		a.Target = Target{SSHPort: 22}
	default: // pm2, and the starting point when nothing was recognised
		a.Runtime = RuntimePM2
		a.Ingress = IngressCloudPanel
		a.Checks = nodeChecks(f)
		if len(a.Checks) > 0 {
			a.Features = append(a.Features, FeatChecks)
		}
		a.Features = append(a.Features, FeatSmoke)
		a.SmokePaths = []string{"/"}
		a.Target = Target{SSHPort: 22, AppPort: 3001}
	}
	return a
}

// FillDefaults gives every field left empty the value the form would propose: a script that passes only --host and
// --domain gets the same files as someone who accepts every default on screen.
func FillDefaults(a *Answers) {
	t := &a.Target
	if t.ID == "" {
		t.ID = "prod"
	}
	if t.SSHPort == 0 {
		t.SSHPort = 22
	}
	if a.HealthPath == "" {
		a.HealthPath = "/"
	}
	if a.Keep == 0 {
		a.Keep = 3
	}
	if a.Runtime == RuntimePHP && a.WebRoot == "" {
		a.WebRoot = "public"
	}
	if a.Runtime == RuntimeCompose {
		if t.SSHUser == "" {
			t.SSHUser = "deploy"
		}
		if a.Ingress == IngressSelect && t.TLSMode == "" {
			t.TLSMode = "cloudpanel"
		}
		if t.ProxyPort == 0 {
			t.ProxyPort = 8081
		}
	} else if a.Runtime == RuntimePM2 && t.AppPort == 0 {
		t.AppPort = 3001
	}
	if a.UsesCloudPanel() && t.SiteUser == "" {
		t.SiteUser = siteUserFrom(t.Domain)
	}
	if t.Path == "" {
		switch {
		case a.Runtime == RuntimeCompose:
			t.Path = "/opt/" + a.Project
		case t.Domain != "":
			t.Path = "/home/" + t.SiteUser + "/htdocs/" + t.Domain
		}
	}
}

// composeAppCommands turns the console commands of a framework project into app.commands: they run in the app container, so
// they use the container's own `php`. The first migration command is `migrate`, which the update and install steps run.
func composeAppCommands(f Findings, services []Service) ([]NamedCommand, string) {
	var out []NamedCommand
	migrate := ""
	for _, c := range f.Console {
		name, run := consoleCommand(c)
		run = strings.ReplaceAll(run, "{{ php }}", "php")
		switch c {
		case "kaliop:migration:migrate", "doctrine:migrations:migrate":
			if migrate != "" {
				continue
			}
			name, migrate = "migrate", run
		}
		out = append(out, NamedCommand{name, run})
	}
	if f.JWT {
		out = append(out, NamedCommand{"jwt_keypair", "php bin/console lexik:jwt:generate-keypair --skip-if-exists"})
	}
	if f.Framework == "ibexa" {
		for _, s := range services {
			switch s.Role {
			case "search":
				out = append(out, NamedCommand{"reindex", "php -d memory_limit=2G bin/console ibexa:reindex --no-interaction"})
			case "cache":
				out = append(out, NamedCommand{"cache_clear", "php bin/console cache:pool:clear cache.redis"})
			}
		}
	}
	slices.SortStableFunc(out, func(a, b NamedCommand) int {
		switch {
		case a.Name == "migrate" && b.Name != "migrate":
			return -1
		case b.Name == "migrate" && a.Name != "migrate":
			return 1
		}
		return 0
	})
	return out, migrate
}

// ImagesFor lists the images to build for the Dockerfiles chosen. An image detection already tied to a chosen Dockerfile
// keeps its name, context and stage (one multi-stage Dockerfile can give several images); a Dockerfile without one gets a
// name from its own.
func ImagesFor(chosen []string, detected []ImageRef) []ImageRef {
	var out []ImageRef
	have := map[string]bool{}
	for _, d := range detected {
		if slices.Contains(chosen, d.Dockerfile) {
			out = append(out, d)
			have[d.Dockerfile] = true
		}
	}
	used := map[string]int{}
	for _, df := range chosen {
		if have[df] {
			continue
		}
		name := imageName(df)
		if name == "" {
			name = "app"
		}
		if n := used[name]; n > 0 {
			name += "-" + strconv.Itoa(n+1)
		}
		used[imageName(df)]++
		out = append(out, ImageRef{Name: name, Dockerfile: df})
	}
	return out
}

// consoleCommand names a detected console command and gives its command line.
func consoleCommand(c string) (name, run string) {
	switch c {
	case "kaliop:migration:migrate":
		return "migrate", "{{ php }} -d memory_limit=512M bin/console kaliop:migration:migrate --no-interaction"
	case "doctrine:migrations:migrate":
		return "doctrine-migrate", "{{ php }} bin/console doctrine:migrations:migrate --no-interaction --allow-no-migration"
	case "ibexa:graphql:generate-schema":
		return "graphql", "{{ php }} bin/console ibexa:graphql:generate-schema"
	}
	return strings.ReplaceAll(c, ":", "-"), "{{ php }} bin/console " + c
}

// AssetCommands are the build commands that compile the front-end assets of a PHP project.
func (a Answers) AssetCommands() []string {
	if !a.Encore {
		return nil
	}
	if a.Framework == "ibexa" {
		return []string{
			"corepack yarn install --frozen-lockfile",
			"php bin/console assets:install public",
			"php bin/console ibexa:encore:compile --config-name app",
			"php bin/console ibexa:encore:compile",
			"php bin/console bazinga:js-translation:dump public/assets --merge-domains",
		}
	}
	return []string{"corepack yarn install --frozen-lockfile", "corepack yarn encore production"}
}

// AssembleFrom is what the release is assembled from: the application folder of the export.
func (a Answers) AssembleFrom() string {
	if a.Workdir == "" {
		return "."
	}
	return a.Workdir + "/."
}

// ImageName derives an image name from a Dockerfile path: docker/Dockerfile.api → api, Dockerfile → app.
func ImageName(dockerfile string) string { return imageName(dockerfile) }

func imageName(dockerfile string) string {
	base := path.Base(dockerfile)
	if i := strings.Index(base, "."); i >= 0 && i+1 < len(base) {
		return Slug(base[i+1:])
	}
	return "app"
}

func nodeChecks(f Findings) []string {
	var out []string
	if _, ok := f.Scripts["lint"]; ok {
		out = append(out, "npm run lint")
	}
	switch {
	case f.Scripts["typecheck"] != "":
		out = append(out, "npm run typecheck")
	case f.Scripts["type-check"] != "":
		out = append(out, "npm run type-check")
	case f.Next:
		// A clean export has no .next/types: generate them before the type check.
		out = append(out, "npx next typegen", "npx tsc --noEmit")
	}
	return out
}

var (
	phpVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
	hostPattern       = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)
	userPattern       = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)
	pathPattern       = regexp.MustCompile(`^/[A-Za-z0-9._/@+-]*$`)
)

// UsesCloudPanel reports whether this target runs behind CloudPanel: always for pm2, by ingress for compose, and by the
// target's own TLS mode when the manifest lets each target choose.
func (t Target) UsesCloudPanel(runtime, ingress string) bool {
	return runtime == RuntimePM2 || runtime == RuntimePHP || ingress == IngressCloudPanel || (ingress == IngressSelect && t.TLSMode != "certbot")
}

// UsesCloudPanel reports the same for the first target of these answers.
func (a Answers) UsesCloudPanel() bool { return a.Target.UsesCloudPanel(a.Runtime, a.Ingress) }

// Validate lists every problem of a target; the form shows them field by field, `target add` and the flags all at once.
func (t Target) Validate(runtime, ingress string) error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if !inventory.ValidID(t.ID) {
		add("target id %q must use letters, digits, '.', '_' and '-'", t.ID)
	}
	if !hostPattern.MatchString(t.Host) {
		add("host %q is not a host name or an address", t.Host)
	}
	if t.SSHPort < 1 || t.SSHPort > 65535 {
		add("ssh port %d is out of range", t.SSHPort)
	}
	if !pathPattern.MatchString(t.Path) || t.Path == "/" {
		add("path %q must be an absolute path without spaces", t.Path)
	}
	cloudpanel := t.UsesCloudPanel(runtime, ingress)
	if t.Domain != "" && !hostPattern.MatchString(t.Domain) {
		add("domain %q is not a valid host name", t.Domain)
	}
	if cloudpanel {
		if t.Domain == "" {
			add("a domain is required: CloudPanel creates the site for it")
		}
		if !userPattern.MatchString(t.SiteUser) {
			add("site user %q must be a lower-case system user name", t.SiteUser)
		}
	}
	if runtime == RuntimeCompose && !userPattern.MatchString(t.SSHUser) {
		add("ssh user %q must be a lower-case system user name", t.SSHUser)
	}
	if runtime == RuntimePM2 && (t.AppPort < 1024 || t.AppPort > 65535) {
		add("app port %d must be between 1024 and 65535", t.AppPort)
	}
	if runtime == RuntimeCompose && cloudpanel && (t.ProxyPort < 1024 || t.ProxyPort > 65535) {
		add("proxy port %d must be between 1024 and 65535", t.ProxyPort)
	}
	if t.TLSMode != "" && t.TLSMode != "certbot" && t.TLSMode != "cloudpanel" {
		add("tls mode %q must be certbot or cloudpanel", t.TLSMode)
	}
	return errors.Join(errs...)
}

// Validate checks the answers as a whole.
func (a Answers) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if !projectName.MatchString(a.Project) {
		add("project %q must use lower-case letters, digits, '.', '_' and '-'", a.Project)
	}
	switch a.Runtime {
	case RuntimePHP:
		if a.Ingress != IngressCloudPanel {
			add("a php-fpm site runs behind CloudPanel: ingress must be cloudpanel, got %q", a.Ingress)
		}
		if !phpVersionPattern.MatchString(a.PHPVersion) {
			add("php version %q must look like 8.2", a.PHPVersion)
		}
		if a.WebRoot == "" || strings.HasPrefix(a.WebRoot, "/") || strings.Contains(a.WebRoot, "..") {
			add("web root %q must be a folder inside the application", a.WebRoot)
		}
		if strings.HasPrefix(a.Workdir, "/") || strings.Contains(a.Workdir, "..") {
			add("workdir %q must be a folder inside the repository", a.Workdir)
		}
		if !strings.HasPrefix(a.HealthPath, "/") {
			add("health path %q must start with /", a.HealthPath)
		}
	case RuntimePM2:
		if a.Ingress != IngressCloudPanel {
			add("a pm2 site runs behind CloudPanel: ingress must be cloudpanel, got %q", a.Ingress)
		}
		if a.Keep < 1 {
			add("keep must be at least 1 release")
		}
		if !strings.HasPrefix(a.HealthPath, "/") {
			add("health path %q must start with /", a.HealthPath)
		}
		for _, p := range a.SmokePaths {
			if !strings.HasPrefix(p, "/") {
				add("smoke path %q must start with /", p)
			}
		}
	case RuntimeCompose:
		if !slices.Contains([]string{IngressCloudPanel, IngressStandalone, IngressSelect}, a.Ingress) {
			add("ingress must be cloudpanel, standalone or select, got %q", a.Ingress)
		}
		if len(a.ComposeFiles) == 0 {
			add("a compose file is required")
		}
		if len(a.Services) == 0 {
			add("at least one service is required")
		}
		if len(a.Images) == 0 {
			add("at least one image (name and Dockerfile) is required")
		}
		if a.AppService != "" && !slices.ContainsFunc(a.Services, func(s Service) bool { return s.Name == a.AppService }) {
			add("the app service %q is not one of the services", a.AppService)
		}
		if a.Has(FeatBackup) && a.Database != "postgres" && a.Database != "mysql" {
			add("backups need a PostgreSQL, MySQL or MariaDB service")
		}
		if a.Has(FeatRestoreTest) && a.Database != "postgres" {
			add("the restore test needs a PostgreSQL service")
		}
		if a.Has(FeatRestoreTest) && !a.Has(FeatBackup) {
			add("the restore test needs backups")
		}
		if !strings.HasPrefix(a.HealthPath, "/") {
			add("health path %q must start with /", a.HealthPath)
		}
	default:
		add("runtime must be pm2 or compose, got %q", a.Runtime)
	}
	if err := a.Target.Validate(a.Runtime, a.Ingress); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// EnvFile renders the target file (inventory: versioned, no secret) with canonical keys.
func (t Target) EnvFile(runtime, ingress string) string {
	var b strings.Builder
	kv := func(k, v string) { b.WriteString(k + "=" + v + "\n") }
	fmt.Fprintf(&b, "# Target %s (no secret). Canonical keys, see docs/targets.md.\n", t.ID)
	kv("HOST", t.Host)
	kv("SSH_PORT", strconv.Itoa(t.SSHPort))
	switch runtime {
	case RuntimePM2, RuntimePHP:
		kv("ROOT_USER", "root")
		kv("SITE_USER", t.SiteUser)
		kv("DOMAIN", t.Domain)
		kv("PATH_ON_SERVER", t.Path)
		if runtime == RuntimePM2 {
			kv("APP_PORT", strconv.Itoa(t.AppPort))
			kv("SITE_URL", "https://"+t.Domain)
		}
	default:
		kv("SSH_USER", t.SSHUser)
		if t.UsesCloudPanel(runtime, ingress) {
			kv("ROOT_USER", "root")
			kv("SITE_USER", t.SiteUser)
		}
		kv("DOMAIN", t.Domain)
		kv("PATH_ON_SERVER", t.Path)
		if ingress == IngressSelect && t.TLSMode != "" {
			kv("TLS_MODE", t.TLSMode)
		}
		if t.UsesCloudPanel(runtime, ingress) {
			kv("PROXY_PORT", strconv.Itoa(t.ProxyPort))
		}
		b.WriteString("# CONTRACT: supervised = scheduled healthcheck in CI; ondemand = on request\n")
		kv("CONTRACT", "ondemand")
	}
	return b.String()
}
