# ADR 0008: Every compose command names its project, and a target may override it

Status: accepted

## Context
A compose file may fix its project name (`name:`). Two instances of the same application on one host would then share
containers and volumes, and a command run for one could act on the other. This was found on a real host while installing
a second instance next to a live one.

## Decision
- Every `docker compose` call carries `-p <project>`.
- The project is the manifest's `project`, unless the target sets `project` (`COMPOSE_PROJECT` or `PROJECT` in its file),
  which is required when several instances share a host.
- A project name must be lowercase letters, digits, `-` or `_`, as compose requires.

## Consequences
- Volumes, networks and containers of an instance are isolated by name; teardown of a throwaway instance cannot touch
  another one.
- An existing deployment keeps working without a `project` in its target: the manifest's value is used.
