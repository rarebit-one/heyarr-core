// Package downloads drives external download clients — Transmission first
// (spec §58). Milestone 3.
//
// # Heyarr decides what to acquire; something else moves the bytes
//
// §58 delegates transfer mechanics deliberately. This package does not
// implement BitTorrent, does not supervise a daemon, does not install one and
// has no opinion about how it got there. It takes an endpoint, a credential
// and a path mapping from configuration and talks RPC to whatever is already
// running.
//
// ADR-0023 pinned FFmpeg by digest because Heyarr INSTALLS FFmpeg. Nothing here
// puts anything on the machine, so nothing here is pinned. What replaces
// pinning is compatibility, checked at connect: the RPC version is read, kept
// in provider health, and reported — never made a startup failure (ADR-0025).
//
// # Four things a real instance taught us that a plausible one would not
//
// The corpus in internal/providers/fixtures was captured from a real
// Transmission 4.1.3 at RPC version 19. Each of these is a fact rather than an
// expectation, and each would have been guessed wrong:
//
//  1. THE FIRST REQUEST IS REFUSED. Transmission answers 409 with an
//     X-Transmission-Session-Id header that must be replayed. A client that
//     treats 409 as an error works against every hand-written fixture and
//     fails against every real instance.
//
//  2. A TRACKER FAILURE IS INVISIBLE AT THE TOP LEVEL. A transfer whose only
//     tracker cannot be reached reports error=0 and errorString="" while
//     trackerStats[].lastAnnounceResult says "Could not connect to tracker".
//     A client watching errorString — the obvious field, and the one its name
//     promises — sees a transfer sitting at 0% looking perfectly healthy,
//     forever. Stall detection therefore reads trackerStats. See stall.go.
//
//  3. MID-TRANSFER PATHS LIE. With incomplete-dir enabled — which the captured
//     instance has — the bytes are NOT under downloadDir until the transfer
//     finishes. Resolving downloadDir + name while it runs yields a path that
//     does not exist, and reports as a mysterious import failure. Paths are
//     resolved only on completion.
//
//  4. LABELS ARE AVAILABLE AND ARE THE RIGHT MECHANISM. Transmission gained
//     them in 3.00 (RPC 16). The *arr stack still implements "category" as a
//     subdirectory of the download directory because it predates them; Heyarr
//     should not inherit a workaround for a limitation fixed years ago. The
//     fallback is carried for older instances and reported in health, so the
//     degradation is legible rather than mysterious.
//
// # The label is a safety property, not a convenience
//
// A download client is SHARED. The operator has their own transfers in it, and
// something that cannot tell them apart must never be allowed to remove,
// re-target or import anything. Every mutating operation here filters on the
// label first, and a transfer without it is invisible — even when its name
// matches exactly what Heyarr wanted.
//
// An acquisition system that can delete an operator's data because a name
// matched is one nobody should run.
package downloads
