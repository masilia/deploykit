# ADR 0007: Remote scripts are read completely before they run

Status: accepted

## Context
Scripts travel on the stdin of the remote shell (ADR 0003), so that no value ever appears in an argument. A shell reads
its script from stdin as it executes it. Some commands forward their stdin: `docker compose exec -T` hands the rest of
the script to the container, which swallows it. On the first real run, the checks written after such a command (the
empty-dump check and the archive listing of a backup among them) silently never ran.

## Decision
Every script sent on stdin is wrapped by `guardStdin` (`internal/transport/quote.go`): the shell first reads the whole
script into a variable, then runs it with an empty stdin. Commands that need input receive it explicitly
(`Argv` + `Stdin`).

## Consequences
- A command that forwards stdin can no longer eat the following lines; the behaviour no longer depends on which command
  happens to precede which.
- A script cannot read interactive input from the SSH session. Nothing in deploykit needs it.
- The Fake transport records the unwrapped script, so traces stay readable.
