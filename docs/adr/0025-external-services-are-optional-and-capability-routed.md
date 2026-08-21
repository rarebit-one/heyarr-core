# 0025. External network services are optional and capability-routed

**Status:** Accepted
**Date:** 2026-08-21

## Context

Spec §58 delegates acquisition to an external download client and §59 delegates
discovery to an external indexer. Milestone 3 is the first time Heyarr depends
on a network **service** it does not run.

ADR-0023 already answered the equivalent question for a **binary**: the media
toolchain is optional at runtime, availability is a job capability rather than a
startup requirement, and a node without it degrades instead of failing to start.
That decision has aged well and most of it transfers unchanged.

One part does not, and it is the reason this needs its own record rather than an
amendment to ADR-0023.

## Decision

**External services are optional at runtime.** A node with no indexer and no
download client starts, scans, ingests, catalogues, verifies, garbage-collects,
serves byte ranges and plays exactly as before. This is a supported, tested
configuration, not an accident.

**Availability is expressed as a job capability.** A worker advertises `indexer`
and/or `download` only if the corresponding provider is configured. Jobs that
need one declare `RequiredCapability`, so on a node without it they stay
**pending and visible** rather than failing into a retry backoff. This is
capability routing's second and third user, after the media toolchain — and the
first where the capability comes from configuration rather than from a binary on
`PATH`.

**Presence is checked at startup; reachability is checked continuously and is a
job.**

This is where ADR-0023's asymmetry **inverts**, and it is the decision worth
recording. For a binary, ADR-0023 made "configured but unresolvable" a startup
error: somebody named that binary, and silently using none is worse than not
starting.

For a network service, "configured but unreachable" **cannot** be a startup
error. A download client that is down at 03:00 must not stop Heyarr from serving
the library at 03:01. Reachability is a property of the world at a moment, not a
property of the configuration, and refusing to start because of it would make
Heyarr's availability the product of every external service's availability.

So the line falls in a different place:

| | startup error | reported, not fatal |
|---|---|---|
| **binary** (ADR-0023) | configured path does not resolve | absent from `PATH` |
| **service** (this ADR) | endpoint is not a URL; required credential missing | endpoint does not answer |

What both have in common: a mistake somebody can fix in ten seconds is a startup
error, because the same mistake found at the first search costs an afternoon of
looking at the wrong system. What differs is which mistakes are knowable without
touching the network.

**Health lives in the registry, not in each integration.** §59 lists health as
one of the five things the provider registry holds, and this is why: every
provider needs the same "is it working, since when, and what version" answer, and
three integrations each keeping their own would give an operator three shapes to
learn.

**A capability is proven, not declared.** A provider reports itself healthy only
after being exercised. A provider that claimed health because it was configured
would be advertising something it might not deliver — work would route to it and
then fail, which is worse than advertising nothing at all.

**`GET /api/v1/providers` reports what is configured and what works**, so a
degraded node can say it is degraded.

## Consequences

**A node with nothing configured is a first-class deployment.** It is what
somebody has on the day they install Heyarr, and it is what the acceptance demo
runs as. The alternative — requiring an indexer to start — would mean the first
thing a new operator meets is a configuration error for a service they have not
installed yet.

**A job that no worker can claim waits forever, silently, by design.** That is
the intended degradation and it is also a way to be confused for hours. The
mitigation is the same as ADR-0023's: the node says so on
`GET /api/v1/providers`, and the job is visible on `GET /api/v1/jobs`. Not a
timeout, because failing a search whose indexer simply is not configured yet
would lose work that a later configuration change would have completed.

**The vocabulary is shared with worker capabilities, and the mechanism is not.**
Both are spelled the same way — structured, dotted, lower-case — so that a
reader who has met one has learnt the other. But a PROVIDER capability describes
what an external service can do for us, and a WORKER capability describes what a
node can execute; `jobs.required_capability` is never used to route to a
provider. `Capability.JobCapability()` is the one deliberate crossing, and it is
a named method so that the crossing is greppable rather than a string that
happens to match.

**Credentials are a type, not a discipline.** `providers.Secret` redacts in
`fmt`, in `slog`, in errors and in JSON; the plaintext is reachable only through
`Reveal()`, which greps cleanly. "Remember not to log it" is a control that works
until somebody adds `slog.Any("config", cfg)` while debugging something else, at
which point every provider's credential is in the log and nothing goes red. This
repository is public and its git history is permanent.

**No credential is stored in the database.** Not encrypted, not hashed, not
redacted-but-present. The plaintext lives in the operator's configuration file
where their other secrets already are; a second copy in a database that gets
backed up to peers (§50) would add a way to leak it in exchange for nothing.

**Configuring a provider whose client is not written yet is not an error.** It
is validated, registered, routed and reported as unhealthy with a detail saying
the client is not implemented. Refusing to start would punish an operator for
configuring something the roadmap says is coming; silently ignoring it would
leave them wondering why their indexer is never consulted.

## What would make us revisit

A provider kind that cannot be checked without side effects — one where
"exercise it" means starting a real transfer. The health model assumes a cheap,
idempotent probe exists, and a service without one would need a different answer
than "check it continuously".

A deployment where several nodes each hold different providers, at which point
`GET /api/v1/providers` describing only the answering node becomes as
unsatisfying as ADR-0023 already finds it for the toolchain. That needs
fleet-wide capability advertisement, which is tracked separately.

Evidence that an operator wants routing to prefer healthy providers. Routing
currently ignores health deliberately — health is stale by definition, and
refusing to route to a provider that was down ninety seconds ago turns a blip
into an outage — but a deployment with many indexers might disagree.
