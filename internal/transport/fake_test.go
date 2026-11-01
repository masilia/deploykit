package transport

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestFakeRecordsAndReplays(t *testing.T) {
	f := NewFake("srv").
		On("readlink", Result{Stdout: "/srv/releases/20260101000000\n"}).
		Fail("pm2 reload", 3, "pm2: not found")

	res, err := f.Run(t.Context(), Spec{Script: "readlink -f current"})
	if err != nil || res.Stdout != "/srv/releases/20260101000000\n" {
		t.Errorf("readlink: %+v, %v", res, err)
	}

	var out bytes.Buffer
	_, err = f.Run(t.Context(), Spec{Argv: []string{"pm2", "reload", "app"}, Out: &out})
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 3 {
		t.Errorf("pm2 reload: err = %v, want exit 3", err)
	}
	if !strings.Contains(out.String(), "pm2: not found") {
		t.Errorf("streamed output = %q", out.String())
	}

	if res, err := f.Run(t.Context(), Spec{Script: "true"}); err != nil || res != (Result{}) {
		t.Errorf("unmatched call should succeed silently: %+v, %v", res, err)
	}
	if n := len(f.Calls()); n != 3 {
		t.Errorf("recorded %d calls, want 3", n)
	}
}

func TestFakeFirstRuleWins(t *testing.T) {
	f := NewFake("x").On("a", Result{Stdout: "first"}).On("a", Result{Stdout: "second"})
	res, _ := f.Run(t.Context(), Spec{Script: "a"})
	if res.Stdout != "first" {
		t.Errorf("stdout = %q", res.Stdout)
	}
}

func TestFakeRecordsStdin(t *testing.T) {
	f := NewFake("x")
	if _, err := f.Run(t.Context(), Spec{Argv: []string{"docker", "login"}, Stdin: strings.NewReader("tok")}); err != nil {
		t.Fatal(err)
	}
	if got := f.Calls()[0].Stdin; got != "tok" {
		t.Errorf("recorded stdin = %q", got)
	}
}

func TestFakeTraceGolden(t *testing.T) {
	f := NewFake("deploy@srv")
	_, _ = f.Run(t.Context(), Spec{
		Script:  "cd /srv\nln -sfn releases/1 current.tmp\n",
		Env:     map[string]string{"PM2_NAME": "app"},
		Secrets: map[string]string{"TOKEN": "s3cr3t"},
		Dir:     "/srv",
	})
	_, _ = f.Run(t.Context(), Spec{Argv: []string{"docker", "build", "-t", "my image", "."}})
	_, _ = f.Run(t.Context(), Spec{Argv: []string{"docker", "login", "--password-stdin"}, Stdin: strings.NewReader("tok")})
	_ = f.Sync(t.Context(), SyncSpec{Src: "build/", Dst: "/srv/releases/1/", Delete: true, LinkDest: "/srv/releases/0", Excludes: []string{".git"}})

	want := `run deploy@srv
  env PM2_NAME=app
  secret TOKEN=***
  dir /srv
  | cd /srv
  | ln -sfn releases/1 current.tmp
run deploy@srv
  $ docker build -t 'my image' .
run deploy@srv
  $ docker login --password-stdin
  stdin <3 bytes>
sync build/ -> /srv/releases/1/ --delete --link-dest=/srv/releases/0 --exclude=.git
`
	if got := f.Trace(); got != want {
		t.Errorf("trace:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(f.Trace(), "s3cr3t") {
		t.Error("trace leaks a secret")
	}
}

func TestFakeImplementsTransport(t *testing.T) {
	var _ Transport = NewFake("x")
	var _ Transport = NewLocal()
	var _ Transport = &SSH{}
}
