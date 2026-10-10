# 0107. Personal media is a second service that mounts only the personal plane

**Status:** Proposed
**Date:** 2026-10-10
**Refs:** [#733](https://github.com/rarebit-one/heyarr-core/issues/733)
**Builds on:** [ADR-0020](0020-managed-linked-and-vault-assets.md), [ADR-0021](0021-encrypted-vault-content.md), [ADR-0049](0049-a-space-key-is-wrapped-for-a-device-encryption-key-and-a-peer-cannot-unwrap-it.md), [ADR-0095](0095-a-vault-is-a-filesystem-as-a-path-keyed-map-crdt.md), [ADR-0096](0096-vault-placement-is-a-device-supplied-blind-desired-fact.md), [ADR-0097](0097-a-vault-blob-is-fixed-frame-ciphertext-with-an-encrypted-manifest.md), [ADR-0104](0104-a-restricted-principal-reaches-only-the-spaces-a-device-granted-it.md)

## Context

ADR-0020 names heyarr a mass-media system — content that exists in the world,
has external identity, is acquired and is replaceable. That framing is what
makes almost every invariant in this codebase cheap to uphold: plaintext, the
control plane orchestrates, the storage fabric moves bytes, clients consume.

The vault surface built through Milestone 9 is a second product sold on
opposite guarantees. Its bytes are irreplaceable; the operator must be blind to
them; there is no control plane that can reason about content; the client is
the only actor that ever holds plaintext (Invariant 6, §38, ADR-0021). These
guarantees are incompatible with the threat model of a plaintext media server,
and the two products sharing one binary and one address forces the operator's
security posture to be the intersection of the two — the harder one everywhere.

The vault/drive/placement/grant surface — ADR-0021 through ADR-0104 — was
built inside heyarr because `internal/personalstate` is where the code lived,
and Go's `internal/` rule makes extracting a shared package before the binary
that uses it either a circular dependency or a surgery with no shipped value.
Neither the code structure nor the build constraint is a reason to keep the
products joined.

## Decision

**The vault surface leaves heyarr as a second service, Mnemosyne. Phase 1 is a
second binary, `cmd/mnemosyne`, in this repository, running a `personal`
profile. Phase 4 extracts to its own repository behind a shared Go module.**

### Phase 1 — `cmd/mnemosyne` in this repo

`cmd/mnemosyne` starts a process that mounts only the personal-state plane. Its
surface is exactly what a device or executor needs and nothing else:

- vault blob PUT and blob content reads;
- placement pins, include provisional-pin confirm (ADR-0096);
- auth, device enrolment and recovery (the device-gateway surface from ADR-0051,
  not the control-plane admin routes);
- encrypted CRDT state push and pull (ADR-0049, ADR-0101);
- pin-driven blob replication between sites (ADR-0096's blind executor);
- GC (ADR-0018, vault pins as durability basis);
- health and backup.

It runs on its own DB (same migration chain, different `data_dir`, different
migration namespace prefix), its own port and socket, and its own token
namespace, so a token minted for heyarr is not valid for Mnemosyne. No route
from the mass-media surface appears in it.

`cmd/heyarr` keeps the vault routes behind a feature flag (default on) until
every household's vault has migrated to a Mnemosyne instance, then sheds them.
Nothing in `internal/` moves yet.

### Phase 4 — own repository, shared Go module

`internal/personalstate`, `internal/cas`, `internal/replication`, and the
device-gateway packages are extracted into a shared module that both
repositories import. Go's `internal/` rule is the sequencing constraint: a
shared module can only be extracted from a binary that already exists and
exercises the API. The module boundary, once declared, becomes the contract
that the Mnemosyne repository must never break.

The repository split is Phase 4, not Phase 1, because no value ships until
the binary exists and migration is proven.

### What stays in heyarr, unchanged

Per-user media state — playback history, starred items, playlists, reading
position, subtitle language preference — stays in heyarr. It is personal state
that describes acquired, plaintext-catalogued content; the control plane can
legitimately see it. `linked` assets (ADR-0020) stay in heyarr. Invariant 6
is unchanged: every byte of vault content that was opaque to the server
remains opaque.

### One device identity, many relying parties

The device identity library (`internal/enrolment`, the phone authenticator)
serves both services unchanged. Enrolment with heyarr does not enrol the
device with Mnemosyne; a device that wants both presents its cert to both and
each mints its own credential. One authenticator, many relying parties — the
same pattern a browser wallet and an app share one key but earn separate
sessions.

## Consequences

- Heyarr's threat model shrinks to a plaintext media server with a guest mode.
  The operator running heyarr alone makes no promise about encryption. That
  promise is Mnemosyne's to make and keep.
- Every existing credential and space keeps working unchanged. ADR-0104 asserts
  this route by route for the executor surface; the migration flag preserves it
  for every device that has not yet switched.
- Adding a route to Mnemosyne's surface requires a deliberate edit to its allow
  list (ADR-0104), the same gate as the executor. A route added to heyarr is
  closed to Mnemosyne by default.
- Two DB files, two ports, two token namespaces. An operator who runs both
  services manages two `data_dir` entries and two credential sets. The docs owe
  a migration guide.
- `internal/` imports do not compile across the boundary before Phase 4. The
  shared module must be extracted before any Mnemosyne-only repository can
  reference personalstate or replication types. The Phase 1 binary in this repo
  has no cross-repo import problem; Phase 4 is the constraint.

## What would make us revisit this

- A household that cannot run two services. Then the `personal` profile inside
  one binary is the right form, not a separate repo — and the profile flag
  already exists in Phase 1.
- The shared module becomes a third-party dependency problem: if other projects
  import `cas` or `personalstate`, the API surface must be versioned, and a
  semver discipline that this repo has never needed becomes load-bearing.
- Mnemosyne's auth surface diverges from heyarr's enough to need a different
  identity library. That is a sign the two products have truly separated and the
  shared module should shed the auth packages.

## Alternatives rejected

- **Keep the vault surface in heyarr.** Two products, one threat model. Every
  security review of heyarr must cover the vault surface, and every vault
  security claim rests on a codebase that is also a plaintext media server. The
  intersection is the harder of two postures everywhere.
- **Extract the repository first.** Go's `internal/` rule makes `personalstate`
  and `replication` unreachable from an external module before a shared module
  is extracted. Extracting the repo first means the surgery — finding every
  internal import, wrapping it, releasing a module — must happen before the
  binary has shipped any value and before the API has stabilised under real use.
- **A generic "profiles" framework.** Two profiles is a flag, not a framework.
  A framework adds a configuration surface that must be tested at every
  combination of flags, and the only combination that matters is "vault only"
  versus "media only." A named binary is clearer and has no combinatorial
  surface.
