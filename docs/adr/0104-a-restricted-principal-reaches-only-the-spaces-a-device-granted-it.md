# 0104. A restricted principal reaches only the spaces an owner's device granted it

**Status:** Proposed
**Date:** 2026-10-08
**Milestone:** M9 — Encrypted personal state
**Builds on:** [ADR-0011](0011-milestone-1-authentication-scoped-bearer-tokens-loopback-by-default.md), [ADR-0049](0049-a-space-key-is-wrapped-for-a-device-encryption-key-and-a-peer-cannot-unwrap-it.md), [ADR-0065](0065-a-device-earns-write-scope-and-the-session-lift-is-retired.md), [ADR-0096](0096-vault-placement-is-a-device-supplied-blind-desired-fact.md)

## Context

An executor is a service that runs work for an owner against a few encrypted
spaces: it pulls ciphertext, decrypts with a key wrapped for it, and pushes
results back. Today any bearer token is node-wide. A `read,write` token lists
every space, fetches every space's changes and wraps, and reaches media, the
catalog, MCP and every other route. ADR-0049 named two gates (may I fetch the
ciphertext; can I decrypt it) and built only the second.

## Decision

**A token may be restricted. A restricted token acts as an `executor`
principal and reaches only an allow-list of vault routes. On those routes it
sees only the spaces for which an owner's device recorded a grant.**

- **Shape.** `heyarr token create <name> --executor` mints a token with
  `restricted = 1` and `read,write`, never admin, for a principal of kind
  `executor`. A name is one kind of principal, so an executor never holds an
  unrestricted token. The identity is restricted if either the token or the
  principal says so.
- **Deny by default.** The confinement is mounted at the root of `/api/v1` and
  on `/metrics`. Only the routes in `restrictedAllowList` pass: space list;
  keys, key history, changes and snapshot reads; change and snapshot pushes;
  blob content reads; vault blob PUT; placement POST. A route added later is
  closed to an executor until someone lists it. Space creation, re-wrap, key
  deletion, rotation, compaction, replication and the grant API also refuse a
  restricted caller in the route itself. The OpenSubsonic and OPDS adapters,
  which verify tokens themselves, refuse one too.
- **The grant is the fetch gate.** `space_grants(space, principal, caps)`
  holds `read` or `read,write` and an optional expiry. Without an active grant
  every space route answers 404, the same as an unknown space, so an executor
  cannot enumerate. Pushing needs `read,write`. A restricted caller is shown
  only its own wraps. Until service recipients exist it is shown none, which
  fails closed. The grant never decrypts anything: that is still a wrapped key
  (ADR-0049).
- **Only a device consents.** `POST`/`DELETE /spaces/{id}/grants` need a
  Device credential carrying write, which means an enrolled device that an
  admin authorised (ADR-0065). A bearer or session token is refused, even an
  admin one. A space records the user who created it from a device, and only
  that user may grant on it. A space with no recorded owner is a pre-existing
  household space, and any authorised device may grant on it. Each grant and
  revoke is an event naming the device. Grants and owners are this node's fact
  and do not replicate.
- **Blobs (owner decision G6).** A blob carries no space by design (ADR-0096,
  Invariant 6), so a blob read cannot be checked against a grant. A restricted
  caller reads a blob only if all of these hold:
  - it holds some active grant;
  - the blob is pinned;
  - no asset or scanned library file references the blob, so media never
    qualifies, even when pinned.

  The read draws on a per-principal budget (600/min). Anything else answers
  404, the same as an absent blob.
- **Unchanged for every existing credential.** Every existing token migrates to
  `restricted = 0` and every existing space to no owner. Neither change alters
  what a non-restricted caller can do, and the tests assert this route by
  route.

## Threat model for hash-addressed blob reads

The unguessable BLAKE3 id is the capability, a 256-bit digest of ciphertext.
An executor learns ids only from manifests it can decrypt. It cannot list
blobs, placements or assets. Guessing is bounded by the budget. A leaked id
gives up ciphertext only: an executor that holds an id from another space but
no wrap for it reads nothing. What this concedes is that an executor with a
grant on space A, holding an id from space B, can fetch B's ciphertext. We
accept that rather than build a blob-to-space index, which Invariant 6 and
ADR-0096 exist to forbid.

## Consequences

- An executor's authority is a list in one file plus rows an owner created.
  Widening it is a reviewed edit, never an accident of mounting.
- Revocation closes fetch on the next request. Recalling a wrap and re-keying
  are separate steps (`DELETE /spaces/{id}/keys/{r}`, rotation), and both
  stay with the owner.
- A grant has no expiry unless one is set. Standing consent is the default.

## What would make us revisit

- A second service accepts Device credentials. Possession proofs then need an
  audience before any grant flow crosses services.
- Per-run delegation, rather than standing consent, becomes verifiable on
  heyarr's request path.
- Blob reads need per-space confinement. That means space-scoped blob ids, not
  a server-side index.
