# Contributing

deploykit is a Go CLI; `CLAUDE.md` and `PLAN.md` describe the architecture and the conventions that are easy to get
wrong.

```sh
make build   # bin/deploykit and the bin/dk symlink
make test    # go test ./...   (before committing: go test -race ./...)
make vet
make lint    # golangci-lint v2
```

- Go is the only toolchain. Some tests use `git`, `sh`, `gpg`, `tar`, `find`, `rsync`, `curl` and `node`, and skip
  themselves when one is missing.
- Everything goes through the `Transport` interface; tests assert on the recorded trace of the Fake transport. A secret
  must never appear in a command argument or in a trace.
- Anything that changes a server belongs in a step's `Run`, not its `Check`; `--dry-run` must leave the server
  untouched, lock included.
- Write code, comments, documentation and commit messages in English. Keep commits focused, with the reason in the
  message.
- A new behaviour comes with a test. A bug found on a real server comes with a regression test.
- Please open an issue before a large change.

## Changelog and releases

Every change a user can notice (a command, a manifest key, a behaviour, a fix) gets a line under `## [Unreleased]` in `CHANGELOG.md`,
in the same commit or pull request, under Added, Changed, Fixed or Removed. To release:

1. Rename `## [Unreleased]` to `## [X.Y.Z] - YYYY-MM-DD`, open a new empty `## [Unreleased]` above it and update the links at the end.
2. Commit, then tag `vX.Y.Z` and push the tag. The release workflow vets and tests, takes the notes from that entry
   (`scripts/release-notes.sh vX.Y.Z` shows them) and creates a **draft** GitHub release with the binaries; publish it by hand.

CI fails when a version tag has no entry. Releases are never edited in the GitHub UI: fix the entry and the notes follow.

By contributing you agree that your contribution is licensed under the Apache License 2.0.
