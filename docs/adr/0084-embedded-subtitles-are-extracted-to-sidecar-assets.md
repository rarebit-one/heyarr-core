# 0084. Embedded subtitle tracks are extracted to sidecar assets on ingest

**Status:** Accepted (2026-09-09)
**Date:** 2026-09-09

## Context

Heyarr's caption path — the in-app players' external-subtitle support and the
DLNA `CaptionInfo.sec` header a television reads (ADR-0040) — serves a subtitle
only when it is its own asset: a blob with `role='subtitle'` and a whitelisted
text MIME, on the video's Edition, whose filename stem prefix-matches the video
(`internal/api/resources/renderers.go` `captionForRenderer`). That is the shape
a release ships when it puts a `Movie.en.srt` next to `Movie.mkv`, and it is why
those releases get captions everywhere with no special handling.

Driving this for real surfaced the gap: a title whose subtitles live *inside* the
container — `mov_text` in an `.mp4`, SubRip/ASS in an `.mkv` — has no such asset.
The concrete case was Yellowstone (thesim-family session): 17 episodes, no
sidecar files, two embedded English `mov_text` tracks per `.mp4`. `ffprobe`
already reports those streams during the §66 probe, but nothing lifts them out,
so every caption consumer sees a video with no subtitle and the television shows
nothing. GoT worked and Yellowstone did not, for this reason alone.

The bytes exist; they are just in a place the caption path cannot reach.

## Decision

A new `extract_subtitles` job lifts a video's embedded **text** subtitle tracks
out into ordinary `role='subtitle'` assets, so every existing consumer picks
them up with no change.

**Enqueued at ingest, beside the probe (§66).** `IngestHandler` enqueues it for
a freshly ingested video (`video/*`), exactly as it enqueues a probe — a job,
not an inline step, because it needs a capability this worker may not have
(invariant 4). A deduplicated blob is skipped, like the probe.

**Gated on the ffmpeg capability; degrades, does not fail (ADR-0023).** The
handler is registered only when both `ffmpeg` (to lift a track out) and
`ffprobe` (to enumerate them) resolved. On a toolchain-less node the job stays
pending and visible in the startup log — the same discipline as the remux and
probe handlers — never failing an ingest that otherwise succeeded.

**Text only; bitmap subtitles are skipped, not OCR'd.** `-c:s srt` losslessly
rewrites SubRip/ASS/`mov_text`/WebVTT into SubRip. PGS (Blu-ray), VobSub/DVD and
DVB are *images* of text; turning them into an `.srt` needs OCR, a different
feature with a different failure mode (wrong characters, not a failed job). The
extractor filters to an allowlist of text codecs — an unknown codec is skipped
rather than fed to a conversion that might emit nonsense.

**The output is an ordinary managed asset, the twin of a remux (ADR-0084 mirrors
RecordDerived).** `RecordExtractedSubtitle` attaches the extracted `.srt` to the
video's Edition with `role='subtitle'`, `mime=application/x-subrip`,
`source_path=NULL` (it has no on-disk file), a filename `<video-stem>.<lang>.srt`
that keeps the resolver's stem match, and the language/forced/origin in
`attributes`. Like a remux it is therefore replicated (M4), integrity-checked,
and GC-reclaimable with no special case — and, the point of the whole change,
**indistinguishable in shape from a sidecar the release shipped**, so the
caption resolver and the DLNA header serve it with zero consumer changes.

**Idempotent (invariant 9).** Keyed on `(edition, blob, role)`: a re-run
produces bytes the CAS deduplicates and converges on the same row. Two genuinely
different tracks are different bytes, so different blobs, and get their own rows
— correct, they are different subtitles. A per-track conversion failure is
logged and skipped (permanent for that track); only a store or database failure
returns an error, because those are the transient conditions a queue retry is
for.

## Consequences

- Yellowstone — and any title with embedded text subtitles — now gets captions
  in the apps and on the television, from the tracks already in the file, with
  no download and no new consumer code.
- A subtitle that exists *nowhere* — neither sidecar nor embedded — is still
  missing. Fetching one from a provider network is a separate concern
  (a subtitle provider adapter), deliberately out of scope here.
- Extraction runs even when a release already shipped an external sidecar,
  which can leave an Edition with both an extracted and a sidecar subtitle for
  the same language. The resolver's first-stem-match still serves one; a
  refinement that prefers the sidecar (or dedupes by language) is a known
  simplification left for later.
- No demo scene or `claims.list` entry yet — `acceptance.sh` is single-owner and
  a fixture video carrying an embedded track is follow-up (the "recorded
  pending" convention). The mechanism ships proven by unit tests, a real-toolchain
  round-trip (`internal/media/ffmpeg` muxes a SubRip into an MKV and extracts it
  back), a handler test covering the per-track failure paths, and a persistence
  test asserting the asset lands on the right Edition.
