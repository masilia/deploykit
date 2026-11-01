package scaffold

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/masilia/deploykit/internal/inventory"
	"github.com/masilia/deploykit/internal/ui"
)

// FormOptions says where the form talks and how.
type FormOptions struct {
	In  io.Reader
	Out io.Writer
	// Accessible replaces the full-screen form by plain line prompts (a dumb terminal, --plain, scripted tests).
	Accessible bool
	Findings   Findings
}

// ErrAborted is returned when the user leaves the form (Ctrl-C, or the input ended).
var ErrAborted = errors.New("aborted")

// draft holds the form's raw text; apply turns it back into Answers after validation.
type draft struct {
	project, runtime, ingress, tls string
	id, host, sshPort, sshUser     string
	domain, siteUser, path         string
	appPort, proxyPort             string
	features                       []string
	composeFile, appService        string
	migrate, health                string
	dockerfiles                    []string
	dockerfile, servicesText       string
	smoke                          string
}

func newDraft(a Answers) *draft {
	d := &draft{
		project: a.Project, runtime: a.Runtime, ingress: a.Ingress, tls: a.Target.TLSMode,
		id: a.Target.ID, host: a.Target.Host, sshPort: strconv.Itoa(a.Target.SSHPort), sshUser: a.Target.SSHUser,
		domain: a.Target.Domain, siteUser: a.Target.SiteUser, path: a.Target.Path,
		appPort: strconv.Itoa(a.Target.AppPort), proxyPort: strconv.Itoa(a.Target.ProxyPort),
		features: a.Features, appService: a.AppService, migrate: a.MigrateCommand, health: a.HealthPath,
		smoke: strings.Join(a.SmokePaths, ", "),
	}
	if len(a.ComposeFiles) > 0 {
		d.composeFile = a.ComposeFiles[0]
	}
	for _, im := range a.Images {
		d.dockerfiles = append(d.dockerfiles, im.Dockerfile)
	}
	if d.id == "" {
		d.id = "prod"
	}
	if d.tls == "" {
		d.tls = "cloudpanel"
	}
	if d.health == "" {
		d.health = "/"
	}
	if d.sshPort == "0" {
		d.sshPort = "22"
	}
	if d.appPort == "0" {
		d.appPort = "3001"
	}
	if d.proxyPort == "0" {
		d.proxyPort = "8081"
	}
	return d
}

// cloudpanel reports whether the first target runs behind CloudPanel.
func (d *draft) cloudpanel() bool {
	return d.runtime == RuntimePM2 || d.ingress == IngressCloudPanel || (d.ingress == IngressSelect && d.tls != "certbot")
}

func (d *draft) compose() bool { return d.runtime == RuntimeCompose }

func siteUserFrom(domain string) string {
	label, _, _ := strings.Cut(domain, ".")
	if label == "www" {
		_, rest, _ := strings.Cut(domain, ".")
		label, _, _ = strings.Cut(rest, ".")
	}
	s := strings.ReplaceAll(Slug(label), ".", "-")
	if s == "" || s[0] >= '0' && s[0] <= '9' {
		s = "site" + s
	}
	return s
}

func (d *draft) defaultPath() string {
	if d.compose() {
		return "/opt/" + d.project
	}
	return "/home/" + d.effectiveSiteUser() + "/htdocs/" + d.domain
}

func (d *draft) effectiveSiteUser() string {
	if d.siteUser != "" {
		return d.siteUser
	}
	return siteUserFrom(d.domain)
}

// apply converts the form's text into answers; Validate reports what is still wrong.
func (d *draft) apply(a *Answers) error {
	port := func(name, s string) (int, error) {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return 0, fmt.Errorf("%s %q is not a number", name, s)
		}
		return n, nil
	}
	var errs []error
	keep := func(n int, err error) int {
		if err != nil {
			errs = append(errs, err)
		}
		return n
	}
	a.Project, a.Runtime, a.Ingress = d.project, d.runtime, d.ingress
	if d.runtime == RuntimePM2 {
		a.Ingress = IngressCloudPanel
	}
	a.Features, a.HealthPath = d.features, d.health
	t := &a.Target
	t.ID, t.Host, t.SSHUser, t.Domain, t.Path = d.id, strings.TrimSpace(d.host), d.sshUser, strings.TrimSpace(d.domain), strings.TrimSpace(d.path)
	t.SSHPort = keep(port("ssh port", d.sshPort))
	if d.runtime == RuntimePM2 {
		t.AppPort = keep(port("app port", d.appPort))
	}
	if d.compose() && d.cloudpanel() {
		t.ProxyPort = keep(port("proxy port", d.proxyPort))
	}
	if d.cloudpanel() {
		t.SiteUser = d.effectiveSiteUser()
	} else {
		t.SiteUser, t.ProxyPort = "", 0
	}
	if t.Path == "" {
		t.Path = d.defaultPath()
	}
	t.TLSMode = ""
	if a.Ingress == IngressSelect {
		t.TLSMode = "certbot"
		if d.tls != "certbot" {
			t.TLSMode = "cloudpanel"
		}
	}
	if d.runtime == RuntimePM2 {
		a.SmokePaths = nil
		for _, p := range strings.Split(d.smoke, ",") {
			if p = strings.TrimSpace(p); p != "" {
				a.SmokePaths = append(a.SmokePaths, p)
			}
		}
		if a.Has(FeatSmoke) && len(a.SmokePaths) == 0 {
			a.SmokePaths = []string{"/"}
		}
		if !a.Has(FeatChecks) {
			a.Checks = nil
		}
	} else {
		a.ComposeFiles = []string{strings.TrimSpace(d.composeFile)}
		a.AppService, a.MigrateCommand = d.appService, strings.TrimSpace(d.migrate)
		a.Images = imagesFrom(d.dockerfiles, d.dockerfile)
		if len(a.Services) == 0 {
			a.Services = parseServices(d.servicesText)
		}
		if a.AppService == "" && a.MigrateCommand != "" {
			errs = append(errs, errors.New("a migration command needs an app service to run in"))
		}
	}
	return errors.Join(errs...)
}

// imagesFrom names an image after each chosen Dockerfile, keeping the names unique.
func imagesFrom(chosen []string, typed string) []ImageRef {
	if len(chosen) == 0 && strings.TrimSpace(typed) != "" {
		chosen = []string{strings.TrimSpace(typed)}
	}
	var out []ImageRef
	used := map[string]int{}
	for _, df := range chosen {
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

// parseServices reads "web:proxy, app:app, db:db"; a bare name gets the role its name suggests.
func parseServices(text string) []Service {
	var out []Service
	for _, item := range strings.Split(text, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, role, ok := strings.Cut(item, ":")
		if !ok {
			role = GuessRole(name, "")
		}
		out = append(out, Service{Name: strings.TrimSpace(name), Role: strings.TrimSpace(role)})
	}
	return out
}

var numberPattern = regexp.MustCompile(`^\d+$`)

func validPort(min int) func(string) error {
	return func(s string) error {
		s = strings.TrimSpace(s)
		n, _ := strconv.Atoi(s)
		if !numberPattern.MatchString(s) || n < min || n > 65535 {
			return fmt.Errorf("a number between %d and 65535", min)
		}
		return nil
	}
}

func validHost(s string) error {
	if !hostPattern.MatchString(strings.TrimSpace(s)) {
		return errors.New("a host name or an address, for example srv.example.com")
	}
	return nil
}

func validUser(s string) error {
	if !userPattern.MatchString(s) {
		return errors.New("a lower-case system user name")
	}
	return nil
}

func validID(taken []string) func(string) error {
	return func(s string) error {
		switch {
		case !inventory.ValidID(s):
			return errors.New("letters, digits, '.', '_' and '-'")
		case contains(taken, s):
			return fmt.Errorf("targets/%s.env already exists", s)
		}
		return nil
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// DetectedFacts are the lines of the first screen: what was recognised in the directory.
func DetectedFacts(f Findings) []string {
	var out []string
	switch f.Kind {
	case KindNode:
		kind := "Node.js project"
		if f.Next {
			kind = "Next.js " + f.NextVersion
		}
		out = append(out, fmt.Sprintf("%s %s · Node %s", ui.IconOK, kind, f.NodeVersion))
	case KindCompose:
		out = append(out, fmt.Sprintf("%s %s · %d services", ui.IconOK, f.ComposeFiles[0], len(f.Services)))
		if f.Database != "" {
			out = append(out, fmt.Sprintf("%s %s database", ui.IconOK, f.Database))
		}
		if len(f.Dockerfiles) > 0 {
			out = append(out, fmt.Sprintf("%s %d Dockerfile(s)", ui.IconOK, len(f.Dockerfiles)))
		}
		if f.Also == KindNode {
			out = append(out, "  a package.json is also present; this scaffold is for the compose stack")
		}
	}
	if f.GitRemote != "" {
		out = append(out, fmt.Sprintf("%s git remote %s", ui.IconOK, f.GitRemote))
	}
	return out
}

// ---- the questions, declared once ------------------------------------------------------------------------------

type choice struct{ Label, Value string }

type qkind int

const (
	qNote qkind = iota
	qInput
	qSelect
	qMulti
)

// question is one prompt. The full-screen renderer and the line-prompt renderer both draw it.
type question struct {
	kind  qkind
	title string
	desc  string
	str   *string
	list  *[]string

	options  func() []choice
	validate func(string) error
	// suggest is what an empty answer means (a placeholder, or the [default] of a line prompt).
	suggest func() string
	// watch is what the suggestion or the options depend on, so the full-screen form refreshes them.
	watch any
}

// screen is a group of questions shown together, hidden when hide says so.
type screen struct {
	title string
	hide  func() bool
	qs    []question
}

func (s screen) hidden() bool { return s.hide != nil && s.hide() }

func note(title, desc string) question { return question{kind: qNote, title: title, desc: desc} }

func input(title, desc string, dst *string, validate func(string) error) question {
	return question{kind: qInput, title: title, desc: desc, str: dst, validate: validate}
}

func (q question) suggesting(f func() string, watch any) question {
	q.suggest, q.watch = f, watch
	return q
}

func pick(title, desc string, dst *string, options ...choice) question {
	return question{kind: qSelect, title: title, desc: desc, str: dst, options: func() []choice { return options }}
}

func initScreens(d *draft, a Answers, f Findings) []screen {
	step := func(n int, name string) string { return fmt.Sprintf("%d/6  %s", n, name) }
	var detected strings.Builder
	for _, l := range DetectedFacts(f) {
		detected.WriteString(l + "\n")
	}
	if detected.Len() == 0 {
		detected.WriteString("Nothing recognised in this directory: the answers start from defaults.")
	}
	projectCheck := func(s string) error {
		if !projectName.MatchString(s) {
			return errors.New("lower-case letters, digits, '.', '_' and '-'")
		}
		return nil
	}
	domainCheck := func(s string) error {
		if s = strings.TrimSpace(s); s == "" && !d.cloudpanel() {
			return nil
		}
		return validHost(s)
	}
	siteUserQ := func() question {
		return input("Site user", "The CloudPanel site's system user; owns the files.", &d.siteUser, nil).
			suggesting(func() string { return siteUserFrom(d.domain) }, d)
	}
	pathQ := func(desc string) question {
		return input("Path on the server", desc, &d.path, nil).suggesting(d.defaultPath, d)
	}
	return []screen{
		{title: step(1, "Project"), qs: []question{
			note("Detected", detected.String()),
			input("Project name", "Lower-case; names the compose project and the pm2 app.", &d.project, projectCheck),
			pick("What are you deploying?", "", &d.runtime,
				choice{"Next.js site on pm2 (CloudPanel Node.js site)", RuntimePM2},
				choice{"Docker Compose stack (images from a registry)", RuntimeCompose}),
		}},
		{title: step(2, "Ingress"), hide: func() bool { return !d.compose() }, qs: []question{
			pick("How does the outside world reach the stack?", "", &d.ingress,
				choice{"CloudPanel reverse proxy (CloudPanel owns ports 80/443)", IngressCloudPanel},
				choice{"Standalone: own nginx and certbot (needs free ports 80/443)", IngressStandalone},
				choice{"Per target: each target's TLS_MODE chooses", IngressSelect}),
		}},
		{title: step(3, "Server"), qs: []question{
			input("Target id", "Names the file targets/<id>.env.", &d.id, validID(nil)),
			input("Host", "Address or name the server is reached at over SSH.", &d.host, validHost),
			input("SSH port", "", &d.sshPort, validPort(1)),
			input("Domain", "The public domain of the site.", &d.domain, domainCheck),
		}},
		{title: step(3, "Server"), hide: func() bool { return !d.compose() }, qs: []question{
			input("SSH user", "The deploy login on the server (a pm2 site deploys as its site user).", &d.sshUser, validUser),
		}},
		{title: step(4, "Site"), hide: func() bool { return !d.compose() || d.ingress != IngressSelect }, qs: []question{
			pick("TLS for this first target", "The manifest lets each target choose.", &d.tls,
				choice{"CloudPanel (reverse-proxy site, Let's Encrypt by CloudPanel)", "cloudpanel"},
				choice{"certbot (standalone nginx)", "certbot"}),
		}},
		{title: step(4, "Site"), hide: func() bool { return d.compose() || !d.cloudpanel() }, qs: []question{
			siteUserQ(),
			pathQ("Where the site lives."),
			input("App port", "The loopback port nginx forwards to; `bootstrap` takes the next free one if it is taken.", &d.appPort, validPort(1024)),
		}},
		{title: step(4, "Site"), hide: func() bool { return !d.compose() || !d.cloudpanel() }, qs: []question{
			siteUserQ(),
			pathQ("The deployment root."),
			input("Proxy port", "The loopback port of the stack's proxy; `bootstrap` takes the next free one if it is taken.", &d.proxyPort, validPort(1024)),
		}},
		{title: step(4, "Site"), hide: func() bool { return !d.compose() || d.cloudpanel() }, qs: []question{
			pathQ("The deployment root."),
		}},
		{title: step(5, "Features"), qs: []question{{
			kind: qMulti, title: "Which sections do you want?", desc: "Each chosen section is written with its comments.",
			list: &d.features, watch: &d.runtime, options: func() []choice { return featureChoices(d, a) },
		}}},
		{title: step(6, "Application"), hide: func() bool { return !d.compose() }, qs: composeQuestions(d, a, f)},
		{title: step(6, "Application"), hide: func() bool { return d.compose() }, qs: []question{
			input("Health page", "Fetched on the server after the switch; a failure rolls back.", &d.health, nil),
			input("Smoke test pages", "Comma separated; fetched from the local release before anything is uploaded.", &d.smoke, nil),
		}},
	}
}

func composeQuestions(d *draft, a Answers, f Findings) []question {
	qs := []question{
		input("Compose file", "The production compose file in this repository.", &d.composeFile, nil),
	}
	apps := []choice{{"none", ""}}
	for _, s := range a.Services {
		if s.Role == "app" {
			apps = append(apps, choice{s.Name, s.Name})
		}
	}
	qs = append(qs, pick("Service the app commands run in", "", &d.appService, apps...),
		input("Migration command", "Optional. Run after the images are pulled, in a one-off container of that service.", &d.migrate, nil))
	if len(f.Dockerfiles) > 0 {
		qs = append(qs, question{kind: qMulti, title: "Dockerfiles to build as images", list: &d.dockerfiles, options: func() []choice {
			out := make([]choice, len(f.Dockerfiles))
			for i, df := range f.Dockerfiles {
				out[i] = choice{df, df}
			}
			return out
		}})
	} else {
		qs = append(qs, input("Dockerfile", "No Dockerfile was found: the one to build as the image.", &d.dockerfile, nil))
	}
	if len(a.Services) == 0 {
		qs = append(qs, input("Services", "No compose services were read. Comma separated name:role, roles: proxy, app, worker, db, cache, realtime.", &d.servicesText, nil))
	}
	return append(qs, input("Health page", "Fetched through the proxy after a deploy.", &d.health, nil))
}

// featureChoices lists the sections the chosen runtime offers. When none of the current selection applies (the user
// picked another runtime than the detected one) it falls back to that runtime's usual set.
func featureChoices(d *draft, a Answers) []choice {
	var opts []choice
	var defaults []string
	if d.runtime == RuntimePM2 {
		opts = []choice{
			{"Quality checks before the build (lint, types)", FeatChecks},
			{"Local smoke test of the release before uploading", FeatSmoke},
		}
		if len(a.Checks) > 0 {
			defaults = append(defaults, FeatChecks)
		}
		defaults = append(defaults, FeatSmoke)
	} else {
		if a.Database == "postgres" {
			opts = append(opts,
				choice{"Encrypted, verified backups with retention", FeatBackup},
				choice{"Monthly restore test of the newest backup", FeatRestoreTest})
			defaults = append(defaults, FeatBackup, FeatRestoreTest)
		}
		opts = append(opts,
			choice{"Supervision every 5 minutes, alerts to Slack", FeatMonitor},
			choice{"A block for your own cron jobs", FeatCrons})
		defaults = append(defaults, FeatMonitor)
	}
	var kept []string
	for _, o := range opts {
		if contains(d.features, o.Value) {
			kept = append(kept, o.Value)
		}
	}
	if len(kept) == 0 {
		kept = defaults
	}
	d.features = kept
	return opts
}

// TargetOptions are the manifest facts `target add` needs: it only asks about the server.
type TargetOptions struct {
	Runtime string
	Ingress string
	Project string
	// Taken lists the ids that already exist.
	Taken []string
}

func targetScreens(d *draft, t TargetOptions) []screen {
	step := func(n int, name string) string { return fmt.Sprintf("%d/2  %s", n, name) }
	return []screen{
		{title: step(1, "Server"), qs: []question{
			input("Target id", "Names the file targets/<id>.env.", &d.id, validID(t.Taken)),
			input("Host", "Address or name the server is reached at over SSH.", &d.host, validHost),
			input("SSH port", "", &d.sshPort, validPort(1)),
			input("Domain", "", &d.domain, func(s string) error {
				if s = strings.TrimSpace(s); s == "" && !d.cloudpanel() {
					return nil
				}
				return validHost(s)
			}),
		}},
		{title: step(1, "Server"), hide: func() bool { return !d.compose() }, qs: []question{
			input("SSH user", "The deploy login on the server.", &d.sshUser, validUser),
		}},
		{title: step(2, "Site"), hide: func() bool { return !d.compose() || d.ingress != IngressSelect }, qs: []question{
			pick("TLS for this target", "", &d.tls,
				choice{"CloudPanel (reverse-proxy site, Let's Encrypt by CloudPanel)", "cloudpanel"},
				choice{"certbot (standalone nginx)", "certbot"}),
		}},
		{title: step(2, "Site"), hide: func() bool { return !d.cloudpanel() }, qs: []question{
			input("Site user", "", &d.siteUser, nil).suggesting(func() string { return siteUserFrom(d.domain) }, d),
			input("Path on the server", "", &d.path, nil).suggesting(d.defaultPath, d),
			input("Loopback port", "App port (pm2) or proxy port (compose); `bootstrap` takes the next free one if it is taken.", &d.appPort, validPort(1024)),
		}},
		{title: step(2, "Site"), hide: func() bool { return d.cloudpanel() }, qs: []question{
			input("Path on the server", "", &d.path, nil).suggesting(d.defaultPath, d),
		}},
	}
}

// RunInitForm asks for everything `init` needs. The answers start from the detection and are updated in place.
func RunInitForm(o FormOptions, a *Answers) error {
	d := newDraft(*a)
	if err := runScreens(o, initScreens(d, *a, o.Findings)); err != nil {
		return err
	}
	a.Features = d.features
	return d.apply(a)
}

// RunTargetForm asks for one more server of an existing project.
func RunTargetForm(o FormOptions, t TargetOptions, a *Answers) error {
	d := newDraft(*a)
	d.runtime, d.ingress, d.project, d.id = t.Runtime, t.Ingress, t.Project, ""
	if err := runScreens(o, targetScreens(d, t)); err != nil {
		return err
	}
	d.proxyPort = d.appPort
	return d.apply(a)
}

func runScreens(o FormOptions, screens []screen) error {
	if o.Accessible {
		return runPlain(o, screens)
	}
	return runFullScreen(o, screens)
}
