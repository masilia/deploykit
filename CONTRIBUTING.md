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

By contributing you agree that your contribution is licensed under the Apache License 2.0.
