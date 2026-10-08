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
  only the wraps of its own service recipients (below), never another
  recipient's. The grant never decrypts anything: that is still a wrapped key
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

## Service recipients: how an executor gets a wrap

An executor must decrypt, but it is never a member device (it holds no
enrolment cert), so enrol-before-wrap (ADR-0049) refuses every wrap for it. We
add a third kind of pinned wrap target beside a device key and a recovery key.

- **Registration.** A *service recipient* is an executor's X25519 public key,
  registered for one executor principal (`service_recipients`, migration
  00059) by a management-authorised Device credential. `heyarr recipient add`
  prints the key's fingerprint and requires the operator to type the one the
  executor's host displayed. The controller re-checks it when sent, so nothing
  that relays the key is trusted to publish it. A key that is live for another
  executor is refused. A key is never both a member's and an executor's: a
  registration is refused for a non-revoked device's key or a recovery key,
  and enrolling, re-keying or materialising a device onto a live service key
  is refused too. Each side checks inside its own write transaction. Wraps
  name their recipient only by key, and this exclusion is what lets removing a
  registration delete every wrap of the key safely.
- **Narrower than enrolment.** A registered key is a valid wrap target only on
  a space where its executor holds an active grant. A re-wrap or a rotation
  that names it anywhere else, or after the grant is revoked or expired, gets
  `403 wrap_recipient_not_allowed`. The check runs inside the transaction
  that writes the wrap, so a revoke or an expiry that lands first always wins.
  A replicated copy that fails it is skipped as superseded. Registration alone
  wraps nothing.
- **Consent is one transaction.** `heyarr space grant <space> --executor <p>
  --recipient x25519:<hex> [--write] [--expires …]` runs on a member device
  that holds the space key. It wraps the current key at the current epoch for
  the recipient and sends the wrap inside `POST /spaces/{id}/grants`. The
  controller records the grant and the wrap in one transaction, so a partial
  state cannot be observed. A grant whose expiry is not in the future is a
  `400`. A refusal (an unregistered key, or a rotation that
  landed in between, which gives `409 key_epoch_stale`) records nothing. A lost
  reply is safe to retry: the grant is renewed and the copy replaced. One wrap
  of the current key reaches the past through the key history (ADR-0103).
- **Revocation closes both gates.** `DELETE /spaces/{id}/grants/{p}` (`heyarr
  space revoke-executor`) revokes the grant and deletes the executor's copies
  on that space in one transaction. Removing a registration deletes its copies
  on every space. Neither is forward secrecy: the executor keeps any key it
  already unwrapped, and a peer that replicated a copy keeps it. Only a
  rotation that names the recipient in `revoke` ends that, so `space rotate
  --revoke <key>` comes first when it is wanted. Until #706 lifts the #700
  guard, a vault space cannot be rotated, and revoking an executor from it is
  grant-and-wrap deletion only (owner gate G2).
- **Rotation counts it.** A service recipient holds a copy of the current
  key, so the recipient compare-and-swap of ADR-0103 (#703) counts it like any
  other. A rotation either re-wraps it (allowed only while its grant is
  active) or names it in `revoke`. Leaving it out is a `409`.

## Executor custody: where the recipient key lives

- **Born sealed.** `heyarr recipient init --sealed <file>` runs on the
  executor's host. It draws the X25519 seed in memory and seals it straight
  into a passphrase-sealed file (void-which-binds-go `custody/sealedfile`, its
  ADR-0021), so the private half is never on disk in the clear. It prints the
  public key and the fingerprint that `recipient add` asks the owner to type.
  `recipient show` prints the same from the file's clear header, without the
  PIN. A second `init` never replaces a key that spaces are wrapped for.
- **The PIN is a credential.** It comes from the systemd credential
  `heyarr-recipient-pin` (`LoadCredentialEncrypted=`), or an owner-only file.
  It is never an argument or an environment variable, and a missing or loose
  source refuses rather than falls back.
- **Reads and writes by ref.** `heyarr vault get-ref hv1:<space>/<object>` and
  `vault put-ref --space <space> -` address one sealed JSON object at
  `.jumpdrive/objects/<object>.json` in the space's drive. The ref grammar is
  the referring system's `^hv1:[0-9a-f-]{36}(/[0-9a-f-]{36})?$`, narrowed to
  canonical UUIDs, and object ids are random. The executor authenticates with
  its restricted token and opens the space with its sealed key through its own
  wrap and the key history (ADR-0103). Plaintext goes only to stdout or a new
  owner-only file the caller names, which should be on a tmpfs it wipes. It
  never goes to stderr or into an error. Exit codes separate an unavailable or
  wrong key (3), a space it cannot see (4), one it cannot decrypt (5) and an
  absent object (6), so a caller maps them without parsing prose.

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
- Revocation closes fetch on the next request and deletes the executor's
  wraps on this node in the same step. Re-keying stays a separate, explicit
  rotation, and it stays with the owner.
- A registration and its wraps are this node's fact. An executor talks only to
  the node that granted it, and that node shows a restricted caller nothing
  without a grant. So a wrap that replication brings back is unreachable. The
  next rotation must name it in `revoke`, because it can no longer be
  re-wrapped.
- A grant has no expiry unless one is set. Standing consent is the default.

## What would make us revisit

- A second service accepts Device credentials. Possession proofs then need an
  audience before any grant flow crosses services.
- Per-run delegation, rather than standing consent, becomes verifiable on
  heyarr's request path.
- Blob reads need per-space confinement. That means space-scoped blob ids, not
  a server-side index.
