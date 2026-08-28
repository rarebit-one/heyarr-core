// Package gateway is the device-side compatibility gateway (§70, §72, §73,
// ADR-0051, Milestone 11).
//
// # What it is
//
// A local Subsonic server a user's DEVICE runs, and a stock Subsonic app points
// at as its ONE origin. It serves the two families of method a stock app needs
// from the two places they honestly live:
//
//   - Personal state — getPlaylists, getPlaylist — served HERE, from state this
//     device decrypts locally. The playlist ciphertext is fetched from the
//     controller, the space key is unwrapped with this device's key, and the CRDT
//     is materialised, all on the device (see [SpaceLibrary]).
//   - Library and stream — ping, getArtists, getArtist, getAlbumList2, getAlbum,
//     stream, download — proxied to the controller's OpenSubsonic adapter
//     (internal/api/subsonic), which serves the server-readable catalogue. The
//     gateway forwards the app's request with the DEVICE's controller bearer
//     substituted, and copies the reply back verbatim.
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
//     clear (§39), so the display name is derived from the space id for now.
//   - history / starred / now-playing / reading-position are personal state with
//     NO CRDT type yet, so they are not served by any route — the same gap the
//     controller-side adapter documents. Adding them is gated on building those
//     CRDT types first; issue #386 tracks it. Serving them now would mean
//     either fabricating them or decrypting something that does not exist.
package gateway
