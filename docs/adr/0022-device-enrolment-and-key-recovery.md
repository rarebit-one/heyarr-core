# 0022. Device enrolment and key recovery

**Status:** Accepted
**Date:** 2026-08-20

## Context

ADR-0021 makes key loss **total data loss**: every replica of a vault becomes
permanently unreadable. That turns recovery from a convenience into the
load-bearing part of the feature — vaults must not ship before this works.

Two problems get conflated because they feel similar, and only one of them needs
secret sharing:

1. **Enrolment** — authorising a second phone, a laptop, a new device. A
   surviving authorised device exists.
2. **Recovery** — no authorised device survives.

## Decision

### Enrolment is pairing, not sharing

The new device generates a keypair; an existing authorised device wraps the
relevant space keys for its public key. That is §41 exactly as written, and the
server only relays ciphertext.

The channel is authenticated with a **short authentication string**: the old
device displays a QR code or a short numeric code, and the new device proves it
saw the same one. This is the pattern Signal, Matrix and 1Password all converged
on independently.

No secret sharing. No server trust. No recovery machinery. This covers the
overwhelming majority of real "I got a new device" events, and it ships with
Milestone 8.

### Recovery has one required mechanism and two optional ones

**Required — a recovery secret.** One high-entropy value, generated at account
creation, displayed once, stored by the user offline. Everything else is
optional on top of it. Unglamorous, and it is where 1Password, Apple and Signal
all ended up.

**Optional — SLIP-39 sharing of that secret.** Where a user wants `k`-of-`n`
across locations or people, use **SLIP-39** rather than a hand-rolled Shamir
split. Plain Shamir has a property that is disqualifying for a recovery
mechanism: a corrupted share does not fail loudly, reconstruction simply yields
garbage, and nothing indicates which share was wrong. SLIP-39 is a reviewed
specification with checksums, share metadata and passphrase support, already
deployed in the field. Getting the field arithmetic right is the easy half; the
operational details are where a bespoke scheme loses.

**Optional — an exported recovery blob.** A small encrypted file the user can
put anywhere: email to themselves, a cloud drive, a USB stick. Encrypted with a
key derived from the recovery secret, so the blob alone is useless.

This is a **durability** mechanism, not a confidentiality one, and must be
described that way. It survives a house fire, which paper does not. But an
export protected by a password alone converts "an attacker needs my device" into
"an attacker needs my email password", which for most people is a downgrade —
and mail providers retain copies in backups the user cannot delete.

### Recovery must not require Heyarr to be running

The chain is: recovery secret → root key → decrypt the exported blob → space
keys. Heyarr is optional at every step.

If the only path back runs through a peer, then losing every device and every
site is permanent loss — which defeats §51's recovery story at exactly the
moment it is supposed to apply.

## Consequences

**What is recovered is tiny.** Not the content: the ciphertext is already
replicated across peers. Only the account root key, which unwraps the
`wrapped_keys` the peers already hold (§79). The recovery artifact is a few
kilobytes, which is what makes every mechanism above cheap. That falls out of
§38's design rather than being something to engineer for.

**Enrolment requires no trust in the infrastructure.** Adding a device is
device-to-device with the server as a dumb relay, consistent with §38.

**Social recovery has a UX cliff worth respecting.** Share holders must keep the
share, remain reachable years later, and understand what they hold. For a
household — a spouse holding share two — that is realistic. For a scheme
depending on five acquaintances it is not, and pretending otherwise produces
recovery that fails precisely when it is needed.

**Device revocation is forward-looking only.** Losing a phone means re-wrapping
space keys under a new key and re-encrypting future content. It does not
retroactively protect what that device already read. Say so plainly rather than
implying otherwise.

## Alternatives rejected

**Escrowing shares to the user's own peers.** Tempting, since a three-peer
deployment looks like a natural quorum. But peers are exactly what the
encryption protects against, so a quorum of peers able to reconstruct the key
makes the vault theatre. The passphrase-gated variant — peer share plus a user
passphrase — is legitimate in principle, but without hardware enforcement a
self-hosted peer cannot rate-limit brute force the way an HSM-backed service
does. If it is ever offered it must be described as protecting against device
loss and *not* against the operator, who is usually the user; that may well be
acceptable, but it is a different guarantee and should be labelled as one.

**A password-derived recovery key with no secret.** Makes recovery convenient
and makes the vault about as strong as a password people will actually remember,
attacked offline. That is not the guarantee ADR-0021 claims.

**Server-side key escrow.** Would make all of this easy, and would make §38
false.

## Sequencing

Enrolment and the recovery secret ship with Milestone 8, before vaults exist,
because ADR-0021 must not ship without them. SLIP-39 sharing and the exported
blob can follow — they are additions to a working recovery path, not
prerequisites for one.

## What ratified this

**Accepted 2026-08-26, on evidence rather than on argument.** The condition this
record set for itself — that *enrolment* and *recovery* both work behaviourally,
not just as unit-tested primitives — is met. Both halves are now reachable from
the binary and proven in `make demo`, which is what moves this from `Proposed`
to `Accepted`:

- **Pairing enrols a device with no operator and no trusted server** (#305).
  An old device authorises a new one over a dumb relay (`internal/pairrelay`,
  ADR-0038): the two exchange public keys and a salt, derive the same short
  authentication string over both keys, and on a human match the old device
  signs the enrolment cert — the relay learns no key material. The
  **commit-before-reveal** ordering (`internal/pairing/commitment.go`,
  `internal/pairflow`) is what makes the short code's security real against a
  rushing attacker: each side commits to its key before either reveals, so a
  man-in-the-middle cannot choose its substituted key after seeing the peer's.
  The demo proves both the honest enrolment and that a mismatched code enrols
  nobody; a substituted key yields a different code.

- **The recovery secret reconstructs the identity offline** (#306). The
  identity is derived deterministically from the secret
  (`recovery.DeriveUserSeed`), so the secret alone rebuilds the *same* identity
  — the public key peers already pinned — on a machine with no surviving device
  and no running server, and the recovered device then authenticates as the
  same user. A mistyped secret is refused by its checksum
  (`recovery.ParseSecret`) rather than reconstructing a different, wrong
  identity. This is the "no authorised device survives" branch of the Decision,
  made real.

What remains is explicitly *additive* and does not gate this record (see
Sequencing): SLIP-39 share splitting for social recovery, and the exported
recovery blob. This ADR is accepted on the base secret path both problems are
built on, exactly as the Decision separated enrolment from recovery.
