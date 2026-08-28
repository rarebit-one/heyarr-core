// Package opds is the OPDS compatibility adapter for publications
// (spec §69, §70, Milestone 11).
//
// # What it is
//
// A read-only projection of Heyarr's server-readable publication catalogue onto
// OPDS 1.2 (the Atom profile the real readers speak — KOReader, Foliate,
// Marvin, Chunky, Panels), so an existing reader can browse the library and
// download from it without Heyarr owning a client. Three routes: a navigation
// feed (the menu), one acquisition feed (the shelf), and a byte download.
//
// # What it is NOT, on purpose
//
// It exposes no personal state. Reading position, bookmarks and shelves are
// personal state, encrypted and opaque to the controller (Invariant 6, §72):
// the server holds no key, so a controller-side adapter cannot serve them
// faithfully and must not fake them. Those belong to a device-side Personal MCP
// (§73) and are a named follow-up, not a gap here.
//
// # Two boundaries it keeps
//
//   - Neither protocol defines Heyarr's canonical model (§70). A publication is
//     a book Work, a format is an Edition, the bytes are an Asset; the model is
//     never reshaped to suit OPDS.
//   - Byte serving is not reimplemented. The download route resolves an edition
//     to a blob hash and delegates to internal/api/blobs (ADR-0013), inheriting
//     Range and 206.
//
// The adapter mounts OUTSIDE the authenticated /api/v1 group and authenticates
// with HTTP Basic, mapping the password a reader sends onto a Heyarr bearer
// token and the `read` scope.
package opds
