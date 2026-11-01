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
