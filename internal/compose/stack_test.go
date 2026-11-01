package compose

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/masilia/deploykit/internal/envfile"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/tmpl"
	"github.com/masilia/deploykit/internal/transport"
)

func composeApp(t *testing.T) *manifest.Compose {
	t.Helper()
	m, err := manifest.Load("../../examples/compose-app/deploykit.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.Compose()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func newStack(t *testing.T, tlsMode string) (*Stack, *transport.Fake) {
	t.Helper()
	f := transport.NewFake("srv")
	return &Stack{
		T: f, Root: "/opt/app", Project: "acme", Cfg: composeApp(t),
		Vars:  tmpl.Vars{"target": map[string]string{"tls_mode": tlsMode}},
		Sleep: func(time.Duration) {},
	}, f
}

func TestFilesFollowTheTLSMode(t *testing.T) {
	s, _ := newStack(t, "certbot")
	if files, _ := s.Files(); strings.Join(files, ",") != "docker-compose.prod.yml" {
		t.Errorf("certbot files = %v", files)
	}
	s, f := newStack(t, "cloudpanel")
	if files, _ := s.Files(); strings.Join(files, ",") != "docker-compose.prod.yml,docker-compose.cloudpanel.yaml" {
		t.Errorf("cloudpanel files = %v", files)
	}
	if _, err := s.Run(t.Context(), nil, "up", "-d"); err != nil {
		t.Fatal(err)
	}
	if got := f.Calls()[0].Spec; strings.Join(got.Argv, " ") != "docker compose -p acme -f docker-compose.prod.yml -f docker-compose.cloudpanel.yaml up -d" || got.Dir != "/opt/app" {
		t.Errorf("call = %+v", got)
	}
}

func TestState(t *testing.T) {
	s, f := newStack(t, "certbot")
	f.OnFunc(func(sp transport.Spec) bool { return sp.Env["DK_SVC"] == "front" }, transport.Result{Stdout: "true healthy\n"})
	f.OnFunc(func(sp transport.Spec) bool { return sp.Env["DK_SVC"] == "api" }, transport.Result{Stdout: "true starting\n"})
	f.OnFunc(func(sp transport.Spec) bool { return sp.Env["DK_SVC"] == "db" }, transport.Result{Stdout: "false \n"})
	f.OnFunc(func(sp transport.Spec) bool { return sp.Env["DK_SVC"] == "hub" }, transport.Result{Stdout: "missing\n"})
	f.OnFunc(func(sp transport.Spec) bool { return sp.Env["DK_SVC"] == "proxy" }, transport.Result{Stdout: "true \n"}) // no healthcheck defined

	for svc, want := range map[string]string{"front": "ok", "api": "not healthy (starting)", "db": "stopped", "hub": "container missing", "proxy": "ok"} {
		st, err := s.State(t.Context(), svc)
		if err != nil || st.String() != want {
			t.Errorf("%s: %q, %v; want %q", svc, st.String(), err, want)
		}
	}
	if st, _ := s.State(t.Context(), "proxy"); !st.Ready() {
		t.Error("a running service without a health check is ready")
	}
}

func TestWaitReadyPollsUntilReady(t *testing.T) {
	s, f := newStack(t, "certbot")
	rounds := 0
	f.OnFunc(func(sp transport.Spec) bool {
		if sp.Env["DK_SVC"] == "api" {
			rounds++
		}
		return true
	}, transport.Result{Stdout: "true healthy\n"})
	// api is "starting" for the first two reads, then healthy.
	f2 := transport.NewFake("srv")
	reads := 0
	f2.OnFunc(func(sp transport.Spec) bool {
		if sp.Env["DK_SVC"] == "api" {
			reads++
			return reads <= 2
		}
		return false
	}, transport.Result{Stdout: "true starting\n"})
	f2.On("DK_SVC", transport.Result{Stdout: "true healthy\n"})
	s.T = f2
	slept := time.Duration(0)
	s.Sleep = func(d time.Duration) { slept += d }
	states, err := s.WaitReady(t.Context())
	if err != nil || !allReady(states) {
		t.Fatalf("states %+v, err %v", states, err)
	}
	if slept != 10*time.Second {
		t.Errorf("slept %s, want two 5s waits", slept)
	}
	_ = f
}

func TestWaitReadyGivesUpAtTheTimeout(t *testing.T) {
	s, f := newStack(t, "certbot")
	f.On("DK_SVC", transport.Result{Stdout: "true unhealthy\n"})
	slept := time.Duration(0)
	s.Sleep = func(d time.Duration) { slept += d }
	states, err := s.WaitReady(t.Context())
	if err != nil || allReady(states) {
		t.Fatalf("states %+v, err %v", states, err)
	}
	if slept != 180*time.Second {
		t.Errorf("waited %s, want the 180s timeout of the manifest", slept)
	}
}

func TestHealthProbes(t *testing.T) {
	s, f := newStack(t, "certbot")
	// First rule wins: the log probe (also a DK_SVC script) must be listed before the service-state rule.
	f.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "docker logs --since") }, transport.Result{ExitCode: 1}) // no tick yet
	f.On("DK_SVC", transport.Result{Stdout: "true healthy\n"})
	f.OnFunc(func(sp transport.Spec) bool {
		return strings.Join(sp.Argv, " ") == "curl -fsS -m 10 https://app.example/api/v1/health"
	}, transport.Result{Stdout: `{"status":"ok"}`})
	f.OnFunc(func(sp transport.Spec) bool {
		return strings.Join(sp.Argv, " ") == "curl -fsS -m 10 https://app.example/api/health"
	}, transport.Result{Stdout: `{"error":1}`}) // 200 but wrong body
	f.OnFunc(func(sp transport.Spec) bool {
		return strings.Contains(strings.Join(sp.Argv, " "), "-I https://app.example/.well-known/mercure")
	}, transport.Result{Stdout: "HTTP/2 200\n"})

	res, err := s.Health(t.Context(), "https://app.example")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]ProbeResult{}
	for _, r := range res {
		byName[r.Name] = r
	}
	if !byName["front"].OK || !byName["health-api"].OK || !byName["hub-sse"].OK {
		t.Errorf("results = %+v", res)
	}
	if byName["health-front"].OK || byName["scheduler"].OK {
		t.Errorf("health-front (wrong body) and scheduler (no tick) must fail: %+v", res)
	}
	if HealthOK(res) {
		t.Error("HealthOK must be false when any probe fails")
	}
	if !strings.Contains(byName["scheduler"].Detail, "scheduler tick") {
		t.Errorf("scheduler detail = %q", byName["scheduler"].Detail)
	}
}

func TestHTTPProbeRetries(t *testing.T) {
	s, f := newStack(t, "certbot")
	calls := 0
	f.OnFunc(func(sp transport.Spec) bool {
		if len(sp.Argv) > 0 && sp.Argv[0] == "curl" {
			calls++
			return calls < 3
		}
		return false
	}, transport.Result{ExitCode: 22, Stderr: "HTTP 502"})
	p := manifest.Probe{Name: "hub", HTTP: "{{ base_url }}/x", Tries: 3, Delay: "2s"}
	r, err := s.httpProbe(t.Context(), p, tmpl.Vars{"base_url": "https://h"})
	if err != nil || !r.OK || calls != 3 {
		t.Errorf("result %+v, calls %d, err %v", r, calls, err)
	}
}

func TestRunAppPassesSecretsThroughTheEnvironment(t *testing.T) {
	s, f := newStack(t, "certbot")
	env := envfile.Parse([]byte("ADMIN_EMAIL=boss@example.com\nADMIN_FIRST_NAME=Boss\nADMIN_LAST_NAME=O'Neil\nADMIN_PASSWORD=pw-secret-1\n"))
	if err := s.RunApp(t.Context(), "create_admin", env, nil); err != nil {
		t.Fatal(err)
	}
	call := f.Calls()[0].Spec
	argv := strings.Join(call.Argv, " ")
	if strings.Contains(argv, "pw-secret-1") {
		t.Errorf("the password must not be in the arguments: %s", argv)
	}
	if call.Secrets["APP_CREATE_USER_PASSWORD"] != "pw-secret-1" || !strings.Contains(argv, "-e APP_CREATE_USER_PASSWORD api sh -c") {
		t.Errorf("call = %v / %v", argv, call.Secrets)
	}
	if !strings.Contains(argv, `--last-name='O'\''Neil'`) {
		t.Errorf("values must be shell-quoted: %s", argv)
	}
	if strings.Contains(f.Trace(), "pw-secret-1") {
		t.Error("the trace leaks the password")
	}

	// A missing secret stops before anything runs.
	f2 := transport.NewFake("x")
	s.T = f2
	if err := s.RunApp(t.Context(), "create_admin", envfile.Parse([]byte("ADMIN_EMAIL=a@b.c\nADMIN_FIRST_NAME=A\nADMIN_LAST_NAME=B\n")), nil); err == nil || len(f2.Calls()) != 0 {
		t.Errorf("err = %v, calls = %d", err, len(f2.Calls()))
	}
	if err := s.RunApp(t.Context(), "nope", env, nil); err == nil {
		t.Error("an undeclared app command must fail")
	}
}

func TestRemoteEnvRoundTripsWithoutPuttingContentInArguments(t *testing.T) {
	f := transport.NewFake("srv").On("cat /opt/app/.env", transport.Result{Stdout: "A=1\n# c\n"})
	e := RemoteEnv{T: f, Path: "/opt/app/.env"}
	if err := e.Apply(t.Context(), []envfile.Assignment{{Key: "B", Value: "top-secret"}}); err != nil {
		t.Fatal(err)
	}
	save := f.Calls()[1]
	if save.Stdin != "A=1\n# c\nB=top-secret\n" {
		t.Errorf("saved content = %q", save.Stdin)
	}
	if strings.Contains(strings.Join(save.Spec.Argv, " "), "top-secret") {
		t.Error("the secret must travel on stdin")
	}
}

// Run the Save script for real: atomic replace, mode preserved.
func TestRemoteEnvSaveAgainstARealFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("A=1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	e := RemoteEnv{T: transport.NewLocal(), Path: path}
	d, err := e.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_ = d.Set("A", "2")
	if err := e.Save(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	st, _ := os.Stat(path)
	if string(got) != "A=2\n" || st.Mode().Perm() != 0o640 {
		t.Errorf("content %q mode %o", got, st.Mode().Perm())
	}
	if _, err := (RemoteEnv{T: transport.NewLocal(), Path: filepath.Join(t.TempDir(), "nope")}).Load(t.Context()); err == nil {
		t.Error("a missing file is an error")
	}
}

func TestEnsureSecrets(t *testing.T) {
	spec := composeApp(t)
	_ = spec
	m, _ := manifest.Load("../../examples/compose-app/deploykit.yaml")
	env, _ := m.EnvConfig()
	d := envfile.Parse([]byte("APP_SECRET=keep-me\nJWT_PASSPHRASE=CHANGE_ME\nPOSTGRES_PASSWORD=\n# BACKUP_PASSPHRASE=commented\n"))
	generated, err := EnsureSecrets(d, env.Secrets)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Get("APP_SECRET", ""); got != "keep-me" {
		t.Errorf("an existing secret must never be overwritten, got %q", got)
	}
	if len(generated) != 7 {
		t.Errorf("generated %v, want 7 (every secret except APP_SECRET)", generated)
	}
	if v := d.Get("JWT_PASSPHRASE", ""); len(v) != 32 || strings.HasPrefix(v, "CHANGE_ME") {
		t.Errorf("JWT_PASSPHRASE = %q (16 bytes = 32 hex chars)", v)
	}
	if v := d.Get("APP_SECRET", ""); v != "keep-me" {
		t.Error("idempotency")
	}
	again, _ := EnsureSecrets(d, env.Secrets)
	if len(again) != 0 {
		t.Errorf("a second run must generate nothing, got %v", again)
	}
}

// The lock script, run for real: take, refuse while the owner lives, take over when it died.
func TestLockAgainstARealDirectory(t *testing.T) {
	root := t.TempDir()
	s := &Stack{T: transport.NewLocal(), Root: root, Cfg: &manifest.Compose{Lock: ".lock-ops"}}

	sleeper := exec.CommandContext(t.Context(), "sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() }()

	unlock, err := s.Lock(t.Context(), sleeper.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	var busy *ErrBusy
	if _, err := s.Lock(t.Context(), 999999); !errors.As(err, &busy) || busy.PID == "" {
		t.Fatalf("a live owner must block: %v", err)
	}
	if err := unlock(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".lock-ops")); err == nil {
		t.Error("unlock must remove the directory")
	}

	// A stale lock (dead pid) is taken over.
	if err := os.MkdirAll(filepath.Join(root, ".lock-ops"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".lock-ops", "pid"), []byte("999999"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lock(t.Context(), os.Getpid()); err != nil {
		t.Fatalf("a stale lock must be recovered: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, ".lock-ops", "pid")); string(data) != strconv.Itoa(os.Getpid()) {
		t.Errorf("pid file = %q", data)
	}
}

func TestJournalAndFreeMB(t *testing.T) {
	root := t.TempDir()
	s := &Stack{T: transport.NewLocal(), Root: root, Cfg: &manifest.Compose{Journal: "versions.log"}}
	now := time.Date(2026, 10, 27, 14, 5, 9, 0, time.UTC)
	if err := s.Journal(t.Context(), now, "OK   1.2.3 (replaces 1.2.2)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Journal(t.Context(), now, "ROLLBACK 1.2.2 (failure of 1.2.3)"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "versions.log"))
	if string(data) != "2026-10-27 14:05:09\tOK   1.2.3 (replaces 1.2.2)\n2026-10-27 14:05:09\tROLLBACK 1.2.2 (failure of 1.2.3)\n" {
		t.Errorf("journal = %q", data)
	}
	if mb, err := s.FreeMB(t.Context(), root); err != nil || mb <= 0 {
		t.Errorf("FreeMB = %d, %v", mb, err)
	}
}

// Regression: two deployment roots sharing a compose file (whose `name:` is the same) must never act on each other's
// containers. Every compose command names its project explicitly.
func TestEveryComposeCommandNamesTheProject(t *testing.T) {
	for _, project := range []string{"acme", "app-cp-test"} {
		s, f := newStack(t, "certbot")
		s.Project = project
		_, _ = s.Run(t.Context(), nil, "up", "-d")
		_, _ = s.State(t.Context(), "api")
		_ = s.RunApp(t.Context(), "migrate", envfile.Parse(nil), nil)
		for _, c := range f.Calls() {
			text := strings.Join(c.Spec.Argv, " ") + c.Spec.Script
			if strings.Contains(text, "docker compose") && !strings.Contains(text, "-p "+project+" ") {
				t.Errorf("a compose command does not name project %q: %s", project, text)
			}
		}
	}
}

func TestHostHeaderIsSentToHTTPProbes(t *testing.T) {
	s, f := newStack(t, "cloudpanel")
	s.HostHeader = "app.example.com"
	_, err := s.httpProbe(t.Context(), manifest.Probe{Name: "api", HTTP: "{{ base_url }}/api/v1/health"}, tmpl.Vars{"base_url": "http://127.0.0.1:8082"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.Calls()[0].Spec.Argv, " ")
	if got != "curl -fsS -m 10 -H Host: app.example.com http://127.0.0.1:8082/api/v1/health" {
		t.Errorf("probe = %q", got)
	}
}
