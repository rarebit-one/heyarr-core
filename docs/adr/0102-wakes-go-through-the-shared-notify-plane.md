# 0102. Wakes go through the shared notify plane, and heyarr holds no subscription registry

**Status:** Accepted
**Date:** 2026-10-01
**Supersedes in part:** [ADR-0055](0055-a-push-login-channel-over-the-voidbind-notify-plane.md) (the embedded registry and the ntfy channel; the push-on-initiation behaviour stands)

## Context

ADR-0055 embedded the Voidbind notify plane in heyarr. heyarr mounted its own
`/v1/subscriptions` registry and ran an in-process `notify.Notifier` over an ntfy
channel. ADR-0098 then used the same notifier to wake a phone for a
cruciform-offload unwrap.

A phone subscribes to exactly **one** notify base: the shared, standalone
`void-which-binds-notify` plane that its settings name. It never subscribes to a
relying party. So heyarr's registry never had a subscriber. The login push and
the unwrap wake reached zero devices, and only the QR and the LAN-direct paths
worked (void-which-binds-go#86). The shared plane already serves any relying
party, because the ping it builds carries `rp_base`. It exposes bearer-gated
`POST /v1/enqueue` (login) and `POST /v1/enqueue-unwrap` (unwrap), and
void-which-binds-go now ships `notify.EnqueueClient` to call them.

## Decision

heyarr is a **client** of the shared plane and no longer embeds one.

- `notify.url` names the plane. The enqueue bearer comes from
  `notify.enqueue_token_file`, or else from `HEYARR_NOTIFY_ENQUEUE_TOKEN`. It is
  never a configuration key, so `config print` still holds no secrets. A
  configured URL with no bearer stops startup. With no URL set, push is off.
- A login initiation calls `/v1/enqueue` once per pinned user, with
  `rp_base` set to this node's origin. The browser's response is flushed first,
  and the wake is bounded and fail-open, so the QR stays the primary channel.
- `/v1/unwrap-wake` still authenticates the desktop by its enrolment cert and
  possession proof, unchanged from ADR-0098 and #685. It then spends the
  plane's `/v1/enqueue-unwrap` for the cert's user. The desktop never holds the
  bearer. With no plane configured it answers `woken: 0`, as for an
  unsubscribed user. A plane failure is a 502, as before.
- Removed: heyarr's `/v1/subscriptions`, the in-process notifier and ntfy
  channel, `notify.ntfy_base_url`, and the registry's bare-cert allowance. The
  bare-cert compatibility on `/v1/unwrap-wake` **stays**. It exists for heyarr
  desktops from before void-which-binds-go v0.18's possession proofs (heyarr v0.5.3, #685), not for phones.

## Consequences

- Push login and the unwrap wake reach the devices that actually subscribed.
- heyarr's surface shrinks: it has no subscription store, no wake transport, and
  no device-facing registry to keep in step with the library.
- heyarr now holds one shared secret, the plane's enqueue bearer. A leaked
  bearer lets its holder send opaque wakes to known user ids. Those wakes carry
  no authority: a woken phone still pulls and signs the real challenge.
- An operator must point heyarr at a plane and provide the bearer. An upgraded
  node that has neither keeps working QR-only.

**Revisit if** phones subscribe per relying party, or if the plane gains
per-relying-party credentials. heyarr would then take its own bearer, but
nothing else here changes.

Refs: void-which-binds-go#86; ADR-0053, ADR-0055, ADR-0098.
