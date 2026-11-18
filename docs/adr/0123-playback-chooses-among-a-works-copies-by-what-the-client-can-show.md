# 0123. Playback chooses among a work's copies by what the client can show

**Status:** Accepted (the maintainer's go-ahead is the rule proposed on #885)
**Date:** 2026-11-18
**Builds on:** ADR-0069 (a client declares what it decodes), ADR-0075 (browse is a projection), ADR-0111 (the `smaller_copy` sibling query)
**Refs:** #885, §61, §68

## Context

§61 says never one version per title. A film can therefore hold a 2160p HDR
remux and a phone-sized 1080p encode, each kept by its own want. Nothing chose
between them at play time. `POST /playback/plan` required an `asset_id`, and the
`asset_id` a client had came from browse's `primary_asset`, which was the oldest
primary asset. So a phone was handed the 4K HDR copy and a re-encode of it,
while the 1080p copy sat unused.

The planner (`Choose`) and the leg (`Negotiate`) both answer "what does it take
to play THIS asset". The missing question is which asset to ask about.

## Decision

**A client may name a film (`work_id`, optionally narrowed by `edition_id`)
instead of a file. The node chooses the copy from what the client can show,
plans that copy exactly as if its `asset_id` had been sent, and says what it
chose, why, and what it passed over.**

- **The rule** (`playback.Select`, pure and table-tested). A copy *fits* when
  it is at or under `max_height` and `max_bitrate_bps`. An unstated limit is no
  limit, and an unknown bitrate is not held against a copy. Among the fits:
  1. one whose video the client decodes beats one needing a video re-encode;
  2. when the client does not declare HDR, SDR beats HDR, because nothing
     tone-maps (#886);
  3. then the tallest picture;
  4. then, at equal height, HDR when it is declared, the higher bitrate, and the
     asset id.

  When nothing fits, the smallest copy over the limit is chosen and scaled
  down as before. Unprobed copies rank last, because their height is unknown.
- **An explicit `asset_id` always wins.** A request carrying one is answered
  byte for byte as before, even if it also names a work.
- **The client's capabilities come from the request, field by field.** These
  are `max_height`, the new `supports_hdr` and `max_bitrate_bps`, and its video
  codecs. Any field the request leaves unstated is filled from the device named
  by `device_id`.
- **`supports_hdr` and `max_bitrate_bps` steer the choice only.** They do not
  feed `Choose` or `Negotiate`, because the repackager neither tone-maps nor
  caps a bitrate. For the same reason, the better HDR flag (the probed transfer
  characteristic: PQ or HLG) does not feed the planner's `hdr_unsupported`
  check. Feeding it would refuse HDR on every device left at the default
  `supports_hdr = 0`.
- **Films only.** A series edition can hold every episode, so "a copy of this
  work" is not a copy of the episode. Any other content type is a 400 that
  points the client to `asset_id`. "The same thing" is defined once, in the
  query the cast route's `smaller_copy` already used (ADR-0111): the same item,
  or any copy of a film. That query now also excludes missing files, and vault
  files for a Guest.
- **The work's assets carry the facts the rule reads.** Probed height, HDR
  flag and bitrate are on `WorkAsset` and `PrimaryAssetRef`, so a client can
  offer a manual choice without parsing `edition_label`.

## Consequences

A phone that sends `work_id` and `max_height: 1080` gets the 1080p copy,
played direct, with the 2160p copy listed as `resolution_too_high`. A
television that registered once sends `work_id` and its `device_id`, and gets
the 4K HDR copy. Probes taken before the transfer characteristic was recorded
read as SDR until the blob is probed again (the ADR-0106 stance).

## Revisit when

A client can name an episode's item, which makes selection safe for series. A
per-user or per-device quality preference (#887) wants a say in the ranking.
Or tone-mapping (#886) makes an HDR copy a fair choice for an SDR screen.
