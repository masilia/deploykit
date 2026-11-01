# ADR 0005: Release builds come from a clean git ref; releases carry metadata

Status: accepted

## Context
The compose scripts build images from `git archive vX.Y.Z`, so uncommitted changes never ship. the Node scripts build from the dirty
worktree and only warns. Its releases are ordered by file mtime (`ls -t`), and a failed deploy exits before cleanup, so
failed release folders accumulate and can push a good release out of the retention window.

## Decision
- `release-dir` artifacts are built from an export of a git ref (`git archive`) by default. `--from-worktree` opts in to
  the faster, dirtier path and prints a warning.
- A release folder is named `YYYYMMDDHHMMSS` and contains `.release.json` (commit, ref, subject, date, author) and a
  sibling `.status` file (`building | live | failed | superseded`). The status lives in its own file so changing it never
  rewrites the immutable metadata; both are protected from `rsync --delete`. A folder without `.status` was made by the
  old shell scripts and counts as a healthy release.
- Rollback and cleanup use the metadata and the folder names, not mtimes. Failed releases are purged first, then the
  oldest superseded ones beyond `keep`. Rollback can take an explicit release as a target.
- Because values such as `NEXT_PUBLIC_SITE_URL` are baked into the build, the build cache key is
  `(commit, build-env hash)`: a fleet shares a build only when the environment matches.

## Consequences
- A clean export means a slower build (a fresh `npm ci`); `--skip-install` and a dependency cache remain available.
- Builds are reproducible and match that rule.
