package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/masilia/deploykit/internal/inventory"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/scaffold"
	"github.com/masilia/deploykit/internal/ui"
)

// answerFlags carry every answer of the form, so a script (or CI, or a test) can do what the screens do.
type answerFlags struct {
	name, runtime, ingress, features    string
	id, host, sshUser, domain, siteUser string
	path, tlsMode                       string
	sshPort, appPort, proxyPort         int
	composeFile, appService, migrate    string
	healthPath                          string
	dockerfiles                         []string
	yes, force                          bool
}

func (f *answerFlags) register(cmd *cobra.Command, withProject bool) {
	fl := cmd.Flags()
	if withProject {
		fl.StringVar(&f.name, "name", "", "project name (default: detected)")
		fl.StringVar(&f.runtime, "runtime", "", "pm2 or compose (default: detected)")
		fl.StringVar(&f.ingress, "ingress", "", "compose: cloudpanel, standalone or select (per-target TLS_MODE)")
		fl.StringVar(&f.features, "features", "", "comma separated: backup, restore-test, monitor, crons (compose); checks, smoke (pm2)")
		fl.StringVar(&f.composeFile, "compose-file", "", "compose: the production compose file (default: detected)")
		fl.StringVar(&f.appService, "app-service", "", "compose: service the app commands run in")
		fl.StringVar(&f.migrate, "migrate", "", "compose: migration command, run in a one-off container of the app service")
		fl.StringVar(&f.healthPath, "health-path", "", "page checked after a deploy (default /)")
		fl.StringArrayVar(&f.dockerfiles, "dockerfile", nil, "compose: a Dockerfile to build as an image (repeatable; default: detected)")
	}
	fl.StringVar(&f.id, "id", "", "target id, the file targets/<id>.env (default: prod)")
	fl.StringVar(&f.host, "host", "", "server address or name")
	fl.IntVar(&f.sshPort, "ssh-port", 0, "SSH port (default 22)")
	fl.StringVar(&f.sshUser, "ssh-user", "", "compose: the deploy login (default deploy)")
	fl.StringVar(&f.domain, "domain", "", "public domain of the site")
	fl.StringVar(&f.siteUser, "site-user", "", "CloudPanel site user (default: from the domain)")
	fl.StringVar(&f.path, "path", "", "path of the site or stack on the server")
	fl.IntVar(&f.appPort, "app-port", 0, "pm2: loopback port nginx forwards to (default 3001)")
	fl.IntVar(&f.proxyPort, "proxy-port", 0, "compose behind CloudPanel: loopback port of the proxy (default 8081)")
	fl.StringVar(&f.tlsMode, "tls-mode", "", "compose with a per-target ingress: cloudpanel or certbot")
	fl.BoolVar(&f.yes, "yes", false, "accept the defaults without asking or confirming")
}

// override applies the flags the user actually typed.
func (f *answerFlags) override(cmd *cobra.Command, a *scaffold.Answers) {
	set := cmd.Flags().Changed
	str := func(name string, dst *string, v string) {
		if set(name) {
			*dst = v
		}
	}
	num := func(name string, dst *int, v int) {
		if set(name) {
			*dst = v
		}
	}
	str("name", &a.Project, f.name)
	str("runtime", &a.Runtime, f.runtime)
	str("ingress", &a.Ingress, f.ingress)
	str("health-path", &a.HealthPath, f.healthPath)
	str("app-service", &a.AppService, f.appService)
	str("migrate", &a.MigrateCommand, f.migrate)
	str("id", &a.Target.ID, f.id)
	str("host", &a.Target.Host, f.host)
	str("ssh-user", &a.Target.SSHUser, f.sshUser)
	str("domain", &a.Target.Domain, f.domain)
	str("site-user", &a.Target.SiteUser, f.siteUser)
	str("path", &a.Target.Path, f.path)
	str("tls-mode", &a.Target.TLSMode, f.tlsMode)
	num("ssh-port", &a.Target.SSHPort, f.sshPort)
	num("app-port", &a.Target.AppPort, f.appPort)
	num("proxy-port", &a.Target.ProxyPort, f.proxyPort)
	if set("compose-file") {
		a.ComposeFiles = []string{f.composeFile}
	}
	if set("dockerfile") {
		a.Images = nil
		for _, df := range f.dockerfiles {
			a.Images = append(a.Images, scaffold.ImageRef{Name: scaffold.ImageName(df), Dockerfile: df})
		}
	}
	if set("features") {
		a.Features = nil
		for _, ft := range strings.Split(f.features, ",") {
			if ft = strings.TrimSpace(ft); ft != "" {
				a.Features = append(a.Features, ft)
			}
		}
	}
}

// canAsk reports whether the questions can be put on a screen: both ends are a terminal and nothing says otherwise.
func (a *app) canAsk(f *answerFlags) bool {
	if f.yes {
		return false
	}
	if a.interactive != nil {
		return a.interactive()
	}
	if a.plain { // line prompts work on a pipe too: the caller said it will answer
		return true
	}
	return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
}

func (a *app) formOptions(f *answerFlags, finds scaffold.Findings) scaffold.FormOptions {
	in := a.in
	if in == nil {
		in = os.Stdin
	}
	accessible := a.plain || os.Getenv("TERM") == "dumb" || a.accessible || !hasScreenSize()
	if accessible {
		in = bufio.NewReader(in) // shared by every prompt of the command, so none of them loses buffered answers
	}
	return scaffold.FormOptions{In: in, Out: a.out, Findings: finds, Accessible: accessible}
}

// hasScreenSize reports whether the terminal can say how big it is: without a size the full-screen form cannot draw.
func hasScreenSize() bool {
	w, h, err := term.GetSize(os.Stdout.Fd())
	return err == nil && w > 0 && h > 0
}

// projectDir is where the manifest lives (or will live).
func (a *app) projectDir() (string, error) {
	path := a.manifestPath
	if path == "" {
		path = "deploykit.yaml"
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Dir(abs), nil
}

func (a *app) initCmd() *cobra.Command {
	var f answerFlags
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create deploykit.yaml and the first target: reads the repository, asks a few questions, writes files that validate",
		Long: "init looks at the repository (package.json, Dockerfiles, compose files), asks for what it cannot know, and writes\n" +
			"deploykit.yaml, targets/<id>.env and, for compose, deploy.env.example. Nothing is overwritten without your say-so.\n\n" +
			"Without a terminal (or with --yes) it does not ask: give --host and --domain and it uses the detected values and\n" +
			"the defaults for the rest. With the global --dry-run it prints the files instead of writing them.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.runInit(cmd, &f) },
	}
	f.register(cmd, true)
	cmd.Flags().BoolVar(&f.force, "force", false, "replace existing files, keeping the old ones as <name>.bak")
	return cmd
}

func (a *app) runInit(cmd *cobra.Command, f *answerFlags) error {
	dir, err := a.projectDir()
	if err != nil {
		return err
	}
	st := ui.NewStyles(a.out)
	finds := scaffold.Detect(dir)
	ans := scaffold.DefaultAnswers(finds)
	f.override(cmd, &ans)
	ask := a.canAsk(f)
	opts := a.formOptions(f, finds)

	if ask {
		_, _ = fmt.Fprint(a.out, st.Banner("deploykit init", "Create the configuration of this project in "+dir))
		if err := scaffold.RunInitForm(opts, &ans); err != nil {
			return a.cancelled(st, err)
		}
	} else if missing := missingFlags(&ans); len(missing) > 0 {
		return fmt.Errorf("init cannot ask here (no terminal, or --yes), so it needs %s", strings.Join(missing, " and "))
	}
	scaffold.FillDefaults(&ans)
	if err := ans.Validate(); err != nil {
		return fmt.Errorf("the answers are not valid:\n%w", err)
	}
	files, err := scaffold.Files(ans)
	if err != nil {
		return err
	}
	var ignore []string
	if ans.Runtime == scaffold.RuntimeCompose {
		ignore = []string{"deploy.env"}
	}
	plan, err := scaffold.NewPlan(dir, files, ignore)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(a.out, a.review(st, ans, plan))

	if a.dryRun {
		for _, pf := range plan.Files {
			_, _ = fmt.Fprintf(a.out, "\n%s\n%s", st.Bold.Render("── "+pf.Path+" ("+string(pf.Status)+")"), pf.Content)
		}
		_, _ = fmt.Fprintln(a.out, "\n"+st.Info("dry run: nothing was written"))
		return nil
	}
	policy, proceed, err := a.resolveConflicts(f, plan, ask, opts)
	if err != nil {
		return a.cancelled(st, err)
	}
	if !proceed {
		_, _ = fmt.Fprintln(a.out, st.Caution("nothing written"))
		return nil
	}
	if ask {
		ok, err := scaffold.Confirm(opts, "Write these files?", "Write", "Cancel")
		if err != nil {
			return a.cancelled(st, err)
		}
		if !ok {
			_, _ = fmt.Fprintln(a.out, st.Caution("cancelled, nothing written"))
			return nil
		}
	}
	done, err := plan.Apply(policy)
	if err != nil {
		return err
	}
	for _, w := range done {
		_, _ = fmt.Fprintln(a.out, st.OK("%s %s", w.Path, st.Muted.Render("("+w.Action+")")))
	}
	a.afterWrite(st, dir, ans)
	return nil
}

// missingFlags lists what a script must pass because there is no sensible default.
func missingFlags(a *scaffold.Answers) []string {
	var missing []string
	if a.Target.Host == "" {
		missing = append(missing, "--host")
	}
	if a.UsesCloudPanel() && a.Target.Domain == "" {
		missing = append(missing, "--domain")
	}
	return missing
}

func (a *app) cancelled(st *ui.Styles, err error) error {
	if errors.Is(err, scaffold.ErrAborted) {
		_, _ = fmt.Fprintln(a.out, st.Caution("cancelled, nothing written"))
		return nil
	}
	return err
}

// resolveConflicts decides what happens to files that exist with other content.
func (a *app) resolveConflicts(f *answerFlags, plan *scaffold.Plan, ask bool, opts scaffold.FormOptions) (scaffold.Conflict, bool, error) {
	conflicts := plan.Conflicts()
	switch {
	case len(conflicts) == 0:
		return scaffold.ConflictStop, true, nil
	case f.force:
		return scaffold.ConflictOverwrite, true, nil
	case ask:
		return scaffold.ChooseConflict(opts, conflicts)
	}
	return scaffold.ConflictStop, false, &scaffold.ErrConflict{Paths: conflicts}
}

// review is the panel shown before anything is written.
func (a *app) review(st *ui.Styles, ans scaffold.Answers, plan *scaffold.Plan) string {
	t := ans.Target
	where := t.SSHUser + "@" + t.Host
	if ans.Runtime == scaffold.RuntimePM2 {
		where = t.SiteUser + "@" + t.Host
	}
	rows := [][2]string{
		{"Project", ans.Project},
		{"Runtime", map[string]string{scaffold.RuntimePM2: "Next.js on pm2", scaffold.RuntimeCompose: "Docker Compose"}[ans.Runtime]},
		{"Ingress", ans.Ingress},
		{"Target", fmt.Sprintf("%s  (%s:%d)", t.ID, where, t.SSHPort)},
		{"Domain", orNone(t.Domain)},
		{"Path", t.Path},
		{"Features", orNone(strings.Join(ans.Features, ", "))},
	}
	if ans.Runtime == scaffold.RuntimePM2 {
		rows[2][1] = "CloudPanel Node.js site"
	}
	body := st.Rows(rows) + "\n"
	for _, f := range plan.Files {
		icon := map[scaffold.Status]string{
			scaffold.StatusNew: st.Success.Render("+"), scaffold.StatusUnchanged: st.Muted.Render("="), scaffold.StatusDiffers: st.Warn.Render("!"),
		}[f.Status]
		body += fmt.Sprintf("%s %s %s\n", icon, f.Path, st.Muted.Render("("+string(f.Status)+")"))
	}
	for _, g := range plan.Gitignore {
		body += fmt.Sprintf("%s .gitignore %s\n", st.Success.Render("+"), st.Muted.Render("adds "+g))
	}
	return st.Panel("Review", body)
}

func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// afterWrite checks the result with the loader `validate` uses and tells what to do next.
func (a *app) afterWrite(st *ui.Styles, dir string, ans scaffold.Answers) {
	_, _ = fmt.Fprintln(a.out)
	if err := checkProject(dir, ans.Target.ID); err != nil {
		_, _ = fmt.Fprintln(a.out, st.Fail("the files were written but do not validate: %v", err))
	} else {
		_, _ = fmt.Fprintln(a.out, st.OK("deploykit validate: ok"))
	}
	var next [][2]string // command, what it does
	if ans.Runtime == scaffold.RuntimeCompose {
		next = append(next, [2]string{"cp deploy.env.example deploy.env", "then fill REGISTRY, GHCR_USER, GHCR_TOKEN (mode 600, not versioned)"})
	}
	next = append(next, [2]string{"deploykit doctor -t " + ans.Target.ID, "tools here and on the server"})
	if ans.Target.UsesCloudPanel(ans.Runtime, ans.Ingress) {
		next = append(next, [2]string{"deploykit bootstrap -t " + ans.Target.ID, "once: creates the CloudPanel site"})
	}
	if ans.Runtime == scaffold.RuntimePM2 {
		next = append(next, [2]string{"deploykit deploy -t " + ans.Target.ID + " --dry-run", "the plan, nothing is changed"})
	} else {
		next = append(next, [2]string{"deploykit deploy -t " + ans.Target.ID + " --install --version X.Y.Z --dry-run", "the plan, nothing is changed"})
	}
	_, _ = fmt.Fprintln(a.out, st.Panel("Next steps", st.Rows(next)))
}

// checkProject loads what was written exactly as `validate` does.
func checkProject(dir, targetID string) error {
	m, err := manifest.Load(filepath.Join(dir, "deploykit.yaml"))
	if err != nil {
		return err
	}
	if err := m.Validate(); err != nil {
		return err
	}
	inv := m.Inventory
	if inv == "" {
		inv = "targets"
	}
	if !filepath.IsAbs(inv) {
		inv = filepath.Join(dir, inv)
	}
	_, err = inventory.Open(inv, targetID, nil)
	return err
}

// ---- deploykit target add ----------------------------------------------------------------------------------

func (a *app) targetCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "target", Short: "Manage the targets (servers) of the inventory"}
	var f answerFlags
	add := &cobra.Command{
		Use:   "add",
		Short: "Add a server to an existing project: the same questions as init, for the server only",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return a.runTargetAdd(cmd, &f) },
	}
	f.register(add, false)
	cmd.AddCommand(add)
	return cmd
}

func (a *app) runTargetAdd(cmd *cobra.Command, f *answerFlags) error {
	p, err := a.load()
	if err != nil {
		return fmt.Errorf("target add works on an existing project: %w", err)
	}
	st := ui.NewStyles(a.out)
	ans := scaffold.Answers{Project: p.m.Project, Runtime: p.m.Runtime.Type, Ingress: ingressOf(p.m)}
	ans.Target = scaffold.Target{SSHPort: 22, SSHUser: "deploy"}
	taken, err := inventory.List(p.inventoryDir())
	if err != nil {
		return err
	}
	f.override(cmd, &ans)
	ask := a.canAsk(f)
	opts := a.formOptions(f, scaffold.Findings{})
	if ask {
		_, _ = fmt.Fprint(a.out, st.Banner("deploykit target add", "Add a server to "+p.m.Project))
		to := scaffold.TargetOptions{Runtime: ans.Runtime, Ingress: ans.Ingress, Project: ans.Project, Taken: taken}
		if err := scaffold.RunTargetForm(opts, to, &ans); err != nil {
			return a.cancelled(st, err)
		}
	} else if missing := missingFlags(&ans); len(missing) > 0 {
		return fmt.Errorf("target add cannot ask here (no terminal, or --yes), so it needs %s", strings.Join(missing, " and "))
	}
	scaffold.FillDefaults(&ans)
	if ans.Target.ID == "prod" && !cmd.Flags().Changed("id") && contains(taken, "prod") {
		return fmt.Errorf("a target called prod already exists: choose another with --id")
	}
	if contains(taken, ans.Target.ID) {
		return fmt.Errorf("targets/%s.env already exists", ans.Target.ID)
	}
	if err := ans.Target.Validate(ans.Runtime, ans.Ingress); err != nil {
		return fmt.Errorf("the answers are not valid:\n%w", err)
	}
	file := scaffold.TargetFile(ans)
	file.Path = relInventory(p, ans.Target.ID)
	plan, err := scaffold.NewPlan(p.dir, []scaffold.File{file}, nil)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(a.out, a.review(st, ans, plan))
	if a.dryRun {
		_, _ = fmt.Fprintf(a.out, "\n%s\n%s\n%s\n", st.Bold.Render("── "+file.Path), file.Content, st.Info("dry run: nothing was written"))
		return nil
	}
	if ask {
		if ok, err := scaffold.Confirm(opts, "Write this target?", "Write", "Cancel"); err != nil || !ok {
			return a.cancelled(st, errOrAbort(err))
		}
	}
	done, err := plan.Apply(scaffold.ConflictStop)
	if err != nil {
		return err
	}
	for _, w := range done {
		_, _ = fmt.Fprintln(a.out, st.OK("%s %s", w.Path, st.Muted.Render("("+w.Action+")")))
	}
	if err := checkProject(p.dir, ans.Target.ID); err != nil {
		_, _ = fmt.Fprintln(a.out, st.Fail("the target was written but does not validate: %v", err))
		return nil
	}
	next := [][2]string{{"deploykit doctor -t " + ans.Target.ID, "tools here and on the server"}}
	if ans.Target.UsesCloudPanel(ans.Runtime, ans.Ingress) {
		next = append(next, [2]string{"deploykit bootstrap -t " + ans.Target.ID, "once: creates the CloudPanel site"})
	}
	_, _ = fmt.Fprintln(a.out, "\n"+st.OK("deploykit validate: ok")+"\n"+st.Panel("Next steps", st.Rows(next)))
	return nil
}

func errOrAbort(err error) error {
	if err != nil {
		return err
	}
	return scaffold.ErrAborted
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// relInventory is the path of a target file relative to the project directory.
func relInventory(p *project, id string) string {
	rel, err := filepath.Rel(p.dir, p.inventoryDir())
	if err != nil {
		rel = "targets"
	}
	return filepath.ToSlash(filepath.Join(rel, id+".env"))
}

// ingressOf reads the ingress mode of a manifest: select, or the type of its only ingress.
func ingressOf(m *manifest.Manifest) string {
	switch {
	case m.Runtime.Type == manifest.RuntimePM2:
		return scaffold.IngressCloudPanel
	case m.Ingress.Select != "":
		return scaffold.IngressSelect
	case m.Ingress.Type == manifest.IngressStandalone:
		return scaffold.IngressStandalone
	}
	return scaffold.IngressCloudPanel
}
