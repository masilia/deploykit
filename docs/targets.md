# Targets (inventory) and global configuration

A **target** is one deployment destination: a file `targets/<id>.env` of plain `KEY=VALUE` lines, versioned, with
**no secret**. `<id>` matches `^[A-Za-z0-9._-]+$`. Files are parsed without evaluation (a value is never executed).
Editor-side secrets (registry credentials, tokens) live in a separate global file (`deploy.env`, git-ignored, mode 600).

Lookup order for any key: the selected target first, then the global file (the legacy `cfg_get`).

## Canonical keys and accepted aliases

Manifests refer to `target.<name>`. Existing files of the legacy scripts work unchanged through the aliases.

| `target.` | Accepted env keys | Meaning |
|---|---|---|
| `host` | `HOST`, `DEPLOY_HOST` | server address |
| `ssh_port` | `SSH_PORT`, `DEPLOY_PORT`, `DEPLOY_SSH_PORT` | default 22 |
| `ssh_user` | `SSH_USER`, `DEPLOY_USER` | deploy login; defaults to `site_user` on CloudPanel targets |
| `root_user` | `ROOT_USER`, `DEPLOY_ROOT_USER` | provisioning login (`clpctl` needs root or a sudoer) |
| `site_user` | `SITE_USER` | CloudPanel site system user, owner of the files |
| `path` | `PATH_ON_SERVER`, `DEPLOY_PATH` | remote root of the deployment or of the site |
| `domain` | `DOMAIN`, `DEPLOY_DOMAIN` | public domain |
| `site_url` | `SITE_URL` | public URL baked into a build (Next.js) |
| `app_port` | `APP_PORT` | internal port nginx forwards to (pm2 runtime) |
| `proxy_port` | `PROXY_PORT` | loopback port of the compose proxy behind CloudPanel |
| `tls_mode` | `TLS_MODE` | `certbot` or `cloudpanel` (selects the compose ingress variant) |
| `hosting` | `HOSTING` | `vps` (dedicated) or `shared` (multi-site) |
| `contract` | `CONTRAT`, `CONTRACT` | `supervised` or `ondemand` |
| `rollout_order` | `ROLLOUT_ORDER` | fleet deploy order, ascending, default 50 |
| `project` | `COMPOSE_PROJECT`, `PROJECT` | compose project name of this target, overriding the manifest's: set it when several instances share a host |

Unknown keys are kept and available as `target.<lowercased key>`.

## Creating targets

`deploykit init` writes the first target and `deploykit target add` each further one, asking only about the server (id, host,
SSH, domain, site user, path, loopback port, and the TLS mode when the manifest lets each target choose). Every answer is
also a flag (`--id`, `--host`, `--ssh-port`, `--ssh-user`, `--domain`, `--site-user`, `--path`, `--app-port`, `--proxy-port`,
`--tls-mode`), so a script can pass `--yes` and no question is asked. An existing id is refused. Nothing is contacted: the
loopback port is only a starting point, `bootstrap` takes the next free one if it is taken.

## Rewritten by the tool

`app_port` / `proxy_port` are written back to the target file when the requested port is taken and the next free one is
chosen. The write is an idempotent upsert of that single key; nothing else in the file is touched.

## Inventory rule

The inventory of other clients is never copied to a server (rule of the legacy scripts, kept).

## Compose targets

- `TLS_MODE` selects the ingress variant of a manifest that uses `select` (`certbot` or `cloudpanel`). When it is
  absent the manifest's default applies (`certbot`).
- `REGISTRY` (and `GHCR_USER`, `GHCR_TOKEN` for a private registry) are read from the target first, then from the global
  configuration file named by `global_config`. Put the token in the global file, never in a target.
- `deploy` uploads the target's own file to the server as `.deploykit/target.env`, so the server knows its settings; the
  other targets of the inventory never leave the workstation.

## Secrets and the server `.env` (compose targets)

A target file holds no secret. The values a server needs (passwords, tokens, hosts, recipients) live in the server `.env`, mode 600,
owned by the deploy user. Three ways to manage them, none of which needs a file manager:

- **`secrets/<target>.env`**, next to the manifest, git-ignored (`init` adds it to `.gitignore`; the manifest's `sync.never` keeps it
  off the server) and mode 600 (like an SSH key, a file readable by others is refused). `deploy` injects every `KEY=VALUE` line on
  every run, before an explicit `--env` file, which wins.
- **`deploykit env set -t <target> KEY`** reads the value from a hidden prompt or from stdin; `--from-file` takes several. The value
  reaches the server on stdin, never in an argument, and is never printed back. It takes effect when the services are recreated:
  `--apply` runs `docker compose up -d` at once (it refuses while an operation holds the lock), or the next deploy does.
  `deploy` and `rollback` own `APP_VERSION` and `APP_VERSION_PREV`: `env set` refuses them.
- **`deploykit env list -t <target>`** shows every variable, the secrets masked (a declared secret, a name that looks like one, or a URL
  with a password in it); **`env get KEY --reveal`** prints one secret, for an off-server copy.

The server holds the only copy of the secrets that deploykit generates, and the encrypted backups contain the `.env` that holds their
own passphrase: keep a copy of those secrets elsewhere (the end of `install` lists them).
