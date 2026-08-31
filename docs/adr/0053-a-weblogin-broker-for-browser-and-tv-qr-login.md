# 0053. A weblogin.Broker for browser/TV QR login (and, later, push)

**Status:** Accepted
**Date:** 2026-08-31

## Context

heyarr authenticates two kinds of caller today. A service presents a bearer token
(ADR-0011). A *user's device* presents the Device scheme (ADR-0048): a user-signed
enrolment cert plus a possession proof, verified offline against a pinned user key
by `internal/deviceauth`, which reuses the shared `voidbind-go/rp` trust core. Both
require the caller to *hold a credential* — a token, or a device key.

A **browser** and a **television** hold neither. They have no bearer token to type
and no device key to sign with, so there has been no way for one to authenticate.
That is the gap this record closes, because the consumption clients the program is
building — a signed-in web player, a TV that browses the library — are exactly
these credential-less surfaces.

The pattern is not new and we do not invent it. `voidbind-go/weblogin` is the
WhatsApp-Web QR login, and All Thing already stood it up end to end (its ADR-0006):
a browser shows a `voidbind:login?rp=&id=` QR, a device the account owner enrolled
scans and approves it with a hardware-gated Ed25519 signature over a one-time
challenge, and the relying party mints a short-lived session token the browser then
carries. All Thing is the worked example; heyarr's only novelty is that it already
owns the trust set — the same pinned users `deviceauth` authenticates device certs
against — so a QR login is honoured for exactly the users a device credential is.

## Decision

**heyarr stands up a `weblogin.Broker` over its existing pinned-user trust, mounts
the QR login routes on the unauthenticated router, and accepts the token the broker
mints as a Bearer credential on the authenticated API. This is heyarr's ADR-0006
analog.** `internal/api/weblogin` is the mount and the DB-backed trust adapter;
`voidbind-go/weblogin` does the challenge minting, the offline cert verification and
the token issuance.

### The trust set is the one deviceauth already pins

The broker is built with `Trust` = an `rp.TrustStore` backed by heyarr's
`deviceauth.Store` (`userTrust`): a user is pinned iff `LookupUser` finds it. This
is deliberate and is the whole reason heyarr's stand-up is small — there is no
second identity source, no login-only membership. A device the account owner
enrolled (the phone that holds the user's key, ADR-0048) is exactly a device that
can approve a QR login, because both resolve against the same pinned user. The
challenge's `Audience` is this node's external origin (`renderBaseURL`), so an
approval for this node cannot be replayed at another.

### The login routes are public; the minted token is not

`POST /login`, `GET /login/{id}`, `GET /login/{id}/challenge`, `POST
/login/{id}/approve` and a static `/signin` page mount **outside** `/api/v1` and its
bearer guard — like the renderer (ADR-0040), the pairing relay (ADR-0022) and the
Subsonic/OPDS/DLNA adapters. A browser starting a login has no credential to
present, and the endpoints grant no authority on their own: an approval is a
hardware-gated signature verified offline against a pinned key, and a token is
issued only after one lands. The token the broker then mints is a normal
short-lived session credential the browser carries as `Authorization: Bearer
<token>`.

### The session token is a first-class Bearer credential, read-scoped

heyarr's `authenticate` middleware gains a third path, mirroring the Device scheme's
opt-in shape. A bearer value is tried against the primary token verifier first; only
if that declines it is it offered to the broker's `SessionValidator`. On a hit the
browser or TV acts as the pinned user its device approved for, carrying the baseline
`read` scope — the same authority an authenticated user *device* holds
(`authenticateDevice`). A QR login is for browse-and-stream; anything finer is a
capability grant (ADR-0048, `internal/grant`), not a scope. The seam
(`SessionValidator`) is nil-disabled exactly like `DeviceVerifier`: a node that
names no external origin (loopback- or socket-only) mounts no login and wires no
validator, and a session token there falls through to a 401 like any other
unrecognised bearer value — the same posture as minting no renderer URL.

### QR is the channel that ships; push is the same mechanism, later

Both QR and push deliver the *identical* `voidbind:login?rp=&id=` tuple to the
approving device; everything after the tuple reaches the phone is byte-identical.
QR delivers it by camera. Push (a wake ping) is deferred: it needs the `voidbind-go`
notify plane (v0.5.0) and a Singpass-style **number-matching** step to replace the
origin-binding the QR's visual channel provides for free. heyarr stays on
`voidbind-go v0.4.0` and ships the QR broker; the push-enqueue-on-login-init is a
tracked follow-up, not part of this record's behaviour.

## Consequences

- A browser or television goes from unauthenticated to holding a session token an
  enrolled device approved, and that token reads the authenticated API — proven by
  `internal/api/weblogin` (end-to-end mint → validate through the real routes and
  signatures) and by `internal/api/http` (the middleware accepts a live session
  token as Bearer, 401s an unknown one, and ignores the scheme with no validator).
- heyarr now consumes `voidbind-go/weblogin` — the first use of that surface here.
- The `/signin` page shows the login *URI* (the exact bytes a QR encodes) and polls;
  a rendered QR *image* is a deferred nicety, exactly as All Thing's ADR-0006
  initially deferred it. It matters for a TV and is a tracked follow-up.
- **Known limitation, stated:** the broker pins on the USER (like All Thing's rp
  trust) and does not consult heyarr's device-revocation table, so a revoked device
  whose user is still pinned can approve a QR login until its user is unpinned. The
  minted token is short-lived and read-only; binding weblogin approval to device
  revocation is a tracked follow-up.

## What would make us revisit this

- **Push (v0.5.0).** The notify plane and number-matching turn the QR-only broker
  into QR-and-push. It reuses this broker unchanged — only the inbound trigger and a
  challenge-v2 step are new — so it is an addition, not a reversal.
- **A real QR image on `/signin`.** A TV cannot be handed a URI to copy; it must
  show a scannable code. This is presentation, not trust, and does not touch this
  record's decision.
- **Device-revocation-aware approval.** If a revoked device approving a read-only
  login proves unacceptable, the broker needs a post-verify hook the pure `rp` trust
  interface cannot express today — a change to `voidbind-go`, tracked separately.
- **Grant-scoped sessions.** A shared TV should be a read-only kiosk scoped to what
  it may show (ADR-0048 grant), not a full read session. The `SessionPrincipal`
  already carries the approving user and device key so a route can enforce that; the
  policy of minting a scoped grant at login is future work.
