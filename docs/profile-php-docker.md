# Profile: Ibexa with its full dependencies in Docker (Redis, Solr, Varnish)

For a PHP project that needs more than a CloudPanel PHP site gives: Redis for the cache pool and the sessions, Solr for search, Varnish for the
HTTP cache. The simple case (legacy search, files) is [the php-fpm profile](profile-php-cloudpanel.md). Reference manifest:
`examples/php-ibexa-docker/deploykit.yaml`. It uses the **compose runtime** as it is; what the project brings is its Dockerfiles.

```
browser ─ HTTPS ─> CloudPanel nginx ─ HTTP ─> varnish ─> web (nginx) ─ FastCGI ─> php (PHP-FPM)
                   (domain, certificate)      127.0.0.1:PROXY_PORT                 │
                                                          db (MariaDB) · redis (cache, sessions) · solr (search)
```

The ingress is a CloudPanel **reverse-proxy site** (`site:add:reverse-proxy`) pointing at Varnish's loopback port: the same shape as any compose
stack behind CloudPanel. Only Varnish publishes a port.

## What the repository provides
- A multi-stage `docker/Dockerfile`: `build` (composer, then yarn and the Ibexa asset compilation, with Node and PHP together), `php` (PHP-FPM, the
  application, no `node_modules`) and `web` (nginx with the same `public/`). deploykit builds one image per `images[]` entry and picks the stage with `target:`.
- `docker/varnish` (Ibexa's own `varnish7.vcl`, which imports `xkey`; the backend and the ACLs in `parameters.vcl`) and `docker/solr` (the project's Solr configset baked into
  `solr:8.11`).
- `docker/docker-compose.prod.yml` and `docker/.env.example`. The compose file lives in a subfolder while the server `.env` is in the deployment root:
  `runtime.project_directory: .` makes deploykit pass `docker compose --project-directory .`.

## Service roles
`edge` (Varnish), `proxy` (nginx, reloaded after an update: `nginx -s reload`), `app` (PHP, where the app commands run), `db`, `cache`, and `search`
(Solr). `install` starts `db`, `cache`, `search` and `realtime` services before the app commands, so Solr is up when `migrate` or `reindex` runs.

## Things measured while building the stack locally
- **Invalidation goes by token, not by address.** Behind CloudPanel every public request reaches Varnish from the Docker gateway, so an ACL with the private
  ranges lets any visitor send `PURGE`. `parameters.vcl` keeps `127.0.0.1` only; the application's `HTTPCACHE_VARNISH_INVALIDATE_TOKEN` authorises purges
  (checked: right token 200, wrong token 405, no token 405, plain `PURGE` 405).
- On the real chain a page is cached, then purged with the token, then cached again; a wrong token, no token and a `PURGE` from the internet answer 405.
- **`TRUSTED_PROXIES` does nothing in a Symfony 5.4 runtime project** unless `framework.trusted_proxies` reads it. Without it Ibexa's invalidation-token route answers
  401 ("not from a trusted proxy"), and the client address and the scheme are wrong. The image carries a one-file config (`zz-docker-trusted-proxies.yaml`) so the
  project's own files stay untouched. Not checked on a CloudPanel PHP site: any site whose vhost only sets the variable deserves the same test.
- **Varnish resolves its backend when the VCL loads**: it is started after `web` and restarted with it (`depends_on … restart: true`, compose 2.24+).
- The official PHP image has no `php.ini`: the CLI would stop at 128 MB while Symfony compiles its container; the build stage raises it.
- Sessions use PHP's Redis handler (`session.save_handler`) because the project's `framework.session.handler_id` is `null`.
- `ibexa:install` on an empty database, then `ibexa:reindex` into Solr, worked; the clean data gives the anonymous user no `user/login` on the project's siteaccess,
  so every Symfony route redirects to `/login` until a real database is loaded.

## Starting from `deploykit init`
`deploykit init` recognises this layout and writes the manifest: the compose file in `docker/` (so `project_directory: .`), the images the file takes from the
registry matched to their Dockerfile (a folder named after the image, or a stage of a multi-stage Dockerfile that nothing else builds on, as `--target`),
the roles (`edge` for Varnish, `search` for Solr, `proxy` for nginx), the app commands of an Ibexa project (`migrate`, `graphql`, `jwt_keypair`, `reindex`,
`cache_clear`), the placeholders of the env template (`CHANGE_ME` secrets generated, the rest asked for in `prompts`), a MariaDB or MySQL backup (and no
restore test, which supports PostgreSQL only), and a `sync.files` limited to the compose file and the env template. The matching is by name: check the
context and target of each image in `artifact.images`, which `init` says. The context is the folder sent to `docker build`, and a Dockerfile can only copy files inside it: `init` reads the
paths a Dockerfile copies and keeps the folder (or the repository root) that holds them. A Dockerfile that only does `COPY .` gives no clue, so write the paths it needs (`COPY conf/prod/solr/conf ...`)
or correct the context by hand. Run on a copy of the reference project, it produced a manifest that validates and
differs from the hand-written one by the build context of the Solr image.

## Variables
The server `.env` is managed with `deploykit env` (list, get, set, `--apply`) and the git-ignored `secrets/<target>.env`: see [targets.md](targets.md). The project keeps no
CloudPanel-side file for them, because the deployment root is outside the site's home on purpose.

## Limits
- Run on a shared host as a throwaway instance, with `release`, `bootstrap`, `deploy --install`, an update, a rollback, HTTPS and the purge (docs/validation.md, round 5), but with a clean database: the project's own migrations were not run.
- `deploykit run` runs the commands of `runtime.app.commands` (`migrate`, `graphql`, `cache_clear`, `reindex`, …) in a one-off container of the `php` service, or with `--exec` in the running one. It does not run the update hooks, and `ibexa:install` is not a declared command: it is a one-time act on an empty database, run by hand with `docker compose run --rm php …`.
- `restore_test` supports PostgreSQL only: this profile has none; backups use `mariadb-dump` and are verified by its closing line.
- Importing an existing database is out of scope.
