// Package gateway is the device-side compatibility gateway (§70, §72, §73,
// ADR-0051, Milestone 11).
//
// # What it is
//
// A local Subsonic server a user's DEVICE runs, and a stock Subsonic app points
// at as its ONE origin. It serves the two families of method a stock app needs
// from the two places they honestly live:
//
//   - Personal state — getPlaylists, getPlaylist, getStarred2, getNowPlaying, and
//     the getAlbumList2 types recent/frequent/starred — served HERE, from state
//     this device decrypts locally. The ciphertext is fetched from the controller,
//     the space key is unwrapped with this device's key, and the matching CRDT is
//     materialised, all on the device (see [SpaceLibrary]). Which space holds the
//     starred set and which holds the play history is the device's own knowledge
//     ([SpaceRoles]) — a space holds one CRDT and that fact is never on the wire.
//   - Library and stream — ping, getArtists, getArtist, the CATALOGUE
//     getAlbumList2 types (newest, byYear, alphabetical…), getAlbum, stream,
//     download — proxied to the controller's OpenSubsonic adapter
//     (internal/api/subsonic), which serves the server-readable catalogue. The
//     gateway forwards the app's request with the DEVICE's controller bearer
//     substituted, and copies the reply back verbatim. getAlbumList2 straddles the
//     split by its `type`: the three personal types are served locally, every
//     catalogue type is proxied.
//
// The split is invisible to the app: it sees one Subsonic origin, authenticates
// to it once, and gets its playlists and its music from what looks like one
// server.
//
// # The invariant this exists to keep (Invariant 6, §72)
//
// The controller must never see personal-state plaintext. Playlists are
// encrypted and opaque to the controller (the M9 plane); only an authorised
// device holds the key. So the gateway decrypts ON THE DEVICE and serves the
// personal-state methods itself — they are NEVER proxied to the controller,
// which holds no key to answer them and must not be handed one. This is why the
// gateway is a device-run process and is deliberately NOT mounted on the
// controller's router: putting a decrypt-capable surface on the controller is
// exactly the violation ADR-0051, §72 and §38 forbid. The controller-side
// OpenSubsonic adapter (internal/api/subsonic) correspondingly serves NO personal
// state, and this package does not add any to it.
//
// # Two credentials, by design
//
// The stock app authenticates to the DEVICE with a Subsonic u+p — a device-local
// password. The device authenticates to the CONTROLLER with its own bearer token,
// held by the proxy and sent as the controller adapter's password. The two are
// distinct secrets: the app never holds the controller token, so a compromised
// app leaks only its device password.
//
// # What is deliberately out of this slice
//
//   - Playlist ENTRIES carry only the item id the CRDT stores (mirrored as the
//     title), not enriched catalogue metadata. Turning an id into a
//     title/album/artist is a per-item catalogue read the controller owns; a
//     first-party client resolves ids through the proxied browse methods. A stock
//     app streams an entry by its id, which the gateway proxies.
//   - A playlist's NAME is itself encrypted state a space does not carry in the
//     clear (§39), so the display name is derived from the space id for now. The
//     same is true of a starred song's or a played item's metadata: the device
//     holds the id, the controller holds the catalogue, so these listings mirror
//     the id and a first-party client resolves each id through the proxied browse
//     methods (exactly as playlist entries do).
//   - WRITES — star/unstar, scrobble, setting a now-playing — are not served yet:
//     this slice is the read path (#387), and a write means encrypting a new CRDT
//     change on the device and pushing it as ciphertext. Tracked as a follow-up.
//   - reading-position is personal state served by the device-side Personal MCP
//     (#372); it feeds OPDS, not Subsonic, and there is no OPDS device-gateway
//     surface here yet. A follow-up (sibling to #376) covers the OPDS far end.
package gateway
