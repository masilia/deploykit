package inventory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/masilia/deploykit/internal/envfile"
	"github.com/masilia/deploykit/internal/tmpl"
)

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestValidID(t *testing.T) {
	for id, want := range map[string]bool{"acme": true, "a.b_c-1": true, "": false, "a b": false, "a/b": false, "../x": false, "é": false, "a;b": false} {
		if got := ValidID(id); got != want {
			t.Errorf("ValidID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestListIsSortedAndIgnoresOtherFiles(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"zeta.env", "alpha.env", "notes.txt", "beta.env.bak"} {
		write(t, dir, n, "X=1\n")
	}
	if err := os.Mkdir(filepath.Join(dir, "sub.env"), 0o750); err != nil {
		t.Fatal(err)
	}
	ids, err := List(dir)
	if err != nil || strings.Join(ids, ",") != "alpha,zeta" {
		t.Errorf("List = %v, %v", ids, err)
	}
	if ids, err := List(filepath.Join(dir, "missing")); err != nil || len(ids) != 0 {
		t.Errorf("missing dir: %v, %v", ids, err)
	}
}

func TestOpen(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "acme.env", "DEPLOY_HOST=srv\n")
	if _, err := Open(dir, "acme", nil); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir, "nope", nil)
	if err == nil || !strings.Contains(err.Error(), "acme") {
		t.Errorf("unknown target: %v (should list the inventory)", err)
	}
	if _, err := Open(dir, "../etc", nil); err == nil {
		t.Error("path traversal id must be rejected")
	}
	if _, err := OpenOrNew(dir, "fresh", nil); err != nil {
		t.Errorf("OpenOrNew of an absent file: %v", err)
	}
}

func TestAliasesAndDefaults(t *testing.T) {
	dir := t.TempDir()
	// A compose-style file and a node-style file resolve to the same canonical names.
	write(t, dir, "acme.env", "DEPLOY_HOST=srv.acme\nDEPLOY_USER=deploy\nDEPLOY_PORT=2222\nDEPLOY_PATH=/opt/app\nDOMAIN=app.acme\nCONTRAT=supervised\nEXTRA_KEY=hello\n")
	write(t, dir, "cp.env", "HOST=srv.cp\nSSH_PORT=22\nROOT_USER=root\nSITE_USER=sitey\nPATH_ON_SERVER=/home/sitey/htdocs/x\nAPP_PORT=3001\n")

	acme, _ := Open(dir, "acme", nil)
	for name, want := range map[string]string{
		"host": "srv.acme", "ssh_user": "deploy", "ssh_port": "2222", "path": "/opt/app", "domain": "app.acme",
		"contract": "supervised", "rollout_order": "50", "tls_mode": "", "EXTRA_KEY": "hello",
	} {
		if got := acme.Get(name); got != want {
			t.Errorf("acme.Get(%q) = %q, want %q", name, got, want)
		}
	}

	cp, _ := Open(dir, "cp", nil)
	for name, want := range map[string]string{
		"host": "srv.cp", "ssh_user": "sitey", "root_user": "root", "path": "/home/sitey/htdocs/x", "app_port": "3001", "contract": "ondemand",
	} {
		if got := cp.Get(name); got != want {
			t.Errorf("cp.Get(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestGlobalFallback(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.env", "HOST=srv\n")
	global := envfile.Parse([]byte("REGISTRY=ghcr.io/org\nHOST=ignored\nDEPLOY_PORT=2200\n"))
	a, _ := Open(dir, "a", global)
	if got := a.Get("REGISTRY"); got != "ghcr.io/org" {
		t.Errorf("global key = %q", got)
	}
	if got := a.Get("host"); got != "srv" {
		t.Errorf("target wins over global: %q", got)
	}
	if got := a.Get("ssh_port"); got != "2200" {
		t.Errorf("canonical field falls back to the global file: %q", got)
	}
}

func TestVars(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "x.env", "DEPLOY_HOST=h\nDOMAIN=d.example\nDB_NAME=mydb\n")
	x, _ := Open(dir, "x", nil)
	vars := x.Vars()
	rendered, err := tmpl.Render("{{ target.id }} {{ target.host }} {{ target.domain }} {{ target.db_name }} {{ target.ssh_port }}", vars, false)
	if err != nil || rendered != "x h d.example mydb 22" {
		t.Errorf("rendered %q, %v", rendered, err)
	}
	if _, ok := tmpl.Lookup(vars, "target.deploy_host"); ok {
		t.Error("aliases must not leak as separate variables")
	}
}

func TestSetKeepsTheExistingAliasStyle(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "acme.env", "DEPLOY_HOST=h\n# note\n")
	x, _ := Open(dir, "acme", nil)
	if err := x.Set("host", "new"); err != nil {
		t.Fatal(err)
	}
	if err := x.Set("app_port", "3002"); err != nil {
		t.Fatal(err)
	}
	if err := x.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "acme.env"))
	if !strings.Contains(string(data), "DEPLOY_HOST=new\n") || !strings.Contains(string(data), "APP_PORT=3002\n") || !strings.Contains(string(data), "# note\n") {
		t.Errorf("file = %q", data)
	}
}

func TestSelectAndRolloutOrder(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "c.env", "HOST=c\nROLLOUT_ORDER=10\nCONTRAT=supervised\n")
	write(t, dir, "a.env", "HOST=a\n")
	write(t, dir, "b.env", "HOST=b\nROLLOUT_ORDER=10\nCONTRAT=supervised\n")
	write(t, dir, "d.env", "HOST=d\nROLLOUT_ORDER=junk\n")

	all, err := Select(dir, nil, true, false, nil)
	if err != nil || len(all) != 4 {
		t.Fatalf("Select all: %d targets, %v", len(all), err)
	}
	SortByRollout(all)
	var order []string
	for _, tg := range all {
		order = append(order, tg.ID)
	}
	if got := strings.Join(order, ","); got != "b,c,a,d" {
		t.Errorf("rollout order = %s, want b,c,a,d (10,10,50,50 then id)", got)
	}

	sup, _ := Select(dir, nil, true, true, nil)
	if len(sup) != 2 {
		t.Errorf("supervised = %d targets, want 2", len(sup))
	}
	if one, err := Select(dir, []string{"a"}, false, false, nil); err != nil || len(one) != 1 {
		t.Errorf("explicit id: %v, %v", one, err)
	}
	if _, err := Select(dir, []string{"a", "typo"}, false, false, nil); err == nil {
		t.Error("an unknown id must fail the whole selection")
	}
	if none, _ := Select(dir, nil, false, false, nil); len(none) != 0 {
		t.Error("no ids and no --all selects nothing")
	}
}

func TestExampleTargetsLoad(t *testing.T) {
	for _, tc := range []struct{ dir, id, hostField string }{
		{"../../examples/nextjs-cloudpanel/targets", "acme", "host"},
		{"../../examples/compose-app/targets", "example", "host"},
	} {
		tg, err := Open(tc.dir, tc.id, nil)
		if err != nil {
			t.Fatal(err)
		}
		if tg.Get(tc.hostField) == "" {
			t.Errorf("%s: host is empty", tc.id)
		}
	}
}

func TestProjectOverridesTheManifestName(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "live.env", "HOST=a\n")
	write(t, dir, "second.env", "HOST=b\nCOMPOSE_PROJECT=app-cp-test\n")
	write(t, dir, "bad.env", "HOST=c\nPROJECT=Bad Name!\n")
	live, _ := Open(dir, "live", nil)
	second, _ := Open(dir, "second", nil)
	bad, _ := Open(dir, "bad", nil)
	if got, err := live.Project("acme"); err != nil || got != "acme" {
		t.Errorf("default = %q, %v", got, err)
	}
	if got, err := second.Project("acme"); err != nil || got != "app-cp-test" {
		t.Errorf("override = %q, %v", got, err)
	}
	if _, err := bad.Project("acme"); err == nil {
		t.Error("an invalid compose project name must be rejected")
	}
	if _, err := live.Project("Not Valid"); err == nil {
		t.Error("an invalid manifest project name must be rejected too")
	}
	if got := second.Vars()["target"].(map[string]string)["project"]; got != "app-cp-test" {
		t.Errorf("template variable target.project = %q", got)
	}
}
