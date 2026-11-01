# Example: Next.js on CloudPanel

Reference profile (no Docker): local `standalone` build, release uploaded with `rsync` into
`releases/<timestamp>/`, `current` symlink switched atomically, pm2 reloaded, health check and automatic rollback.
Axes: `artifact: release-dir`, `runtime: pm2`, `ingress: cloudpanel` (site type `nodejs`).

- `deploykit.yaml`: proposed manifest (`version: 1`), not frozen.
- `targets/acme.env`: one target, no secret.
- Explanations, pitfalls and figures: [../../docs/profile-nextjs-cloudpanel.md](../../docs/profile-nextjs-cloudpanel.md).
