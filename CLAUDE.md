# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`deploykit` (alias `dk`) is a Go CLI that replaces two families of shell scripts (the compose family's `deployment/*.sh` for Docker
Compose, the Node family's `deploy/*.sh` for Next.js + pm2) with one binary driven by a per-project `deploykit.yaml`. No
project-specific constant lives in the code. Database tooling (`db-dump`, `db-push`, restore of a dump) is **out of
scope**. `PLAN.md` is the design and holds the implementation status table; `docs/adr/` records the decisions.

**Everything is written in English** (code, comments, docs, manifests, CLI messages, commits). Never commit `deploy.env`,
`.env.deploy` or `clients/*.env`. The original shell scripts are no longer in the repository: the parity tests in
`internal/envfile` and `internal/image` use them when `DEPLOYKIT_REFERENCE_DIR` points at a checkout (a directory holding `lib.sh`) and skip otherwise.

The pm2 flow and the compose flows (status, backup, restore-test, monitor, `update` with rollback, `install` behind
CloudPanel) were validated on a real shared CloudPanel host (`docs/validation.md`). The standalone certbot ingress has
**not** run on a real host. Never run `update`, `install`, restarts or cron changes on a live host without an explicit
go-ahead; test on a uniquely named throwaway instance (own path, own `COMPOSE_PROJECT`, own proxy port) and clean up
after yourself, diffing against a snapshot taken first.

## Commands

```sh
make build     # CGO-free build into bin/deploykit + the bin/dk symlink, version injected through ldflags
make test      # go test ./...   (one test: go test ./internal/compose -run TestUpdateRollsBackAutomatically)
make vet
make lint      # needs golangci-lint v2; otherwise: go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run
make snapshot  # needs goreleaser v2; otherwise: go run github.com/goreleaser/goreleaser/v2@latest release --snapshot --clean
```

Use `go test -race ./...` before committing. Some tests need `git`, `sh`, `gpg`, `tar`, `find`, `rsync`, `curl`, `node`
and skip themselves when a tool is missing. CI runs vet + race tests, golangci-lint and a GoReleaser snapshot.

## Architecture

Manifest = three axes, `artifact` (image | release-dir) × `runtime` (compose | pm2 | php-fpm) × `ingress` (cloudpanel |
standalone); an ingress may use `select` + `variants` (the `TLS_MODE` key: variant keys `certbot`, `cloudpanel`).

- `internal/transport`: **every effect goes through `Transport`** (Local, SSH over the system `ssh`/`rsync`, Fake).
  Scripts travel on the stdin of the remote shell; `Env`/`Secrets` are exported inside that script, never put in argv;
  `Argv`+`Stdin` is for `--password-stdin` style input. The Fake records calls (`Trace()` gives a stable text) and
  replays canned results.
- `internal/runner` + `internal/ui`: flows are lists of `runner.Step` (`Check` is read-only and also runs in `--dry-run`;
  `Run` does the work). The runner emits events; `ui.Plain` is the only renderer so far (no Bubble Tea TUI yet).
- `internal/manifest`: strict YAML (unknown keys are errors). Axes are decoded on demand into typed structs
  (`ReleaseDir()`, `PM2()`, `Image()`, `Compose()`, `BackupConfig()`, `MonitorConfig()`…); `Validate` runs them all, so
  `examples/*/deploykit.yaml` double as schema tests. `internal/tmpl` is the template language (`{{ a.b }}`,
  `| default: x`, shell-quoting mode), not text/template.
- `internal/envfile` and `internal/inventory`: env files are parsed **without evaluation** (parity-tested against the
  real `lib.sh`/`env-set.sh`); targets live in `targets/<id>.env` with canonical keys and legacy aliases
  (`docs/targets.md`).
- pm2 flow: `internal/releasedir` (clean `git archive` export, build, assemble, local smoke test) and `internal/deploy`
  (`node.go` deploy/rollback, `bootstrap.go`, `releases.go` remote layout, lock, status files, pruning, `pm2.go`).
  `internal/cloudpanel` wraps `clpctl` (root identity provisions, site user deploys; no CloudPanel API).
- compose flow: `internal/image` (release from a tag), `internal/deploy/compose.go` (workstation: upload files + a
  same-version binary, configure `.env`, run `server update|install` **on the server**), `internal/compose` (server side:
  `Stack`, `Update` with automatic rollback, `Install`, `Backup`, `Monitor`, `RestoreTest`, crontab block), `internal/fleet`.
  `cmd/deploykit/server.go` is the `deploykit server …` entry used by deploy and cron.

- `internal/scaffold` + `cmd/deploykit/cmd_init.go`: `init` and `target add`. `Detect` reads the repository; `Answers`
  (flags, or the form) feed `Files`, which renders the manifest from the annotated templates in `templates/` (delimiters
  `[[ ]]`, because the manifest's own `{{ }}` must pass through) and the target file; `NewPlan`/`Apply` write them without
  overwriting silently. The questions are declared once (`initScreens`) and drawn by two renderers: `huh` full-screen
  (`form_tui.go`) and a line-prompt one (`form_plain.go`, used by `--plain`, a dumb terminal and the tests; huh's own
  accessible mode ignores hidden screens and dynamic options, so it is not used). Every rendered combination must load,
  `Validate()` and decode into its typed sections (`render_test.go`); `examples/*` stay hand-written references.

## Conventions that are easy to get wrong

- **Fake rules are first-match.** When a test needs a failure, register it before the success rules (see `newUpdateOpts`
  `pre`, `newInstall`); a late `Fail(...)` is silently ignored.
- Tests assert on `Fake.Trace()`; secrets must never appear in it (`Secrets` are masked, `Stdin` shows only its size).
- Anything that mutates must sit behind `Run`, not `Check`; `--dry-run` must leave the server untouched, **including the
  lock** (`Node.locked` guards the deferred unlock).
- Builds happen in a temp dir **outside** the repository (`--build-only` keeps it and prints the path): Next.js takes its
  tracing root from the topmost lockfile above the build, so a build under `<repo>/.deploykit` puts `server.js` in
  `.next/standalone/.deploykit/src/`. A clean `git archive` export also has no `.next/types`, so the Next.js manifest runs
  `npx next typegen` before `tsc`.
- rsync: a directory source needs its trailing slash (`filepath.Join` drops it); the pm2 upload excludes `.status` and
  `.release.json` so `--delete` cannot remove them.
- cobra resets a flag variable to the flag's default when it is declared: declare flags with the variable's current value
  as the default (see `newRootCmdTo`) or injected test values are lost.
- Health polling sleeps for real unless `Stack.Sleep` / `app.sleep` is replaced; a failing check in a test otherwise waits
  for the 180 s manifest timeout.
- `gofmt` rewrites doc comments (it turns `''` into a typographic quote); describe such strings in words.
- On-disk contracts of the legacy scripts to keep: `.env` `APP_VERSION`/`APP_VERSION_PREV`, `.installed`, `versions.log`, `.lock-ops/pid`,
  `log/monitor.state`, `<prefix>-backup-*.tar.gpg[.sha256]` (retention reproduces `find -mtime +N`, tested against real
  `find`), legacy `cd <root> &&` crontab lines (replaced by a marked block).
- Scripts are wrapped by `guardStdin` (read fully, then run with an empty stdin): without it a `docker compose exec -T`
  in the middle of a script swallows the following lines. Every compose command carries `-p <project>`. `main` adds the
  standard directories to PATH because cron may start jobs with none.
- Secrets never as a command argument. Sole exception: `clpctl site:add:* --siteUserPassword` (ADR 0006). Error messages
  must not echo a value that could be a secret.

## Not built yet

PHP runtime and its manifest (hypothesis, no reference script), the Bubble Tea TUI for deploys, the standalone certbot ingress
on a real host, and the PHP web root and opcache behind CloudPanel. See PLAN.md.
