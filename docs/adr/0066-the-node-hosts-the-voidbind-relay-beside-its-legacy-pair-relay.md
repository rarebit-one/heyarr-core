# 0066. The node hosts the Voidbind relay beside its legacy pair relay

**Status:** Accepted
**Date:** 2026-09-02
**Builds on:** ADR-0022 (pairing, not sharing), ADR-0038 (the relay is untrusted), ADR-0048 (device-cert enrolment), ADR-0054 (a first-party device-side key-holder is the product)

## Context

Heyarr's pairing relay (`internal/pairrelay`, `/pair/sessions/{s}/slots/{slot}`)
and its handshake (`internal/pairflow`) predate the extraction of the identity
core into voidbind-go. They speak a private wire shape: six named slots, a
uvarint-framed reveal, a plaintext cert. voidbind-go v0.5.0 carries its own
`relay` and `pairflow`: `POST /v1/sessions`, `PUT|GET
/v1/sessions/{id}/{role}/{type}`, a JSON reveal, and a cert sealed to the
responder's X25519 key so the relay holds only ciphertext.

That second shape is the one every Voidbind client speaks — the `voidbind` CLI's
`pair-initiate` / `pair-join`, and the phone (`voidbind-kmp`, and heyarr-mobile
on top of it). So a phone cannot pair through the node it is pairing *for*: today
it pairs through a separately-run `voidbind relay` on whichever machine holds the
user identity, and only then presents the cert to heyarr. The node that will
verify the device is the one machine that cannot host the rendezvous. That is a
gap, not a design (#419).

## Decision

**Mount voidbind-go's `relay.Server` on the public router, under
`/pair/v1/...`, beside the legacy relay at `/pair` — additive, not a
replacement.**

- A Voidbind client is given `<node>/pair` as its relay *base*; the client
  appends the `/v1/...` paths itself, exactly as against a standalone
  `voidbind relay`. The invite QR `pair-initiate` prints therefore names the
  node, and the phone dials the node.
- The relay is voidbind-go's, unmodified: opaque, write-once, in-memory, no
  parsing, no keys. It is served with the **same caps the legacy relay
  enforces** (per-message bytes, live sessions, TTL) so the two mounts are one
  abuse surface, not two. It is public for the reason ADR-0038 already gave: it
  grants nothing.
- The two protocols have disjoint paths (`/pair/sessions/...` vs
  `/pair/v1/sessions/...`), and a test proves both answer on one router and a
  Voidbind pairflow completes end to end through the node with voidbind-go's
  own client, ending in a cert the user key verifies and that the relay held
  only sealed.

**`heyarr pair` is NOT switched onto voidbind-go/pairflow in this change, and
`internal/pairflow` + `internal/pairrelay` are kept.** The switch is not the
one-line dedup that retiring `internal/pairing` and `internal/enrolment` was.
voidbind-go's `pairflow.Initiator` takes the raw user private key, where heyarr's
initiator signs through the identity store's `SignCert` and never holds the key;
and voidbind-go's `pairflow.Responder` generates its own device keypair, where
heyarr's responder pairs the key `heyarr device` already minted and stored.
Bridging either means new surface in voidbind-go (an initiator over a signer
callback, a responder over an existing key) before heyarr can drop its copy.
That is a voidbind-go change first, then a heyarr retirement — two changes, each
clean — rather than a heyarr change that reaches around the library.

## Consequences

- A phone pairs through its own node. The user-identity machine runs
  `voidbind pair-initiate --relay <node>/pair`, the phone scans the invite, and
  the cert lands on the phone without a third machine in the loop.
- Two relays run in one process. The cost is one more in-memory map bounded by
  the same caps; the benefit is that nothing that pairs today stops pairing.
- The OpenAPI specification documents both (ADR-0015); the parity test walks
  both.

## What would make us revisit

- **voidbind-go grows a signer-backed initiator and a key-supplied responder.**
  Then `heyarr pair` moves onto voidbind-go/pairflow, `internal/pairflow` and
  `internal/pairrelay` retire the way `internal/pairing` and `internal/enrolment`
  did, and `/pair/sessions/...` is removed after a deprecation window. The
  relay mount in this ADR is the survivor of that change, unchanged.
- **The clients converge on something other than voidbind-go's relay.** There is
  no sign of it; the point of ADR-0054 is that the first-party client and the
  library share one identity core.
