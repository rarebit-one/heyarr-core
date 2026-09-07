# 0076. Heyarr resolves the .torrent and hands the client the metainfo

**Status:** Accepted
**Date:** 2026-09-07
**Builds on:** ADR-0025 (external services are optional and capability-routed), ADR-0028 (discovery binds to Torznab, not Prowlarr), ADR-0031 (provider credentials are typed)
**Refs:** #492, §58, §64

## Context

A grab parked at SELECTED forever and never moved. The download client refused
it with `transmission: the call was declined: Couldn't fetch torrent: No
Response (0)` (#492).

The cause was who fetched the `.torrent`. The acquisition flow hands a download
client a `secret.Value` source — from a Torznab indexer this is the enclosure
download URL — and the Transmission client passed it straight to `torrent-add`
as `filename`, which Transmission then fetches itself. In the reference
topology the indexer is a loopback-bound Prowlarr on `127.0.0.1:9696`.
Host-native Heyarr can reach that loopback; the download client, in its own
container network namespace, sees `127.0.0.1` as its own empty loopback, so its
fetch of the URL gets "No Response". The URL was correct and reachable — just
not from where the client stands.

This is not a Transmission quirk. Any download client asked to fetch a URL is
subject to whatever the client can reach, which is not what Heyarr can reach.
The client that discovers a release and the client that moves its bytes have
different network vantage points, and coupling the second to the first's
reachability is the defect.

Two constraints shaped the fix.

**A private tracker embeds a passkey in the `.torrent` itself.** So "resolve the
source to a bare magnet from its infohash and hand that over" is wrong: it drops
the passkey, and the transfer never announces. Whatever Heyarr does has to
preserve the bytes the indexer served.

**The source is a secret (ADR-0031, ADR-0025).** It carries a credential —
Prowlarr's apikey, a tracker passkey — which is exactly why `secret.Value`
hides it and why ADR-0025 refuses userinfo in a configured endpoint. A fix that
fetched the URL must keep it out of every log line and error string, as the
existing code already keeps `source.Reveal()` out of them.

## Decision

**When the source is an http(s) URL, Heyarr fetches the `.torrent` itself and
hands the download client the bytes as `metainfo`, never the URL. A `magnet:`
source is passed through unchanged.**

Concretely, in the Transmission client's `Add`:

- **http(s) URL → `metainfo`.** Heyarr GETs the URL over its own HTTP path —
  the same vantage point that reached the indexer to discover the release — and
  passes the base64 of the response as `torrent-add`'s `metainfo`, the
  documented alternative to `filename`. The client is never asked to reach the
  indexer. Fetching preserves the passkey embedded in the `.torrent`, which a
  magnet-from-infohash would drop.
- **`magnet:` → `filename`.** A magnet has no file to fetch; the swarm moves the
  bytes via DHT and trackers. It is passed through byte-for-byte, so nothing
  Heyarr does can drop a tracker from it.
- **Anything else → `filename`.** A local file path an operator configured is
  left untouched: opening a local file never depended on reaching the indexer,
  so pre-#492 behaviour is preserved for it.

The decision is on the **scheme**, not a `.torrent` suffix: a Torznab download
link routinely has no extension — Prowlarr's is `/<n>/download?apikey=…` — and
the only reason a torrent client is ever handed an http(s) link is a `.torrent`
behind it.

**The credential rides in the URL.** A Torznab download link is self-contained:
it carries its own apikey/passkey query, the same self-contained link the
plain-HTTP download client already fetches. The fetch issues the URL verbatim
and adds no credential of its own — in particular not the client's RPC basic
auth, which would otherwise leak to the indexer. The URL is never named in an
error; it reaches an operator's log through `registry.Grab` exactly as `source`
does, which is to say redacted.

The fetch is bounded (16 MiB — a `.torrent` is metadata, not payload) and runs
on the grab job's own context, because resolving a small metadata file is part
of the add, not a background transfer like the plain-HTTP client's.

This is the Transmission client only. qBittorrent has the same exposure, but its
`torrents/add` takes uploaded bytes as a multipart `torrents` file part rather
than a form field, so its transport needs a multipart path first; it is left as
a follow-up and still passes the URL as `urls` until then.

## Consequences

- A grab against a loopback-bound or otherwise client-unreachable indexer
  completes: the client receives bytes, not an address it cannot resolve. The
  #492 failure mode cannot recur for Transmission because the client is never
  asked to fetch.
- Heyarr now spends one small HTTP round trip inside the grab for an http(s)
  source. It is bounded and on the job's context, so a slow or hostile indexer
  fails the grab cleanly rather than hanging or exhausting memory.
- Private-tracker correctness is preserved: the passkey in the `.torrent`
  survives, where a magnet-from-infohash simplification would have silently
  dropped it and produced transfers that never announce.
- A magnet is untouched, so nothing changes for the case Heyarr already handled
  correctly.

## What would make us revisit

- **qBittorrent parity.** The same fix wants the multipart `torrents` upload on
  the qBittorrent client; until then it carries the same exposure, and an
  operator running qBittorrent behind a client-unreachable indexer would see
  #492 there.
- **A source that is neither magnet nor a fetchable `.torrent`.** If an indexer
  emerges whose http(s) link is a landing page rather than the file, the scheme
  test is too broad and the discrimination moves upstream to where the source is
  produced (the indexer's `sourceOf`), not here.
- **A very large or streamed metainfo.** The 16 MiB bound is generous for
  metadata; if a real `.torrent` ever approaches it, the number is the thing to
  change, not the mechanism.
