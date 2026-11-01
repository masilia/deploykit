# deploykit: plan

A generic deployment CLI (Go, binary `deploykit`, alias `dk`) that replaces two families of shell scripts with one
binary driven by a per-project manifest:

- The compose scripts `deployment/*.sh` (~2,200 lines, POSIX sh): Docker Compose stack, release from a git tag, SSH update with
  verified backup and automatic rollback, multi-client fleet, monitoring.
- The Node scripts `deploy/*.sh` (bash): Next.js standalone built locally, rsync releases, pm2, CloudPanel bootstrap
  with `clpctl`.

Their behaviour is the parity reference (the scripts were kept as test fixtures in a private archive). No project-specific constant may live in the
code: everything comes from the manifest.

**Out of scope** (a separate project later): database tooling (`db-dump`, `db-push`, restore of a dump).

## Decisions

| # | Decision | Record |
|---|---|---|
| D1 | **Go** (cobra, Bubble Tea / Lip Gloss / huh, yaml.v3, GoReleaser). Rust rejected: the workload is I/O glue, cross-compiling is harder, the team onboards faster on Go. | [ADR 0001](docs/adr/0001-go.md) |
| D2 | The manifest composes **three axes**: `artifact` (image \| release-dir), `runtime` (compose \| pm2 \| php-fpm), `ingress` (cloudpanel \| standalone). `version: 1`, strict parsing. No `kind` switch. | [ADR 0002](docs/adr/0002-manifest-axes.md) |
| D3 | Every command goes through a `Transport` interface (Local, SSH via system `ssh`/`rsync`, Fake recorder). `deploykit server …` is the same steps with the Local transport. A server-side binary is only needed for the compose runtime. | [ADR 0003](docs/adr/0003-transport.md) |
| D4 | `internal/cloudpanel` is a first-class provisioner around `clpctl`. No CloudPanel API or login: `clpctl` runs on the server over SSH. Two SSH logins per target: root (or a sudoer) for provisioning only, the site user for every deploy. | [ADR 0004](docs/adr/0004-cloudpanel-provisioner.md), [docs/cloudpanel-cli.md](docs/cloudpanel-cli.md) |
| D5 | The runner emits events (step start, output, step end). The plain renderer (timestamped lines: CI, cron, `NO_COLOR`, no TTY) and the TUI are two consumers. Steps have `Check()` and `Run()`; rollback belongs to the runtime. | |
| D6 | Templating with a tiny purpose-built language (`{{ path.to.value }}` and `| default: x`; no functions, no control flow, so a manifest cannot run code). Values interpolated into shell are quoted automatically; remote scripts always go on stdin of `bash -l -s`. | `internal/tmpl` |
| D7 | Env files are parsed **without evaluation**, upserts are idempotent, lookup is layered (target, then global). | |
| D8 | Release-dir builds come from a clean git ref (`git archive`); `--from-worktree` is opt-in. Releases are named by timestamp and carry `.release.json` (commit, ref, date, author, status). Failed releases are marked and purged first. | [ADR 0005](docs/adr/0005-release-builds.md) |
| D9 | Backward compatibility with live servers managed by the legacy scripts (see below). `docker` and `gpg` stay external CLIs; the need for curl, openssl and awk disappears. | |
| D10 | `clpctl site:add:* --siteUserPassword` puts a password in argv: a **documented exception** (random, sent inside a stdin script, shown once, never used by deploykit). | [ADR 0006](docs/adr/0006-clpctl-password-argv.md) |
| D11 | v1 scope: release, deploy, rollback, install/bootstrap, status, healthcheck, fleet, backup, restore-test, monitor (generic webhook notifier, Slack payload first). Deferred: restore from a backup, all DB tooling. | |
| D12 | English everywhere: docs, manifests, ADRs, CLI messages, code. | |

## Target matrix

| Target | Artifact | Runtime | Ingress | Reference |
|---|---|---|---|---|
| Docker stack behind CloudPanel | image | compose (bound to `127.0.0.1:PROXY_PORT`) | cloudpanel (`site:add:reverse-proxy`) | `TLS_MODE=cloudpanel` |
| Docker stack, standalone | image | compose | standalone (nginx container + certbot) | `TLS_MODE=certbot` |
| Next.js / Node | release-dir | pm2 | cloudpanel (`site:add:nodejs`) | the Node scripts |
| PHP | release-dir | php-fpm | cloudpanel (`site:add:php` + vhost template) | none yet (hypothesis) |

Manifest sketch (full versions in `examples/`):

```yaml
version: 1
project: acme-site
artifact: { type: release-dir, build: { ... }, assemble: [ ... ] }
runtime:  { type: pm2, entry: server.js, ... }
ingress:  { type: cloudpanel, site: nodejs, tls: { require_dns: [...] } }
inventory: targets/
```

## Architecture

```
cmd/deploykit/                          cobra commands
internal/envfile/                       parse (no eval), upsert, layered lookup     <- lib.sh file_get/cfg_get, env-set.sh
internal/manifest/                      load, validate, version, axes, templating
internal/inventory/                     targets/<id>.env, id validation, rollout order, contract filter   <- fleet.sh
internal/transport/                     local, ssh (stdin scripts), rsync, fake recorder
internal/runner/                        steps, events, dry-run
internal/ui/                            plain + tui renderers, prompts (input/secret/confirm/select, type-the-domain)
internal/cloudpanel/                    clpctl wrapper, site detection, ports, certs, keys, pm2 startup
internal/artifact/{image,releasedir}/   docker build/push from git archive; local build, assemble, smoke test
internal/runtime/{compose,pm2,phpfpm}/  deploy, rollback, health, status per runtime
internal/ingress/{cloudpanel,standalone}/
internal/server/                        lock, journal, backup, retention, restore-test, monitor state, crontab block
internal/fleet/                         status/healthcheck/backups/deploy across targets (parallel reads, ordered writes)
internal/notify/                        webhook (Slack payload)
```

Commands: `deploykit setup|deploy|release|rollback|status|healthcheck|backup|doctor|fleet …` on the workstation, and
`deploykit server update|install|backup|healthcheck|monitor|restore-test` on a compose server.

Distribution: static binaries linux amd64/arm64 and darwin amd64/arm64 (GoReleaser, GitHub Releases, `go install`).
For the compose runtime the server receives **the binary of the same version** (scp + sha256) instead of rsync'd
scripts, so scripts and version cannot drift.

## Compatibility with live servers of the legacy scripts (D9)

Keep these on-disk contracts, because deployed servers and other tooling read them:
`.env` keys `APP_VERSION` / `APP_VERSION_PREV` (rollback swaps them), `.installed`, `versions.log`, `.lock-ops/pid`,
`log/monitor.state`, backups `<prefix>-backup-<stamp>[-monthly].tar.gpg` + `.sha256`, and crontab entries of the form
`cd $ROOT && …` (recognised and migrated to a marked block).

## Business rules to keep (from the scripts)

- **Release** (compose): images built from `git archive vX.Y.Z` (existing tag, otherwise created on HEAD), never from the
  worktree; pushed with an extra `sha-<commit>` tag; registry must be explicit `host/namespace`; gates (`make ci`) only on
  request.
- **Release** (release-dir): clean git ref by default, local build, assembly, **local smoke test before any upload**
  (cookie jar, redirects, `Range` request), same-architecture check, `--link-dest` only if the previous release exists.
- **Update** (compose): exclusive lock (stale lock recovered), disk-space check, verified backup, pull, migrations,
  project hooks, `up -d`, proxy reload, health **with waiting** (healthy services, first scheduler tick) plus ORM mapping
  validation, **automatic rollback** to the previous version (can be disabled; the schema is not undone), image purge,
  journal.
- **Switch** (release-dir): `ln -sfn` + `mv -Tf` atomic swap, runtime reload, health check **on the server**, automatic
  rollback to the previous release, retention of N releases, failed releases purged first.
- **Backup**: dump + volumes + `.env`, gpg AES-256, verified (dump readable, archive decrypts and lists, deleted if
  unreadable), retention (monthly `-monthly.*` 30 days, daily 7 days), optional off-server copy, reminder to keep the
  passphrase off the server.
- **Fleet**: secret-free inventory, rollout order, stop at first failure or `--continue-on-error`, `--plain` supervision
  for CI/cron, backups older than 26 h reported.
- **Monitor** (cron every 5 min): fail threshold, reminder every N hours, recovery message, silent while the lock is held.
- **Security**: secrets never in a command argument (environment or stdin; the single exception is D10); other clients'
  inventory is never copied to a server; two SSH identities per CloudPanel target.

## TUI design

- Lip Gloss theme (accent, success, error, muted), icons `✔ ✖ ◐ ›`, rounded borders.
- Vertical step list: spinner on the current step, duration on the right, command output folded (`ctrl+o` to unfold,
  unfolded by default on failure).
- Prompts: visible default, masked secrets, selectors (targets, versions = sorted git tags), confirmation by typing the
  domain for destructive actions.
- Final summary panel (URL, version, next steps); status bar "target · version · host".
- Clean degradation: no TTY / `NO_COLOR` / `--plain` gives timestamped lines.

## Implementation status

Everything below is implemented and covered by unit tests, by tests against real tools where the sandbox has them (sh,
git, gpg, tar, find, rsync, curl, node) and by the recorded command traces of the Fake transport. The pm2 flow and the
read-only and backup parts of the compose flow were also run against a real server (see
[docs/validation.md](docs/validation.md)). `server update` (a real 1.1.3 to 1.1.4 release of the live pilot, with automatic rollback exercised on a
throwaway instance) and `server install` behind CloudPanel were validated the same evening. **The standalone certbot
ingress has not run on a real host.**

| Phase | State |
|---|---|
| 1 Core | Done: `envfile` (shell parity tests), `manifest` (strict, typed), `tmpl`, `inventory`, `transport` (Local, SSH, Fake), `runner`, plain renderer, `doctor` |
| 2 CloudPanel | Done and validated on a real server: `internal/cloudpanel` (sites, certificates with DNS check, ports, keys, pm2 boot). The vhost question is settled (no directive needed, see `docs/cloudpanel-cli.md`); the PHP web root and opcache after a switch are still open |
| 3 Node + pm2 | Done and validated on a real server: `bootstrap`, `deploy` (clean export, build, assemble, local smoke test, lock, atomic switch, health, automatic rollback, pruning), `rollback`, `status`. Rollback, pruning and hard links confirmed |
| 4 Compose read-only | Done; `status`, `healthcheck`, `fleet status|backups` validated on a real pilot. `release` (image from a tag) used for real releases |
| 5 Compose server | Done: `server update` (lock, disk, backup, migrate, restart, verify, automatic rollback), `install` (standalone and CloudPanel ingress), `backup` (real gpg end to end), `monitor`, `restore-test`, `renew-certs`, `cron`, `crons`. `backup`, `restore-test`, `monitor`, `update` (with rollback) and `install` behind CloudPanel validated on a real host; the standalone ingress and `renew-certs` are not |
| 6 PHP | Not started: the PHP manifest is a hypothesis and there is no reference script (open question 2) |
| 7 Orchestration | Partly: `deploy --version/--rollback/--install/--env` and `fleet deploy` for compose. Not done: the interactive `setup` wizard and the Bubble Tea TUI (the plain renderer is complete) |
| 8 Switch-over | Done for both projects: each one deploys through deploykit (a compose application with a live pilot, a Next.js site under pm2) and its old scripts are archived. Still shell by choice: the database tooling |

Deliberate differences from the shell scripts are listed in the ADRs and in the commit messages: no secret echoed in an
error, `--dry-run` that really changes nothing (including the lock), the deploy lock for pm2 sites, failed releases purged
first, `.status` files instead of mtimes, one `rsync` trailing-slash rule for mirrored folders.

## Roadmap (one phase per session, each shippable and tested)

| Phase | Content | Exit criterion |
|---|---|---|
| 0 | Docs, ADRs, manifests, parity fixtures; Go skeleton, Makefile, CI, GoReleaser | batch A and B of the implementation plan |
| 1 Core | envfile, manifest + JSON Schema, inventory, transport (+fake), runner, plain UI, `dk doctor` (docker compose >= 2.24, rsync, gpg, clpctl, pm2, arch) | unit tests with parity fixtures for `file_get` / `env-set` edge cases; examples validate |
| 2 CloudPanel | `internal/cloudpanel` + stub `clpctl`; spike on a sandbox server for the vhost gaps (Mercure SSE buffering and timeouts, body size, PHP root `current/public`, opcache after a switch) | provisioning trace goldens; spike report in `docs/cloudpanel-cli.md` |
| 3 Node + pm2 | `bootstrap`, `deploy`, `rollback` for the Next.js profile | trace parity with the reference scripts; bootstrap, deploy, rollback on staging; one real deploy |
| 4 Compose read-only | `release`, `status`, `healthcheck`, `fleet status/healthcheck/backups` | results identical to `fleet.sh` on a real client |
| 5 Compose server | binary upload (sha256), `server backup/update/install` (both ingress modes), lock, journal, crons, `monitor`, `restore-test` | staging cycle: install, update, broken image, automatic rollback; backup decrypts with the gpg CLI |
| 6 PHP | php-fpm runtime and PHP profile on a chosen reference project | bootstrap, deploy, rollback on staging |
| 7 Orchestration | `setup` wizard, `deploy --rollback`, `fleet deploy`, TUI renderer with `teatest` goldens | wizard covers install and update |
| 8 Switch-over | The two projects call `deploykit` (Makefile / npm aliases), sh scripts archived, v1.0 | one real production deploy each, preceded by a backup |

## Verification strategy

- Unit: env parse/upsert, target selection, version/tag, retention (monthly case), stale lock, rollback decision,
  manifest validation, release ordering with failed releases.
- Parity: the Fake transport records the commands each flow would run; they are compared with goldens derived from
  the original scripts. `status` / `healthcheck` compared on a real client.
- Integration: staging instances (compose and CloudPanel); a container with sshd, rsync, node, pm2 and a stub `clpctl`
  for CI.
- TUI: `teatest` goldens for key screens; manual checks with a TTY, without a TTY, and with `NO_COLOR`.

## Open questions

1. Sandbox CloudPanel server for the phase 2 spike? Does provisioning log in as root, or through a sudoer?
2. Reference PHP project (Ibexa, Symfony, WordPress, Drupal)? Needed before phase 6.
3. Settled: module `github.com/masilia/deploykit`, public, Apache-2.0, neutral examples.
4. Mercure behind CloudPanel: settled and measured (docs/cloudpanel-cli.md). `clpctl` cannot set vhost directives and
   they are not needed: the stack's own proxy sends `X-Accel-Buffering: no` (in the application's own proxy). Deployed to the
   live pilot with release 1.1.5.

## Remaining work

`fleet status`, `healthcheck` and `deploy` work for pm2 targets too (`fleet backups` is compose-only). SSH retries are done: a connection that cannot be established is retried twice (2 s, then 4 s; `ssh.retries`, `ssh.connect_timeout`), an rsync that was interrupted is retried, and a session that breaks while a script runs is reported as such, never replayed (a script can repeat its effects).

- The standalone (certbot) ingress and `renew-certs` have not run on a real host; neither has a reboot test of the pm2 systemd unit and the crons.
- PHP runtime (needs a reference project), PHP web root and opcache behind CloudPanel.
- The interactive `setup` wizard and the Bubble Tea TUI.
- Restore from a backup (the database tooling stays a separate project).
