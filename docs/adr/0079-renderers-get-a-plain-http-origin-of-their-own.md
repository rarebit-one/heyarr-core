# 0079. Renderers get a plain-HTTP origin of their own

**Status:** Accepted
**Date:** 2026-09-07
**Builds on:** ADR-0040 (a renderer fetches bytes with a capability, not a credential), ADR-0072 (opt-in native TLS on the client API, and an explicit public origin)

## Context

ADR-0072 let a node serve its client API over HTTPS and name the `https://`
public origin that browsers, phones and the QR web-login need. ADR-0040 hands a
renderer a capability URL built on the same origin. Those two decisions were
each right and together broke every television.

Measured against a Samsung QN85B at Site A, on a node serving
`https://heyarr.<site>.example:7777` with a valid Let's Encrypt chain:
`SetAVTransportURI` is accepted, the TV opens a connection to port 7777 — and
the node logs `client sent an HTTP request to an HTTPS server`. A DLNA renderer
takes the URL it is given and fetches it in plaintext; it has no TLS stack to
speak of and no way to be given one. The user sees "An error has occurred" and
nothing plays. Nothing about the metadata, the codec or the bytes is involved:
the same URL range-serves correctly to anything that can speak TLS.

Options considered:

- **Serve the whole client API over plain HTTP.** Undoes ADR-0072 and the login
  it exists for. Rejected.
- **Put the render mount behind a reverse proxy that terminates nothing.** Moves
  the problem to an operator-run proxy the node cannot see, for one route.
  Rejected for the same reason ADR-0072 rejected "a reverse proxy somewhere".
- **Let the renderer fetch over HTTP on the API port.** Go's server cannot serve
  both on one socket, and a listener that quietly accepts plaintext for some
  paths is exactly the state ADR-0072 refuses to fall into.

## Decision

**An optional second TCP listener, `http.render_addr`, that is always plain HTTP
and serves exactly one mount: `/render` (ADR-0040).** The router behind it is
the same router, behind a gate that answers 404 to any path outside the
capability mount before the router sees it — the bearer API, the login, the
relays and metrics are never reachable in plaintext.

When `render_addr` is set, a renderer is handed `http://<render_addr>/render/…`.
Browsers, phones and the login keep `public_origin`. The split lives in
`controller.rendererBaseURL` (renderers) beside `renderBaseURL` (everything
else); nothing outside the renderer path changes.

`render_addr` must name a concrete, non-loopback host and a fixed port —
a wildcard names no address to mint a URL from, and loopback would hand every
renderer a link to itself — and is refused at config load otherwise, as
`http.addr` and `public_origin` already are.

## Consequences

- A television plays again on a TLS node. The capability URL keeps every
  property ADR-0040 gave it: unguessable, expiring, single-blob, no identity.
  What it loses on this listener is transport encryption on the home LAN, which
  ADR-0040 already declined to treat as a trust boundary — the bytes were
  fetchable by anyone holding the URL before, and the URL was already in a
  UPnP request sent in plaintext to the television.
- The plaintext surface is one route wide, enforced at the listener, not by
  each mount's own auth. `TestRenderOnlyAdmitsOnlyTheCapabilityMount` pins it.
- A node without `render_addr` is unchanged: renderers get the public origin
  exactly as before, and a plain-HTTP node never needed this.
- The systemd unit's `IPAddressAllow` must cover the LAN the render listener
  binds on, exactly as it does for the API listener; nothing new to open.
