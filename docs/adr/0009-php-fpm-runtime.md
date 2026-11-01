# ADR 0009: The php-fpm runtime serves release folders behind a CloudPanel PHP site

Status: accepted

## Context
A PHP application (Symfony, Ibexa) differs from a Node one: nothing to start, a cache that holds absolute paths, folders that
must survive a release (uploads, logs, sessions, the file with the secrets), migrations to run before the new code serves,
and PHP-FPM keeping opcache and the realpath cache per file path. CloudPanel provisions the site (nginx, an FPM pool per
site, optional Varnish) but `clpctl site:add:php` has no root-directory option, and a site user cannot reload PHP-FPM.

## Decision
- **Same pipeline as pm2** (`internal/deploy/node.go`): build here from a clean export, upload a release folder with rsync, switch
  the `current` link atomically, check, roll back. What differs sits behind `releaseRuntime` (`runtime.go`, `php.go`).
- **Build on the workstation** (or in a throwaway container with `build.image`): `composer install --no-dev --no-scripts`, then the
  asset compilation. The server needs no composer and no node. A Symfony cache holds absolute paths, so `var/cache` is never sent:
  it is created empty and warmed on the server, in the new release, before the switch.
- **`shared/`**: `runtime.shared` lists files and folders kept outside the releases and linked into each one. A required file (the
  `.env.local` with the secrets) is checked before anything is uploaded; secrets never travel with a release.
- **Named commands** (`runtime.commands`): `prepare` runs in the new release before the switch (a failure leaves the live release
  alone), `after_switch` runs after the switch and after a rollback (a failure is a warning). `{{ php }}` is `php<version>`, because the
  site user's `php` is another version. They also run on demand with `deploykit run <name>` and `deploy --also <name>`.
- **No reload needed.** The custom vhost template that bootstrap derives from a base one (`clpctl vhost-template:view`) sets the root
  directory with its first line `#{"rootDirectory":"current/public","phpVersion":"8.2"}` and passes PHP-FPM `$realpath_root`
  instead of `$document_root`, so opcache sees the path of the release itself and a switch needs no `systemctl reload`. `reload:
  root-systemctl` remains for an existing site whose vhost cannot be changed.
- **Health is asked of the site's own backend**, `127.0.0.1:8080` with the site's Host header: past Varnish and the HTTPS redirect.
- **Dependencies**: PHP and its extensions come from CloudPanel; deploykit checks them (`php<ver> -m` against the `ext-*` of
  composer.json) and reports what is missing, with the `apt` command, but never installs a package.

## Consequences
- Redis, Solr and Varnish are not part of this profile: a project that needs them runs as a Docker stack (compose runtime).
- Schema migrations are not undone by a rollback, as for the other runtimes: they must stay compatible with the previous release.
- The vhost template is created once; changing it afterwards is done in CloudPanel.
