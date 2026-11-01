# Profile: a PHP application (Symfony, Ibexa) on a CloudPanel PHP site

For a **simple installation**: legacy search, sessions and cache on the file system, no Redis, no Solr, no Varnish. A project that needs
those runs as a Docker stack (compose runtime). Reference: an Ibexa OSS 4.6 project whose application lives in `ibexa/`
(`examples/php-ibexa`); `deploykit init` writes the manifest.

## What happens on a deploy
1. **Build on this machine** from a clean `git archive` export: `composer install --no-dev --no-scripts`, then the asset compilation
   (for Ibexa: `yarn install`, `assets:install`, `ibexa:encore:compile`, `bazinga:js-translation:dump`). With `build.image` the same commands run
   in a throwaway container (only Docker is needed here); the environment the kernel needs to boot goes in `build.env`.
2. **Preflight** on the server: `releases/` and `shared/` exist, the required shared files exist, `php<ver>` exists, and the `ext-*` of
   `composer.json` are loaded (reported, never installed).
3. **Upload** into `releases/<stamp>` (rsync, hard links to the previous release), then **link the shared paths** (`.env.local`,
   `var/log`, `var/sessions`, `public/var`, `config/jwt`…) and create `var/cache`.
4. **`prepare`** commands in the new release (cache warm-up, migrations, GraphQL schema…): a failure stops here, the live site is untouched.
5. **Switch** the `current` link atomically; the vhost follows it (`$realpath_root`), no reload.
6. **Health** of `127.0.0.1:8080` with the site's Host header; a failure switches back and runs `after_switch` again.
7. **`after_switch`** commands (cache pool clear, purge), then status files and pruning.

`deploykit run <command>` runs a named command in the live release; `deploykit rollback` goes back to the previous release that did not fail.

## Server layout
```
/home/<user>/htdocs/<domain>/
  releases/<stamp>/      one folder per release, .release.json and .status inside
  shared/                .env.local, var/log, var/sessions, public/var, config/jwt (outside the releases)
  current -> releases/<stamp>
```
The CloudPanel document root is `current/public`. `deploykit bootstrap` creates the site with a vhost template derived for it
(`deploykit-<project>`); create `shared/.env.local` (mode 600, owned by the site user) before the first deploy.

## Not in this profile
Redis, Solr and Varnish (the app is wired to them through its own `.env.local`; a deploy of such a project is better done as a Docker
stack), the database import (a separate project), and installing PHP extensions or system packages (deploykit reports what is missing).
