// Package identification turns a library-relative path into a Work / Edition /
// Asset candidate using deterministic, dependency-free filename heuristics
// (spec §66 "identify", M1-11).
//
// It is the Milestone 1 stand-in for the real metadata identifier of Milestone
// 3. Everything here is a guess made from a path, and every guess is recorded
// on the asset as identification_source = "path-heuristic" plus the name of the
// rule that made it, so the real identifier can find exactly the rows it is
// allowed to re-resolve.
//
// Three properties are load-bearing:
//
//   - Pure. No filesystem, no clock, no network, no randomness — a path in, a
//     candidate out. The package deliberately imports "path" and not
//     "path/filepath": a library-relative path is always slash-separated data,
//     never a thing to open (ADR-0006, invariant 2).
//   - Convergent. The WorkKey is normalised hard enough that renaming a file to
//     an equivalent form yields the same key, so a rescan gets-or-creates the
//     same Work instead of multiplying it.
//   - Total. Identify never fails. An unparseable path lands under the
//     synthetic Unidentified Work; identification failure must never be ingest
//     failure.
//
// # What it is not
//
// A path heuristic guesses. Two limits are worth knowing before wiring it up:
//
//   - The owning library's content type carries real weight. A companion file
//     with no content of its own — "Artist/Album (2001)/cover.jpg" — is typed by
//     the library it was found in; asked with no library type at all, the rules
//     fall back to registration order and read that path as a movie. Ingest
//     always knows the library, and libraries.content_type is NOT NULL, so this
//     only bites a caller that deliberately passes "".
//   - Normalisation is tuned for convergence over accuracy. Where a release
//     token appears inside a real title, the title is truncated — but it is
//     truncated the same way in every spelling of that filename, so the Work
//     still converges. Milestone 3's identifier is what fixes the title.
package identification
