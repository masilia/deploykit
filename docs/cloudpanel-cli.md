# CloudPanel CLI (`clpctl`) reference for deploykit

Sources: [root user commands](https://cloudpanel.io/docs/v2/cloudpanel-cli/root-user-commands) and
[site user commands](https://cloudpanel.io/docs/v2/cloudpanel-cli/site-user-commands). Flags below are those shown in
the documentation; re-check them with `clpctl <command> --help` on the target version before relying on them.

## Who can run what

`clpctl` runs **on the server**, over SSH. There is no CloudPanel API or login involved.

- **Root** (or a sudoer): everything under "Root commands". deploykit uses this identity only to provision.
- **Site user**: four commands only (below). deploykit uses this identity for all deploys.

## Root commands we use

| Need | Command | Flags |
|---|---|---|
| Node.js site | `site:add:nodejs` | `--domainName --nodejsVersion --appPort --siteUser --siteUserPassword` |
| PHP site | `site:add:php` | `--domainName --phpVersion --vhostTemplate --siteUser --siteUserPassword` |
| Reverse proxy site | `site:add:reverse-proxy` | `--domainName --reverseProxyUrl --siteUser --siteUserPassword` |
| Static site | `site:add:static` | `--domainName --siteUser --siteUserPassword` |
| Python site | `site:add:python` | `--domainName --pythonVersion --appPort --siteUser --siteUserPassword` |
| Delete a site | `site:delete` | `--domainName [--force]` |
| Let's Encrypt | `lets-encrypt:install:certificate` | `--domainName [--subjectAlternativeName]` (needs valid DNS) |
| Custom certificate | `site:install:certificate` | `--domainName --privateKey --certificate [--certificateChain]` |
| Create a database | `db:add` | `--domainName --databaseName --databaseUserName --databaseUserPassword` |
| PHP vhost templates | `vhost-templates:list`, `vhost-template:add`, `vhost-template:view`, `vhost-template:delete`, `vhost-templates:import` | `--name`, `--file` (path or URL) |
| Cloudflare IPs | `cloudflare:update:ips` | none (cron already runs it daily) |

Not needed by deploykit: the `user:*` commands (CloudPanel's own users) and `cloudpanel:enable|disable:basic-auth`.
`db:export`, `db:import` and `db:show:master-credentials` exist but belong to the separate database project.

## Site-user commands

| Command | Use |
|---|---|
| `system:permissions:reset` (`--directories=770 --files=660 --path=.`) | fix ownership and modes after an rsync (PHP) |
| `varnish-cache:purge` (`--purge=all\|tags\|url`) | purge after a deploy when Varnish is enabled |
| `db:export`, `db:import` | database project |

## Gaps (nothing documented)

| Gap | Impact | Plan |
|---|---|---|
| No `site:list` / exists command | idempotency | detect by filesystem: site folder, `/etc/nginx/sites-enabled/<domain>.conf` |
| No custom nginx directives per site | a Mercure SSE location, `client_max_body_size` | spike (phase 2): `X-Accel-Buffering: no` from the stack, a vhost edit, or one documented manual step |
| No cron management | the backup, monitor and restore-test crons | managed by deploykit in the site user's crontab (marked block) |
| No pm2 or systemd management | Node runtime | `pm2` as the site user; `pm2 startup` command run as root |
| No web-root option on `site:add:php` | `current/public` after a symlink switch | solved: the first line of a vhost template, `#{"rootDirectory":"current/public","phpVersion":"8.2"}`, sets it; deploykit derives such a template (below) |
| No PHP-FPM reload for a site user | opcache after a switch | solved without a reload: the derived template passes `$realpath_root`; `reload: root-systemctl` is the fallback |
| Passwords only as flags | secrets rule | ADR 0006 |

## Facts from the first real server (see `docs/profile-nextjs-cloudpanel.md` §8)

- `clpctl` is in `/usr/bin`; `pm2` and `node` in `/usr/local/bin`; each site has a system user and its own nvm.
- Nginx vhosts live in `/etc/nginx/sites-enabled/<domain>.conf` and forward to `127.0.0.1:<port>`.
- CloudPanel adds `www.<domain>` to the certificate: without DNS for `www`, Let's Encrypt fails and a self-signed
  certificate is installed meanwhile.

## Custom nginx directives: what was found (CloudPanel 6.0.8, read-only inspection of a real server)

**There is no supported way to set them from the CLI or the API.**

- `clpctl` exposes exactly the command classes listed above. None edits a site's vhost. `vhost-template:add|view|delete`
  and `vhost-templates:list|import` only manage *templates*, and the only command that takes one is `site:add:php
  --vhostTemplate`.
- `site:add:reverse-proxy` (and nodejs, static, python) look up a template with a **hard-coded name** (`ReverseProxy`,
  `Nodejs`, …) in the `vhost_template` table, so a custom template cannot be selected for them. Those rows are system
  templates (`type 1`) that `vhost-templates:import` refreshes every night, so editing one would be overwritten and
  would also change every future site of that type.
- The HTTP API (`api_token` table) has only two routes in the source (`test`, `createUser`), and they are commented out
  in `config/routes.yaml`. No vhost endpoint.

**Where the directives live.** Each site stores its own copy of the template in the column `site.vhost_template` of
`/home/clp/htdocs/app/data/db.sq3` (SQLite, with placeholders such as `{{root}}`, `{{settings}}`,
`{{reverse_proxy_url}}`). The directives the pilot had by hand (`client_max_body_size 12m;` and the
`/.well-known/mercure` location) are plain text in that column.

**How CloudPanel applies a change.** The UI's Vhost tab calls `Site\Updater::updateNginxVhostWithRollback()`: it renders
the column, writes `/etc/nginx/sites-enabled/<domain>.conf` after saving the old file as `.bak`, runs `nginx -t`,
restores the backup and reports `InvalidVhostException` on failure, reloads nginx on success, and records an audit event.
`updateNginxVhost()` (without the rollback) re-renders the file from the same column whenever a certificate is installed
or renewed (`site:install:certificate`, `lets-encrypt:install:certificate`, `lets-encrypt:renew:certificates`).

Consequences:

- **Editing the nginx file by hand is not durable**: it is rewritten from the database column at the next certificate
  operation. Only the column is durable.
- The only programmatic routes are unsupported internals: writing the column (SQLite) and re-rendering through
  CloudPanel's own `Updater` (a PHP script booting its kernel as root; the sources are minified and the class names
  change between releases, and `0 3 * * * clp-update` updates CloudPanel nightly), or writing the column and forcing a
  re-render with `site:install:certificate` using the site's existing certificate files, which has **no rollback** on an
  invalid vhost and can leave the shared nginx unable to reload.

**Do those directives matter at all?** Measured on the same server:

| Directive in the pilot's vhost | Needed behind CloudPanel? |
|---|---|
| `client_max_body_size 12m;` | No. nginx's global value is `64M`, and the project's inner proxy (`docker/nginx/default.conf`) already enforces `12m`. The outer directive is redundant |
| `location ^~ /.well-known/mercure { proxy_buffering off; proxy_read_timeout 3600s; … }` | Only to stop the *outer* nginx from buffering the SSE stream. The inner proxy (`docker/nginx/snippets/mercure.conf`) already does this for its own hop. No `proxy_ignore_headers` exists anywhere in the nginx config, so the outer nginx honours an `X-Accel-Buffering: no` header sent by the upstream |

**Measured (27 October 2026, on that server).** A throwaway reverse-proxy site created with `clpctl` (so with the
default template and no custom directive) in front of a small SSE server that emits one event per second for six
seconds, timed through CloudPanel's real nginx on :443:

| Client protocol | Upstream sends no header | Upstream sends `X-Accel-Buffering: no` |
|---|---|---|
| HTTP/1.1 | **buffered**: the first event arrives at +6.0 s, all six together | streams: +1.0 s, +2.0 s, … |
| HTTP/2 | streams (+1.0 s, +2.0 s, …) | streams |

So the outer buffering is real, but only for HTTP/1.1 clients (which includes any CDN or proxy in front of the origin),
and the header cures it with no vhost edit. A first run with curl's default protocol (HTTP/2) looked unbuffered and was
misleading: always force `--http1.1` when testing streaming.

**Recommended fix, in the project and not in CloudPanel.** The stack's own proxy sends the header from its Mercure
location. In the application used for this measurement it is a `mercure.conf` snippet included by the proxy's `default.conf` and
by its TLS template:

```nginx
location /.well-known/mercure {
    …
    add_header X-Accel-Buffering no always;
    add_header X-Content-Type-Options nosniff always;     # repeated on purpose, see below
    add_header X-Frame-Options SAMEORIGIN always;
    add_header Referrer-Policy strict-origin-when-cross-origin always;
    add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;
}
```

Two nginx rules explain the shape of the change:

- It must be `add_header` in that nginx. A proxy consumes the `X-Accel-*` headers coming from its own upstream (Mercure)
  and does not forward them; a header it adds itself reaches the next hop.
- A `location` that defines any `add_header` stops inheriting the `add_header` lines of its `server`. A single new line
  would therefore silently drop the security headers on the realtime route, so they are repeated. Verified with the real
  `nginx:1.30.5-alpine` image: `nginx -t` passes, the route sends all of them, other routes are unchanged. HSTS is also
  sent on the plain-HTTP server, where browsers ignore it.

**Measured through the real two-hop chain** (throwaway instance of the application behind a CloudPanel reverse-proxy site
with the default vhost; HTTP/1.1 subscriber on :443; three updates published 1 s apart into the hub;
`scripts/sse-check.sh`):

| Stack's `mercure.conf` | Result |
|---|---|
| before the fix | **not received** (held back by CloudPanel's nginx) |
| with the fix | **received 0.0 s after publishing**, all three |

`client_max_body_size` needs nothing: nginx's global value is 64M and the project's inner proxy already enforces 12m.
Writing `site.vhost_template` remains possible but is unsupported (see above) and is not needed for that application.

## PHP sites (CloudPanel 6, read-only inspection of the staging server)

- `clpctl vhost-template:add --name --file`: the file's **first line** is a JSON comment, `#{"rootDirectory":"public","phpVersion":"8.4"}`, that
  prefills the site wizard and sets the root: `/home/<user>/htdocs/<domain>/<rootDirectory>`. `site:add:php` itself has no root option,
  so a custom template is the way to a root of `current/public`. `clpctl vhost-template:view --name "Symfony 7"` prints the body of a
  system template (without that first line), which deploykit reads and rewrites (`cloudpanel.PHPTemplate`).
- The vhost of a PHP site is nginx :443 (with `{{varnish_proxy_pass}}`) → a second nginx server on `127.0.0.1:8080` (matched by server_name)
  → PHP-FPM on a per-site TCP port (`{{php_fpm_port}}`). `SCRIPT_FILENAME` is `$document_root$fastcgi_script_name`; with a symlinked
  release that keeps the old real path in opcache. The derived template uses `$realpath_root` and adds `DOCUMENT_ROOT`, so the path given
  to PHP-FPM is the release itself.
- Every PHP version CloudPanel offers is installed side by side (`php8.2`, `/usr/bin/php8.2`, service `php8.2-fpm`); a site user's plain `php` is the
  system one (8.4), so remote commands name the versioned binary.
