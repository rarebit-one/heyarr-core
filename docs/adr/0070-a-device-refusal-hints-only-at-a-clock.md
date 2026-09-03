# 0070. A device refusal hints only at a clock, a proof's life is capped, and a session dies with its approver

**Status:** Accepted (2026-09-03)
**Date:** 2026-09-03

## Context

Three things surfaced when a REAL client landed on the device scheme — the
byte-exact voidbind-go port in `heyarr-mobile` (#420, rarebit-one/heyarr-mobile#7)
— rather than when the scheme was designed. All three are the same shape: a
mechanism whose only caller was a test, meeting a caller with a battery, a
biometric prompt and a drifting clock.

**1. Every device refusal looked identical.** `rejectCredential` answers one
opaque 401 for all of them, by design: "no such user" and "bad signature" must be
indistinguishable, or an unauthorised caller can map who is enrolled here by
probing. But `docs/design/mobile-client.md` §1a tells a phone to re-mint on
`expired` / `not_yet_valid` and never fail hard — and with no way to tell those
from anything else, the client re-mints and retries on EVERY 401, including a
revoked device and a wrong cert, where retrying is pure noise against a server
that will never say yes.

**2. Nothing capped a possession proof's TTL.** `enrolment.VerifyPossession`
honours whatever window the device signed. The reference signer uses
`PossessionTTL` (two minutes); nothing rejected a proof minted for an hour, and
`heyarr-mobile` was reusing one for up to an hour precisely because each signature
costs a biometric on a sealed key. The proof is stateless by design, so its window
IS its replay window (voidbind-go says so in as many words): a proof captured off
a compromised channel is reusable until it expires.

**3. A web-login session outlived its approver.** A session pins the approving
`DeviceKey` (ADR-0053), and `sessionIdentity` never re-read it. Revoking a device
left every session it had already approved authenticating until its own expiry.
Post-ADR-0065 a session is read-scope, so the exposure is bounded — and revoking a
device is the one action an operator takes when they believe it is in someone
else's hands, which is exactly when "bounded" is not the reassurance it sounds
like.

## Decision

**A refusal hints at a clock, and at nothing else.** The two clock-window
refusals — and only those — set a header:

```
WWW-Authenticate: Device error="expired"
WWW-Authenticate: Device error="not_yet_valid"
```

The body is unchanged and stays opaque. This discloses nothing, and that is the
whole test the exception has to pass: the caller already knows the window it
signed, so it learns only that this server's clock disagrees with its own — a
fact it could derive from its own watch. Every other refusal (unknown user,
unknown device, revoked, cert mismatch, bad signature, wrong cert, and the TTL
policy refusal below) carries no header at all. The client contract becomes:
**re-mint and retry once when the header is present; treat a bare 401 as
terminal.**

**A possession proof may not grant itself more than ten minutes** (raised to one hour on 2026-09-03 until the phone client can re-mint without a biometric — heyarr-core #444 — then back to ten).
`deviceauth.MaxPossessionTTL = 10m`, checked on `exp - iat` — the window the
device CLAIMED — inside `verifyPossession`, after the signature is verified, so
both the authentication path and self-enrolment (ADR-0067) inherit it and neither
can be reached with an unsigned proof. A CEILING rather than a fixed value,
because a hardware-key client has a real reason to batch beneath it; five times
the reference TTL, so batching is genuinely useful, and short enough that a stolen
proof is stale quickly. Judged on the claimed window and not on the remaining
time, because an hour-long proof looks perfectly ordinary nine minutes before it
expires. Refused with the plain opaque 401 under its own metric label
(`device_possession_ttl_too_long`) — a policy refusal, not a clock one, so no hint:
a client that re-minted the same over-long proof would loop.

**A session is only as live as the device that approved it.** The HTTP layer
takes an optional `DeviceMembership` (satisfied by `deviceauth.Store`), and the
session path re-checks the pinned approving key on every request. It fails
CLOSED — the opposite direction from `managementAuthorized`, deliberately: that
one decides whether to GRANT more than the floor, so an unanswerable question
keeps the caller at the floor; this one decides whether the credential is valid at
all, and an unanswerable question there must not be resolved in the caller's
favour. A session naming no approving device is refused for the same reason. With
no membership checker wired, the path is exactly what it was — the state of a
deployment with no identity store to revoke in.

## Consequences

- The 401 is no longer undifferentiated, in exactly one axis, and the doc a
  client is written against now says which refusals are worth retrying. The
  reconnaissance surface is unchanged: the header distinguishes two states a
  caller can already tell apart from its own clock.
- The stateless proof's replay window is bounded by the SERVER, not by the
  politeness of the client. A client that wants fewer biometric prompts batches
  under the ceiling; one that wants an hour is refused by name.
- Revocation is now immediate on both credential paths, not just the device one.
  A revoked device's sessions stop on their next request.
- **Revisit if** a client appears for which ten minutes is genuinely too short —
  the honest fix then is a server-issued challenge (voidbind-go already records it
  as the hardening path), which shrinks the replay window to zero at the cost of a
  round trip and per-request state, not a longer ceiling.
- **Revisit if** a second refusal is ever proposed for the hint. The test is not
  "would it be useful to the client" — it is "does it disclose anything about
  identity". `expired` and `not_yet_valid` pass it; "revoked" does not, and the
  usefulness of "revoked" to an honest client is exactly its usefulness to a
  dishonest one.

---

*Provenance: filed as #420 from the heyarr-mobile device-credential landing
(rarebit-one/heyarr-mobile#7). Implemented in the same change as this record.*
