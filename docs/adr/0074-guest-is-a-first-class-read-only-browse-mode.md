# 0074. Guest is a first-class read-only browse mode over the shared library

**Status:** Accepted
**Date:** 2026-09-04
**Builds on:** ADR-0011 (scoped bearer tokens, read/write/admin), ADR-0020 (managed / linked / vault assets), ADR-0024 (one consumption-session model), ADR-0053 (a web-login session acts at the read floor), ADR-0065 (a device earns write; a session never lifts)

> This ADR number is **0074**. 0073 is left for a concurrent change already in
> flight; the ADR sequence tolerates a gap, and claiming 0073 here would collide
> with that PR.

## Context

A television on the household network wants to boot straight into browsing and
playing the shared library, with no login. Today that is only reachable by an
accident: either authentication is disabled — which the server permits only on a
loopback listener (ADR-0011) — or the caller happens to hold a read token. A
credential-less request on an authenticated listener falls through the auth
middleware with no identity, and `RequireScope(read)` turns it into a 401. There
is no *named* anonymous read mode; there is only the absence of a credential.

The product wants the opposite: a **Guest** — an explicit, anonymous, read-only
stance over the shared library — as a first-class mode, so a client can boot into
it deliberately rather than by tripping over a missing device cert. Guest is the
floor of a larger design (a profile switcher, where a profile is a real identity
that unlocks its own encrypted personal state); this ADR lands only the floor.

Two facts bound the scope of what Guest can mean today:

- **A Guest holds only `read`.** Every route that writes or administers already
  requires `write` or `admin` (ADR-0011), so a Guest is excluded from all of them
  by scope alone. Nothing new is needed to keep a Guest out of them.
- **There is no personal content to hide yet.** ADR-0020 declares `managed`,
  `linked` and `vault` asset classes, but only `managed` is ever written; `vault`
  (encrypted/personal) is unimplemented. So a content filter that hides personal
  material from a Guest has, today, nothing to filter.

## Decision

**Make Guest an explicit, opt-in server mode: a credential-less caller is admitted
as a first-class Guest identity carrying only `read`, scoped to the shared
library, and refused from any per-identity or personal-state surface.**

1. **A named Guest identity.** `internal/guest` owns the mode. `guest.Identity()`
   returns a synthetic identity marked `Guest: true`, a `guest` principal, and the
   single scope `read`. It is the sibling of the disabled-auth `anonymous`
   identity and the web-login `session` identity — a third, deliberately narrow,
   way to be someone on this server.

2. **Admission is opt-in and defaults off.** A new `http.guest.enabled` config
   flag gates it. With it **off** (the default), a credential-less request is
   refused exactly as before — the safe stance for a server that range-serves an
   entire media library. With it **on**, a request that presents *no* credential
   is admitted as `guest.Identity()`. A *rejected* credential is never downgraded
   to Guest: an auth failure stays a 401, so enabling Guest never masks a bad
   token. Turning Guest on is an operator's explicit decision to let anyone who
   can reach the listener read the shared library; the flag, not an accident,
   carries that weight.

3. **Server enforcement beyond scope: `RefuseGuest`.** A Guest holds `read`, and
   some read routes expose state that is *somebody's* — playback history
   (ADR-0024) and encrypted personal spaces (the M9 surface). A scope check cannot
   tell a Guest apart from an enrolled reader, because both hold `read`. A
   `RefuseGuest` middleware refuses a Guest identity with 403, and guards those
   read routes: the personal-state `GET /spaces…` reads and the
   `GET /consumption/sessions…` history reads. Guest keeps the shared-library read
   surface (works, editions, assets, blobs, libraries, search) and loses exactly
   the per-identity surface.

4. **The content boundary is a seam, wired but inert today.** `guest.Visible(class)`
   is the predicate: an asset is guest-visible iff its source class is `managed` or
   `linked`; `vault` is never guest-visible, and an unknown class fails closed. It
   is wired into the asset read path — the assets a Guest lists are filtered to the
   visible classes in the query, and a single non-visible asset reads as 404 to a
   Guest. Because only `managed` is ever written today, this changes nothing a
   Guest observes; it is the hook the `vault` boundary needs the moment personal
   content becomes real, placed now so it does not have to be retrofitted onto the
   read surface later.

5. **A client-facing signal.** `GET /api/v1/system` reports `guest.enabled`, so a
   client (or operator) can see whether the mode is available. A client boots into
   Guest by simply reading the API with no credential: a 200 means Guest is live,
   a 401 means it must sign in.

## Consequences

- Guest is now a *thing* in the code — an identity, a scope stance, a predicate —
  not the shape of an absence. The profile switcher (a later phase) has a floor to
  build the step-up from.
- Enabling Guest on a non-loopback listener serves the shared library to anyone
  who can reach it, unauthenticated. That is the point of the mode for a shared
  television, and it is why the flag defaults off and is documented as a
  deliberate exposure rather than a convenience.
- No new schema, no new persisted state: a Guest keeps no history and unlocks no
  personal state, so P0 adds no migration.
- The `guest.Visible` seam is deliberately over-built for today's data. When
  `vault` content lands, the boundary tightens without touching the read handlers
  again — the decision about how fine-grained "personal" must be (a per-work flag
  versus the whole `vault` class) is deferred to when there is real content to
  make it against.

## What this does NOT decide

- **Profiles.** A profile is a real identity that unlocks its own encrypted
  personal state and tags its own history. That is a later phase, gated on the
  device-credential write path; this ADR lands none of it.
- **Session-local resume.** A Guest keeps no history. Whether a Guest gets a
  throwaway "resume where this session left off" that dies with the session is
  left open; P0 ships none.
- **Idle return-to-Guest and per-profile restrictions.** Client-surface and
  later-phase concerns, out of scope here.
