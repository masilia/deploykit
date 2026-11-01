# deploykit

One Go binary (`deploykit`, alias `dk`) that builds, ships, runs and supervises applications on servers you manage over
SSH, with [CloudPanel](https://www.cloudpanel.io/)'s `clpctl` doing the site provisioning. A per-project
`deploykit.yaml` describes the application; nothing project-specific lives in the code.

| Target | Artifact | Runtime | Ingress | Status |
|---|---|---|---|---|
| Next.js / Node | release folder (rsync) | pm2 | CloudPanel Node.js site | validated on a real server |
| Docker stack behind CloudPanel | registry image | Docker Compose | CloudPanel reverse-proxy site | validated on a real server |
| Docker stack, standalone server | registry image | Docker Compose | own nginx + certbot | written, **not run on a real host** |
| PHP (Symfony, Ibexa), simple installation | release folder (rsync) | PHP-FPM | CloudPanel PHP site | written, tested with fakes and a real build; **not yet run on a server** |

## Install

Download the archive for your platform from the GitHub releases (linux and macOS, amd64 and arm64), or:

```sh
go install github.com/masilia/deploykit/cmd/deploykit@latest
```

`go install` puts the binary in `~/go/bin`: make sure that folder is in your `PATH`, ahead of any older copy
(`command -v deploykit` shows which one runs). From a checkout, `make install` builds and installs `deploykit` and `dk`
into `~/.local/bin` (`PREFIX=/usr/local make install` for another place).

You need `git`, `ssh` and `rsync` on the workstation, plus `node` for Next.js builds or `docker` for image releases.
`deploykit doctor` checks the workstation and the server. For Docker Compose stacks the server also needs `docker`
(compose 2.24+), `gpg` and `cron`; deploykit uploads its own binary there.

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
| `bootstrap`, `deploy`, `rollback`, `status` | provision a site once; deploy; go back (`rollback` is pm2; for Compose deploy an older `--version`); show the live release or version |
| `release` | build the images from a git tag and push them (`deploy` does it when they are missing) |
| `run <command>` | php-fpm: run a named command of `runtime.commands` in the live release (clear a cache, reindex…); `deploy --also <command>` runs one after a deploy |
| `healthcheck`, `fleet …` | health probes of a target; status, health, backups and ordered deploys across targets |
| `server update\|install\|backup\|monitor\|restore-test\|renew-certs\|cron\|crons` | run on the server by `deploy` and by cron; not meant to be typed by hand |

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

[PLAN.md](PLAN.md) (design, status), [docs/adr/](docs/adr/) (decisions), [docs/cloudpanel-cli.md](docs/cloudpanel-cli.md)
(what `clpctl` can and cannot do), [docs/targets.md](docs/targets.md) (target files),
[docs/validation.md](docs/validation.md) (what was run on a real server, and the bugs it found).

## Licence

Apache License 2.0, see [LICENSE](LICENSE).
