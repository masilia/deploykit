package scaffold

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// files writes a small repository into a temporary directory.
func files(t *testing.T, content map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range content {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const nextPackage = `{
  "name": "@acme/site",
  "scripts": {"lint": "eslint", "build": "next build", "start": "next start"},
  "dependencies": {"next": "^16.4.2", "react": "19"},
  "engines": {"node": ">=22"}
}`

const composeProd = `name: shop
services:
  proxy: {image: "nginx:1.30"}
  front: {image: "${REGISTRY}/frontend:${APP_VERSION}"}
  api:   {image: "${REGISTRY}/backend:${APP_VERSION}"}
  worker: {image: "${REGISTRY}/backend:${APP_VERSION}"}
  scheduler: {image: "${REGISTRY}/backend:${APP_VERSION}"}
  db:    {image: "postgres:16"}
  redis: {image: "redis:7"}
  hub:   {image: "dunglas/mercure:v1"}
volumes:
  pgdata: {}
  documents: {}
`

func TestDetectsANextProject(t *testing.T) {
	dir := files(t, map[string]string{"package.json": nextPackage})
	f := Detect(dir)
	if f.Kind != KindNode || !f.Next || f.NextVersion != "16" || f.NodeVersion != "22" || f.Name != "site" {
		t.Errorf("findings = %+v", f)
	}
	if f.Scripts["lint"] != "eslint" || f.Scripts["build"] != "next build" {
		t.Errorf("scripts = %v", f.Scripts)
	}
}

func TestNodeVersionComesFromNvmrcFirst(t *testing.T) {
	dir := files(t, map[string]string{"package.json": nextPackage, ".nvmrc": "v20.11.1\n"})
	if f := Detect(dir); f.NodeVersion != "20" {
		t.Errorf("NodeVersion = %q, want 20", f.NodeVersion)
	}
}

func TestDetectsAComposeStack(t *testing.T) {
	dir := files(t, map[string]string{
		"docker-compose.prod.yml":       composeProd,
		"docker-compose.cloudpanel.yml": "services: {}\n",
		"docker/Dockerfile.api":         "FROM php\n",
		"docker/Dockerfile.front":       "FROM node\n",
	})
	f := Detect(dir)
	if f.Kind != KindCompose || f.Name != "shop" || f.Database != "postgres" {
		t.Errorf("findings = %+v", f)
	}
	if !reflect.DeepEqual(f.ComposeFiles, []string{"docker-compose.prod.yml", "docker-compose.cloudpanel.yml"}) {
		t.Errorf("the production file must come first: %v", f.ComposeFiles)
	}
	roles := map[string]string{}
	for _, s := range f.Services {
		roles[s.Name] = s.Role
	}
	want := map[string]string{"proxy": "proxy", "front": "app", "api": "app", "worker": "worker", "scheduler": "worker", "db": "db", "redis": "cache", "hub": "realtime"}
	if !reflect.DeepEqual(roles, want) {
		t.Errorf("roles = %v", roles)
	}
	if !reflect.DeepEqual(f.Dockerfiles, []string{"docker/Dockerfile.api", "docker/Dockerfile.front"}) || !reflect.DeepEqual(f.Volumes, []string{"documents", "pgdata"}) {
		t.Errorf("dockerfiles %v, volumes %v", f.Dockerfiles, f.Volumes)
	}
}

func TestComposeWinsWhenBothAreFound(t *testing.T) {
	dir := files(t, map[string]string{"package.json": nextPackage, "docker-compose.prod.yml": composeProd})
	if f := Detect(dir); f.Kind != KindCompose || f.Also != KindNode {
		t.Errorf("Kind = %q, Also = %q", f.Kind, f.Also)
	}
}

func TestAnEmptyDirectoryGetsAValidNameFromItsFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "My Cool_App")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	f := Detect(dir)
	if f.Kind != KindNone || f.Name != "my-cool_app" {
		t.Errorf("findings = %+v", f)
	}
}

func TestGitRemoteNamesTheProjectWhenNothingElseDoes(t *testing.T) {
	dir := files(t, map[string]string{".git/config": "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = git@github.com:acme/Billing.Service.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"})
	f := Detect(dir)
	if f.GitRemote != "git@github.com:acme/Billing.Service.git" || f.Name != "billing.service" {
		t.Errorf("remote %q, name %q", f.GitRemote, f.Name)
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{"My App": "my-app", "  Acme/Site ": "acme-site", "---": "", "ok.name_1": "ok.name_1", "ÉTÉ": "t"} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGuessRole(t *testing.T) {
	for _, c := range []struct{ name, image, want string }{
		{"queue-worker", "app:1", "worker"}, {"database", "mariadb:11", "db"}, {"cache", "", "cache"},
		{"edge", "caddy:2", "proxy"}, {"api", "app:1", "app"}, {"mercure", "", "realtime"},
	} {
		if got := GuessRole(c.name, c.image); got != c.want {
			t.Errorf("GuessRole(%q, %q) = %q, want %q", c.name, c.image, got, c.want)
		}
	}
}
