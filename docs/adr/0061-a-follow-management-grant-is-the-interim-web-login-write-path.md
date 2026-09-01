# 0061. A follow-management grant is the interim web-login write path

**Status:** Accepted (2026-09-01) — M12, Phase 1 completion
**Date:** 2026-09-01

## Context

Following a source is ordinary operator traffic, so `POST`/`DELETE
/api/v1/followed-sources` require the `write` scope (ADR-0011, §55). But a
browser or a television does not hold a service token: it logs in the WhatsApp-Web
way, showing a `voidbind:login` QR that a device the account owner enrolled
approves, and the broker mints it a session token (ADR-0053). That token is minted
**read-only** — `sessionIdentity` carried only `ScopeRead`, the same floor an
authenticated user device holds — because a QR login authenticates *who* the
browser acts as, not *what authority* the surface should have. A shared surface
(a television in a living room) logging in as the account owner must not thereby
gain the authority to change what the household archives.

The consequence is that today a client on a **trusted personal device** — the
one surface that legitimately should manage subscriptions — cannot follow or
unfollow at all: every write 403s. The client work that consumes the
followed-sources API (built in parallel) is blocked on a way for such a device to
obtain write.

The eventual answer is **device-cert enrolment (ADR-0048)**: the surface enrols
its own device, presents a cert, and authorises as itself — authority bound to
the surface. That path is gated elsewhere on the voidbind-client artifact and is
not yet available to a browser/TV. We need an interim path now, and it must not
weaken the read-only-by-default posture a shared surface depends on.

## Decision

Add a **follow-management grant**: an operator-issued, per-device authorization
that lifts a web-login session from the read floor to `write`.

- A grant names an **approving device key** — the only stable identity a web-login
  session carries (the device that signed the challenge, surfaced as the session
  principal's `device_key`). It is stored in `session_management_grants`
  (single-writer control-plane state, invariant 5) and its transitions emit
  `identity.device.management_*` events (invariant 7).
- On a **session**-authenticated request, the auth layer consults a
  `ManagementAuthorizer` for the approving device key. A grant lifts that session
  to `{read, write}`; no grant keeps it at `{read}`. The lookup **fails closed**:
  no authorizer wired, or an error, keeps the session read-only. It is consulted
  **only on the session path** — a service token and a device credential are
  untouched, keeping their exact existing paths.
- The grant is issued and revoked by an **operator** (`admin` scope), like a token:
  `POST`/`DELETE /api/v1/session/management-grants`. Granting is idempotent.
  `GET /api/v1/session` lets any caller read its own `kind`, `device_key`, `scopes`
  and `can_write`, so a client can discover it is read-only and surface the
  `device_key` an operator must authorise.

A client drives it as: read `/session` → if `can_write` is false, show the
`device_key` and prompt the operator → operator `POST`s a grant → read `/session`
again, now `can_write`. Revocation is per-request, so it takes effect on the very
next request, not at a token expiry.

## Why keyed on the approving device

A web-login session carries exactly one authorisable identity: the approving
device's key. That is what an operator can name, so the grant keys on it, and the
default (no row) is read-only. This has a **sharp edge, recorded here honestly**:
the grant follows the *approver*, not the *surface*. A television whose login was
approved BY a granted device would inherit write. In practice a household grants a
personal phone and approves a shared TV's login from a different, ungranted device
(or, later, the TV enrols its own cert); the interim is safe under that discipline
and the convergence removes the edge entirely.

## Consequences

- A trusted personal device can manage followed sources today, unblocking the
  client wiring, without waiting on device-cert enrolment.
- The read-only-by-default posture holds: a surface writes only after an explicit,
  operator-issued, per-device authorization.
- This is **interim and non-gated** (it does not wait on the voidbind-client
  artifact). **ADR-0048 device-cert enrolment is the eventual convergence**: when
  a browser/TV can present its own device identity, authority binds to the surface
  and the approving-device grant is retired. The grant table and its events are
  additive and cheap to remove when that lands.

## What would make us revisit

- Device-cert enrolment reaching the browser/TV surface (retire this path).
- A need to scope a grant more tightly than per-approving-device — e.g. a
  phone-driven "elevate THIS session" re-approval (a scoped challenge over the
  weblogin broker), if the approver-not-surface edge proves to bite in practice.
