package scaffold

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The layout of an Ibexa project that runs as a Docker stack: the compose file and the Dockerfiles in docker/, one multi-stage
// Dockerfile for two images (php and web), a Dockerfile of their own for Varnish and Solr, MariaDB, Redis.
func ibexaDockerFiles() map[string]string {
	return map[string]string{
		"ibexa/composer.json":                               `{"name":"ibexa/oss-skeleton","require":{"php":">=8.2","ibexa/oss":"4.6.13","ibexa/graphql":"4.6.*","tanoconsulting/ibexa-migration-bundle":"^1.0","lexik/jwt-authentication-bundle":"^2"}}`,
		"ibexa/package.json":                                `{"name":"x"}`,
		"ibexa/public/index.php":                            "<?php\n",
		"ibexa/config/packages/cache_pool/cache.redis.yaml": "services: {}\n",
		"docker/Dockerfile": "FROM php:8.2-fpm AS php-base\n" +
			"FROM php-base AS build\nRUN composer install\n" +
			"FROM php-base AS php\nCOPY --from=build /app /app\n" +
			"FROM nginx:1.27 AS web\nCOPY --from=build /app/public /app/public\n",
		"docker/Dockerfile.dockerignore": "**\n",
		"docker/varnish/Dockerfile":      "FROM debian:bookworm-slim\n",
		"docker/solr/Dockerfile":         "FROM solr:8.11.2\n",
		"docker/docker-compose.prod.yml": `services:
  varnish: {image: "${REGISTRY:?missing}/acme-varnish:${APP_VERSION:?missing}"}
  web:     {image: "${REGISTRY}/acme-web:${APP_VERSION}"}
  php:     {image: "${REGISTRY}/acme-php:${APP_VERSION}", volumes: ["public_var:/app/public/var", "jwt:/app/config/jwt"]}
  db:      {image: "mariadb:10.11", volumes: ["db_data:/var/lib/mysql"]}
  redis:   {image: "redis:7-alpine", volumes: ["redis_data:/data"]}
  solr:    {image: "${REGISTRY}/acme-solr:${APP_VERSION}", volumes: ["solr_data:/var/solr"]}
volumes:
  db_data: {}
  redis_data: {}
  solr_data: {}
  public_var: {}
  jwt: {}
`,
		"docker/.env.example": "REGISTRY=ghcr.io/CHANGE_ME\nAPP_VERSION=\nPROXY_PORT=8081\nDOMAIN=CHANGE_ME\nAPP_SECRET=CHANGE_ME\n" +
			"DB_PASSWORD=CHANGE_ME\nDB_ROOT_PASSWORD=CHANGE_ME\nHTTPCACHE_VARNISH_INVALIDATE_TOKEN=CHANGE_ME\nMAILER_DSN=CHANGE_ME\n# SOLR_JAVA_MEM=CHANGE_ME\nSITE_HOST=CHANGE_ME\n",
	}
}

func TestDetectsAnIbexaDockerStack(t *testing.T) {
	f := Detect(files(t, ibexaDockerFiles()))
	if f.Kind != KindCompose || strings.Join(f.ComposeFiles, ",") != "docker/docker-compose.prod.yml" {
		t.Fatalf("a compose file in docker/ makes a compose project: kind=%q files=%v", f.Kind, f.ComposeFiles)
	}
	roles := map[string]string{}
	for _, s := range f.Services {
		roles[s.Name] = s.Role
	}
	want := map[string]string{"varnish": "edge", "web": "proxy", "php": "app", "db": "db", "redis": "cache", "solr": "search"}
	for name, role := range want {
		if roles[name] != role {
			t.Errorf("service %s has role %q, want %q", name, roles[name], role)
		}
	}
	if f.Database != "mysql" || !strings.Contains(f.DBImage, "mariadb") {
		t.Errorf("database = %q (%q)", f.Database, f.DBImage)
	}
	got := map[string]ImageRef{}
	for _, im := range f.Images {
		got[im.Name] = im
	}
	for name, w := range map[string]ImageRef{
		"acme-php":     {Name: "acme-php", Dockerfile: "docker/Dockerfile", Context: ".", Target: "php"},
		"acme-web":     {Name: "acme-web", Dockerfile: "docker/Dockerfile", Context: ".", Target: "web"},
		"acme-varnish": {Name: "acme-varnish", Dockerfile: "docker/varnish/Dockerfile", Context: "docker/varnish"},
		"acme-solr":    {Name: "acme-solr", Dockerfile: "docker/solr/Dockerfile", Context: "docker/solr"},
	} {
		if got[name] != w {
			t.Errorf("image %s = %+v, want %+v", name, got[name], w)
		}
	}
	if strings.Join(f.Volumes, ",") != "jwt,public_var" || !f.JWT {
		t.Errorf("the files of the application are archived, the data of the database, the cache and the search engine are not: %v (jwt %v)", f.Volumes, f.JWT)
	}
	if len(f.ImageNotes) != 0 || len(f.BindDirs) != 0 {
		t.Errorf("every image has a Dockerfile and nothing is mounted from the host: %v %v", f.ImageNotes, f.BindDirs)
	}
	if f.EnvTemplate != "docker/.env.example" || !slices.Equal(f.EnvKeys, []string{"REGISTRY", "DOMAIN", "APP_SECRET", "DB_PASSWORD", "DB_ROOT_PASSWORD", "HTTPCACHE_VARNISH_INVALIDATE_TOKEN", "MAILER_DSN", "SITE_HOST"}) {
		t.Errorf("env template %q keys %v (a commented line is not a placeholder)", f.EnvTemplate, f.EnvKeys)
	}
}

func TestABundleThatIsOnlyADependencyIsFoundByItsConfiguration(t *testing.T) {
	content := ibexaDockerFiles()
	content["ibexa/composer.json"] = `{"name":"ibexa/oss-skeleton","require":{"php":">=8.2","ibexa/oss":"4.6.13","tanoconsulting/ibexa-migration-bundle":"^1.0"}}`
	f := Detect(files(t, content))
	if f.JWT || slices.Contains(f.Console, "ibexa:graphql:generate-schema") {
		t.Fatalf("neither is required nor configured: jwt %v console %v", f.JWT, f.Console)
	}
	content["ibexa/config/packages/lexik_jwt_authentication.yaml"] = "lexik_jwt_authentication: {}\n"
	content["ibexa/config/packages/graphql.yaml"] = "overblog_graphql: {}\n"
	f = Detect(files(t, content))
	if !f.JWT || !slices.Contains(f.Console, "ibexa:graphql:generate-schema") {
		t.Errorf("their configuration files say the project uses them: jwt %v console %v", f.JWT, f.Console)
	}
}

func TestADockerignoreFileIsNotADockerfile(t *testing.T) {
	f := Detect(files(t, ibexaDockerFiles()))
	if !slices.Equal(f.Dockerfiles, []string{"docker/Dockerfile", "docker/solr/Dockerfile", "docker/varnish/Dockerfile"}) {
		t.Errorf("Dockerfiles = %v: the ignore file next to a Dockerfile is not offered as an image", f.Dockerfiles)
	}
}

// The form must offer what the defaults chose: a MariaDB stack gets backups (the restore test is PostgreSQL-only), a PostgreSQL stack both.
func TestTheFormOffersBackupsForMySQLButTheRestoreTestOnlyForPostgres(t *testing.T) {
	values := func(opts []choice) []string {
		var out []string
		for _, o := range opts {
			out = append(out, o.Value)
		}
		return out
	}
	my := DefaultAnswers(Detect(files(t, ibexaDockerFiles())))
	d := &draft{runtime: RuntimeCompose, features: my.Features}
	opts := values(featureChoices(d, my))
	if !slices.Contains(opts, FeatBackup) || slices.Contains(opts, FeatRestoreTest) {
		t.Errorf("MariaDB: backups yes, restore test no: %v", opts)
	}
	if !slices.Contains(d.features, FeatBackup) || !slices.Contains(d.features, FeatMonitor) {
		t.Errorf("the defaults are kept on screen: %v", d.features)
	}

	pg := composeAnswers(t) // PostgreSQL
	d = &draft{runtime: RuntimeCompose, features: pg.Features}
	if opts := values(featureChoices(d, pg)); !slices.Contains(opts, FeatBackup) || !slices.Contains(opts, FeatRestoreTest) {
		t.Errorf("PostgreSQL: both: %v", opts)
	}
}

func TestLeafStagesAreTheOnesNothingBuildsOn(t *testing.T) {
	dir := files(t, ibexaDockerFiles())
	if got := leafStages(filepath.Join(dir, "docker/Dockerfile")); strings.Join(got, ",") != "php,web" {
		t.Errorf("leaves = %v, want php and web (php-base and build are steps)", got)
	}
	if got := leafStages(filepath.Join(dir, "docker/solr/Dockerfile")); got != nil {
		t.Errorf("a single-stage Dockerfile needs no target: %v", got)
	}
	if got := leafStages(filepath.Join(dir, "docker/missing")); got != nil {
		t.Errorf("a missing file has no stages: %v", got)
	}
}

func TestAnImageNothingMatchesIsReportedNotInvented(t *testing.T) {
	content := ibexaDockerFiles()
	content["docker/docker-compose.prod.yml"] = strings.Replace(content["docker/docker-compose.prod.yml"], "acme-solr", "acme-search-engine", 1)
	f := Detect(files(t, content))
	if len(f.Images) != 3 || len(f.ImageNotes) != 1 || !strings.Contains(f.ImageNotes[0], "acme-search-engine") {
		t.Errorf("images %v notes %v", f.Images, f.ImageNotes)
	}
}

func TestBindMountedFoldersAreSyncedAndNamedVolumesAreNot(t *testing.T) {
	content := ibexaDockerFiles()
	content["docker/docker-compose.prod.yml"] = strings.Replace(content["docker/docker-compose.prod.yml"],
		`web:     {image: "${REGISTRY}/acme-web:${APP_VERSION}"}`,
		"web:\n    image: \"${REGISTRY}/acme-web:${APP_VERSION}\"\n    volumes: [\"./conf/nginx/site.conf:/etc/nginx/conf.d/site.conf:ro\", \"public_var:/app/public/var\", \"./.env:/x\"]", 1)
	f := Detect(files(t, content))
	if strings.Join(f.BindDirs, ",") != "conf/" {
		t.Errorf("BindDirs = %v: the folder of a mounted file, not a named volume or a file at the root", f.BindDirs)
	}
}

func ibexaDockerAnswers(t *testing.T) Answers {
	t.Helper()
	a := DefaultAnswers(Detect(files(t, ibexaDockerFiles())))
	a.Target.ID, a.Target.Host, a.Target.Domain = "prod", "srv.example.com", "www.example.com"
	FillDefaults(&a)
	return a
}

func TestAnIbexaDockerStackRendersAManifestThatLoadsAndIsComplete(t *testing.T) {
	a := ibexaDockerAnswers(t)
	m, dir := check(t, a)

	c, err := m.Compose()
	if err != nil {
		t.Fatal(err)
	}
	if c.ProjectDirectory != "." || strings.Join(c.Files, ",") != "docker/docker-compose.prod.yml" {
		t.Errorf("the compose file is in docker/ and the .env in the root: dir %q files %v", c.ProjectDirectory, c.Files)
	}
	roles := map[string]string{}
	for _, s := range c.Services {
		roles[s.Name] = s.Role
	}
	if roles["varnish"] != "edge" || roles["solr"] != "search" || roles["web"] != "proxy" {
		t.Errorf("roles = %v", roles)
	}
	for _, name := range []string{"migrate", "graphql", "reindex", "cache_clear", "jwt_keypair"} {
		if c.App.Commands[name].Run == "" {
			t.Errorf("app command %q is missing: %v", name, c.App.Commands)
		}
	}
	if run := c.App.Commands["migrate"].Run; !strings.Contains(run, "kaliop:migration:migrate") || strings.Contains(run, "{{ php }}") {
		t.Errorf("migrate runs in the container with its own php: %q", run)
	}
	if !slices.Contains(c.Install.Steps, "jwt_keypair") {
		t.Errorf("the key pair is made on the first install: %v", c.Install.Steps)
	}
	if c.App.Service != "php" || !slices.Contains(c.Update.Steps, "migrate") || !slices.Contains(c.Update.Steps, "proxy_reload") {
		t.Errorf("app service %q update steps %v", c.App.Service, c.Update.Steps)
	}

	img, err := m.Image()
	if err != nil {
		t.Fatal(err)
	}
	var tgt []string
	for _, im := range img.Images {
		tgt = append(tgt, im.Name+":"+im.Target+":"+im.Context)
	}
	slices.Sort(tgt)
	if strings.Join(tgt, " ") != "acme-php:php:. acme-solr::docker/solr acme-varnish::docker/varnish acme-web:web:." {
		t.Errorf("images = %v", tgt)
	}

	env, err := m.EnvConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(env.Prompts, []string{"DOMAIN", "MAILER_DSN", "SITE_HOST"}) {
		t.Errorf("prompts = %v: what the operator sets, neither generated nor managed", env.Prompts)
	}
	for key, n := range map[string]int{"APP_SECRET": 32, "DB_PASSWORD": 16, "DB_ROOT_PASSWORD": 16, "HTTPCACHE_VARNISH_INVALIDATE_TOKEN": 32, "BACKUP_PASSPHRASE": 16} {
		if env.Secrets[key].HexBytes != n {
			t.Errorf("secret %s = %d bytes, want %d", key, env.Secrets[key].HexBytes, n)
		}
	}
	if _, ok := env.Secrets["MAILER_DSN"]; ok {
		t.Error("a DSN cannot be generated: it is asked for")
	}
	if !slices.Contains(env.FromTargetOnDeploy, "PROXY_PORT") {
		t.Errorf("the proxy port reaches the server .env: %v", env.FromTargetOnDeploy)
	}

	data, back, err := m.BackupConfig()
	if err != nil {
		t.Fatal(err)
	}
	if data.Database.Type != "mysql" || data.Database.Service != "db" || !strings.Contains(data.Database.Dump, "mariadb-dump") || back == nil {
		t.Errorf("database %+v", data.Database)
	}
	if strings.Contains(data.Database.Dump, "pg_dump") || strings.Contains(data.Database.Dump, "MARIADB_ROOT_PASSWORD=") {
		t.Errorf("the dump is MariaDB's, and the password comes from the container environment: %s", data.Database.Dump)
	}
	text, err := os.ReadFile(filepath.Join(dir, "deploykit.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(text), "restore_test") {
		t.Error("the restore test supports PostgreSQL only: a MySQL stack gets none")
	}
	if strings.Contains(string(text), "docker/\n") && strings.Contains(string(text), "sync:") && strings.Contains(string(text), "files: [docker/docker-compose.prod.yml, docker/.env.example, docker/]") {
		t.Error("build files are not sent to the server")
	}
}

func TestAnIbexaDockerStackRendersInEveryIngressAndFeatureSet(t *testing.T) {
	for _, ingress := range []string{IngressSelect, IngressCloudPanel, IngressStandalone} {
		for _, features := range [][]string{nil, {FeatMonitor}, {FeatBackup}, {FeatBackup, FeatMonitor, FeatCrons}} {
			a := ibexaDockerAnswers(t)
			a.Ingress, a.Features = ingress, features
			if ingress != IngressSelect {
				a.Target.TLSMode = ""
			}
			FillDefaults(&a)
			check(t, a)
		}
	}
}

func TestImagesForKeepsWhatDetectionTiedToAChosenDockerfile(t *testing.T) {
	detected := []ImageRef{
		{Name: "acme-php", Dockerfile: "docker/Dockerfile", Context: ".", Target: "php"},
		{Name: "acme-web", Dockerfile: "docker/Dockerfile", Context: ".", Target: "web"},
		{Name: "acme-solr", Dockerfile: "docker/solr/Dockerfile", Context: "docker/solr"},
	}
	got := ImagesFor([]string{"docker/Dockerfile", "docker/extra/Dockerfile.worker"}, detected)
	var names []string
	for _, im := range got {
		names = append(names, im.Name+":"+im.Target)
	}
	if strings.Join(names, " ") != "acme-php:php acme-web:web worker:" {
		t.Errorf("a multi-stage Dockerfile keeps both images, an unknown one is named from its file, an unchosen one is dropped: %v", names)
	}
}

func TestAPlainComposeProjectKeepsItsOldBehaviour(t *testing.T) {
	a := composeAnswers(t)
	if a.ProjectDirectory != "" || len(a.AppCommands) != 0 {
		t.Errorf("a compose file at the root needs no project directory: %q %v", a.ProjectDirectory, a.AppCommands)
	}
	if len(a.Notes) != 0 {
		t.Errorf("images that match no Dockerfile fall back to one image per Dockerfile without noise: %v", a.Notes)
	}
	if strings.Join(a.SyncDirs, ",") != "docker/" {
		t.Errorf("the old rule still sends docker/: %v", a.SyncDirs)
	}
}

func TestCopySourcesAreWhatTheDockerfileTakesFromItsContext(t *testing.T) {
	dir := files(t, map[string]string{"Dockerfile": "FROM alpine AS build\n" +
		"COPY --chown=app:app conf/site.conf ./vendor/ /etc/app/\n" +
		"COPY --from=build /out /out\n" +
		"COPY . /everything\n" +
		"COPY $SRC /var\n" +
		"ADD https://example.com/x.tgz /tmp/\n" +
		"COPY [\"a b\", \"/dst/\"]\n" +
		"COPY multi \\\n   line.txt /etc/\n" +
		"# COPY commented /out\n" +
		"RUN echo COPY not-an-instruction /x\n"})
	got := copySources(filepath.Join(dir, "Dockerfile"))
	if strings.Join(got, ",") != "conf/site.conf,vendor/,multi,line.txt" {
		t.Errorf("sources = %v: flags, stage copies, the whole context, variables, URLs and JSON-form copies are not paths to check", got)
	}
}

func TestTheBuildContextFollowsWhatTheDockerfileCopies(t *testing.T) {
	dir := files(t, map[string]string{
		// copies a folder that is at the repository root: the Dockerfile's own folder holds nothing but the Dockerfile
		"docker/solr/Dockerfile":    "FROM solr:8.11\nCOPY --chown=solr:solr conf/prod/solr/conf /opt/configset/conf\n",
		"conf/prod/solr/conf/a.xml": "<a/>",
		// copies files that sit next to it: its own folder is the context
		"docker/varnish/Dockerfile":  "FROM debian\nCOPY default.vcl parameters.vcl /etc/varnish/\n",
		"docker/varnish/default.vcl": "vcl 4.1;\n", "docker/varnish/parameters.vcl": "backend x {}\n",
		// copies the whole context: nothing says where the files are, the guess stands (and is flagged to the user)
		"docker/whole/Dockerfile": "FROM alpine\nCOPY . /app\n",
		"docker/docker-compose.prod.yml": `services:
  solr:   {image: "${REGISTRY}/acme-solr:${APP_VERSION}"}
  varnish: {image: "${REGISTRY}/acme-varnish:${APP_VERSION}"}
  whole:  {image: "${REGISTRY}/acme-whole:${APP_VERSION}"}
`,
	})
	f := Detect(dir)
	got := map[string]ImageRef{}
	for _, im := range f.Images {
		got[im.Name] = im
	}
	for name, want := range map[string]ImageRef{
		"acme-solr":    {Name: "acme-solr", Dockerfile: "docker/solr/Dockerfile", Context: "."},
		"acme-varnish": {Name: "acme-varnish", Dockerfile: "docker/varnish/Dockerfile", Context: "docker/varnish"},
		"acme-whole":   {Name: "acme-whole", Dockerfile: "docker/whole/Dockerfile", Context: "docker/whole"},
	} {
		if got[name] != want {
			t.Errorf("%s = %+v, want %+v", name, got[name], want)
		}
	}
}

func TestAStageKeepsItsFolderAsContextWhenThatIsWhereItsSourcesAre(t *testing.T) {
	dir := files(t, map[string]string{
		"docker/jobs/Dockerfile": "FROM alpine AS base\nFROM base AS worker\nCOPY src/ /app/\n",
		"docker/jobs/src/run.sh": "#!/bin/sh\n",
		"docker/docker-compose.prod.yml": `services:
  worker: {image: "${REGISTRY}/acme-worker:${APP_VERSION}"}
`,
	})
	f := Detect(dir)
	want := ImageRef{Name: "acme-worker", Dockerfile: "docker/jobs/Dockerfile", Context: "docker/jobs", Target: "worker"}
	if len(f.Images) != 1 || f.Images[0] != want {
		t.Errorf("images = %+v, want %+v: the stage matched with the repository root, but its sources are in its folder", f.Images, want)
	}
}
