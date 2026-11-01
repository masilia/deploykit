package doctor

import (
	"strings"
	"testing"

	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/transport"
)

func TestAtLeast(t *testing.T) {
	for _, tc := range []struct {
		have, want string
		ok         bool
	}{
		{"Docker Compose version v2.27.0", "2.24", true},
		{"2.24.0", "2.24", true},
		{"2.24.5-desktop.1", "2.24", true},
		{"v2.23.9", "2.24", false},
		{"1.29.2", "2.24", false},
		{"2.9", "2.24", false}, // numeric, not lexical
		{"10.0", "2.24", true},
		{"2", "2.24", false},
		{"no version here", "2.24", false},
		{"3.1", "3.1.2", false},
	} {
		if got := AtLeast(tc.have, tc.want); got != tc.ok {
			t.Errorf("AtLeast(%q, %q) = %v, want %v", tc.have, tc.want, got, tc.ok)
		}
	}
}

func TestCheck(t *testing.T) {
	f := transport.NewFake("srv").
		On("docker compose version", transport.Result{Stdout: "Docker Compose version v2.20.0\n"}).
		Fail("gpg --version", 127, "not found").
		Fail("pm2 -v", 127, "")
	got := Check(t.Context(), f, []Tool{
		{Name: "docker compose", Command: "docker compose version", Min: "2.24", Required: true},
		{Name: "gpg", Command: "gpg --version", Required: true},
		{Name: "pm2", Command: "pm2 -v", Required: false},
		{Name: "curl", Command: "curl --version", Required: true},
	})
	want := []Level{Fail, Fail, Warn, OK}
	for i, w := range want {
		if got[i].Level != w {
			t.Errorf("%s: level %v (%s), want %v", got[i].Name, got[i].Level, got[i].Detail, w)
		}
	}
	if !strings.Contains(got[0].Detail, "need 2.24") {
		t.Errorf("detail = %q", got[0].Detail)
	}
	if Worst(got) != Fail {
		t.Error("Worst must be FAIL")
	}
	if Worst(got[2:]) != OK && Worst(got[2:3]) != Warn {
		t.Error("Worst of a warning is warn")
	}
}

func TestToolSetsFollowTheManifest(t *testing.T) {
	names := func(tools []Tool) string {
		var n []string
		for _, tool := range tools {
			n = append(n, tool.Name)
		}
		return strings.Join(n, ",")
	}
	node, err := manifest.Load("../../examples/nextjs-cloudpanel/deploykit.yaml")
	if err != nil {
		t.Fatal(err)
	}
	compose, err := manifest.Load("../../examples/compose-app/deploykit.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := names(LocalTools(node)); got != "git,ssh,rsync,node,npm" {
		t.Errorf("node local = %s", got)
	}
	if got := names(LocalTools(compose)); got != "git,ssh,rsync,docker" {
		t.Errorf("compose local = %s", got)
	}
	if got := names(ServerTools(node, true)); got != "curl,clpctl" {
		t.Errorf("node root = %s", got)
	}
	if got := names(ServerTools(node, false)); got != "curl,node,pm2,rsync" {
		t.Errorf("node site = %s", got)
	}
	if got := names(ServerTools(compose, false)); got != "curl,docker,docker compose,gpg,crontab" {
		t.Errorf("compose server = %s", got)
	}
}

func TestLoginFailureIsNotReportedAsAMissingTool(t *testing.T) {
	f := transport.NewFake("site@srv").Fail("curl --version", 255, "Permission denied (publickey)")
	got := Check(t.Context(), f, []Tool{{Name: "curl", Command: "curl --version", Required: true}, {Name: "pm2", Command: "pm2 -v"}})
	for _, r := range got {
		if r.Detail != LoginFailed && r.Name == "curl" {
			t.Errorf("curl: %+v", r)
		}
	}
	if got[0].Level != Fail || got[0].Detail != LoginFailed {
		t.Errorf("an ssh failure is a failure of the login, not of the tool: %+v", got[0])
	}
	if !AllLoginFailed([]Result{got[0]}) || AllLoginFailed(nil) || AllLoginFailed([]Result{{Detail: "ok"}}) {
		t.Error("AllLoginFailed must only be true when every result is a login failure")
	}
}
