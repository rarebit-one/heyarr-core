# 0051. Personal state reaches clients through a device gateway, not the controller

**Status:** Proposed (2026-08-28) — awaiting maintainer sign-off on the bridge shape
**Date:** 2026-08-28

## Context

Milestone 11 shipped two compatibility adapters — OpenSubsonic for music
(`internal/api/subsonic`, #374) and OPDS for publications (`internal/api/opds`,
#377). Both are **read-only projections of the server-readable catalogue** and
both deliberately serve **no personal state**: Subsonic `getPlaylists` / `star`
/ `scrobble` / `getNowPlaying` are not served, its history/starred
`getAlbumList2` types return empty rather than a fabricated ranking, and OPDS
carries no reading-position. That was decided up front (epic #30): personal
state is encrypted and **opaque to the controller** (Invariant 6, §72; the M9
plane), so a controller-side adapter holds no key and must not fake it. #372
tracks closing that gap on the device side, where the key actually lives (§73).

**Two things already exist and are load-bearing here — this ADR builds on them,
it does not invent them:**

1. **The device-side Personal MCP** (`internal/device/personalmcp`, #326/#357).
   A *local, stdio* JSON-RPC server an agent on the user's own machine talks to.
   Its `personal_playlist` tool returns a space's playlist **decrypted on the
   device** — the ciphertext is fetched from the controller, the space key is
   unwrapped with the device's X25519 key, the CRDT is materialised, all
   locally. It has no bearer tokens and no scopes because there is no remote
   caller (§74); containment is the OS (stdio, `0600` key, `0700` dir). A
   boundary test on both MCP surfaces asserts the controller-side MCP exposes no
   tool that returns this plaintext.

2. **The on-device decrypt path** (`internal/personalstate/client`). Opening a
   space takes an `Unwrapper` *interface*, not a raw key, so the desktop CLI's
   exportable key and a phone's enclave-backed key are the same shape. The CLI's
   `device mcp` command wires this reader into the Personal MCP.

So the invariant-safe machinery — decrypt on the device, never on the server —
is done. **What #372 actually needs is a bridge**, and it needs it because of a
protocol mismatch that no amount of device-side code removes on its own:

> A stock Subsonic or OPDS app speaks HTTP to **one** base URL and expects that
> one server to hold its playlists. The device-side personal state lives behind
> a **stdio** Personal MCP that only a local agent can reach. A stock app cannot
> talk to it.

There is a second, smaller gap: only the **playlist** CRDT exists
(`internal/personalstate/crdt/playlist.go`). Play-counts / scrobble history,
starred items, and OPDS reading-position are personal state with **no CRDT type
yet** — a prerequisite for surfacing them by any route.

## Decision

**Personal state reaches a client through the device, never through the
controller — and for now through the Personal MCP (a first-party / agent
surface), with a device-side protocol *gateway* named as the path to stock-app
support but not built until it is wanted.**

Concretely:

- **The controller gets no personal-state endpoint. Ever.** Not on `/api/v1`,
  not on the Subsonic `/rest` mount, not on OPDS. This is the one option this
  ADR forecloses rather than weighs: it would put a key-decryptable surface on
  the machine that holds no key, or move plaintext onto it — exactly what §72,
  §38 and ADR-0032 exist to prevent.

- **Now (built): first-party / agent access via the Personal MCP.** The
  personal state a client cannot get from the controller, an *authorised device*
  surfaces through `internal/device/personalmcp`. A first-party client, or an
  agent the user runs, composes controller-library + device-personal itself.
  This is what `personal_playlist` already is; the work left here is additive
  read verbs (history, starred, reading-position) as their CRDT types land.

- **Later (named, not built): a device-side compatibility gateway.** For a
  *stock* Subsonic/OPDS app to show the user's playlists, the device runs a
  local endpoint the app points at, which serves the personal-state methods by
  decrypting on-device and forwards library/stream to the controller. The app
  sees one origin (the device); the split is invisible to it. This is deferred,
  with eyes open, for the reasons under Consequences.

The prerequisite for either surfacing more than playlists — new CRDT types for
history/starred/reading-position — is called out so it is not mistaken for
adapter work.

## Why the gateway is named but not built now

The gateway (option (a): device runs a local Subsonic/OPDS server the stock app
points at) is the only shape that makes a *stock* app show playlists. It is also
a milestone-sized build with a real cost that should be paid deliberately:

- **It couples library access to the device being up.** A stock client uses one
  base URL. If that URL is the device, the device must handle — and therefore
  reverse-proxy — *every* request, including the library browsing and streaming
  the controller already serves perfectly well. The controller stops being
  reachable by the client except through the device. A phone that is asleep
  takes the library down with it.
- **It re-implements auth translation and reachability.** The gateway must
  present a controller credential upstream while accepting the app's Subsonic
  credential downstream, and it must be discoverable on the LAN. That is a
  second front door with its own security surface.
- **The 80% is already delivered.** Both adapters already reach a real client
  for library browsing and playback (proven by the byte-identity demo scenes).
  Playlists-in-stock-apps is the harder 20%, and gating it behind a device-proxy
  build would hold the shipped value hostage to the unbuilt part — the same
  reasoning that deferred the video client (#202) rather than blocking M2 on it.

Option (b) — first-party/agent only — is therefore the default now: it is
already built, it keeps the boundary clean, and it loses nothing that was ever
promised (stock apps were always scoped to library+stream, epic #30).

## Consequences

- Stock Subsonic/OPDS apps continue to browse and play the library and get **no
  playlists/scrobble/starred/reading-position** until the gateway is built. This
  is the documented state, not a silent gap: the adapters already refuse those
  methods rather than faking them.
- The device Personal MCP is where personal state is read, and it grows read
  verbs as CRDT types land — additive, invariant-safe, no controller path.
- Building the gateway later changes no controller code and no invariant: it is
  a new device-side process, and this ADR is the record that its cost
  (availability coupling, a second auth surface) was seen before it was paid.
- New CRDT types (history, starred, reading-position) are a **precondition** for
  surfacing anything past playlists, by MCP verb or by gateway alike. They are
  not adapter work and should not be filed against the adapters.

## What would make us revisit

- A concrete demand for stock-app playlist support (a household member who wants
  their playlists in Symfonium/DSub, not a first-party client) — that is the
  trigger to build the gateway and accept its availability coupling.
- A first-party client (a phone app) reaching the point where it composes
  controller-library + device-personal — that would exercise option (b) end to
  end and is the natural next proving ground for the Personal MCP.
