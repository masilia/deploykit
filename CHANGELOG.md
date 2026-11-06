# Changelog

All notable changes to deploykit are listed here, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the versions follow [Semantic Versioning](https://semver.org/).
Before 1.0 a minor version may change the manifest schema: it is still marked as a proposal (see `PLAN.md`).

The notes of a release are the entry of its tag in this file (`scripts/release-notes.sh`): a tag without an entry does not release.
Versions 0.1.0 to 0.1.2 were tagged in a private repository whose history was not kept; 0.1.3 is the first public version and contains
all of it.

## [Unreleased]

### Added
- **`deploykit init` for Docker stacks with their Dockerfiles in `docker/`** (the Ibexa profile and any compose project laid out like it): it finds the compose
  files in `docker/` and sets `project_directory`; matches the images a compose file takes from the registry to their Dockerfile, build context and, for a
  multi-stage Dockerfile, the stage to build (`target`); recognises the roles `edge` (Varnish) and `search` (Solr, Elasticsearch, OpenSearch, Meilisearch, Typesense);
  writes the app commands of an Ibexa project (`migrate`, `graphql`, `jwt_keypair`, `reindex`, `cache_clear`) and the `jwt_keypair` install step; turns the
  `CHANGE_ME` placeholders of the env template into generated `secrets` or `prompts`; and syncs only the compose file, the env template and the folders the stack
  mounts from the host. It checks the paths a Dockerfile copies (`COPY conf/site.conf ...`) against the build context it guessed, and takes the other one (the Dockerfile's folder or the repository root) when the files are only there. A Dockerfile that copies the whole context (`COPY .`) says nothing about where its files are: the guess stands and `init` asks you to check it. It also says when the images were matched by name, and which image no Dockerfile matches.
- The generated backup supports MariaDB and MySQL (`mariadb-dump` or `mysqldump`, the password taken from the container's own environment, a dump verified by its
  closing line). The restore test stays PostgreSQL-only and is not generated for them.
- **`bootstrap` for the standalone ingress** (a server with no CloudPanel). It used to refuse; it now checks that Docker and Compose are installed, that nothing listens on port 80 and that the domain points at the server (`ingress.preflight`, which the manifest accepted and nothing read until now), then creates the deployment root for the deploy user. Once the stack is installed, the first-install checks are skipped, so it can be run again.
- **`deploykit run` for the compose runtime**: runs a command of `runtime.app.commands` (`migrate`, `reindex`, `cache_clear`, …) on a target from your workstation, in a one-off container of the app service, or with `--exec` in the running one. Without a name it lists the commands. A command's secrets come from the server `.env` through the environment, never a command line; it refuses while an update, an install or a backup holds the operations lock; `--dry-run` starts nothing. The update hooks are not run.

### Changed
- `init` no longer archives the volumes of the database, the cache and the search engine with the application's files (`data.volumes`): the database has its dump and the
  others can be rebuilt.

### Fixed
- The TLS configuration that the standalone ingress generates inside the deployment root (`ingress.tls.domain_conf.target`) is no longer deleted by the next deploy: a folder is mirrored with `--delete`, and the file the server wrote there was not part of the project, so the next reload of the proxy would have dropped the HTTPS.
- `init` no longer offers `Dockerfile.dockerignore`, the ignore file of a Dockerfile, as a Dockerfile to build.

## [0.5.1] - 2026-11-02

### Fixed
- The release archives now include `LICENSE` (Apache-2.0) and `CHANGELOG.md`; they used to carry only the README.

### Changed
- In the archives, `dk` is a link to `deploykit` instead of a second copy of the binary: each archive is half the size.

## [0.5.0] - 2026-11-02

### Added
- **A Docker stack profile for PHP projects with Redis, Solr and Varnish** (`examples/php-ibexa-docker`, `docs/profile-php-docker.md`):
  the compose runtime behind a CloudPanel reverse-proxy site, with PHP-FPM, nginx, Varnish, MariaDB, Redis and Solr. Validated as a
  throwaway instance on a shared host, HTTPS and cache purge included (`docs/validation.md`, round 5).
- `images[].target` builds one stage of a multi-stage Dockerfile, so one Dockerfile can give several images.
- `runtime.project_directory` keeps the compose files in a subfolder while the server `.env` stays in the deployment root.
- The service role `search` is started with the data services, so Solr is up before the migrations and the reindex.
- **`deploykit env list|get|set`** manages the server `.env` over SSH, with no file manager needed. `list` masks the secrets (declared
  ones, names that look like a secret, URLs with a password); `get` prints one, a secret only with `--reveal`; `set` takes a value from a
  hidden prompt or stdin, or several from a mode-600 file (`--from-file`), never from an argument, and never prints it back. `--apply`
  recreates the services whose environment changed and refuses while an operation holds the lock; `--dry-run` changes nothing.
- **`secrets/<target>.env`**, next to the manifest, is injected by every `deploy` before an explicit `--env` file (which wins). It must
  be readable by you only, `init` adds it to `.gitignore`, and `sync.never` keeps it off the server.
- `install` ends by listing the secrets that only the server holds, with the command that reads one: the encrypted backups contain the
  `.env` that holds their own passphrase.

### Fixed
- `env.from_target_on_deploy` was read and never applied: the loopback port that `bootstrap` picks never reached the server `.env`, so a
  second stack on a host tried the first one's port. The listed keys are now copied from the target (`PROXY_PORT` from `proxy_port`), an
  explicit `--env` value still wins, and the `init` template lists `PROXY_PORT`.

### Changed
- The examples and the `init` template list `secrets/` in `sync.never`, and `install` prints one more reminder.

## [0.4.0] - 2026-11-01

### Added
- **The `php-fpm` runtime**: a PHP project (Symfony, Ibexa) behind a CloudPanel PHP site, deployed as release folders with an atomic
  `current` link. The release is built here, or in a throwaway container (`build.image`, only Docker needed), and sent as a folder.
  See `docs/adr/0009-php-fpm-runtime.md` and `docs/profile-php-cloudpanel.md`.
  - `runtime.shared` (files and folders kept across releases and linked into each), `writable`, `env`, and named `commands` run in the new
    release before the switch (`prepare`) and again after a rollback (`after_switch`).
  - The PHP extensions of `composer.json` are checked on the server and reported, never installed. Health is asked of the site's own
    backend (`127.0.0.1:8080`) with its `Host` header.
  - `bootstrap` derives a vhost template whose first line sets the root directory `current/<web_root>` and that hands PHP-FPM
    `$realpath_root`, so a switch needs no reload (`reload: root-systemctl` is the fallback).
- `deploykit run <command>` runs a named command in the live release; `deploy --also <command>` runs one after a deploy.
- Builds can start in a subfolder (`build.workdir`), run several commands under `set -e`, take variables for the kernel to boot
  (`build.env`) and run in a container (`build.image`).
- `assemble` exclusions can be anchored with a leading `/` (`/tests` is the root's, `tests` is any).
- `init` recognises an Ibexa or Symfony project (also in a subfolder: PHP constraint, Encore, console commands) and warns when it uses
  Redis, Solr or Varnish; `doctor` checks the PHP tools here and on the server.
- `examples/php-ibexa` (replaces the `examples/php-cloudpanel` draft).

### Fixed
Found by running it on a real CloudPanel server (`docs/validation.md`, round 4):
- `vhost-template:add` reads its file as another user: the temporary file is now world-readable.
- CloudPanel creates `current/public` as real folders and a link cannot replace a folder: the first switch sets them aside.
- A template made by `vhost-template:add` has no Varnish settings, so the front served the document root as static files (403): the
  derived template forwards to the site's backend itself.
- An unanchored `tests` exclusion removed `vendor/*/tests`, whose fixtures Ibexa reads at run time.
- An OpenSSH keep-alive timeout (`Connection to … timed out`) is recognised as a lost session and is never replayed.

### Changed
- The release pipeline of the Next.js flow now sits on a `releaseRuntime` interface shared with PHP; its behaviour and tests are unchanged.

## [0.3.0] - 2026-11-01

### Added
- **A styled layout at a terminal**: one line per step (a spinner while it runs, a check mark and its duration after, command output
  folded unless the step fails); `deploy`, `rollback`, `bootstrap` and `release` are framed by a banner and a result line; `validate`,
  `doctor`, `status` and `fleet` show aligned, coloured tables.
- `--plain` is a global flag. Pipes, files, cron, CI, `TERM=dumb` and `--plain` keep the plain timestamped lines.
- `make install` puts `deploykit` and `dk` in `~/.local/bin`.

### Changed
- The palette no longer asks the terminal for its background colour (which some terminals never answer): `DEPLOYKIT_THEME`, then
  `COLORFGBG`, then dark.

## [0.2.0] - 2026-11-01

### Added
- **`deploykit init` and `deploykit target add`**: reads the repository (`package.json`, Dockerfiles, compose files), asks a few questions
  in a full-screen form (line prompts with `--plain` or without a terminal) and writes `deploykit.yaml`, the first target and, for
  compose, `deploy.env.example`. The manifest is assembled from annotated templates by feature (backups, restore test, monitoring, crons,
  quality checks, smoke test); every combination is tested to load, validate and decode. Nothing is overwritten without a choice
  (`.new`, or `.bak` with `--force`), and every answer is also a flag, so a script can pass `--yes`.

## [0.1.4] - 2026-11-01

### Added
- `fleet status`, `healthcheck` and `deploy` for pm2 targets: the live release, its commit and the release to go back to; each server
  fetches its own health path; targets are deployed in rollout order (`--ref`, `--skip-install`, `--skip-checks`).
- `ssh.retries` and `ssh.connect_timeout`. A connection that cannot be established is retried (two retries, 2 s then 4 s); an
  interrupted `rsync`, which is idempotent, is retried too, but not after a permission, authentication or disk error.
- Contributing guide, security policy, issue templates and two more decision records.

### Fixed
- A session that breaks while a script runs is reported as such and never replayed, because a script can repeat its effects.

## [0.1.3] - 2026-11-01

The first public version: one binary, driven by a per-project `deploykit.yaml`, that replaces two families of shell scripts.

### Added
- **The manifest**: three axes, `artifact` (image or release folder), `runtime` (compose or pm2) and `ingress` (CloudPanel or standalone),
  parsed strictly; targets in `targets/<id>.env` with canonical keys and legacy aliases; a restricted template language.
- **Next.js on pm2 behind CloudPanel**: `bootstrap` (site, certificate, folders, key, pm2), `deploy` (clean `git archive` export, build,
  local smoke test, upload, atomic switch, health check, automatic rollback, pruning), `rollback`, `status`.
- **Docker Compose stacks**: `release` (images from a git tag), `deploy` (install or update, run on the server by a same-version
  binary), `update` with a lock, disk check, backup, migrations and automatic rollback, encrypted and verified backups with
  retention, a monthly restore test, a monitor, certificate renewal and a managed crontab block. Behind CloudPanel (a reverse-proxy
  site) it was validated on a real server; the standalone certbot ingress is written but has **not** run on a real host.
- `validate`, `doctor`, `healthcheck` and `fleet status|backups|healthcheck|deploy`.
- `--dry-run` that changes nothing, the lock included; secrets that never reach a command line, a log or an error message.
- Documentation: `PLAN.md`, decision records, a CloudPanel CLI reference and the record of what ran on a real server.

[Unreleased]: https://github.com/masilia/deploykit/compare/v0.5.1...HEAD
[0.5.1]: https://github.com/masilia/deploykit/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/masilia/deploykit/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/masilia/deploykit/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/masilia/deploykit/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/masilia/deploykit/compare/v0.1.4...v0.2.0
[0.1.4]: https://github.com/masilia/deploykit/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/masilia/deploykit/releases/tag/v0.1.3
