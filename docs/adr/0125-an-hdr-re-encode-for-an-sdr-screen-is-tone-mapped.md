# 0125. An HDR re-encode for an SDR screen is tone-mapped, and one that cannot be is refused

**Status:** Accepted (2026-11-19)
**Date:** 2026-11-18
**Builds on:** ADR-0069 (the on-the-fly repackage), ADR-0123 (copy selection), ADR-0023 (the toolchain is optional)
**Refs:** #886, #885, §33, §68

## Context

A re-encode (ADR-0069) decodes the source and writes eight-bit H.264. From an
HDR10 or HLG source that output is still tagged PQ or HLG with BT.2020
primaries, because libx264 inherits the tags. Played on an SDR screen it looks
grey and washed out. The probe could not tell this apart from SDR: it recorded
no Dolby Vision at all, and no transfer before #885. ADR-0123 made the fact
steer selection only, "because nothing tone-maps".

Dolby Vision profile 5 is a separate problem. Its samples are IPTPQc2, which
only make sense once the RPU is applied. A decoder that ignores the RPU shows
them green and purple. The pinned ffmpeg build has zimg (`zscale`) and
`tonemap`, but no libplacebo, so it cannot apply the RPU.

## Decision

**When `Negotiate` re-encodes the video and the client does not declare HDR,
an HDR source is tone-mapped to BT.709 SDR and tagged as such. When the source
is Dolby Vision with no compatible base layer, a video re-encode is refused for
every client.**

- **What the picture is** comes from the probe (#886). Dolby Vision's
  base-layer compatibility decides when a Dolby Vision record is present,
  because that base layer is what the decoder hands the encoder. Otherwise the
  transfer decides. The result is `pq` (HDR10, DV 7 and 8.1), `hlg`
  (including DV 8.4), `sdr` (including DV 8.2's base), or
  `dolby_vision_only` (profile 5, or any record with no compatible base).
- **A tone-map never causes a re-encode.** HDR that the client decodes is
  still handed over as it is. A remux copies the video and so copies its
  colour. Only a re-encode that is already happening, for a codec or a
  height, gains the stage. Forcing a re-encode for HDR alone would transcode
  every HDR file for every device left at the default `supports_hdr = 0`
  (ADR-0123's concern).
- **`supports_hdr` now feeds the leg**, field by field as in ADR-0123: the
  request's value first, then the named device's stored value, otherwise false.
  An HDR client's re-encode is not tone-mapped. That is the eight-bit PQ
  stream it got before this change.
- **The tone-map is software**: `zscale` to linear light, then into the BT.709
  gamut, then `tonemap=hable`, then `zscale` back to BT.709, after the height
  cap. The node's encoder is libx264, so a hardware tone-mapper would save
  nothing on the decode or the encode. HDR is assumed to be mastered in
  BT.2020.
- **A build without the filters degrades and says so.** The streamer asks
  ffmpeg once for its filter list. If `zscale` or `tonemap` is missing, the
  stream runs without a tone-map, and the plan's `reason` says the picture
  will look washed out. A missing filter must not turn into a stream that fails
  to start.
- **Dolby Vision profile 5 is refused, not mis-encoded.** The plan answers
  `mode: unplayable` with `dolby_vision_no_compatible_base` among its reasons.
  The refusal applies to HDR clients too, because the re-encode drops the RPU
  either way. A client that decodes the file as it is still gets `direct`: it
  may well be a screen that decodes Dolby Vision, and the node cannot tell
  that from the declaration.
- **Selection is unchanged.** ADR-0123's rule 2 still prefers an SDR copy for
  a screen without HDR. A tone-map approximates the SDR grade; it is not the
  grade itself, and it costs a core.

## Consequences

A phone at 1080p playing a 2160p HDR10 remux gets correct colours. A browser
that cannot decode HEVC gets the same. A stream token now carries the
tone-map as flag bits. A node older than this rejects such a token as
malformed rather than streaming it without the tone-map. A profile 5 file
offered to a client that needs a re-encode answers with a code the client can
act on: try another copy, or play it direct on a Dolby Vision screen.

## Revisit when

A build with libplacebo (or a hardware path that applies the RPU) is pinned,
which would let profile 5 be re-encoded. A client can declare Dolby Vision,
which would let profile 5 be refused only where it would actually break. Or a
measurement shows that a tone-mapped re-encode is good enough to outrank an
SDR copy in ADR-0123's selection.
