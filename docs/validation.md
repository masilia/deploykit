# Validation on a real CloudPanel server

deploykit was run against a real, shared CloudPanel host (Ubuntu 24.04, 2 CPU, ~8 GB) that also served about ten live
sites, among them a Docker Compose application with a real database (the "pilot", deployed in `/opt/app`) and a Next.js
site under pm2. Every round was agreed beforehand, used throwaway instances with their own path, compose project, proxy
port and domain, and ended with the server diffed against a snapshot taken first (vhosts, home directories, listening
ports, pm2 units, containers, volumes). No existing site was touched.

What is **not** covered: the standalone certbot ingress (its proxy binds ports 80/443, which CloudPanel's nginx owns on
that host, and certbot needs a public domain), the PHP runtime, and a reboot test of the pm2 systemd unit and the crons.

## Round 1: pm2 and read-only compose flows

| Flow | Result |
|---|---|
| `fleet status`, `fleet backups`, `healthcheck`, `doctor` on the pilot | Version, previous version and backup age match the server; all services and probes pass; the compose override and the `!override` syntax work with Docker Compose 5.3 |
| `bootstrap` of a new CloudPanel Node.js site | Picked a free port, created site, user, folders, key, pm2 and its systemd unit; the certificate step warned and continued because the subdomain had no DNS. A second run changed nothing |
| `deploy` v1, v2, v2 again | Clean export, local smoke test, lock, upload, atomic switch, health check, status files |
| `deploy` of a release that passes the local smoke test but fails the server health check | Detected (503), `current` put back on the previous release, pm2 reloaded, logs shown, lock released, release marked `failed` |
| `rollback` | Skipped the failed release, chose the previous healthy one, marked the abandoned release `failed` |
| `deploy` v4 | Failed releases pruned first, `keep=3` respected |
| `server backup` on the pilot | 2 s; archive holds the database dump, the documents tarball and `.env` exactly like the legacy backup of the same morning, sha256 OK, decrypts with the passphrase; retention removed the right pair, as `find -mtime +7` does |
| `server restore-test` on the pilot | Same table count as production, all migrations, throwaway database dropped |
| `server monitor --dry-run` | Same diagnosis as the legacy cron at the same minute |

Bugs that only the real server could show (all fixed, with regression tests):

1. **Releases never shared files.** A `git archive` export stamps every file with the commit time, so rsync saw every
   file as changed and `--link-dest` saved nothing. Fix: constant mtimes in the release plus `rsync --checksum`.
2. **Off-site copy with no target configured.** The unrendered template `{{ env.BACKUP_RSYNC_TARGET }}` was passed to
   rsync, which created a local directory with that name. Fix: an undefined optional value now means "nothing to do".
3. `doctor` reported "curl not found" for a site user that does not exist before the bootstrap. It now says the user
   cannot log in yet.
4. A pm2 banner flooded the bootstrap output.

Practical findings: the server accepted an RSA key but not ed25519 (pass `--pubkey` to `bootstrap`; check with
`ssh -v … | grep "Server accepts key"`); a new subdomain without wildcard DNS gets no certificate; memory was tight, so
test apps must stay small.

## Round 2: release, live update and a second instance

A real new release of the application was built from a git tag, pushed to the registry and deployed to the live pilot;
then a second instance was installed behind CloudPanel (tested through the loopback, no DNS) and removed.

| Flow | Result |
|---|---|
| `release --version` | Both images built from the tag export (uncommitted work ignored) and pushed in 3 minutes |
| `deploy --version` to the live pilot (`server update`) | 2 minutes: new backup, pull, additive migrations, a new application secret generated before the restart, restart, health probes and ORM mapping check, secret re-encryption after verification, old images cleaned. All containers healthy, lock released, journal written |
| `bootstrap` of a CloudPanel reverse-proxy site | Free loopback port picked, site and user created, deployment root created, certificate skipped (no DNS) |
| `deploy --install` of the second instance (own compose project) | 1.5 minutes. Own containers, volumes, database and port; all secrets generated; the whole chain CloudPanel nginx :443 → proxy → app answers 200. The pilot was not affected |
| Automatic rollback with real containers (failure injected on the throwaway instance) | A "new" version that failed verification was detected, the stack went back to the previous one, was re-verified; the journal shows `KO` then `ROLLBACK` |
| Teardown | Cron block, stack with volumes and networks, CloudPanel site, user, vhost, certificates and directory removed; the server is identical to the snapshot |

Findings that only a real host could produce (all fixed, with regression tests):

1. **Scripts lost the lines after a command that forwards stdin.** Scripts travel on the stdin of the remote shell, and
   `docker compose exec -T` forwards that same stdin to the container, swallowing the rest of the script. Scripts are
   now read completely before they run and run with an empty stdin.
2. **A second instance would have managed the first.** The compose file sets a fixed project `name:`; commands did not
   pass `-p`. Every compose command now names its project explicitly, and a target can set its own (`COMPOSE_PROJECT`).
3. **Cron started the monitor with no PATH.** A Go program, unlike a shell, has no default search path, so the monitor
   cron failed with `cat: executable file not found`. The binary now adds the standard directories to PATH at start-up.
4. A first install recorded the template's placeholder version as the "previous" one, so an automatic rollback would
   have aimed at images that do not exist.
5. The project's own update script generated a secret and re-encrypted stored secrets after the health check; the
   manifest now does the same (`update.after` runs any declared app command once the update is verified).
6. Health probes behind CloudPanel go to the loopback proxy with the public domain as the Host header, so they need
   neither DNS nor a certificate.

Consequences when taking over a live host: the legacy cron lines are replaced by one deploykit block (marked
`# BEGIN/END deploykit <root>`); the legacy scripts are no longer scheduled; the monitor's problem signature is not the
shell's `cksum`, so an existing warning is announced once more as new; a newly generated application secret now exists in
`.env` and in every backup, so keep a copy off the server.

## Round 3: vhost directives behind CloudPanel

Question: can custom nginx directives (a body-size limit, a server-sent-events location) be set from the CLI? Findings,
from read-only inspection of CloudPanel 6.0.8 and one measurement (details in `docs/cloudpanel-cli.md`):

- **No.** No `clpctl` command and no API route edits a vhost; templates are looked up by a hard-coded name and refreshed
  nightly. The text lives in `site.vhost_template` (SQLite), is re-rendered from there at every certificate operation, and
  the UI applies it with `nginx -t` and automatic restore. Writing it from outside is unsupported and was not done.
- **They are not needed.** The body-size directive is redundant (global 64M, the inner proxy enforces its own limit).
- **Outer SSE buffering is real, for HTTP/1.1 clients only**, and an upstream `X-Accel-Buffering: no` cures it. Measured
  through a throwaway reverse-proxy site: HTTP/1.1 without the header delivered six one-second events together at +6.0 s,
  with the header at +1.0 s, +2.0 s, …; HTTP/2 streamed in both cases. A first run with curl's default (HTTP/2) wrongly
  looked fine: always force `--http1.1`.
- The project's inner proxy now sends `add_header X-Accel-Buffering no always;` on its SSE route, with the security
  headers of the `server` repeated (an `add_header` in a `location` cancels their inheritance).
- Measured on the real two-hop chain with `scripts/sse-check.sh`: **before the fix no event was received; with the fix
  each arrived 0.0 s after publishing.** After the operator removed the hand-added directives from the pilot's vhost (so
  it equals the default reverse-proxy template), the check still showed 0.0 s and all services stayed healthy.
- Building the publisher token took longer than the measurement: Mercure 1.0 wants `typ: at+jwt`, `iss`, `aud` (the hub
  URL as it sees the request), `exp` and an RFC 9396 `authorization_details` grant, and subscribers use `match=`, not
  `topic=`. `scripts/sse-check.sh` encodes all of it.

## Round 4: the php-fpm runtime on a throwaway CloudPanel PHP site

A snapshot of the server was taken first (vhosts, home folders, users, listening ports, PHP-FPM pools, CloudPanel sites and templates, MySQL
databases and users, certificates, containers, crontabs); afterwards the server was **identical** to it.

| Flow | Result |
|---|---|
| `bootstrap` | derived a vhost template from "Symfony 7" (first line `#{"rootDirectory":"current/public","phpVersion":"8.2"}`, `$realpath_root`, an explicit `proxy_pass` to the site's backend), created the site with it (CloudPanel stored the root directory `…/current/public`), a PHP 8.2 FPM pool, `releases/` and `shared/`; the certificate step warned (no DNS) |
| `deploy` of a small PHP app, four versions | upload, shared links, `prepare` commands in the new release, atomic switch, `after_switch`, pruning to three releases; the extension check against `composer.json` passed |
| the switch with **no PHP-FPM reload** | the very next request to the site's backend was served by the new release (its real path in the response): `$realpath_root` works |
| a version whose home page answers 503 | detected, switched back to the previous release, `after_switch` run again, the release marked failed |
| a failing `prepare` command (a migration) | the deploy stopped before the switch: the live release was untouched |
| `run`, `rollback`, `rollback --to`, `status`, `fleet status`, `fleet healthcheck`, `doctor` | all behaved as for the other runtimes |
| the **real Ibexa 4.6 project** | built in a throwaway container (composer, yarn, Encore, 1 min 40 s), 422 MB uploaded in 18 to 27 s, `cache:warmup` on the server, switched, served through nginx and PHP-FPM (the home page redirects to the login page); a 500 before the project's own environment variables were set was detected and rolled back by the health check |

Findings that only the real server could show (all fixed, with tests):

1. `clpctl vhost-template:add --file` is read by another user than root: a mode 0600 temporary file fails with "Permission denied".
2. CloudPanel creates `current/public/index.php` as **real folders** when the root directory is `current/public`, and a link cannot replace a
   folder (`mv -T`): the first switch now sets such a folder aside (`current.replaced.<time>`), never deletes it.
3. A template made with `vhost-template:add` has no Varnish settings, so CloudPanel renders `{{varnish_proxy_pass}}` as nothing and the HTTPS front
   served the document root as static files (403 "directory index is forbidden"). The derived template forwards to the backend itself.
4. An unanchored exclusion `tests` also removed `vendor/*/tests`, whose fixtures Ibexa reads at run time. A leading `/` now anchors an exclusion at
   the root of the copied folder (`/tests`).
5. Building from a clean export needs the variables the kernel reads without a default (`build.env`): one was only a comment in the project's `.env`.
6. The workstation's own PHP lacked the extensions of the project: `build.image` runs the build commands in a throwaway container instead.
7. A session ended by unanswered keep-alives prints `Connection to <host> port <n> timed out`; it is now recognised as a lost session, never replayed.

Not covered: Redis, Solr and Varnish (profile B, the Docker stack), a database dump of a real site (the test used a clean install), and the
live site's cutover.

## Round 5: the Ibexa Docker stack (Redis, Solr, Varnish) as a throwaway instance on a shared host

An Ibexa 4.6 project (`examples/php-ibexa-docker`) deployed with the compose runtime behind a CloudPanel reverse-proxy site, on the host that also
runs live sites, as its own instance: own path, own compose project, own loopback port, own domain. A clean `ibexa:install` provided the data. The
domain had no DNS record at first (the probes ran through the loopback port with the site's `Host` header); the record was added later and the
certificate issued, so the last rows were measured through HTTPS.

| Flow | Result |
|---|---|
| `release` (four images, two stages of one multi-stage Dockerfile) | Built from the tag export and pushed: PHP image 1 min 39 s, the three others a few seconds, push 3 min |
| `bootstrap` | Reverse-proxy site and its user created, next free loopback port picked, deployment root created; the certificate step reported the missing DNS and the run went on |
| `deploy --install` | Six services healthy: MariaDB, Redis, Solr, PHP-FPM, nginx, Varnish. Memory kept small with the `.env` sizing keys |
| `ibexa:install` through `docker compose run` | Schema, clean data and Solr re-indexing; `/` answers 302 and `/login` 200 through Varnish |
| `healthcheck`, `status` | Eight probes pass |
| `deploy --version` (update) | Backup made and verified, pull, migration, recreation, `nginx -s reload`, verification: 1 min 17 s. Varnish restarted after nginx (`depends_on … restart: true`) |
| Deploying the older version (rollback) | 1 min 30 s; the journal shows both versions; the monitor cron wrote its state |
| Update whose migration fails | The project's migrations need data a clean install lacks. The update stopped before any restart: backup kept, lock released, the running version still served |
| HTTPS through CloudPanel | Let's Encrypt certificate (verified chain), `/` redirects to `https://<domain>/login` (the scheme survives the proxies), HTTP redirects to HTTPS, Varnish is in the circuit |
| Cache and purge on that chain | A page is a MISS, then a HIT; a hard purge with the right token answers 200 and the next request is a MISS again; a wrong token, no token, and a plain `PURGE` from the internet through CloudPanel all answer 405 |
| `env list`, `get`, `set --apply` | The listing masks the five secrets (length only); a secret needs `--reveal`; a managed key is refused; changing one variable with `--apply` recreated only the service that uses it (the others kept their uptime) and the site kept answering; the value was restored the same way |
| `run` (compose) | The commands of the manifest are listed; `cache_clear` and `reindex` ran in a one-off container and `--exec` in the running one (no container left behind); a failing migration came back with exit code 1 and its message; with the operations lock held it refused and started nothing, and it ran again once the lock was gone; `--dry-run` started nothing |
| Before/after comparison with the host | Only the instance's own resources differ: vhost, user, port, CloudPanel site and certificate, containers, volumes, network, cron block, files |

Findings (the first is fixed, with a regression test):

1. **`PROXY_PORT` never reached the server `.env`.** `bootstrap` picks a free loopback port and writes it to the target, but the server `.env` kept the
   template's value, which was the live pilot's port: Varnish could not bind. `env.from_target_on_deploy` was parsed and never applied; it now copies
   each listed key from the target (`PROXY_PORT` → `proxy_port`), and an explicit `--env` value still wins.
2. **A migration that fails leaves `.env` ahead of the containers.** `APP_VERSION` already names the new version while the old one runs. The message says so
   ("manual rollback: set APP_VERSION=… and run the update again"); the next `up` of any service would use the new tag.
3. **Running the migration command recreates the dependencies whose image tag changed** (here Solr, whose image carries the release tag). Its volume survives,
   but the service restarts before the update has passed its own checks.
4. The install flow cannot load a database: it ends before the probes and tells the operator to load one (a dump, or `ibexa:install`) and then run an update.
5. **The DNS check of `bootstrap` uses the workstation's resolver.** A home router that had cached the "no such name" answer kept the certificate step
   failing for the better part of an hour while the authoritative servers, public resolvers and the server itself already answered. The certificate was
   issued on the server with the very command `bootstrap` would have run. Checking from the server, or retrying, would avoid the wait.
6. **Test hosts matter to Ibexa.** With every host variable set to the same name, the host matcher picked an administration siteaccess, where the
   anonymous user may not log in, and every Symfony route (the invalidation-token route included) redirected to the login page. With distinct hosts the
   public site served and the purge worked. The clean data also limits the anonymous `user/login` policy to the stock siteaccess: a real database
   already carries the project's.

Not covered: the hidden prompt of `env set` at a terminal (the stdin path was exercised), the project's own migrations on a real database, the automatic rollback of a version that fails its probes (covered by
round 2 for another stack), and anything about the host's other sites beyond the before/after comparison.

## Round 6: the standalone ingress on a fresh server, with a real database

The Ibexa Docker stack on a **fresh Debian 13 VPS with no CloudPanel** (3.9 GB of RAM, no swap), reached through a domain of its own: the stack's
nginx container owns ports 80 and 443, asks Let's Encrypt for the certificate and forwards to Varnish. The server was prepared from root (Docker from the
vendor's repository with its key fingerprint checked, `gnupg`, `rsync`, a deploy user of the docker group); firewall and SSH were not touched.

| Flow | Result |
|---|---|
| `bootstrap` (standalone) | Docker and Compose found, port 80 free, the domain resolves to the server, deployment root created for the deploy user |
| `deploy --install` | 2 min 35 s: seven services healthy (nginx proxy, Varnish, nginx, PHP-FPM, MariaDB, Redis, Solr), Let's Encrypt certificate issued, the domain's TLS configuration written and the proxy reloaded, three crons (backup, monitor, certificate renewal) |
| HTTPS from outside | Verified chain, HTTP redirects to HTTPS (301), Varnish in the circuit and serving from its cache |
| Real database | A dump streamed into the database container (65 tables, 3 s), the uploaded files copied into the volume as the application's user (156 MB, 1 165 files, 114 s), the content roots of the real data set in the server `.env` |
| The project's migrations on that data | See finding 3: the first run failed for a reason that was not the stack's, then two migrations were executed; Solr re-indexed (232 items), GraphQL schema generated |
| `deploy` (update) of the same version | 1 min 01 s: new backup of the real data, the generated TLS file still there (same checksum), no container touched, HTTPS still answered |
| `server renew-certs` | certbot reported the certificate not yet due, the proxy was reloaded, the site kept answering (an actual renewal was not exercised) |
| `healthcheck` | Seven services and two page probes through the public URL |

Findings:

1. **`bootstrap` refused any target that was not behind CloudPanel**, and the preflight options of the standalone ingress (`port_80_free`, `dns_matches_server`) were parsed
   and read by nothing. `bootstrap` now checks Docker, port 80 and the DNS, then creates the deployment root.
2. **The generated TLS file would have been deleted by the next deploy.** A folder is synced with `--delete` and the file the install writes into it is not in the project:
   the next reload of the proxy would have dropped the HTTPS. It is now excluded from the mirroring. Checked on the real server across an update.
3. **After loading a dump, re-index Solr before running the migrations.** A migration that loads a content by its remote id goes through the search index, which is empty
   until `reindex`: it failed with "found 0 matching contents" although the content was in the database. A migration that fails is recorded as failed and is not
   retried by itself, and the next one then fails for lack of what the first creates: remove the record (`kaliop:migration:migration --delete <file>`), re-index, run again.
4. A first install writes `replaces unknown` in the journal (no previous version is known): cosmetic.

Not covered: an actual certificate renewal, a reboot of the server, the provider's own firewall (ports 80 and 443 were open), and a stack other than the Ibexa one.
