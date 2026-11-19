# 0126. A stream plan is also a seekable HLS presentation, cut where every run cuts it

**Status:** Proposed
**Date:** 2026-11-19
**Amends:** [ADR-0069](0069-a-client-that-cannot-decode-the-source-gets-an-on-the-fly-repackage.md) ("no HLS", "no seeking in v1", and its first revisit trigger)
**Builds on:** ADR-0040 (a renderer fetches with a capability), ADR-0079 (the plain-HTTP render origin), ADR-0106 (the planned audio track), ADR-0125 (the tone-map)
**Refs:** #908, §33, §68

## Context

ADR-0069's repackage is a live fragmented MP4. It is seeked by restarting it
with `?start=`. It named the case that would retire this: "seeking that the
client cannot fake". Cast is that case. The Google Cast Default Media Receiver
reports the buffered end as the duration, so the TV's own scrub bar stops at
the buffer. A seek from the TV remote past it snaps back. The senders now carry
restart-seek and `sourceOffset` logic that guesses at the receiver's clock
(heyarr-kmp #248, #249), and a seek started on the TV remote still never
reaches it. The players that can seek a VOD playlist (the Cast receiver,
AVPlayer, Media3, mpv) are all given a stream they cannot seek.

## Decision

**A `stream` plan also offers `hls_url`: a VOD playlist of the whole film,
whose segment boundaries are fixed before anything is produced. The segments
are made on demand by one ffmpeg per client, which is retargeted when a player
seeks.** The live stream and `url` stay as they are.

- **Boundaries come first.** A player maps a time to a segment by adding up
  `EXTINF`s, and then fetches that segment by number. So segment *n* has to be
  the same span of the film whichever run produced it. ffmpeg's HLS muxer
  cuts at the first keyframe after a running total, which depends on where the
  run started. So we compute the cuts ourselves and cut a continuous
  fragmented MP4 at them. A copied picture is cut at the source's own
  keyframes: the first keyframe at least 6 s after the previous boundary. They
  are read once per blob with an ffprobe packet pass, kept by digest, and the
  read starts at planning time. A re-encoded picture is cut every 6 s, and the
  encoder is forced to put a keyframe at each cut.
- **One clock for every run.** Each run writes fragments whose decode times are
  on a timeline shared by all runs: `-avoid_negative_ts disabled`,
  `+frag_discont`, no edit list, a fixed timescale, and a 10 s offset so that
  B-frames and AAC priming never decode before zero. A copy keeps the source's
  timestamps (`-copyts`). A re-encode seeks exactly to its first boundary and
  offsets its output by that boundary instead. `-copyts` is not used there,
  because what the forced-keyframe clock counts from under `-copyts` differs
  between ffmpeg releases. The init segment comes out byte-identical from
  every run. That was measured, and a run whose init differs is logged.
- **fMP4 (CMAF-style) segments, not MPEG-TS.** One init segment plus
  `moof`/`mdat` segments is what Cast, AVPlayer, Media3 and mpv all take. It
  is the only HLS packaging Apple accepts for HEVC, and it carries every codec
  the live stream already copies. The segmenter is the live stream's muxer,
  cut at the boundaries. It parses only box headers and `tfdt`/`trun`, and
  never rewrites a byte.
- **On demand, bounded, retargeted.** A request for a segment the current run
  will reach within two segments waits for it. A request for any other segment
  stops that run and starts one at the requested segment, so a seek far ahead
  does not produce the film in between. A run stays at most four segments
  ahead of the furthest segment requested, and then stops reading ffmpeg. The
  pipe fills, and ffmpeg blocks without being killed. Segments more than three
  behind the playhead are deleted. A run nobody has asked anything of for 30 s
  is stopped. A session untouched for ten minutes is forgotten and its
  directory removed. The cache (`media.hls_cache_dir`, default
  `<data_dir>/hls`) is cleared at start: it holds derived bytes nothing
  references.
- **The cap is shared.** A run takes a `media.stream_concurrency` slot through
  the same streamer as the live stream. Past the cap, the answer is 429 with
  `Retry-After`, as before. A paused player keeps its slot until the 30 s idle
  limit and then gives it back.
- **A capability, not a credential.** A Cast receiver or a TV fetches the
  playlist and every segment itself and has no credential to present. So the
  routes are a public mount under `/render/hls/{capability}/`, beside
  ADR-0040's render route. The plain-HTTP render listener, the access-log
  redaction and the constant problem `instance` all apply as they do there.
  The capability is the stream token's plan (blob, per-stream copy or
  re-encode, height cap, tone-map, ADR-0106's audio track) signed under a
  third key derived from the same secret. So a stream token, a render
  capability and an HLS capability never verify as one another. It records
  the planning credential for the log and does not check it. It is good for
  six hours plus the film's running time, because every segment is a new
  request. CORS allows any origin, since the capability is the authority and
  no cookie rides along. This is the second user of `MountPublic`, and this
  ADR is the record that field's comment asks for.
- **Advertised, never imposed.** `hls_url` and `hls_mime`
  (`application/vnd.apple.mpegurl`) appear beside `mode: stream` only on a
  node with ffmpeg, ffprobe and a cache directory, and only for a source with
  a picture: segments are cut on video keyframes and fragments, so audio alone
  is offered the live stream. A client that ignores them plays `url` exactly
  as before.

## Consequences

- A receiver shows the film's real length and seeks it natively, from the
  sender or the remote. A client can drop its restart-seek tricks when it has
  `hls_url`.
- A copied picture's first playlist costs one demux of the whole file. The
  pinned ffmpeg listed every keyframe of a 1.5 GB, 10-minute file in 0.2 to
  0.5 s from the page cache. A cold read was not measured; it is bounded by
  the disk's sequential read, which for a large remux on a spinning disk is
  most of a minute. The pass starts at planning time and is remembered for the
  process's life, but the first playlist for a just-planned large remux on
  slow storage can still wait for it.
- Audio re-encoded by two runs need not share an AAC frame grid. At a seek
  seam, a player can see a sub-frame overlap or gap. Players absorb that, and
  no seam exists within one run.
- The repackage has two shapes on one arm. A change to the codec decisions
  (`codecArgs`) changes both.

## What would make us revisit

- **The first-playlist wait** on real storage proves long enough that
  receivers time out. The next step is reading the container's own index
  (Matroska Cues, MP4 `stss`) instead of demuxing, or persisting the keyframe
  list beside the blob.
- **A receiver that refuses HTTP segments from an HTTPS page** (mixed
  content), so that Cast through a plain-HTTP origin or relay does not play.
  That is a deployment requirement on the origin, not a change here, unless
  it becomes common.
- **More than one rendition** (a ladder, or audio as its own rendition so a
  client can switch tracks live). That is a multivariant playlist over this
  one, and it is ADR-0106's own revisit trigger.
- **The live stream has no users left.** Then `?start=` and the restart
  machinery can go.
