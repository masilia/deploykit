# ADR 0003: Transport interface over the system ssh and rsync

Status: accepted

## Context
The compose scripts run on the server (copied by rsync); the Node scripts drive everything from the workstation over SSH.
The two models need unifying. The notes of the Node scripts show that the system `ssh` behaviours matter (agent key choice,
`~/.ssh/config`, non-interactive PATH, quoting).

## Decision
All remote and local effects go through a `Transport` interface with three implementations: `Local` (os/exec), `SSH`
(system `ssh` and `rsync`, honouring the agent and `~/.ssh/config`) and `Fake` (records commands, replays canned output).
Remote scripts are always written to the stdin of `bash -l -s`, never passed as quoted arguments. `deploykit server …`
is the same steps run with the Local transport on the server. A server-side binary is only needed for the compose
runtime (lock, backup, monitor, cron); pm2 and PHP targets are driven entirely over SSH.

## Consequences
- Tests compare recorded command traces with goldens derived from the original scripts.
- No embedded Go SSH client for now; this can be revisited behind the same interface.
- `rsync` becomes a workstation prerequisite. macOS ships `openrsync`, which may lack some flags: check in `dk doctor`
  and consider a tar-over-ssh fallback.

## Addendum: retries
A connection that ssh could not establish (refused, timed out, unresolvable, no banner) is retried twice with a doubling
pause, because the remote command never started. Nothing else is retried for a script: once it has started, running it
again could repeat its effects, so a session that breaks mid-run is reported with "it may have partly run, check the
state before running it again". rsync is idempotent and is retried after an interrupted transfer, but not after a
permission, authentication or disk error. A command that consumes a reader is never retried. `ssh.retries` (0 disables)
and `ssh.connect_timeout` in the manifest tune it; dead connections end after about 45 s (`ServerAliveInterval=15`,
`ServerAliveCountMax=3`).
