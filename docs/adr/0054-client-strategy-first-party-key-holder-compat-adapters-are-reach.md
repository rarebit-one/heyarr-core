# 0054. Client strategy: a first-party device-side key-holder is the product; compat adapters are reach

**Status:** Accepted
**Date:** 2026-08-31

## Context

M11 gives heyarr read-only compatibility projections of the library: Subsonic
(`/rest`), OPDS (`/opds`) and DLNA (`/dlna`), each authenticating with a heyarr
bearer token as the protocol password (DLNA: no auth, capability URLs per
ADR-0040). ADR-0053 adds a QR web-login so a browser or TV can hold a session token
too. Between them, **a generic Subsonic/OPDS/DLNA client gets library browse and
byte streaming for free** — Range/206/progressive-partial inherited from M10.

That raises the question the mobile and TV clients force: is a stock client enough,
or does heyarr need a first-party one? The temptation is to answer "a stock Subsonic
app plus the compat adapters" and build no client of our own. This record rejects
that, and says why the answer is *both, layered* — with the first-party client as
the product and the adapters as reach.

The load-bearing fact is Invariant 6 and ADR-0049/0051. **Encrypted personal state —
play position, playlists, starred, history — is CRDT state the controller stores
only as ciphertext and can neither decrypt nor merge.** It is decrypted *only on an
authorised device that unwraps the space key with its own X25519 device key*
(ADR-0049). The client that shows personal state is therefore *necessarily a
device-side key-holder*. A generic client is not one and never can be — by
construction it holds no device key, so the compat adapters serve it none of that
state (§72). This is not a gap to be filled; it is what the adapters are.

## Decision

**Build the first-party KMP mobile client as the product; keep Subsonic/OPDS/DLNA as
the "any stock client" reach and the credential-less TV path. Do not try to make a
stock client do personal state.** Three surfaces, three trust levels:

### The first-party mobile client is a Voidbind device-side key-holder

It authenticates with the Device scheme (ADR-0048/0053): on-device Ed25519 signing
and X25519 encryption keypairs — the same `voidbind-kmp` `DeviceIdentity` the
authenticator uses — enrolled by pairing (SAS/QR, #336), presenting `Authorization:
Device <cert>~<proof>` with a wake-time re-minted possession proof (the clock-skew
re-mint of `docs/design/mobile-client.md`). It is a Voidbind device in its own
right. Only it can hold the two orthogonal gates personal state requires: an
ADR-0048 **grant** authorises *fetching* the ciphertext, and a **wrapped key**
authorises *reading* it — decryption happening on-device only, through the device
gateway / Personal MCP surface (#372/#387, ADR-0051). This is the client that gives
resume, playlists, starred and history, and it is *architecturally required* to be
first-party, not a preference.

### The compat adapters are reach, not the product

A stock Subsonic app, an OPDS reader, a DLNA browser get day-one library browse and
byte streaming, bearer-as-password. This is real value — "it works now", and it is
the sane path for a credential-less TV — but it is a **degraded** experience by
construction: no resume, no playlists, no cross-device state, because none of that
is server-readable. We ship and document the adapters as exactly that, and never
grow one toward personal state; a stock client that showed a playlist would mean the
server could read one, which Invariant 6 forbids.

### The TV is a credential-less renderer the phone casts to

A television is a poor key-holder — a shared screen, no guaranteed secure element,
no per-user presence — so it never holds encrypted personal state. The primary TV
model is **phone-as-controller, TV-as-renderer**: the phone (which *can* decrypt
personal state) resolves "continue watching" locally and casts the content to the TV
via an ADR-0040 signed capability URL (HMAC-signed, MIME-baked, TTL'd) — the exact
pattern DLNA `res` URLs already use. The TV needs no key, no login, no personal
state, which turns the encrypted-state constraint into a *non-issue* for the TV
rather than a limitation. A standalone QR-login Tizen app (ADR-0053) is offered as
the simpler, more degraded option for people who want an on-TV browser; it is a
lower-trust surface either way.

## Consequences

- heyarr-core owns the mobile client's **contract**, not the app: the client is a
  separate repo (`heyarr-mobile`, KMP, per #330/#341), and its integration surface —
  QR login, Device auth, Subsonic/OPDS reach, and the device-side personal-state
  gateway — is recorded in `docs/design/mobile-client.md`. This ADR is why that repo
  is first-party and device-shaped rather than a thin wrapper over a stock protocol.
- The compat adapters' scope is fixed: reach and the credential-less TV, never
  personal state. That boundary is Invariant 6 at the client layer, and it is the
  reason a first-party client exists at all.
- The TV story gates on the DLNA/cast work (#202/#382 SSDP advertisement), not on any
  TV-side key or login. The credential-less-renderer decision is what lets the TV
  ship without ever solving key-holding on a shared screen.

## What would make us revisit this

- **The device gateway (ADR-0051) as the personal-state path.** A local Subsonic
  server the *device* runs, serving decrypted playlists to a stock app, is a third
  option — useful for someone who insists on a stock client but still wants
  playlists. It couples the app's library to the phone being awake, so the
  first-party client supersedes it for the product; if that coupling proves
  acceptable, the balance shifts.
- **A TV platform that is a real key-holder.** If a TV gains a per-user secure
  element and presence, the credential-less-renderer decision could relax — but the
  cast model is strictly safer, so the bar is high.
- **Personal state becoming server-readable.** It will not (Invariant 6), but if that
  invariant were ever revisited the entire "first-party is required" argument
  collapses into "first-party is nicer", and the strategy is a different record.
