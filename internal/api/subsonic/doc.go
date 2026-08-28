// Package subsonic is the OpenSubsonic compatibility adapter for music
// (spec §70, Milestone 11).
//
// # What it is
//
// A read-only projection of Heyarr's server-readable catalogue onto the
// OpenSubsonic REST protocol, so an existing music client — a real Subsonic app
// — can browse the library and play from it without Heyarr owning a client.
// It answers the smallest honest slice that reaches such a client: the handshake
// (ping, getLicense, getOpenSubsonicExtensions, getMusicFolders), browse
// (getArtists, getArtist, getAlbumList2, getAlbum) and byte serving (stream,
// download).
//
// # What it is NOT, on purpose
//
// It exposes no personal state. Playlists, play-counts, scrobbles, starred
// items and now-playing are personal state, which Milestone 9 made encrypted
// and opaque to the controller (Invariant 6, §72): the server holds no key, so
// a controller-side adapter cannot serve them faithfully and must not fake
// them. Those features belong to a device-side Personal MCP (§73) and are a
// named follow-up, not a gap here. getAlbumList2 types that depend on that
// history (recent, frequent, starred) return an empty list rather than a
// fabricated one.
//
// # Two boundaries it keeps
//
//   - Neither protocol defines Heyarr's canonical model (§70). An artist is not
//     an entity; it is derived by grouping music Works on their artist
//     attribute. The model is never reshaped to suit Subsonic.
//   - Byte serving is not reimplemented. stream/download resolve a track to a
//     blob hash and delegate to internal/api/blobs (ADR-0013), inheriting Range,
//     206 and M10 progressive partial serving, and staying piece-agnostic.
//
// The adapter mounts OUTSIDE the authenticated /api/v1 group and authenticates
// in Subsonic's own terms, mapping the password a client sends onto a Heyarr
// bearer token and the `read` scope.
package subsonic
