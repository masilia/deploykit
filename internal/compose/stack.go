// Package compose runs a Docker Compose stack on a server: services, health, app commands, the version state kept in
// .env, the operations lock and the journal. It is written against transport.Transport, so the same code runs on the
// server (Local, `deploykit server …`) and, for read-only commands, from the workstation (SSH).
package compose

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/masilia/deploykit/internal/envfile"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/tmpl"
	"github.com/masilia/deploykit/internal/transport"
)

// Stack is one deployment root on one machine.
type Stack struct {
	T       transport.Transport
	Root    string // the deployment directory holding .env and the compose files
	Project string // the compose project name (prefix of volumes)
	Cfg     *manifest.Compose

	// Vars feeds override conditions and templates (target.*, env.*, base_url).
	Vars tmpl.Vars
	// Sleep waits between polls; replaced in tests.
	Sleep func(time.Duration)
	// HostHeader, when set, is sent as the Host header of every HTTP probe. Behind CloudPanel the probes go to the
	// loopback proxy (no DNS, no certificate needed) and name the public domain this way.
	HostHeader string
}

func (s *Stack) sleep(d time.Duration) {
	if s.Sleep != nil {
		s.Sleep(d)
		return
	}
	time.Sleep(d)
}

// Files returns the compose files in effect: the base files plus every override whose condition holds.
func (s *Stack) Files() ([]string, error) {
	files := append([]string(nil), s.Cfg.Files...)
	for _, o := range s.Cfg.Overrides {
		ok, err := tmpl.Cond(o.When, s.Vars)
		if err != nil {
			return nil, fmt.Errorf("overrides: %w", err)
		}
		if ok {
			files = append(files, o.File)
		}
	}
	return files, nil
}

// argv builds `docker compose -f … args`.
func (s *Stack) argv(args ...string) ([]string, error) {
	files, err := s.Files()
	if err != nil {
		return nil, err
	}
	argv := []string{"docker", "compose"}
	// Always name the project explicitly. A compose file carries its own `name:`, so two deployment roots that share
	// the same file would otherwise manage the same containers and volumes: a second instance would act on the first.
	if s.Project != "" {
		argv = append(argv, "-p", s.Project)
	}
	if s.Cfg.ProjectDirectory != "" {
		argv = append(argv, "--project-directory", s.Cfg.ProjectDirectory)
	}
	for _, f := range files {
		argv = append(argv, "-f", f)
	}
	return append(argv, args...), nil
}

// Run runs a compose subcommand in the deployment root.
func (s *Stack) Run(ctx context.Context, out io.Writer, args ...string) (transport.Result, error) {
	argv, err := s.argv(args...)
	if err != nil {
		return transport.Result{}, err
	}
	return s.T.Run(ctx, transport.Spec{Argv: argv, Dir: s.Root, Out: out})
}

// Script runs a shell script in the deployment root with `compose` available as a function.
func (s *Stack) Script(ctx context.Context, out io.Writer, script string, env, secrets map[string]string) (transport.Result, error) {
	argv, err := s.argv()
	if err != nil {
		return transport.Result{}, err
	}
	fn := "compose() { " + transport.QuoteArgs(argv) + " \"$@\"; }\n"
	return s.T.Run(ctx, transport.Spec{Script: fn + script, Dir: s.Root, Env: env, Secrets: secrets, Out: out})
}

// RunApp runs a named app command of the manifest in a one-off container of the app service. Values mapped by EnvFrom
// are read from the server .env and passed through the environment, never through the arguments.
func (s *Stack) RunApp(ctx context.Context, name string, env *envfile.Doc, out io.Writer) error {
	cmd, ok := s.Cfg.App.Commands[name]
	if !ok {
		return fmt.Errorf("app command %q is not declared in the manifest", name)
	}
	if err := s.RunShell(ctx, cmd.Run, cmd.EnvFrom, env, out); err != nil {
		return fmt.Errorf("app command %s: %w", name, err)
	}
	return nil
}

// RunShell runs a command line in a one-off container of the app service (`compose run --rm svc sh -c …`). Templates
// in command see the server .env as env.*, and every interpolated value is shell-quoted.
func (s *Stack) RunShell(ctx context.Context, command string, envFrom map[string]string, env *envfile.Doc, out io.Writer) error {
	run, err := tmpl.Render(command, tmpl.Vars{"env": envMap(env)}, true)
	if err != nil {
		return err
	}
	secrets := map[string]string{}
	args := []string{"run", "--rm"}
	for variable, key := range envFrom {
		value := env.Get(key, "")
		if value == "" {
			return fmt.Errorf("%s is missing from .env", key)
		}
		secrets[variable] = value
		args = append(args, "-e", variable)
	}
	args = append(args, s.Cfg.App.Service, "sh", "-c", run)
	argv, err := s.argv(args...)
	if err != nil {
		return err
	}
	_, err = s.T.Run(ctx, transport.Spec{Argv: argv, Dir: s.Root, Secrets: secrets, Out: out})
	return err
}

// ExecApp runs a named app command in the running container of the app service (compose exec -T).
func (s *Stack) ExecApp(ctx context.Context, name string) error {
	cmd, ok := s.Cfg.App.Commands[name]
	if !ok {
		return fmt.Errorf("app command %q is not declared in the manifest", name)
	}
	_, err := s.Run(ctx, nil, "exec", "-T", s.Cfg.App.Service, "sh", "-c", cmd.Run)
	return err
}

// envMap exposes a .env document to templates.
func envMap(d *envfile.Doc) map[string]string {
	m := map[string]string{}
	for _, k := range d.Keys() {
		m[k] = d.Get(k, "")
	}
	return m
}

// EnvVars is the template variable tree for a .env document.
func EnvVars(d *envfile.Doc) tmpl.Vars { return tmpl.Vars{"env": envMap(d)} }

// ServiceState is the runtime state of one service.
type ServiceState struct {
	Name    string
	Present bool
	Running bool
	Health  string // "", starting, healthy, unhealthy
}

// Ready reports whether the service is up and, when it has a health check, healthy.
func (s ServiceState) Ready() bool {
	return s.Present && s.Running && (s.Health == "" || s.Health == "healthy")
}

func (s ServiceState) String() string {
	switch {
	case !s.Present:
		return "container missing"
	case !s.Running:
		return "stopped"
	case s.Health != "" && s.Health != "healthy":
		return "not healthy (" + s.Health + ")"
	}
	return "ok"
}

// State reads one service: container present, running, health status.
func (s *Stack) State(ctx context.Context, service string) (ServiceState, error) {
	res, err := s.Script(ctx, nil, `cid=$(compose ps -q "$DK_SVC" 2>/dev/null || true)
[ -n "$cid" ] || { echo missing; exit 0; }
docker inspect -f '{{.State.Running}} {{if .State.Health}}{{.State.Health.Status}}{{end}}' "$cid"
`, map[string]string{"DK_SVC": service}, nil)
	if err != nil {
		return ServiceState{}, err
	}
	st := ServiceState{Name: service}
	fields := strings.Fields(res.Stdout)
	if len(fields) == 0 || fields[0] == "missing" {
		return st, nil
	}
	st.Present, st.Running = true, fields[0] == "true"
	if len(fields) > 1 {
		st.Health = fields[1]
	}
	return st, nil
}

// States reads every service of the manifest.
func (s *Stack) States(ctx context.Context) ([]ServiceState, error) {
	var out []ServiceState
	for _, svc := range s.Cfg.Services {
		st, err := s.State(ctx, svc.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

func allReady(states []ServiceState) bool {
	for _, st := range states {
		if !st.Ready() {
			return false
		}
	}
	return true
}

// WaitReady polls until every service is ready or the manifest's wait timeout passes (HEALTH_WAIT of healthcheck.sh).
// It returns the last states seen.
func (s *Stack) WaitReady(ctx context.Context) ([]ServiceState, error) {
	deadline := s.Cfg.Health.Wait.Timeout
	var waited time.Duration
	for {
		states, err := s.States(ctx)
		if err != nil {
			return nil, err
		}
		if allReady(states) || waited >= deadline {
			return states, nil
		}
		if err := ctx.Err(); err != nil {
			return states, err
		}
		s.sleep(s.Cfg.Health.Wait.Interval)
		waited += s.Cfg.Health.Wait.Interval
	}
}

// ProbeResult is the outcome of one health probe.
type ProbeResult struct {
	Name   string
	OK     bool
	Detail string
}

// Health waits for the services, then runs the HTTP and log probes against baseURL. Every failure is reported, none
// short-circuits (healthcheck.sh reported all KO lines).
func (s *Stack) Health(ctx context.Context, baseURL string) ([]ProbeResult, error) {
	states, err := s.WaitReady(ctx)
	if err != nil {
		return nil, err
	}
	var out []ProbeResult
	for _, st := range states {
		out = append(out, ProbeResult{Name: st.Name, OK: st.Ready(), Detail: st.String()})
	}
	vars := tmpl.Vars{"base_url": baseURL}
	for _, p := range s.Cfg.Health.Probes {
		var r ProbeResult
		switch {
		case p.HTTP != "":
			r, err = s.httpProbe(ctx, p, vars)
		case p.LogContains != "":
			r, err = s.logProbe(ctx, p)
		default:
			err = fmt.Errorf("probe %s: needs http or log_contains", p.Name)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// HealthOK reports whether every probe passed.
func HealthOK(rs []ProbeResult) bool {
	for _, r := range rs {
		if !r.OK {
			return false
		}
	}
	return true
}

func (s *Stack) httpProbe(ctx context.Context, p manifest.Probe, vars tmpl.Vars) (ProbeResult, error) {
	url, err := tmpl.Render(p.HTTP, vars, false)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("probe %s: %w", p.Name, err)
	}
	tries, delay := p.Tries, 2*time.Second
	if tries < 1 {
		tries = 1
	}
	if p.Delay != "" {
		if delay, err = time.ParseDuration(p.Delay); err != nil {
			return ProbeResult{}, fmt.Errorf("probe %s: delay: %w", p.Name, err)
		}
	}
	head := p.Method == "HEAD"
	for i := 0; i < tries; i++ {
		if i > 0 {
			s.sleep(delay)
		}
		args := []string{"curl", "-fsS", "-m", "10"}
		if s.HostHeader != "" {
			args = append(args, "-H", "Host: "+s.HostHeader)
		}
		if head {
			args = append(args, "-I")
		}
		res, runErr := s.T.Run(ctx, transport.Spec{Argv: append(args, url)})
		if runErr == nil && (p.Contains == "" || strings.Contains(res.Stdout, p.Contains)) {
			return ProbeResult{Name: p.Name, OK: true, Detail: "ok"}, nil
		}
	}
	return ProbeResult{Name: p.Name, Detail: "no valid answer from " + url}, nil
}

func (s *Stack) logProbe(ctx context.Context, p manifest.Probe) (ProbeResult, error) {
	since := p.Since
	if since == "" {
		since = "5m"
	}
	check := func() (bool, error) {
		res, err := s.Script(ctx, nil, `cid=$(compose ps -q "$DK_SVC" 2>/dev/null || true)
[ -n "$cid" ] || exit 1
docker logs --since "$DK_SINCE" "$cid" 2>&1 | grep -qF -- "$DK_TEXT"
`, map[string]string{"DK_SVC": p.Service, "DK_SINCE": since, "DK_TEXT": p.LogContains}, nil)
		if err == nil {
			return true, nil
		}
		if res.ExitCode == 1 {
			return false, nil
		}
		return false, err
	}
	var waited time.Duration
	for {
		ok, err := check()
		if err != nil {
			return ProbeResult{}, err
		}
		if ok {
			return ProbeResult{Name: p.Name, OK: true, Detail: "ok"}, nil
		}
		if !p.Wait || waited >= s.Cfg.Health.Wait.Timeout {
			return ProbeResult{Name: p.Name, Detail: fmt.Sprintf("%q not seen in the last %s", p.LogContains, since)}, nil
		}
		s.sleep(s.Cfg.Health.Wait.Interval)
		waited += s.Cfg.Health.Wait.Interval
	}
}
