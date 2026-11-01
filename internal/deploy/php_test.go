package deploy

import (
	"strings"
	"testing"

	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/runner"
	"github.com/masilia/deploykit/internal/transport"
)

const phpManifest = `
version: 1
project: shop
artifact:
  type: release-dir
  build:
    workdir: app
    env: { APP_ENV: prod }
    install: "mkdir -p vendor && echo vendor > vendor/autoload.php"
    command: |
      mkdir -p public
      echo '<?php echo 1;' > public/index.php
  assemble: [{ from: "app/.", to: ".", exclude: [var] }]
  keep: 2
runtime:
  type: php-fpm
  php_version: "8.2"
  web_root: public
  shared:
    - { path: .env.local, kind: file, required: true }
    - { path: var/log, kind: dir }
    - { path: public/var, kind: dir }
  writable: [var/cache]
  env: { APP_ENV: prod }
  commands:
    warmup:  "{{ php }} bin/console cache:warmup"
    migrate: "{{ php }} bin/console migrate"
    purge:   "{{ php }} bin/console purge"
  prepare: [warmup, migrate]
  after_switch: [purge]
  health: { path: /, expect: [200, 302], tries: 3, interval: 1s }
ingress:
  type: cloudpanel
  site: { type: php, php_version: "8.2" }
`

var phpRepo = map[string]string{"app/composer.json": `{"require":{"php":">=8.2","ext-intl":"*","ext-gd":"*","symfony/framework-bundle":"5.4.*"}}`}

func phpFixture(t *testing.T) *fixture {
	t.Helper()
	fx := newFixtureWith(t, phpManifest, phpRepo)
	fx.site.On("php8.2 -m", transport.Result{Stdout: "[PHP Modules]\ngd\nintl\nmbstring\n"})
	return fx
}

func TestPHPDeploySuccessLinksSharedPreparesAndSwitches(t *testing.T) {
	fx := phpFixture(t)
	fx.site.On("DK_TRIES", transport.Result{Stdout: "200\n"})
	ev := &events{}
	if err := fx.node.Deploy(t.Context(), &runner.Runner{Reporter: ev}); err != nil {
		t.Fatalf("%v\n%s", err, ev.text())
	}
	trace := fx.site.Trace()
	inOrder(t, trace,
		"test -f /home/demo/htdocs/demo.example/shared/.env.local", // required shared file, before anything is uploaded
		"php8.2 -m",
		"sync ",
		"link_file .env.local /home/demo/htdocs/demo.example/shared/.env.local",
		"link_dir var/log", "link_dir public/var", "mkdir -p var/cache",
		"php8.2 bin/console cache:warmup", "php8.2 bin/console migrate", // prepare, in the new release
		"ln -sfn \"releases/$DK_STAMP\" current.tmp", // the switch
		"DK_BACKEND=127.0.0.1:8080", "DK_EXPECT=200 302", "DK_HOST=demo.example",
		"php8.2 bin/console purge", // after_switch, in the live release
	)
	if strings.Contains(trace, "ecosystem") || strings.Contains(trace, "pm2") {
		t.Errorf("a php site has no pm2:\n%s", trace)
	}
	for _, want := range []string{"step prepare", "step after switch", "$ warmup", "$ migrate", "$ purge", "answers"} {
		if !strings.Contains(ev.text()+fx.site.Trace(), want) {
			t.Errorf("missing %q in:\n%s", want, ev.text())
		}
	}
	if !strings.Contains(trace, "DK_RELEASE=/home/demo/htdocs/demo.example/releases/"+newStamp) {
		t.Errorf("the shared links are made inside the new release:\n%s", trace)
	}
}

func TestPHPDeployStopsBeforeUploadWhenARequiredFileIsMissing(t *testing.T) {
	fx := phpFixture(t)
	fx.site.Fail("test -f /home/demo/htdocs/demo.example/shared/.env.local", 1, "")
	ev := &events{}
	err := fx.node.Deploy(t.Context(), &runner.Runner{Reporter: ev})
	if err == nil || !strings.Contains(ev.text(), ".env.local is missing") {
		t.Fatalf("err = %v\n%s", err, ev.text())
	}
	if strings.Contains(fx.site.Trace(), "sync ") {
		t.Error("nothing may be uploaded")
	}
}

func TestPHPDeployReportsMissingExtensionsAndNeverInstallsThem(t *testing.T) {
	fx := newFixtureWith(t, phpManifest, phpRepo)
	fx.site.On("php8.2 -m", transport.Result{Stdout: "[PHP Modules]\ngd\n"})
	ev := &events{}
	err := fx.node.Deploy(t.Context(), &runner.Runner{Reporter: ev})
	if err == nil || !strings.Contains(ev.text(), "lacks the extension(s) intl") || !strings.Contains(ev.text(), "apt install php8.2-intl") {
		t.Fatalf("err = %v\n%s", err, ev.text())
	}
	if strings.Contains(fx.site.Trace(), "apt") && strings.Contains(fx.site.Trace(), "$ apt") {
		t.Error("deploykit must never install packages")
	}
}

func TestPHPDeployWithoutAnExtensionCheck(t *testing.T) {
	m := strings.Replace(phpManifest, "  prepare: [warmup, migrate]", "  php_extensions: none\n  prepare: [warmup, migrate]", 1)
	fx := newFixtureWith(t, m, phpRepo)
	fx.site.On("DK_TRIES", transport.Result{Stdout: "200\n"})
	if err := fx.node.Deploy(t.Context(), &runner.Runner{Reporter: &events{}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fx.site.Trace(), "php8.2 -m") {
		t.Error("php_extensions: none skips the check")
	}
}

func TestPHPPrepareFailureLeavesTheLiveReleaseAlone(t *testing.T) {
	fx := phpFixture(t)
	fx.site.Fail("php8.2 bin/console migrate", 3, "SQLSTATE[42S22]")
	ev := &events{}
	err := fx.node.Deploy(t.Context(), &runner.Runner{Reporter: ev})
	if err == nil || !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("err = %v\n%s", err, ev.text())
	}
	trace := fx.site.Trace()
	if strings.Contains(trace, "current.tmp") {
		t.Errorf("the link must not move when prepare fails:\n%s", trace)
	}
	// "failed\n" is 7 bytes: the status file of the release is rewritten after the failing command.
	inOrder(t, trace, "php8.2 bin/console migrate", "releases/"+newStamp+"/.status", "stdin <7 bytes>")
	if strings.Contains(trace, "bin/console purge") {
		t.Error("after_switch must not run when nothing was switched")
	}
}

func TestPHPHealthFailureRollsBackAndRunsAfterSwitchAgain(t *testing.T) {
	fx := phpFixture(t)
	fx.site.On("DK_TRIES", transport.Result{Stdout: "503\n", ExitCode: 1})
	ev := &events{}
	err := fx.node.Deploy(t.Context(), &runner.Runner{Reporter: ev})
	if err == nil || !strings.Contains(err.Error(), "does not answer [200 302] on 127.0.0.1:8080/ (last code 503)") || !strings.Contains(err.Error(), "rolled back to "+prevStamp) {
		t.Fatalf("err = %v", err)
	}
	trace := fx.site.Trace()
	if strings.Count(trace, `ln -sfn "releases/$DK_STAMP" current.tmp`) != 2 {
		t.Errorf("the link moves to the new release and back:\n%s", trace)
	}
	if strings.Count(trace, "php8.2 bin/console purge") != 1 {
		t.Errorf("after_switch runs once, after the rollback (the optional step does not run on a failed deploy):\n%s", trace)
	}
}

func TestPHPReloadAsRootWhenAsked(t *testing.T) {
	m := strings.Replace(phpManifest, "  prepare: [warmup, migrate]", "  reload: root-systemctl\n  prepare: [warmup, migrate]", 1)
	fx := newFixtureWith(t, m, phpRepo)
	fx.site.On("php8.2 -m", transport.Result{Stdout: "gd\nintl\n"})
	fx.site.On("DK_TRIES", transport.Result{Stdout: "200\n"})
	root := transport.NewFake("root@srv")
	fx.node.Root = root
	if err := fx.node.Deploy(t.Context(), &runner.Runner{Reporter: &events{}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(root.Trace(), "systemctl reload php8.2-fpm") {
		t.Errorf("root must reload the pool:\n%s", root.Trace())
	}
}

func TestPHPRollbackSwitchesBackAndRunsAfterSwitch(t *testing.T) {
	fx := phpFixture(t)
	fx.site.OnFunc(func(s transport.Spec) bool { return strings.Contains(s.Script, "for d in *") }, transport.Result{
		Stdout: prevStamp + "\tlive\t\n20261025000000\tsuperseded\t\n"})
	fx.site.On("DK_TRIES", transport.Result{Stdout: "200\n"})
	if err := fx.node.Rollback(t.Context(), &runner.Runner{Reporter: &events{}}, ""); err != nil {
		t.Fatal(err)
	}
	trace := fx.site.Trace()
	inOrder(t, trace, "DK_STAMP=20261025000000", "DK_BACKEND=127.0.0.1:8080", "php8.2 bin/console purge")
}

func TestPHPManifestRules(t *testing.T) {
	bad := map[string]string{
		"version": strings.Replace(phpManifest, `php_version: "8.2"
  web_root`, `php_version: "eight"
  web_root`, 1),
		"undeclared name":  strings.Replace(phpManifest, "prepare: [warmup, migrate]", "prepare: [warmup, nope]", 1),
		"web root escapes": strings.Replace(phpManifest, "web_root: public", "web_root: ../etc", 1),
		"shared escapes":   strings.Replace(phpManifest, "path: var/log", "path: ../log", 1),
		"required dir":     strings.Replace(phpManifest, "{ path: var/log, kind: dir }", "{ path: var/log, kind: dir, required: true }", 1),
		"extensions":       strings.Replace(phpManifest, "  prepare: [warmup, migrate]", "  php_extensions: maybe\n  prepare: [warmup, migrate]", 1),
		"reload":           strings.Replace(phpManifest, "  prepare: [warmup, migrate]", "  reload: kick\n  prepare: [warmup, migrate]", 1),
		"workdir escapes":  strings.Replace(phpManifest, "workdir: app", "workdir: ../x", 1),
	}
	for name, y := range bad {
		m, err := manifest.Parse([]byte(y))
		if err == nil {
			err = m.Validate()
		}
		if err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
	m, err := manifest.Parse([]byte(phpManifest))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("the reference manifest must validate: %v", err)
	}
}

func TestPHPRunACommandInTheLiveRelease(t *testing.T) {
	fx := phpFixture(t)
	if err := fx.node.RunCommand(t.Context(), &runner.Runner{Reporter: &events{}}, "clear"); err == nil {
		t.Fatal("clear is not declared in this manifest")
	}
	if err := fx.node.RunCommand(t.Context(), &runner.Runner{Reporter: &events{}}, "purge"); err != nil {
		t.Fatal(err)
	}
	trace := fx.site.Trace()
	inOrder(t, trace, "dir /home/demo/htdocs/demo.example/current", "php8.2 bin/console purge")
	if strings.Contains(trace, "current.tmp") || strings.Contains(trace, "sync ") {
		t.Errorf("run must only run the command:\n%s", trace)
	}
}

func TestPHPRunNeedsALiveRelease(t *testing.T) {
	fx := phpFixture(t)
	fx.site.OnFunc(func(s transport.Spec) bool { return strings.Contains(s.Script, "readlink -f") }, transport.Result{})
	// the fixture already answers readlink with the previous release: a second, fresh fake has none
	fx2 := newFixtureWith(t, phpManifest, phpRepo)
	site := transport.NewFake("demo@srv")
	fx2.node.Site = site
	if err := fx2.node.RunCommand(t.Context(), &runner.Runner{Reporter: &events{}}, "purge"); err == nil || !strings.Contains(err.Error(), "no live release") {
		t.Errorf("err = %v", err)
	}
}

func TestPHPDeployThenAlsoRunsTheNamedCommands(t *testing.T) {
	fx := phpFixture(t)
	fx.site.On("DK_TRIES", transport.Result{Stdout: "200\n"})
	fx.node.Opts.Also = []string{"clear"}
	if err := fx.node.Deploy(t.Context(), &runner.Runner{Reporter: &events{}}); err == nil {
		t.Fatal("clear is not declared: the deploy itself succeeded but --also fails")
	}
	fx = phpFixture(t)
	fx.site.On("DK_TRIES", transport.Result{Stdout: "200\n"})
	fx.node.Opts.Also = []string{"warmup"}
	if err := fx.node.Deploy(t.Context(), &runner.Runner{Reporter: &events{}}); err != nil {
		t.Fatal(err)
	}
	trace := fx.site.Trace()
	if strings.Count(trace, "php8.2 bin/console cache:warmup") != 2 {
		t.Errorf("warmup runs once in prepare and once after the deploy:\n%s", trace)
	}
}

func TestPM2HasNoNamedCommands(t *testing.T) {
	fx := newFixture(t, nodeManifest)
	if err := fx.node.RunCommand(t.Context(), &runner.Runner{Reporter: &events{}}, "x"); err == nil || !strings.Contains(err.Error(), "php-fpm runtime") {
		t.Errorf("err = %v", err)
	}
}
