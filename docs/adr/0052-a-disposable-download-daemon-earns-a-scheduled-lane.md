# 0052. A disposable download-client daemon earns a scheduled acceptance lane

**Status:** Accepted
**Date:** 2026-08-31
**Amends:** [ADR-0026](0026-indexers-are-not-reproducible-so-fixtures-are-the-primary-test-strategy.md)

## Context

ADR-0026 set the test strategy for the external services Heyarr targets: an
indexer and a download client. It decided **neither is pinned and neither runs
in CI**, because Heyarr installs neither — they are operator-managed services it
reaches by configuration. A download client's live exercise became an *opt-in
test pointed at whatever instance you have* (`TestLiveQBittorrent`), skipped when
unset, and deliberately **read-only**: adding a torrent to somebody's client
would mutate their queue and spend their bandwidth.

That read-only ceiling has a consequence ADR-0026 did not close. The claim
`acquires-over-daemon-clients` — *"a real qBittorrent transfer completed"* —
cannot be proven by a read-only test no matter how often it runs. So it sits
`pending` forever, and a permanently-pending claim about a shipped mechanism is
exactly the shape `scripts/claims.list` exists to make uncomfortable: a
download client whose *transfer* path no running daemon ever exercises is a
mechanism one assertion short of a caller.

ADR-0026 named its own revisit trigger:

> A second download client, at which point "an opt-in test pointed at whatever
> you have" needs to say *which* client it was pointed at, and the environment
> variable becomes a small matrix.

qBittorrent is that second download client (beside Transmission). The trigger
has fired.

## Decision

**A DISPOSABLE download-client daemon may be stood up in a NON-merge-path,
scheduled lane, and drive a full transfer to completion.** This amends ADR-0026
for the download-client column only. The indexer column is untouched: an indexer
proxies third-party services with real credentials and is *not reproducible at
any price* — ADR-0026's reasoning there stands in full.

Two things make this consistent with what ADR-0026 actually rejected rather than
a reversal of it.

**ADR-0026 rejected a container lane *on the merge path*, and this is not one.**
The objection was precise: "a lane living off the merge path — and a gate nobody
runs is a gate people stop running." This lane does not live off the merge path
in that dormant sense. It runs on a **schedule** (nightly) and on demand
(`workflow_dispatch`), never on `pull_request` or `push`. It gates no merge and
hangs no PR — the exact failure mode ADR-0026 named. And a scheduled cadence is
what ADR-0026 itself prescribed for the sibling drift problem: "the capture
procedure needs to run on a schedule rather than when somebody remembers."

**The read-only objection was about *somebody else's* daemon.** The whole reason
the live test cannot add a torrent is that the instance belongs to an operator.
A daemon the harness starts, seeds from a web seed on its own private network,
and tears down with `down -v` belongs to no one. Mutating it costs nobody, and
the bytes it pulls come from a local HTTP server, not a third party. The
objection does not apply, so the ceiling it justified does not either.

**Pinning here is test reproducibility, not supply chain.** ADR-0026 declined to
pin because Heyarr does not *install* these services — there is no supply chain
to secure. That argument is about production. In a disposable test lane the
images are test fixtures, and pinning them is the ordinary reproducibility
concern every fixture has. The harness pins nginx to a version tag and should
pin both images to digests once the lane has resolved them (see the harness
README); until then a moved tag surfaces as a scheduled-run diff, never a red PR.

**The transfer is proven by bytes, not by a status code.** The harness generates
a real single-file `.torrent` carrying a BEP-19 web seed, adds it through the
ordinary client `Add` path, watches it complete through real qBittorrent state,
and asserts the completed bytes are **byte-identical** to what the web seed
offered — resolving the path through the client's own path map. That is the same
standard the plain-HTTP and OpenSubsonic scenes hold, and it defeats the
mechanism-with-no-caller trap the same way: a real caller drove a real fetch.

## What this does NOT change

- **The merge-path demo still proves only what is honest without a daemon.** The
  `acquires-over-daemon-clients` claim stays `pending` in `scripts/claims.list`,
  because the *demo* does not prove it and should not pretend to. Its proof lives
  in the scheduled `daemon-acceptance` lane, and the claim's rationale now says
  so.
- **`TestLiveQBittorrent` stays** — it is the operator-instance, read-only
  exercise, a different and still-useful thing (it proves the client against a
  version of qBittorrent the harness image is not).
- **The indexer stance is unchanged.** Newznab/Torznab live-search against a real
  indexer remains out of reach here, for the reason ADR-0026 gave.

## Consequences

**SABnzbd and NZBGet are not yet in this lane, because their clients do not yet
exist.** §58 lists them, but no `providers.Downloader` implements them today.
Standing up those daemons now would be a daemon with no caller — the very defect
this project guards against. The harness pattern is built to extend: when a
SABnzbd or NZBGet client lands, it gets a compose service and a harness test of
the same shape, and its transfer leg flips from documented-pending to proven the
same way qBittorrent's did. Until then their transfer is a documented pending
step (see `scripts/claims.list` and the #379 checklist), not a faked green.

**The lane is non-blocking, so its own green is read, not assumed.** A scheduled
lane that nobody looks at is the dormant gate ADR-0026 warned about wearing a
different hat. Its result is narrated the way every other beat is, and a red
scheduled run is investigated like any other red — the difference is only that
it never blocks a human waiting on a merge.

## What would make us revisit

A download daemon that cannot be driven headless and deterministically — one
that needs a display, a paid credential, or a live swarm to complete a transfer.
That daemon goes back to the documented-manual column, because a lane that needs
a human or a third party is the read-only live test again by another name.
