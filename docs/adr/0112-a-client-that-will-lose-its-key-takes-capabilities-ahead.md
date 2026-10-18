# 0112. A client that is about to lose its key takes capabilities ahead

**Status:** Accepted
**Date:** 2026-10-18
**Builds on:** [ADR-0040](0040-a-renderer-fetches-bytes-with-a-capability-not-a-credential.md) (capability URLs), [ADR-0013](0013-blob-serving-is-a-contract-not-an-endpoint.md) (no player-shaped token on the blob contract), [ADR-0054](0054-client-strategy-first-party-key-holder-compat-adapters-are-reach.md) (first-party clients authenticate with the Device scheme), [ADR-0079](0079-renderers-get-a-plain-http-origin-of-their-own.md) (render origin)

## Context

A first-party phone authenticates each range read with a Device possession
proof that it signs on demand. The proof is reused for about two minutes. The
phone's device key is usable only while the phone is **unlocked**, which is
the key's posture and not this node's to loosen. Music playing under the lock
screen therefore cannot fetch its next range once the last proof is retired.
The client can buffer ahead, but it cannot buffer an evening.

For as long as it stays locked, that phone is the device ADR-0040 was written
for: one that cannot send an `Authorization` header. The only route that
handed out a capability was `POST /playback`. That route opens a consumption
session, emits an event and mints a whole-library `read` token **per call**,
so a twelve-track queue would be twelve plays nobody made. It also mints at
the renderer origin, which is LAN-only plain HTTP when `http.render_addr` is
set.

## Decision

`POST /api/v1/render/capabilities {asset_ids}` mints one ADR-0040 capability
per asset. A first-party client calls it while it can still authenticate, when
it builds a queue. The route:

- needs only `read`, the floor under which the caller can already fetch these
  bytes;
- writes nothing: no session, no event, no token;
- mints at the **public origin**, the one the client already reaches the node
  at (ADR-0079 keeps `public_origin` for phones), never at the render listener;
- answers per asset with a URL or a reason, never neither. A node with no
  render secret says so, and bytes that routing cannot find get a refusal
  instead of a URL that would fetch nothing;
- keeps the guest content boundary (ADR-0074). A capability is an
  unauthenticated URL, so a Guest is told "no such asset" for anything it
  could not see, just as `GET /assets/{id}` tells it.

The capability itself is unchanged: one blob, one MIME type, six hours,
stateless HMAC, valid only at the minting node. ADR-0013 is untouched,
because the blob endpoint gains no token. This is the existing third mount,
reached by a new kind of caller.

## Consequences

- A `read` holder can now turn its access into **unbound** URLs that others
  can use for six hours. It could already proxy the same bytes, so this adds
  convenience, not reach. The blast radius is one blob for six hours, which
  ADR-0040 already accepted.
- A client still uses the Device scheme for everything else. It falls back to
  the blob endpoint for any asset the node will not mint for, such as a
  replica on another peer.
- Revisit if capabilities gain revocation, or if a first-party client gets a
  credential it can present while locked. Either would make this route
  unnecessary.
