# 0039. A renderer fetches bytes with a capability, not a credential

**Status:** Accepted
**Date:** 2026-08-24

## Context

Spec §68 plans a playback and returns somewhere to play from. Nothing on a home
network can use it.

A television, a speaker or a projector is handed a URL and told to fetch it.
That is the whole of its vocabulary: it cannot send an `Authorization` header,
cannot read a JSON plan, and cannot be taught to. Measured against a 2022
Samsung QN85B and a Devialet Phantom II, both of which are ordinary UPnP
MediaRenderers on the LAN:

```
bare GET  /api/v1/blobs/{hash}/content     → 401
GET ...?token=<playback token>             → 401   (refused deliberately)
GET with Authorization: Bearer <token>     → 206, ranges correct
```

The 401 on `?token=` is not an oversight. `internal/api/http/auth.go` refuses
query credentials because they end up in proxy logs, browser history and
`Referer` headers. That reasoning is sound and this ADR does not weaken it.

There is a second, independent gap: blob content is served as
`application/octet-stream`, because a blob is bytes and bytes have no type
(ADR-0006). DLNA renderers routinely refuse `application/octet-stream`. The
MIME type is a property of the *Asset*, which the blob endpoint does not and
should not know about.

Three options were considered.

**Put a session token on the blob endpoint.** ADR-0013's consequences forbid
exactly this — "adding a player-shaped session token" — and
`internal/api/resources/playback.go` refuses to invent "a second authorisation
model to reconcile with the real one later". Rejected on the existing record.

**Serve blobs unauthenticated when bound to a private address.** Fastest, and
it is the weaker authorisation model ADR-0013 and §77 both warn against, with
the added property that a flat home LAN is not a trust boundary. Rejected.

**Have the client proxy the bytes.** Puts the client in the content data path,
which §32 exists to prevent, and playback then dies when the laptop closes.
Rejected.

## Decision

A fourth thing: a **capability URL**, on its own mount, with its own trust root.

`GET /render/{capability}` where the unguessable, expiring, single-blob
capability *is* the authority. It carries no identity and grants exactly one
thing: these bytes, until this instant.

The capability is a signed token, not a database row: `v1.<blob>.<expiry>.<mime>`
with an HMAC over all four fields, so tampering with any of them — pointing it
at a different blob, extending its life, changing the declared type — fails
verification. It is stateless by design; there is no table to grow and no sweep
to write.

This honours ADR-0013 literally rather than evading it. That ADR forbids
bolting a player token onto the blob *contract*; `blobs.go` already establishes
the pattern this follows — "one function, two mount points, two trust roots",
the bearer router and the peer's mTLS listener. This is the third mount and the
third trust root, and the blob endpoint is untouched.

The renderer route serves an **Asset view of a blob**, which is what lets it set
a real `Content-Type` without the blob endpoint learning about MIME types: the
type is fixed at mint time, when an Asset is in hand, and signed into the
capability. A renderer is told `video/mp4` because the Asset said so, not
because anything sniffed the bytes.

**The signing key belongs to the node that serves the bytes.** It is a 32-byte
secret in the data directory, mode 0600, generated on first use — deliberately
NOT derived from the peer's Ed25519 identity, which exposes "the path, never
the bytes" (`internal/peer/identity`) and should keep doing so.

**Consequently a capability is only valid at the peer that minted it**, and this
milestone mints one only when the routed replica is on the node answering the
playback request. A cross-site replica returns a plan with no renderer URL and
a reason saying why, in the same idiom as every other refusal in §68 — "the
refusal is as much the deliverable as the success". §31 prefers a local replica
anyway, so this is the uncommon path.

## Consequences

This is a narrow slice of §77's grants arriving before Milestone 8, and it is
recorded as such rather than pretended otherwise. Milestone discipline says not
to smuggle later work into an earlier milestone; the defence is that §68 cannot
reach any real device without it, and that this slice is per-blob and
per-expiry from the outset — the shape M8 will generalise, not a lesser model
it will have to unpick. What M8 adds is principals, revocation and delegation.

**There is no revocation before expiry.** A leaked capability is good until it
expires, and shortening the lifetime is the only mitigation available. That is
the price of statelessness and it is the thing most likely to make us revisit
this: if capabilities ever need to be withdrawn — a device sold, a guest gone
home — they need a row, and that row is M8's grant.

**A capability is a bearer secret in a URL**, which is the property `auth.go`
objects to for credentials. The difference that makes it acceptable: it names
one blob and expires, so a leaked one discloses one film for one window,
whereas the leaked `read` token that `playback.go` warns about reads the whole
library. The access log already redacts query values; this puts the secret in
the *path*, so path redaction on the renderer route is required and is
implemented alongside it.

**Cross-peer playback to a renderer does not work yet**, per the scoping above.
Making it work means either a mint call to the owning peer over the existing
mTLS peer surface, or a shared signing key distributed through membership. The
first is preferred — it keeps the secret local to the node that holds the bytes
— and neither is in this change.

**The capability does not authenticate the renderer**, only the request. Any
device on the LAN that obtains the URL can fetch it. On a flat home network
that is the same exposure as the television's own unauthenticated control
surface, and it is bounded by one blob and one expiry.
