// Package dlna is the DLNA/UPnP ContentDirectory MediaServer adapter (spec §70,
// Milestone 11; addresses the video-and-audio client gap #202).
//
// # What it is
//
// A read-only projection of Heyarr's server-readable catalogue onto a UPnP
// ContentDirectory, so the devices that already speak DLNA natively — a
// television, a networked speaker — can browse the library and play from it
// with no app, no account and no credential. It answers the SOAP Browse action
// over a two-level tree (a folder per content type, its playable items inside)
// and hands back DIDL-Lite whose resource URLs are render capabilities
// (ADR-0040): the device fetches bytes from the unauthenticated render route,
// which exists precisely because such a device has no Authorization header to
// present.
//
// # What it is NOT
//
//   - It is not internal/renderer. That package is the CONTROL edge — it
//     discovers renderers and drives them (push a URL, play, pause). This is the
//     SERVER edge — it is browsed. The two are opposite directions of UPnP.
//   - It exposes no personal state (§72); it is a projection, never a writer.
//   - It does not reshape the model to suit UPnP (§70): a folder is the
//     content_type column grouped, an item is one Asset. Only assets the render
//     route can actually serve (a blob-backed managed/vault asset with a
//     servable MIME) become items — advertising an unfetchable item is the
//     dishonesty this refuses.
//
// # Deferred, on purpose
//
// SSDP LAN advertisement (a UDP-multicast responder) and the real-device proof
// are a tracked follow-up. They cannot be exercised headlessly, and the browse
// contract does not need them: a control point given the description URL browses
// and plays without discovery, which is what makes this slice provable — a real
// caller (a Browse) driving a real byte fetch, asserted by digest.
package dlna
