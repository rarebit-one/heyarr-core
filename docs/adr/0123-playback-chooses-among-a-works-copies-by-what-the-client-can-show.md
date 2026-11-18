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

- **Browse's `primary_asset` for a film is the copy this rule chooses for a
  client that declares nothing.** That means SDR before HDR, then the tallest,
  then the highest bitrate, then the id, with unprobed copies last. It replaces
  "the oldest primary asset". A card tap then plays a copy every screen shows
  correctly, and the answer is stable across requests. The pick is SQL and the
  rule is Go, so a test holds the two to one answer. Other content types keep
  creation order: a series' primary files are different episodes.

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

## Addendum (2026-11-18): a person's preferred quality has a say (#887)

The first "revisit when" arrived. A person wants *best quality*, *fit this
screen* or *data saver*, with an optional height or bitrate cap of their own,
and wants it to follow them from phone to television, with one device able to
differ.

**Decision.** The preference is personal state, so the node never reads it.
The client resolves it and sends the result as `preference` on
`POST /playback/plan`; the node applies what it is sent and stores nothing.

- **Where it lives.** The synced default is a last-writer-wins register in the
  person's encrypted personal state: a `preferences` role space, folded with
  the reading-position lattice (a per-key LWW map of opaque strings, §45),
  under the key `playback.quality`. No new CRDT and no new wire format; the
  server sees ciphertext (Invariant 6). A device's override is a local setting
  on that device and is never synced. The client lays the override over the
  default (an override replaces the default's quality; the cap is the
  person's), and an absent preference is `fit`.
- **Finding the space on another device.** Role spaces are recorded per
  device. A device that has recorded no preferences space adopts one it can
  open whose fold holds the key, the lowest space id when there are several,
  so two devices that each minted one converge on the same space.
- **What the node does with it** (`playback.Preference`, pure). `fit` keeps
  the screen's height as the limit. `best` lifts it: the tallest copy is
  chosen and planned without scaling, because the screen downscales better than
  a re-encode does. `data_saver` limits the height to 720 lines or the screen,
  whichever is shorter, and takes the lower bitrate at equal height. The
  person's cap is a further ceiling under any quality. The same height bounds
  the choice and the plan, by `asset_id` as well as by `work_id`. The codecs
  and HDR stay the device's facts, so `best` on a screen without HDR still
  prefers an SDR copy. The bitrate cap steers the choice only, like
  `max_bitrate_bps`.
- **The answer echoes the preference it applied.** A client that sent one and
  gets no echo is talking to a node that predates this and ignored the field.
  It can then fall back to choosing the copy itself.

**Revisit when** the node gains a personal-state reader of its own (a trusted
service recipient, ADR-0107), which would let a television that cannot run a
client still follow the person's preference; or when role spaces are
discovered by a tagged kind (ADR-0113) rather than by what they hold.
