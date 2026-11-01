# ADR 0004: CloudPanel as a first-class provisioner through clpctl

Status: accepted

## Context
The goal is to get the most out of CloudPanel. Its CLI, `clpctl`, runs on the server and requires root for site
creation, certificates and databases; only four commands are available to a site user (`system:permissions:reset`,
`db:export`, `db:import`, `varnish-cache:purge`). There is no documented `site:list`, cron, pm2 or custom-nginx-directive
command. See `docs/cloudpanel-cli.md`.

## Decision
`internal/cloudpanel` wraps `clpctl` and is used by every CloudPanel ingress:

- Create sites with `site:add:{nodejs,php,reverse-proxy,static,python}`; detect an existing site by filesystem
  (`test -d <path>`, nginx vhost file), never by running `clpctl` again.
- Issue certificates with `lets-encrypt:install:certificate`, passing `--subjectAlternativeName=www.<domain>` only if the
  `www` DNS record resolves; failure is a warning, the site stays served with a self-signed certificate.
- Discover a free application port (listening sockets plus ports declared in nginx vhosts) and write it back to the target.
- Use `vhost-template:add` to ship per-framework PHP vhost templates.
- There is **no CloudPanel API or web login**. Everything is SSH. Each target has two identities: **root** (or a sudoer
  through `sudo -n`) for provisioning only, and the **site user** for every deploy, authenticated by the operator's SSH
  key that bootstrap installs in the site user's `authorized_keys`.

## Consequences
- Provisioning is idempotent and testable against a stub `clpctl`.
- Gaps (Mercure SSE vhost directives, PHP web root, opcache after a switch) need a spike on a sandbox CloudPanel server
  before they are designed (roadmap phase 2).
- Never run `pm2 list`, `pm2 kill` or `pm2 save` as root (see `docs/profile-nextjs-cloudpanel.md`, pitfall 7).
