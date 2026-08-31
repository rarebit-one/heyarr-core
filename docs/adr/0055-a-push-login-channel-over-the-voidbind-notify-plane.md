# 0055. A push-login channel over the Voidbind notify plane

**Status:** Accepted
**Date:** 2026-08-31

## Context

ADR-0053 stood up heyarr's QR web-login: a browser or a television that holds no
credential shows a `voidbind:login?rp=&id=` QR, an enrolled device approves the
one-time challenge offline against a pinned user key, and the broker mints a
short-lived session token. That ADR named push as the *same mechanism* deferred to
keep the broker change tight — the QR and a push deliver the **identical** opaque
tuple to the approving device, and everything after the tuple reaches the phone is
byte-identical.

`voidbind-go` **v0.5.0** (bumped from v0.4.0 here) lands the `notify` plane that
push needs, with no drift to the v1 QR API the broker already consumes:

- a cert-authenticated **subscription registry** (`notify.Handler`, `POST`/`DELETE
  /v1/subscriptions`) — an enrolled device registers the ntfy/UnifiedPush wake
  endpoint its phone listens on, authenticated by its enrolment cert against the
  RP's **pinned trust set** (package `rp`), so only an enrolled device may
  subscribe and the subscription is bound to the authenticated `(user, device)`;
- a `WakeChannel` transport seam with an `NtfyChannel` (ntfy/UnifiedPush)
  implementation, plus the hardening slice's delivery-ack (`Receipt`), fail-closed
  per-user rate limiter, and a durable `FileStore`;
- a `Notifier` whose `Enqueue(user)` fans an **opaque** `voidbind:login?rp=&id=`
  ping — byte-identical to the QR — to that user's subscribed devices.

The load-bearing property is that the ping is a **wake signal, not a crypto
path**: it carries only the public `(rp, id)` tuple, never a cert, a challenge, a
match number, or any secret. The woken phone still pulls the real challenge from
the RP over TLS and signs it hardware-gated (`weblogin`); the notify plane never
touches a key. heyarr becomes a relying party of this plane the same way All Thing
did first (its ADR-0009), reusing the exact pinned trust that already backs the
Device scheme (ADR-0048) and the QR broker (ADR-0053).

## Decision

heyarr adds a push channel alongside the QR, over the **same pinned identities**
(`internal/deviceauth`) the cert authenticator and the login broker already use.
It is **additive and fail-open**: the QR stays the primary, decided channel, and a
push failure or an unsubscribed user never blocks a login.

- **Subscription registry, behind the plane's own cert auth.**
  `weblogin.SubscriptionRoutes` wraps `notify.Handler{Registry: {Store, Trust}}`
  and mounts at `/v1/subscriptions` on the **unauthenticated** router — the same
  place `/login` and `/signin` mount, and for the same reason: the registry
  verifies the device's enrolment cert against the pinned trust set *itself* on
  every register/unsubscribe, so an un-pinned or bad cert is a 401 and the
  subscription's `(UserID, DeviceKey)` come from the verified cert, never from
  client fields. `Trust` is heyarr's `userTrust` — the exact adapter the broker
  verifies a login against. `Store` defaults to an in-memory `notify.MemStore`
  (homelab-scale, rebuilt as devices re-register on app open); a `notify.FileStore`
  may be injected for a deployment that wants the address book to survive a restart.

- **Push on login initiation.** `loginInitPush` wraps the weblogin routes so a
  successful `POST /login` (a login *initiation*) also wakes the pinned users'
  subscribed devices. It **tees** the create response to read the minted login id
  without altering what the browser receives, and fires the wake **after** the
  response is written — push is additive to the QR, on nobody's critical path. The
  `/login/{id}` sub-routes (poll, challenge, approve) pass straight through; only
  the initiation wakes.

- **User addressing — resolved per initiation.** A QR login is user-agnostic at
  initiation (any pinned device may approve), so heyarr wakes **every pinned
  user's** subscribed devices. Unlike All Thing (single-user, a static pinned list
  at construction), heyarr resolves the pinned users from `deviceauth.ListUsers` on
  *each* initiation, so a user enrolled or revoked through the API is woken (or
  not) on the very next login without a restart. `Enqueue` is a no-op for any
  pinned user who never registered a subscription — exactly "only push to users who
  registered".

- **ntfy transport, config-driven, no live server required.** The wake channel is
  `notify.NtfyChannel` (the only background push channel Voidbind ships —
  Android/UnifiedPush; iOS/APNs stays deferred, foreground-only with QR fallback).
  A device registers its **full** ntfy topic URL as its subscription endpoint, so
  the channel POSTs there directly; `HEYARR_NOTIFY_NTFY_BASE_URL` records the
  deployment's self-hosted ntfy origin for operators and is logged at startup. The
  ntfy **server deployment is a separate ops task (deferred)** — heyarr runs and the
  plane is exercised in tests without it, via a fake `WakeChannel`.

- **Mounted only when the broker is.** The push plane exists exactly where the QR
  broker does — a node that can name an origin a device can dial back
  (`renderBaseURL`). A loopback- or socket-only node mounts neither.

## Consequences

- A browser can be approved by a phone buzz instead of a scan, end to end, using
  only the pinned identities already in the registry. Proven by unit tests against
  the real `Handler`: a subscribed user's login-init fires exactly one Enqueue
  (captured via a fake `WakeChannel`); an unsubscribed user fires none and does not
  error, and the login still returns its QR; the pushed ping decodes to exactly
  `(rp, id)`, byte-matches the QR, and leaks no cert / key / signature / nonce
  (`TestPushPingIsOpaque`); the `/v1/subscriptions` routes register and unsubscribe
  behind cert auth and refuse an un-pinned cert 401.
- heyarr is a relying party of `voidbind-go/notify`, over the same trust core it
  already shares for `rp.Verify`.
- No session/authZ path is touched: the push plane is orthogonal to ADR-0053's
  token issuance — it changes how a login is *initiated*, not what an authenticated
  principal may do.
- The parity guard (ADR-0015) is unaffected: like `/login` and `/signin`, the
  `/v1/subscriptions` routes mount only on a node that names a reachable origin, so
  they are outside the parity walk's loopback fixture and the hand-written OpenAPI —
  consistent with the QR broker.
- **Deferred:** the self-hosted ntfy server deployment (ops); binding weblogin
  approval to device revocation (ADR-0053's known limitation — a revoked device
  whose user is still pinned can still approve until the user is unpinned), now with
  a push path that could carry a post-verify hook; iOS APNs (foreground-only, QR
  fallback — `notify`'s own deferral); a rendered QR *image* on `/signin` for a TV.

Refs: #393; voidbind-go v0.5.0 (`notify` plane); ADR-0048 (Device scheme),
ADR-0053 (QR web-login), All Thing ADR-0009 (first relying party of the plane).
