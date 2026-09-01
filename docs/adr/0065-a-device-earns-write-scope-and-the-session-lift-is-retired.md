# 0065. An enrolled device earns write scope; the session-lift is retired

**Status:** Accepted
**Date:** 2026-09-01
**Supersedes the interim of:** ADR-0061 (follow-management grant)
**Builds on:** ADR-0048 (device-cert enrolment), ADR-0053 (web-login session), ADR-0011 (bearer scopes)

## Context

Two credentials reach the write surface today, and they are not equal:

- A **device credential** (ADR-0048): an enrolled device presents a user-signed
  cert plus a *per-request possession proof*, verified offline against a pinned
  user key. It cannot be replayed (the possession proof is time-bound) and it is
  bound to a hardware-held key. Yet `authenticateDevice` minted it **read-only** —
  the strong credential carried the least authority.
- A **web-login session** (ADR-0053): a browser or TV holds an *opaque, replayable
  bearer token* after a QR login. ADR-0061 added an interim lift: an admin
  "grants" an approving device key, and every session that device approves is
  lifted to **write**. This was explicitly temporary, "until the voidbind-client
  artifact lands" — its own code comment named device-cert enrolment as the
  convergence.

The voidbind-client artifact has landed. Keeping both paths would cap the write
door's strength at the *weaker* credential: a replayable session token opening the
same door as a hardware-bound device makes the hardware binding worthless. Time to
converge, and to converge by **subsuming** — not by adding a third path beside the
two.

## Decision

### 1. An enrolled, authorised device earns write via its own credential

`authenticateDevice` now grants `write` when the device's key is authorised
(`ManagementAuthorizer`), and the read floor otherwise. This rests write on two
independent facts: the device is **enrolled** (proven by the credential verifying
at all — an unenrolled or revoked device authenticates as nobody) and an admin has
**authorised** it. The authorization lookup fails **closed**: any error keeps the
device on the read floor.

### 2. The web-login session-lift is retired — a session never writes

`sessionIdentity` carries the read floor, always. A session is a replayable bearer
token, and a replayable credential must not reach write now that a non-replayable
one can. A browser or TV that needs to manage the library does so **from an
authorised device presenting its device credential**, not by lifting its own
session. This is the whole of the subsume: the weaker path is removed, not left
coexisting.

### 3. The grant becomes the durable device-write-authorization

The admin surface (`POST/DELETE /session/management-grants`, admin-scoped) and its
store are kept — but repurposed. It no longer lifts sessions; it authorises a
**device key** for the write the device's own credential now carries. It is
inherently enrolment-gated: only an enrolled device can present the credential this
authorises, so an authorization for a key that is not enrolled is **inert** — never
a way in — rather than a hole. (Refusing to record an authorization for a
not-yet-enrolled key is a possible nicety, not a security requirement.) Revocation
needs no cascade: a revoked device fails at authentication (the verifier reads the
store every request), so its authorization is dead the instant the device is.

### 4. Break-glass, and the read floor, are preserved

An **admin-scoped bearer token** (ADR-0011, issued out of band) always carries
write and is how the first device is enrolled and authorised — so retiring the
session-lift can never lock an operator out. Every other credential — an
unauthorised device, and *every* session — stays at the **read floor** by default.
A shared TV, and a browser, read but do not write, which is exactly the guest/
read-only stance the surface wants.

## Consequences

- Write now requires a hardware-bound, non-replayable credential + an admin
  authorization. The strong credential carries the authority; the weak one cannot.
- The interim (ADR-0061) is retired, not carried as debt — one write path, one
  revocation story, one thing to audit.
- The just-merged client read-only surfaces (heyarr-mobile #5, heyarr-tizen #2)
  still hold: a session's `can_write` is `false` and the client shows read-only.
  What changes is the *follow-up*: to actually write, a client presents its
  **device credential** (the seam swap), not its session — the "authorise this
  device → then manage from it" flow, rather than lifting the session in place.
- This is the device identity the guest-mode/profiles design (§5) delegates
  through: the same enrolled-device primitive that earns library write is what a
  profile trusts to unlock its personal state on a shared TV.

## What would make us revisit

- **Granularity.** Authorization is per-device today (this phone writes, that TV
  does not). If a household wants "any of this user's devices write", a per-user
  authorization is a small addition behind the same seam.
- **Refusing inert authorizations.** Gating the grant endpoint on enrolment (refuse
  to authorise a key no device has enrolled) is a clarity nicety; security does not
  need it because the authorization is inert without enrolment.
- **The client follow-up.** heyarr-mobile presenting a device credential for write
  actions (not its session bearer) is the client half of this convergence, tracked
  with the seam-swap work; until it ships, write happens via an admin token or a
  device that already presents a credential.
