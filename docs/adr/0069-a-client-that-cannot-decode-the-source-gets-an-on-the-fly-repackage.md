# 0069. A client that cannot decode the source gets an on-the-fly repackage

**Status:** Accepted
**Date:** 2026-09-03
**Builds on:** ADR-0013 (blob serving is a contract), ADR-0023 (the toolchain is optional), ADR-0040 (a renderer fetches with a capability), ADR-0044 (the client blob route serves partials)
**Refs:** #202, #432, §33, §68, §70

## Context

The first-party phone client streamed the raw blob and got two failures the
planner already knew how to name and nothing knew how to fix. An episode with
AC-3 5.1 audio in MP4 played picture with no sound — the phone has no AC-3
decoder. An AVI holding H.264 and MP2 showed a duration and a black frame — the
player's AVI extractor does not yield frames for that pairing. In both cases
`POST /playback/plan` would have answered TRANSCODE or REMUX, and in both cases
that answer pointed at nothing: M2 built a remux *job* that writes a derived
asset minutes later, and #202 recorded that "§33 promises HTTP/HLS and nothing
produces it". The planner could refuse; it could not serve.

Two constraints were already settled and shaped the answer.

**ADR-0013 forbids tuning the blob endpoint for a player** — no transcoding on
it, no guessed media type, no player-shaped session token. Its consumers are
replication, remote probing and the web seed as much as playback, and the
route serves immutable, hash-named bytes with a length and ranges. A repackage
has none of those properties: the bytes do not exist until ffmpeg makes them,
have no digest, and cannot be ranged.

**ADR-0023 makes the toolchain optional**, so any answer that needs ffmpeg has
to degrade honestly on a node without it, and has to say so rather than fail.

The planner's request schema was also the wrong shape for a client: it named a
*registered device*, and a phone reporting what its own decoders are is not a
device row an operator authored. The issue noted the schema rejected the shape
the client would naturally send.

## Decision

**A client that says what it can decode is told what to fetch, and when it
cannot decode the source it is handed a second route that repackages the blob
on the fly: fragmented MP4, video copied where the client decodes it, audio
re-encoded to stereo AAC where it does not, one ffmpeg per request, killed when
the client disconnects.**

Concretely:

- `POST /api/v1/playback/plan` accepts `client: {containers, video, audio,
  max_height}` beside — or instead of — `device_id`, and then answers `mode`
  (`direct` | `stream` | `unplayable`), `url`, `mime`, `reason` and `source`
  in addition to the decision it already gave. The decision and its coded
  reasons are unchanged and still come from the same planner (`Choose`), with
  the client's declaration rendered as a device profile; a client and a
  registered device are explained in one vocabulary. The leg is a second pure
  function beside it (`Negotiate`), because "what would it take" and "what do
  I fetch" are different questions and the second carries what the
  repackager needs.
- `direct` is the ordinary blob endpoint, untouched. `stream` is
  `GET /api/v1/playback/stream/{token}`: a new route, not a mode on the old one.
  ADR-0013's prohibition is honoured by being a different route serving
  different bytes.
- **The repackage has one output shape.** Fragmented MP4 (`frag_keyframe +
  empty_moov + default_base_moof + delay_moov`), the *first* video and audio
  stream only. Video is copied unless the client cannot decode it or the
  picture is taller than `max_height`, in which case it is re-encoded with
  libx264 (`veryfast`, CRF 23) and scaled; audio is copied when the client
  declares the codec and fragmented MP4 can carry it (AAC, MP3, AC-3, E-AC-3,
  Opus, FLAC, ALAC), and re-encoded to stereo AAC at 192k otherwise. So the
  AC-3 case copies the picture and re-encodes the sound; the AVI case rewraps
  the picture and re-encodes MP2; a client that declares AC-3 gets the
  original track. There is no ladder and no HLS — this is not #202's device
  decision, it is the smallest thing that makes the phone play.
- **The token is the plan.** Signed by the node (a key derived from the render
  secret under a fixed label, so a render capability can never verify here),
  it names the blob, the copy/re-encode decision per stream, the height cap,
  an expiry an hour out, and **the credential that asked for the plan**. The
  route sits behind the same middleware as everything else — Bearer or Device,
  `read` scope — and additionally refuses a token presented by any credential
  other than the one it was minted for. The playback token (§68) is
  deliberately not scoped to one blob; this one is scoped to one blob *and*
  one caller. Every refusal is the same opaque 404 with the reason logged
  under a bounded label.
- **Bounded.** `media.stream_concurrency` (default 2) caps live streams; past
  it the answer is 429 with `Retry-After`, never a queue. ffmpeg is killed —
  not interrupted — the moment the request context ends. Its stderr is kept
  as a bounded tail for the log. A gauge, `heyarr_playback_streams_active`,
  reports what is running.
- **No seeking in v1.** No `Content-Length`, no ranges: a player treats the
  stream as live and unseekable (Media3 does, for fragmented MP4 without Range
  support). `?start=<seconds>` restarts a new ffmpeg with an input seek, which
  is a client's seek for now and is cheap because `-ss` before `-i` lands on a
  keyframe without decoding up to it.
- **A stream is produced where the bytes are.** A token is minted only when
  the routed replica is this node; a controller routing to another peer
  answers `direct` on that peer and says why — the same idiom as the render
  capability. Under `heyarr all` this is invisible.
- **Probe on demand.** With `client`, a blob nothing has probed is probed now
  when the node has ffprobe and holds the bytes, and recorded in `blob_probes`
  exactly as a worker's probe would be — keyed by hash, one row per blob. No
  new table: the cache the request asked for already existed as the worker's
  output, and a second one would be a second answer to one question.
- **Degrades by saying so.** A node with no ffmpeg answers `direct` with the
  reason and the note that it cannot repackage; a client that knows why can
  say "this needs ffmpeg on the server" instead of showing a black player.

## Consequences

- The phone plays the two files that opened #432, and the class of file they
  stand for (any decodable video with an undecodable audio track, any
  container Media3's extractors will not open), with no client-side decoder
  work and no derived assets on disk.
- **A stream costs a process.** A copy is cheap; a re-encode is a core. The
  cap is the control, and it is per node — a controller with a large library
  and many phones is exactly where §32's "the controller stays out of the data
  path" starts to matter, and where a Full Peer holding the replica is the
  right node to produce the stream. The routing rule above already sends the
  client there.
- `content_url` on a TRANSCODE/REMUX plan stays absent; `url` carries the
  stream. A client that only reads `content_url` keeps refusing what it cannot
  play, which is the old behaviour, not a regression.
- §33's "HTTP/HLS" is still half true. Ordinary HTTP over incomplete content
  (ADR-0044) and now a repackaged HTTP stream exist; a playlist still does
  not. #202 keeps the device decision — DLNA, HLS or a compatible API — and
  this route is the byte producer any of those would sit on.
- The acceptance demo proves it with a real ffmpeg on the Linux leg: a client
  that cannot open the container is served a stream that probes as H.264 and
  AAC in MP4. The bare leg records it as unexercised rather than passing
  vacuously.

## What would make us revisit

- **Seeking that the client cannot fake.** A scrub bar over a live stream is
  poor. If restart-with-`?start=` proves insufficient, the next step is
  segments — which is HLS, and which is #202's decision to make with the
  device story, not a flag here.
- **A second output shape.** WebM for a browser without MP4 support, or an
  audio-only leg for a speaker, would each be a `mode` the domain decides and
  a spec the streamer produces; the token already carries the decision.
- **Hardware encoders.** ADR-0039's advertised encoder capabilities exist;
  when a node holds one, the video re-encode should route to it. That is a
  change to the arguments, not to the decision.
- **A queue instead of a refusal.** If clients turn out to retry badly, a
  short bounded wait for a slot beats a 429. The cap stays; only what happens
  at it would change.
