# Reference profile: Next.js standalone on CloudPanel (local build, releases, pm2)

Notes from the real deployment of a Next.js site (October 2026). This profile uses **neither Docker nor a
registry**: the site is built on the workstation, a release is uploaded into a dated folder, a symlink is switched
and pm2 is reloaded. Together with the compose profile (Docker Compose) it is one of the two reference cases for parity.

The shell scripts this profile replaces (`deploy`, `bootstrap`, `rollback`, `common`, an `ecosystem.config.cjs` and a
`.env.deploy.example`) are no longer in this repository.

> In the manifest this profile is `artifact: release-dir` + `runtime: pm2` + `ingress: cloudpanel` (site type
> `nodejs`). See PLAN.md for the axis model. Nothing here depends on the implementation language.

## 1. Principles
- No build on the server (a Next.js build needs 1.5 to 2 GB of memory): local build in `standalone` mode.
- **Immutable releases** `releases/<YYYYMMDDHHMMSS>/`; a `current` symlink points at the active one.
- **Atomic switch**, health check, **automatic rollback**; the N most recent releases are kept.
- **Idempotent**: the initial installation can be re-run, every step checks its state first.
- No secret in the repository; runtime secrets go to `shared/` on the server.

## 2. Target configuration (today `deploy/.env.deploy`)
| Key | Example | Role |
|---|---|---|
| `DEPLOY_HOST`, `DEPLOY_SSH_PORT` | `www.example.com`, `22` | SSH target |
| `DEPLOY_ROOT_USER` | `root` | installation only (`clpctl` requires root) |
| `DEPLOY_DOMAIN` | `www.example.com` | CloudPanel site domain |
| `DEPLOY_USER` | `acme` | the site's system user, owner of the files |
| `DEPLOY_PATH` | `/home/acme/htdocs/www.example.com` | site root |
| `APP_PORT` | `3001` | internal port Nginx forwards to |
| `NODE_VERSION`, `PM2_NAME` | `22`, `acme-site` | |
| `SITE_URL` | `https://www.example.com` | injected at build time (`NEXT_PUBLIC_SITE_URL`: sitemap and robots) |
| `KEEP_RELEASES`, `DEPLOY_SSH_PUBKEY` | `3`, `~/.ssh/id_rsa.pub` | retention; key to install for the site user |

The manifest equivalent is in `examples/nextjs-cloudpanel/deploykit.yaml`. A target (inventory, no secret) holds
`host`, `ssh_port`, `root_user`, `domain`, `site_user`, `path`, `app_port`, `site_url`.

## 3. Remote layout
```
$DEPLOY_PATH/
  releases/20261026192635/   # server.js, .next/, minimal node_modules, public/
  current -> releases/…      # active link
  shared/                    # secrets and persistent data (empty for now)
  ecosystem.config.cjs       # pm2 config, re-sent on every deploy
```

## 4. "Bootstrap" phase (once, as root): `bootstrap.sh`
1. Checks: `uname -sm`, `clpctl` present, `pm2` present.
2. **Port selection** (point 8 of §9).
3. Site, if `$DEPLOY_PATH` does not exist:
   `clpctl site:add:nodejs --domainName=… --nodejsVersion=22 --appPort=… --siteUser=… --siteUserPassword=<random 24 chars>`
   (the password is shown once; it only serves SFTP and the file manager).
4. Certificate: `clpctl lets-encrypt:install:certificate --domainName=…` (failure tolerated, warning).
5. `releases/` and `shared/` folders; SSH key added to `~user/.ssh/authorized_keys` (perms 700/600, correct owner,
   no duplicate via `grep -qF`).
6. The user's pm2: present, otherwise `npm i -g pm2`. **Start at boot**: `pm2 startup systemd -u <user> --hp <home>`
   prints a `sudo env PATH=… pm2 startup …` command that is run **as root without `sudo`**. It creates
   `pm2-<user>.service` (`PM2_HOME=/home/<user>/.pm2`, `pm2 resurrect`).

## 5. "Deploy" phase (every time): `deploy.sh`
1. Shows branch and commit; warns if the tree is dirty; reminds what is still a demo.
2. Build: `npm ci` (optional), `npm run lint`, `npx tsc --noEmit`, `NEXT_PUBLIC_SITE_URL=… npm run build`.
3. **Assembly** of `.deploy/release/`: `.next/standalone/.` + `.next/static` (into `.next/static`) + `public/`
   (deleting the partial standalone `public/` first).
4. **Local smoke test**: starts the release's `node server.js` (random port 3300–3799, `HOSTNAME=0.0.0.0`,
   `NODE_OPTIONS` removed), cookie jar, redirects followed (3 max), on: `/fr /en /fr/services /fr/contact
   /fr/projets /fr/credits /icon.png /apple-icon.png /favicon.ico /manifest.webmanifest /robots.txt /sitemap.xml
   /brand/logo.png`, a `/_next/static/chunks/*.css` sheet extracted from the page, and `/video/hero.mp4` with
   `Range: bytes=0-1023` (206 expected). Any code other than 200/206 → stop **before anything is sent**.
5. Server checks: same `uname -sm` as the workstation (otherwise `sharp` does not load); `releases/` and `shared/`
   exist; **port free** if pm2 does not manage the app yet (`pm2 pid <name>` empty or 0, then `ss -ltnH`).
6. Upload: `rsync -az --delete --info=stats1 --link-dest=<active release>` to `releases/<stamp>/`
   (`--link-dest` only added if the folder exists), then upload `ecosystem.config.cjs`.
7. **Switch**: `ln -sfn releases/<stamp> current.tmp && mv -Tf current.tmp current`, then
   `APP_PORT=… PM2_NAME=… pm2 startOrReload ecosystem.config.cjs --update-env && pm2 save`.
8. **Health**: up to 30 tries, 1 s apart, of `curl http://127.0.0.1:$APP_PORT/fr` **run on the server** (200).
9. **Failure**: puts `current` back on the previous release, `pm2 reload`, prints `pm2 logs --lines 30 --nostream`,
   exits with an error.
10. Cleanup: `ls -1dt */ | tail -n +$((KEEP+1)) | xargs -r rm -rf`.

## 6. "Rollback" phase: `rollback.sh`
Lists `releases/` (newest first), finds the current one (`basename $(readlink -f current)`), takes **the next one
in the list**, switches back (same `ln` + `mv -Tf`), `pm2 reload`, waits 3 s, checks for 200; otherwise error.

## 7. Observed results
Standalone release 87–89 MB (66 MB of `node_modules`, 14 MB of `.next`, 9 MB of `public`); first upload
33.8 MB in ≈ 22 s; local build 2 to 3 min. First real deployment: site returns 200 on `/fr`, HTTP → HTTPS.

## 8. Facts about the CloudPanel server
- `clpctl` in `/usr/bin`; `pm2` and `node` (v24) in `/usr/local/bin`; each site has its own user
  (`/home/<user>/htdocs/<domain>`) and its own nvm (`~/.nvm/versions/node/v22.x`).
- Nginx vhosts: `/etc/nginx/sites-enabled/<domain>.conf`, which forward to `127.0.0.1:<port>`.
- `clpctl site:add:*`: `nodejs`, `static`, `php`, `python`, `reverse-proxy`; common options `--siteUser`,
  `--siteUserPassword`. Help: `clpctl` without arguments lists the commands. Full list: `docs/cloudpanel-cli.md`.
- CloudPanel adds **`www.<domain>`** to the certificate: without DNS for `www`, Let's Encrypt fails; in the
  meantime CloudPanel installs a self-signed certificate (HTTPS with a browser warning).

## 9. Pitfalls met (to encode in the tool)
1. **`HOSTNAME=127.0.0.1` breaks next-intl in standalone mode**: translated paths (`/fr/projets`, `/fr/a-propos`)
   redirect to themselves (looping 307). Use `0.0.0.0` (or `localhost`); the port stays closed on the firewall,
   only Nginx reaches it.
2. **The standalone `public/` is partial** (only what the tracer saw): copying `public/` over it gives
   `public/public/`. Delete, then copy. Standalone does not contain `.next/static` either.
3. **`--link-dest` to a missing folder** (first deployment): rsync warning; test `-d` first.
4. **SSH quoting**: no apostrophes in `bash -lc '…'`; send the script **on the standard input** of
   `bash -l -s`.
5. **PATH and nvm in non-interactive SSH**: `.bashrc` stops early; load `~/.nvm/nvm.sh` then `~/.profile`.
6. **SSH key**: the agent may refuse a key (here a locked `id_ed25519`) while root accepts `id_rsa`.
   To know which key works: `ssh -v … | grep "Server accepts key"`. Install for the site user the key that
   is actually authenticated.
7. **`pm2 list` as root starts a pm2 daemon for root** (side effect). To inspect without effect:
   `pgrep`, `systemctl`, or `pm2 jlist` as the user concerned; never `pm2 kill`/`pm2 save` as root.
8. **Port detection**: `ss -ltnH | awk '{print $4}' | sed 's/.*://'` **plus**
   `grep -rhoE '(127\.0\.0\.1|localhost|0\.0\.0\.0):[0-9]{2,5}' /etc/nginx/sites-enabled /etc/nginx/conf.d`
   (apps that are stopped but declared). First free port from the requested value; write the chosen value back
   to the target configuration. Ports already taken here: 3000, 3003, 8080, 8081, 19001–19005…
9. **Architecture**: `sharp` is compiled for the build machine; compare local and remote `uname -sm`.
10. **Language redirects**: the smoke test must keep a cookie jar and follow redirects.

## 10. Differences from the compose profile (Docker Compose)
| Topic | Compose profile | This profile |
|---|---|---|
| Artifact | Docker image, registry | release folder, `rsync` |
| Rollback | previous image version | previous `current` link |
| Health | containers `healthy`, scheduler tick | HTTP 200 `curl` on a route |
| Process | compose | pm2 (then systemd at boot) |
| Lock, backup, migrations | yes | **not yet** (see §11) |
| Binary on the server | yes (same version) | not needed: everything is driven over SSH |

## 11. Not covered today (to plan)
- Lock against two simultaneous deployments; per-release metadata file (commit, branch, date, author, status).
- Failed releases are never cleaned up (cleanup only runs after a success) and count against the retention.
- Secret synchronisation (`shared/.env`), database migrations, backups before the switch.
- DNS validation before the certificate (domain **and** `www`), then re-running the certificate step.
- Notifications, deployment journal, per-step durations.

## 12. Implementation notes
- Named steps in `internal/runner` with `Check() (done bool)` and `Run()`: idempotence, `--dry-run`, folded output.
  Rollback is owned by the runtime (pm2 / compose), not by each step.
- Abstractions: `Runtime` (pm2), `Ingress` (CloudPanel via `clpctl`), `Transport` (system ssh + rsync, as decided in
  PLAN.md: agent and `~/.ssh/config` inherited). No separate `Framework` abstraction: the build is fully described
  by the manifest.
- Rule from PLAN.md to keep: **secrets never in a command argument**; here, commands go on stdin. The one documented
  exception is `clpctl site:add:* --siteUserPassword` (see `docs/adr/0006-clpctl-password-argv.md`).
- Tests: a fake `Transport` that records commands; parity test against the recorded commands of `deploy.sh`;
  configurable smoke-test page list.
- Switch criterion for this profile: one `bootstrap → deploy → rollback` cycle on a staging target, then a real
  deployment.
