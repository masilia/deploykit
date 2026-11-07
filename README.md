# deploykit

One Go binary (`deploykit`, alias `dk`) that builds, ships, runs and supervises applications on servers you manage over
SSH, with [CloudPanel](https://www.cloudpanel.io/)'s `clpctl` doing the site provisioning. A per-project
`deploykit.yaml` describes the application; nothing project-specific lives in the code.

| Target | Artifact | Runtime | Ingress | Status |
|---|---|---|---|---|
| Next.js / Node | release folder (rsync) | pm2 | CloudPanel Node.js site | validated on a real server |
| Docker stack behind CloudPanel | registry image | Docker Compose | CloudPanel reverse-proxy site | validated on a real server |
| Docker stack, standalone server | registry image | Docker Compose | own nginx + certbot (Let's Encrypt) | validated on a fresh VPS: bootstrap, install, update, renewal |
| PHP (Symfony, Ibexa), simple installation | release folder (rsync) | PHP-FPM | CloudPanel PHP site | validated on a throwaway site of a real server |
| PHP (Ibexa) with Redis, Solr, Varnish | registry images (multi-stage Dockerfile) | Docker Compose | CloudPanel reverse-proxy site | validated as a throwaway instance on a real server, HTTPS included |

## Install

Download the archive for your platform from the GitHub releases (linux and macOS, amd64 and arm64), or:

```sh
go install github.com/masilia/deploykit/cmd/deploykit@latest
```

`go install` puts the binary in `~/go/bin`: make sure that folder is in your `PATH`, ahead of any older copy
(`command -v deploykit` shows which one runs). From a checkout, `make install` builds and installs `deploykit` and `dk`
into `~/.local/bin` (`PREFIX=/usr/local make install` for another place).

### Updating

`deploykit self-update` replaces the binary by the latest release: it downloads the archive of your platform from the public GitHub releases, checks it against
the release's `checksums.txt`, makes sure the new binary runs, and only then swaps it in (the old one stays next to it as `deploykit.old`, and `dk` follows since it is a
link). `--check` only says whether a newer release exists, `--version X.Y.Z` picks one, `--pre` also offers release candidates, and `--dry-run` shows the plan. A build
made from a checkout (`make build`) is left alone unless you pass `--force`. The servers keep the binary of their last deploy: it follows at the next `deploykit deploy`.
`self-update` only handles releases: a checksum shows that the download is intact, not who published it.

**The daily look.** Once a day, at a terminal, deploykit asks `api.github.com` which release is the latest (one request, nothing about you or your projects, in the
background so that no command waits for it) and says on stderr when a newer one exists. It stays silent in a pipe, a script, CI and `--plain`, for a build from a checkout, and
for the commands cron and the shell completion run. `DEPLOYKIT_NO_UPDATE_CHECK=1` turns it off; the answer is cached in `~/.cache/deploykit/update.json`.

You need `git`, `ssh` and `rsync` on the workstation, plus `node` for Next.js builds or `docker` for image releases.
`deploykit doctor` checks the workstation and the server. For Docker Compose stacks the server also needs `docker`
(compose 2.24+), `gpg` and `cron`; deploykit uploads its own binary there, with every `deploy`. To change only that binary (after `self-update`, say), use
`deploykit deploy --binary-only -t <target>`, or `deploykit fleet upgrade --all`: it checks that the binary can run there, sends it, reads back its version and leaves the stack alone.

## Quick start

```sh
deploykit init                         # reads the repository, asks a few questions, writes deploykit.yaml + the first target
deploykit target add                   # one more server later, same questions
deploykit validate                     # check deploykit.yaml and the targets
deploykit doctor -t acme               # tools, here and on the server

# Next.js on CloudPanel (pm2)
deploykit bootstrap -t acme --pubkey ~/.ssh/id_ed25519.pub   # once: port, site, certificate, folders, key, pm2
deploykit deploy -t acme               # build, smoke-test locally, upload, switch, health check, roll back on failure
deploykit rollback -t acme             # back to the previous release that did not fail

# Docker Compose stack
deploykit bootstrap -t acme            # behind CloudPanel: reverse-proxy site and deployment root
deploykit deploy --install --version 1.0.0 -t acme --env first-run.env
deploykit deploy --version 1.1.0 -t acme           # backup, pull, migrate, restart, verify, roll back on failure
deploykit deploy --version 1.0.0 -t acme           # an older version is a deliberate rollback
deploykit fleet status --all           # version, previous version and backup age of every target
deploykit fleet deploy --all            # pm2 sites: build and deploy each target in rollout order (compose: --version X.Y.Z)
```

Every command takes `--dry-run`: read-only checks run, nothing is changed.

**Choosing the target.** `-t/--target` names the target a command works on. With a single target in the inventory it is implied. With several and no `--target`,
a command run at a terminal asks which one (a list of the targets with their domain and host; type to filter, Enter to choose, Ctrl-C to leave), says
what to type next time, then goes on. `fleet status`, `fleet healthcheck`, `fleet backups` and `fleet upgrade`, which work on several targets, ask for any number of them (Space marks, Enter validates) when you give neither
`--all` nor `--target`; `fleet deploy` never does, since a deploy of several servers is never implicit. A script, a pipe and CI never get a question: they fail with the list of targets, as before.

`deploykit init` recognises a Next.js project (pm2) or a Docker Compose stack, proposes the project name, the services and
the Dockerfiles it found, and lets you choose the sections you want (backups, restore test, monitoring, crons, quality
checks, smoke test). It never overwrites a file without asking. Without a terminal, give the answers as flags:
`deploykit init --yes --host srv.example.com --domain www.example.com`. `--plain` replaces the full-screen form by line
prompts. The generated files are documented by the examples in [`examples/`](examples/): `nextjs-cloudpanel`,
`compose-app` (every section), `php-ibexa` (an Ibexa 4.6 site: legacy search, files, no Redis/Solr/Varnish).

## What it looks like

At a terminal, deploykit draws each flow as a short list of steps (a spinner while one runs, a check mark and its duration
when it ends, the command output folded away unless a step fails), and shows `validate`, `doctor`, `status` and `fleet`
as aligned, coloured tables. Pipes, files, cron, CI, `TERM=dumb` and `--plain` keep the timestamped plain lines. The
palette follows `DEPLOYKIT_THEME=light|dark` (then `COLORFGBG`, then dark). If a terminal never answers the colour
query that the terminal library sends at start-up, `TERM=dumb` skips it.

## Commands

| | |
|---|---|
| `init`, `target add` | create `deploykit.yaml` and the first target; add another server |
| `validate`, `doctor` | check the manifest and the targets; check the tools needed here and on the server |
| `deploy --binary-only`, `fleet upgrade` | compose: put this deploykit on the server of one target, or of several, and nothing else: no files, no `.env`, no update, no restart (`deploy` sends it too, together with everything else) |
| `bootstrap`, `deploy`, `rollback`, `status` | provision a site once; deploy; go back (`rollback` is pm2; for Compose deploy an older `--version`); show the live release or version |
| `release` | build the images from a git tag and push them (`deploy` does it when they are missing) |
| `run [command]` | run a named command of the manifest: php-fpm, in the live release (`runtime.commands`; `deploy --also <command>` runs one after a deploy); compose, in a one-off container of the app service (`runtime.app.commands`, `--exec` for the running one). Without a name, compose lists the commands |
| `env list\|get\|set` | compose: manage the server `.env` over SSH: list it with the secrets masked, read one variable (a secret needs `--reveal`), set one or several without a full deploy (`--apply` recreates the services) |
| `self-update` | replace this deploykit by the latest release (`--check`, `--version`, `--pre`, `--force`) |
| `healthcheck`, `fleet …` | health probes of a target; status, health, backups and ordered deploys across targets |
| `server update\|install\|backup\|monitor\|restore-test\|renew-certs\|cron\|crons` | run on the server by `deploy` and by cron. Typed on a workstation, in a project, they run on a target over SSH (`--target`, or a question at a terminal), with the deploykit that `deploy` uploaded; `update` and `install` stay with `deploy` |

## What it guarantees

- **Rollback:** a failed health check puts the previous release (pm2) or version (Compose) back and says so. The database
  schema is not undone, so migrations must stay compatible with the previous version.
- **Backups** are encrypted (gpg AES-256), verified before they are kept, and a monthly restore test proves they can be
  restored into a throwaway database.
- **Secrets** never appear on a command line (environment or stdin), in logs, or in `--dry-run` output. The one exception
  is the throwaway password `clpctl site:add` insists on taking as an argument.
- **One deploy at a time** per site (a lock), and every compose command names its project explicitly, so two instances on
  one host cannot act on each other.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md); security reports go through [SECURITY.md](SECURITY.md).

## Documentation

[CHANGELOG.md](CHANGELOG.md) (what changed in each version), [PLAN.md](PLAN.md) (design, status), [docs/adr/](docs/adr/) (decisions), [docs/cloudpanel-cli.md](docs/cloudpanel-cli.md)
(what `clpctl` can and cannot do), [docs/targets.md](docs/targets.md) (target files),
[docs/validation.md](docs/validation.md) (what was run on a real server, and the bugs it found).

## Licence

Apache License 2.0, see [LICENSE](LICENSE).
