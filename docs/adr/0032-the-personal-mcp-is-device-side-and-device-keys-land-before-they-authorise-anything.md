# 0032. The Personal MCP is device-side, and device keys land before they authorise anything

**Status:** Accepted
**Date:** 2026-08-22

## Context

§40 gives every user device its own private key, and §41 wraps space keys for
those keys. ADR-0022's enrolment story begins with *"an existing authorised
device"* — and there is no first-party mobile app, so no such device exists.
The story has no first device to start from.

Two decisions follow from making the desktop CLI that device, and both are
cheap now and expensive later.

## Decision

### The device key store lands now, in the shape Milestone 8 will populate

This is ADR-0010's argument, made a second time. That ADR put the `peers` and
`replicas` tables in the first migration with exactly one row, so that
*"Milestone 4 becomes a protocol addition rather than a schema migration and a
rewrite of every query that assumed locality."*

The same holds here: a device-key store that exists early, with one device and
no delegations, means Milestone 8 populates a shape rather than retrofitting
one. The cost today is one keypair and two files.

**The key lives in the user's config directory, never the server's `data_dir`.**
They have different owners, different backups and different blast radii: the
data directory belongs to the service account, is backed up with the catalog and
is readable by whoever operates the host. A private key there would be inside the
radius the key exists to stay out of.

### It is labelled `unproven` and `not_enrolled`, on the wire

ADR-0011's bearer tokens remain the only thing that authorises a caller against
a controller. A device keypair that exists but authorises nothing must say so,
in the CLI and in every response — the idiom placement established, where
`unproven` was a required *response field* because a caveat that lives only in
the domain is one the edge forgets.

**A key called self-sovereign that is not yet load-bearing is worse than no key
at all, because someone will trust it.** The labelling is the deliverable, not a
comment on one.

### The Personal MCP can never be a tool on the controller's MCP

§72: controller-side MCP **cannot** decrypt user artifacts. §73 answers that by
putting playlists, history, ratings and annotations behind a separate *Personal*
MCP, exposed by a client the user runs.

So `heyarr device mcp` is a local stdio server on the user's own machine, and
`internal/api/mcp` is untouched. This is not a matter of degree. Adding
device-key tools to the controller's MCP would put private key material on the
server — reachable with a bearer token, logged by the controller's middleware,
backed up with the catalog — which is the vulnerability §38 and `SECURITY.md`
exist to prevent. No scope check makes it safe, because the server would hold
the material either way.

The two servers therefore share no code and no transport, and a test enumerates
each tool surface exactly.

## Consequences

The device store is a client concern with no controller, no database, no
migration and no OpenAPI surface. `heyarr device` takes no `--config`: reading
the server's configuration here would be the first step towards writing the key
into it.

Containment on the device side is the operating system's (§74) — 0600 on the
key, 0700 on the directory, stdio rather than a socket — because there is no
remote caller to authorise and inventing an authorisation scheme for a local
pipe would be theatre.

Absent rather than stubbed, following ADR-0019: wrapped space keys, delegation,
capability grants, pairing by short authentication string, and the recovery
secret. A tool that answers "not implemented" is a published promise with a hole
in it.

The Personal MCP has a second, larger job in Milestone 9 — playlists, history,
ratings, annotations — and this establishes where it runs before there is any
personal state to argue about.

## What would make us revisit this

**Milestone 8 giving the key something to authorise.** When a device key is
enrolled with a user identity and grants stop being bearer scopes, the
`unproven` and `not_enrolled` labelling comes off — and it must come off in the
same change, because a caveat left behind after it stops being true teaches
people to ignore the next one.

Nothing would make the Personal MCP move onto the controller. If that ever looks
necessary, the design has gone wrong: it means something server-side wants to
read private state, which is what §38 forbids.
