# 0068. Membership ops replace root-only certs

**Status:** Accepted
**Date:** 2026-09-02
**Amends:** ADR-0048 (device-cert authentication), ADR-0049 (a space key is wrapped for a device encryption key), ADR-0067 (a paired device enrols itself)
**Builds on:** voidbind-go ADR-0007 (membership op-set: any member admits or removes); ADR-0032, ADR-0065, ADR-0066

## Context

Until now a device authenticated by ONE cert the user's genesis key had signed
for its device key (ADR-0048), and a peer honoured it only after that device
was enrolled here (ADR-0067's self-enrolment, or the admin route). The genesis
key was therefore the only thing that could admit a device: the machine
holding the recovery secret enrolled every phone, a phone could never invite a
second phone, and the secret was handled far more often than "recovery only"
implies.

voidbind-go ADR-0007 changes the identity model. An identity is a **set of
device keys** evolved by signed **ops** — `add` / `remove` — that any current
member may issue. `usr` stays the genesis public key, so heyarr's pin
(`user_identities.public_key`) is unchanged. A v1/v2 cert *is* a v3 add with
`by = usr, prev = []`: nothing already issued is reissued. Evaluation is a pure
function of the op set and the clock (a state-based CRDT: merge is set union
by hash; remove wins causally; seniority resolves concurrency), so every
relying party — heyarr, All Thing, the notify plane — computes the same view
from the same ops. What it costs is one piece of state: an RP must **keep the
ops it has seen**, because a member-signed op is judged only against its own
causal past (`prev`), and a device ships the ops it knows beside its
credential so a first contact can be judged.

## Decision

**heyarr evaluates an identity's device set from a persisted op log rather than
verifying one root-signed cert. `device_identities` becomes the materialised
view of that log. Devices and the node exchange ops over the wire.**

1. **The op log is `membership_ops`** (migration `00022`, filling the lowest
   reserved gap): a G-set keyed by op hash — insert-only, idempotent, cascade
   on unpinning the user. `deviceauth.Store` implements voidbind-go's
   `rp.Membership` over it (`Ops` / `RecordOps`), and every authentication
   runs `rp.Verifier{Trust, Membership}.Verify(credential, presented, now)`:
   the pinned genesis key selects the identity, `enrolment.Evaluate` runs over
   the stored ops ∪ the presented ops ∪ the credential, the accepted ops the
   node had not seen are recorded, and the credential's device must be a
   current member. The node never signs an op. It evaluates; it does not
   author.

2. **`device_identities` is the view, not the truth.** `RecordOps` reconciles
   it in the same transaction: a member with no row gets one (unnamed; `cert`
   = its admitting op token, `encryption_key` = what the add bound,
   `expires_at` = the add's expiry); a renewal refreshes those columns; a
   device the evaluation removes is tombstoned (`revoked_at`) exactly as the
   admin's `RevokeDevice` tombstones it, under its own event type
   (`identity.device.removed`). The reconciliation never clears an existing
   `revoked_at`. Listing, naming, the encryption key (ADR-0049) and the write
   scope (ADR-0065, still keyed on the admitted device key) all keep reading
   the view; only authentication reads the log.

3. **The admin's tombstone stays local and is NOT an op** (`RevokeDevice`,
   `DELETE /identities/devices/{key}`). The node is not a member of the
   identity; its word does not travel. A tombstoned device is refused however
   many valid adds the log holds for it, and a member-signed remove is what
   tells the *other* RPs and devices. Revoking a user still deletes the pin,
   and with it (cascade) the log.

4. **The wire.** `Authorization: Device <op>~<proof>` survives — the cert slot
   holds the admitting op, the possession proof still binds to
   `sha256(token)`. Every Device-scheme request may carry
   `Voidbind-Membership: <op>,<op>…` (≤ 64), merged into the evaluation, so a
   device admitted by a phone this node has never met authenticates on first
   contact. Possession is verified **before** the evaluation, because the
   evaluation writes: a caller who cannot prove the key leaves no row and no op
   behind.
   - `POST /enrol {cert|op, proof, name, ops?}` — ADR-0067's route, backward
     compatible (`cert` and `op` are the same slot; `ops` is the header's body
     form). Under this ADR the evaluation materialises the row, so what
     `/enrol` adds is the **name** and the created/existing answer (201/200).
   - `GET /membership/{usr}` → `{usr, ops:[…]}`, every op token the node
     holds; `POST /membership/{usr} {ops:[…]}` → the same plus `recorded`.
     Both public, both fail-closed and opaque like `/enrol`: junk anywhere in
     a push is one 401 and nothing is written; an unpinned identity is 404.
     This is how a device learns that a member removed it (or another device)
     while it was offline, and how a phone that removed a device tells the
     node.
   - The self-enrolment body cap rises from 16 KiB to 96 KiB to hold an op
     list; it is shared by the membership routes.

5. **Legacy rows are backfilled at startup** (`BackfillLegacyCerts`, once,
   idempotent): a device enrolled before this ADR has a v2 cert in `cert`,
   which is a genesis add, and recording it means `GET /membership` speaks for
   the device before it next calls — and a second device that cites it as
   `prev` is judgeable.

6. **Behaviour that changes.** A pinned user's genesis-signed cert now
   authenticates on **first contact**, with no enrolment step — the pin is the
   trust root (ADR-0032) and the add is the admission. Before, an unenrolled
   device was `ErrUnknownDevice`. What still refuses: an unpinned user, a
   removed device, an expired add, an add citing a past the node cannot see
   (send the header), the admin's tombstone, and every possession refusal.
   The web-login broker (ADR-0053) and the notify plane's subscription
   registry evaluate over the same log, so a member-signed remove also ends
   the device's ability to approve a QR login or hold a wake endpoint; the
   admin's tombstone is still not consulted there (the ADR-0053 note stands).

## Consequences

- Pairing no longer needs the secret: a phone admitted by the Mac's genesis
  add can admit the next phone (voidbind-go ADR-0007 §pairing), and that phone
  reads here on first contact by presenting both ops. The secret returns to
  recovery-only.
- `RevokeDevice` grows an ADR-0049 hook: revoking (or learning the removal of)
  a device re-wraps the spaces it could unwrap — `space rotate --revoke` for
  that one device, no descendant walk (tracked as the next PR).
- The golden vectors (`internal/deviceauth/testdata/vectors/membership/`,
  copied from voidbind-go v0.9.0) are replayed through the store: recording,
  reading back, evaluating and reconciling the view must reproduce every
  vector's members, removals, heads and ineffective ops. A divergence there
  is a port defect, never a flaky key.
- The node holds state per identity it pins. That is the one property the
  stateless cert model had and this gives up — accepted for no depth cap, no
  cascade, no re-rooting, and pairing without the secret (plan §2).

## What would make us revisit

- **A quorum rule for removes** (ADR-0007's reserved `cosig`, v1.1). The log
  and the routes do not change; `Evaluate` does, in voidbind-go.
- **The admin tombstone as an op.** If the node were ever made a member (a
  node key admitted into the identity), `RevokeDevice` could sign a real
  remove and the local tombstone would retire. Today the node is deliberately
  not a member.
- **A per-user "any device writes" stance** (ADR-0065's trigger) — unchanged
  by this ADR; enrolment still grants the read floor only.
