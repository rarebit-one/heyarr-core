# 0002. One binary, roles as subcommands

**Status:** Accepted
**Date:** 2026-08-19

## Context

Spec §4 ships Heyarr as one Go executable with explicit roles — `controller`,
`worker`, `peer`, and `all` for simple deployments — and requires that roles be
"independently runnable as OS processes even when located on the same machine."

## Decision

One binary. Roles are cobra subcommands wired in `internal/cli` and nowhere
else, so that `heyarr all` and a three-process deployment share exactly one
wiring path. Roles communicate **only** through the controller database (jobs
and leases) and HTTP.

## Consequences

The rule that makes this real is the negative one: no role may hold a pointer to
another role's internals, even when they happen to share a process. If a call
could not survive being replaced by a network boundary, it is not allowed. This
is easy to violate accidentally and nearly free to prevent up front.

The acceptance script (§ADR-0009's sibling, `scripts/acceptance.sh`) runs in both
`heyarr all` mode and split-process mode. That, not code review, is what keeps
this decision honest.

## Alternatives rejected

- **Separate binaries per role.** Triples release artefacts and makes the
  single-machine case, which is the common one, worse.
- **Build tags.** Makes the split a compile-time property, so the two
  configurations are never both tested.
