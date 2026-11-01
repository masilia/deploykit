# ADR 0002: Manifest composed of artifact, runtime and ingress axes

Status: accepted (replaces the earlier `kind: compose | node-release` idea)

## Context
Four targets are wanted: a Docker stack behind CloudPanel, a Docker stack on a standalone server, a Next.js app on pm2,
and a PHP app. Three of them sit on CloudPanel. The two Docker targets differ only by ingress (the legacy
`TLS_MODE=cloudpanel|certbot`), which a single `kind` field cannot express without duplicating the schema.

## Decision
A `deploykit.yaml` has `version: 1` and three typed axes, each with its own small config:

- `artifact`: `image` (registry) or `release-dir` (local build, rsync)
- `runtime`: `compose`, `pm2` or `php-fpm`
- `ingress`: `cloudpanel` (with a site type) or `standalone` (own nginx + certbot)

Invalid combinations are rejected at load time (for example `image` + `pm2`). Unknown keys are errors.

When clients of the same project differ on one axis (`TLS_MODE=certbot|cloudpanel` per client), the axis may be
written as `select: "{{ target.<key> }}"` plus `variants:`, each variant being a complete value of that axis. The
selected key must be present in every target; this is the only place where a manifest depends on a target.

## Consequences
- New targets are new axis values, not a new schema.
- The loader needs a compatibility matrix and good error messages.
- Shared sections (inventory, hooks, env schema, notifications) sit at the top level.
