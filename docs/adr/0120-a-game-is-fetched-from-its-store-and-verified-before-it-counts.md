# 0120. A game is fetched from its store, and verified before it counts

**Status:** Proposed
**Date:** 2026-10-20
**Follows:** ADR-0118 (`game` content type, the store route), ADR-0062 (a tagged
subprocess transport), ADR-0085 (a direct-route want paced by its own schedule)

## Context

ADR-0118 put games on `acquisition.RouteStore` and made the edition's store
facts the only thing the `owned` profile judges, so a scanned installer part
could never pass for a whole game. That left nothing able to write those facts,
and nothing to start a fetch: a game want sat MISSING forever.

A game's bytes come from a store the operator owns it on. For GOG,
`lgogdownloader` downloads a game's offline installers against the operator's
own library, and `--status` checks every file against GOG's manifest (size and
MD5). That check is the evidence ADR-0118 asked for: GOG itself says the
package is whole.

## Decision

### 1. The trigger is a beat over game wants, not a followed source

A followed source projects its items onto one Work (a series' episodes, a
feed's articles). A store library is the opposite shape: every item is a
different game, a Work of its own. So the trigger is a controller beat, the
sibling of the subtitle fetch beat (ADR-0085): it finds game wants that are
resting, unsatisfied, and whose Work names the game on a store (an `external_ids`
row, `source = 'gog'`, value = the GOG gamename), and paces them with a
`store_fetch_schedule` (attempt at once, then 6 h doubling to a 7-day cap, never
abandoned). An attempt is claimed with a compare-and-set before anything starts,
so two controllers never begin the same tens-of-gigabytes fetch twice, and it
counts as fruitless until the want is satisfied, so a fetch that fails backs off
without anyone recording the failure.

Two wants for one game (two profiles) get one fetch: a want is not due while
another want for the same Work is in flight or backing off (the backoff is the
game's, not one want's), and a pass starts at most one per Work. The single ingest satisfies both. A selection whose grab never reached
the queue — a shutdown between recording SELECTED and enqueueing — is found
after a ten-minute grace and its grab re-queued; the grab's dedupe key makes
that safe to repeat, and nothing else would ever move a want stuck there.

A want already holding bytes that do not satisfy it (scanned installer files, a
package that lost a part) is still due: the verified package is what it lacks,
and the held bytes stay held while it is fetched. Only a satisfied want is past
a store's help.

The beat records the store package as the want's direct release (the walk a
podcast enclosure takes, with its own `release/store-owned` provenance) and
enqueues the ordinary grab. From there it is an ordinary acquisition: grab,
poll, verify, ingest, with every phase event the download path already emits.

A game is named on its store by the want itself: a work descriptor's
`store_ref` (`gog:<gamename>`) is matched before the title — the store's name for
a game is exact where a title is not — and recorded as the Work's external id.
`heyarr stores sync gog`, run as the service user, lists what the account owns
(`lgogdownloader --list json`) and wants each game that way; re-running it wants
nothing twice, so a host timer can keep a library backed up as purchases land.
The sync is a CLI over the public API rather than a job: listing a library needs
the store login, which lives with the service user, and the API's want path is
the one every other door already goes through.

### 2. The fetch is a download client that refuses anything but `gog:`

`KindLgogdownloader` is a download client in the yt-dlp mould (ADR-0062). It
accepts only `gog:<gamename>` sources, so it composes with the HTTP, torrent and
yt-dlp clients regardless of registration order. The gamename is checked
against `^[a-z0-9_]+$` and refused otherwise, never escaped, because the store
tool takes it as a regular expression. Fetches run one at a time: a backfill of
a whole library must not split one link and one disk across every game at once.
The release a want records is `gog:<gamename>@<want>`: one transfer and one
acquisition per want, so two profiles wanting one game never share — and strand —
an acquisition row. Both fetch into the same per-game directory, one at a time.
A transfer that is over, failed or done, is fetched again by the next attempt:
that attempt exists only because the earlier package did not stick, and
lgogdownloader keeps the files that are already right, so it costs a
verification rather than a download.

### 3. A transfer is done only when GOG vouches for every file

After `--download`, the client runs `--status --no-fast-status-check` (the fast
check compares sizes only) over the same game's installers, base game and DLC.
The transfer completes only if every file is `OK`. A missing part, a checksum
mismatch, a short file, or a package GOG lists nothing for fails the transfer
with the counts, and nothing reaches ingest.

What `OK` proves is precise, and the ADR should not claim more. The file list
is always GOG's, so a missing part (`ND`) is always caught: that is the
completeness ADR-0118 needed. The MD5 comparison runs against GOG's published
checksum, which GOG provides for installers (the only files fetched here),
patches and language packs; for a file GOG publishes no checksum for,
lgogdownloader reports `OK` without comparing. So an `OK` package is "every
part GOG lists, each matching GOG's checksum where GOG states one".

The bytes can change between that check and the ingest job, so the client
leaves what GOG vouched for beside the package (`.heyarr-store-manifest.json`:
each file's name and size), and ingest holds a store release to it before
anything is hashed. A part missing or a different size fails the acquisition as
a verification failure; it never shrinks what is recorded as the package. BLAKE3 stays the byte identity
(invariant 1); GOG's MD5 is the store's statement that the set is complete, not
a second identity.

### 4. Ingest is the one writer of an edition's store facts

When the selected release is a store release, ingest records the store's facts
(`store`, and `drm_free` where the store states it) on every edition the
package filled, before INGESTED, emitting `content.edition.verified`.
Re-running it changes nothing and emits nothing.

The facts are recorded with the package they vouch for: `package_blobs`, every
file the fetch verified, base game and DLC together, written on each edition the
package filled. A store vouched for the set, never for one file, so the set is
the unit everywhere after:

- **Content.** A package's store facts count only while every one of its blobs
  is held. A data part that goes missing, or a DLC installer held without its
  game, falls back to undetermined. Store facts with no package list count for
  nothing.
- **Placement.** A package is placed only when every one of its blobs is, at
  every required peer (`EvaluatePackagePlacement`, the worst file's verdict).
  Placement for any other content is still the satisfying asset's blob; a
  one-file album track satisfying an album want is a separate, older question. No other release, and no scan,
writes them. Game files are not ffprobed: an installer has no stream to read,
and probing one costs reading it.

### 5. The GOG account is lgogdownloader's

Heyarr holds no GOG credential. The operator logs in once as the service user
(`lgogdownloader --login`), and platform and language choices live in
lgogdownloader's own `config.cfg`. An expired login surfaces as the transfer's
error. That is the same stance as a torrent daemon, configured outside Heyarr.

```yaml
providers:
  - name: gog
    type: lgogdownloader
    path_map:
      - remote: /srv/downloads/games
        local: /srv/downloads/games
```

## Consequences

- A game want whose Work names a GOG game is fetched, verified, ingested, and
  satisfied under `owned`, with no human step past the one-time login.
- A failed fetch is visible where every failed acquisition is (the transfer's
  error, the blocked release, `get_acquisition_status`), and is retried on the
  store schedule.
- Download transfers are in-memory, as the yt-dlp client's are: a restart
  mid-fetch loses the transfer, and the next attempt resumes lgogdownloader's
  partial files rather than starting over.

## What would make us revisit

- **Epic and Steam.** `legendary` and DepotDownloader slot in as more
  `<store>:` schemes and clients. A Steam depot is DRM-bound and its verification
  is the depot manifest; it will write `store` without `drm_free`.
- **Extras.** Soundtracks and manuals are excluded (`--include i`). If they are
  wanted, they are their own editions, verified the same way.
- **Concurrency.** One fetch at a time is right for one home link. A box on a
  faster link may want a configurable slot count.
