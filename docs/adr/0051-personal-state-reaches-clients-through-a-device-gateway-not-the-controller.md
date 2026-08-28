# 0051. Personal state reaches clients through a device gateway, not the controller

**Status:** Accepted (2026-08-28)
**Date:** 2026-08-28

## Context

Milestone 11 shipped two compatibility adapters — OpenSubsonic for music
(`internal/api/subsonic`, #374) and OPDS for publications (`internal/api/opds`,
#377). Both are **read-only projections of the server-readable catalogue** and
both deliberately serve **no personal state**: Subsonic `getPlaylists` is not
served, its history/starred `getAlbumList2` types return empty rather than a
fabricated ranking, and OPDS carries no reading-position. That was decided up
front (epic #30): personal state is encrypted and **opaque to the controller**
(Invariant 6, §72; the M9 plane), so a controller-side adapter holds no key and
must not fake it. #372 tracks closing that gap on the device side, where the key
actually lives (§73).

**Two things already exist and are load-bearing — this ADR builds on them:**

1. **The device-side Personal MCP** (`internal/device/personalmcp`, #326/#357).
   A local, stdio JSON-RPC server whose `personal_playlist` tool returns a
   space's playlist **decrypted on the device**: the ciphertext is fetched from
   the controller, the space key is unwrapped with the device's key, and the CRDT
   is materialised, all locally. A boundary test asserts the controller-side MCP
   exposes no tool that returns this plaintext.

2. **The on-device decrypt path** (`internal/personalstate/client`,
   `internal/personalstate/statesync`). Opening a space takes an `Unwrapper`
   interface, not a raw key, so a desktop's exportable key and a phone's
   enclave-backed key are the same shape (ADR-0049, #330).

So the invariant-safe machinery — decrypt on the device, never on the server —
is done. What #372 needs is a **bridge**, because of a protocol mismatch no
device-side code removes on its own:

> A stock Subsonic app speaks HTTP to **one** base URL and expects that one
> server to hold its playlists. The device's personal state lives behind a
> **stdio** Personal MCP that only a local agent can reach. A stock app cannot
> talk to it.

An earlier draft of this ADR proposed *naming but deferring* the gateway,
serving personal state only through the first-party Personal MCP. That deferral
is now lifted: the gateway is built (this ADR's decision), because a stock-app
path to playlists is wanted now and the M9 decrypt machinery it needs is in
place. The Personal MCP remains the agent surface; the gateway is the stock-app
surface. Both decrypt on the device; neither adds anything to the controller.

## Decision

**Personal state reaches a client through the device, never through the
controller. A device runs a local Subsonic *gateway* (`internal/device/gateway`)
that a stock app points at as its one origin; the gateway serves the
personal-state methods by decrypting on-device, and proxies the
library/stream methods to the controller.**

Concretely:

- **The controller gets no personal-state endpoint. Ever.** Not on `/api/v1`,
  not on the Subsonic `/rest` mount. This is the one option this ADR forecloses
  rather than weighs: it would put a key-decryptable surface on the machine that
  holds no key, or move plaintext onto it — exactly what §72, §38 and ADR-0032
  exist to prevent. The controller-side adapter (`internal/api/subsonic`)
  therefore stays personal-state-free, and the gateway carries its own Subsonic
  envelope types rather than growing playlist shapes on the controller.

- **The gateway serves `getPlaylists` / `getPlaylist` locally**, from state it
  decrypts through the same `personalstate/client` + `statesync` path the CLI's
  `space read` and the Personal MCP use. These methods are **never proxied** to
  the controller.

- **The gateway proxies `ping`, `getArtists`, `getArtist`, `getAlbumList2`,
  `getAlbum`, `stream`, `download`** to the controller's `/rest` adapter,
  substituting the device's controller bearer for the app's credential and
  copying the reply back verbatim (byte-identical streams, ADR-0013).

- **Two credentials by design.** The stock app authenticates to the *device*
  with a Subsonic `u`+`p`; the device authenticates to the *controller* with its
  own bearer. The app never holds the controller token.

- **The Personal MCP stays the agent surface** for a first-party client or an
  agent that composes controller-library + device-personal itself.

## Consequences

- A stock Subsonic app pointed at the device now shows the user's playlists AND
  browses/plays the library, from one origin, with the split invisible to it.
- The gateway **couples the app's single origin to the device being up**: because
  the app talks to one base URL, the device reverse-proxies every library and
  stream request too. A phone that is asleep takes that app's library access down
  with it. This is the cost the earlier draft flagged; it is accepted as the
  price of stock-app playlist support, and it changes no controller code and no
  invariant — the controller remains independently reachable by any client that
  talks to it directly.
- The gateway is a **second front door with its own auth surface** (the
  device-local password). It is a device-run process, contained by the device.
- **New CRDT types are still a precondition** for surfacing anything past
  playlists. Only the playlist CRDT exists; play-counts/scrobble history, starred
  items and reading-position have **no CRDT type yet**, so the gateway serves
  none of them (neither fabricated nor empty-but-successful). Issue #386 tracks
  building those types; until then this gap is documented, not silent.
- **Playlist entry metadata and names are minimal in this slice.** An entry
  carries the CRDT item id (mirrored as its title); a playlist's display name is
  derived from the space id, because a name is itself encrypted state a space
  does not hold in the clear (§39). Enriching an entry to full catalogue metadata
  is a per-item controller read a first-party client can do through the proxied
  browse methods; it is out of this slice.

## What would make us revisit

- The availability coupling proving painful in practice (a household that wants
  library access to survive the phone sleeping) — that would argue for a
  first-party client composing the two surfaces itself, rather than a
  reverse-proxy through the device.
- The history/starred/reading-position CRDT types landing — that widens what the
  gateway (and the Personal MCP) can serve, additively, with no change to this
  decision.
