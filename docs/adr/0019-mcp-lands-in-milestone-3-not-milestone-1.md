# 0019. MCP lands in Milestone 3, not Milestone 1

**Status:** Accepted
**Date:** 2026-08-19

## Context

Spec §71 lists MCP as a canonical interface, with tools like `want_content`,
`search_releases`, `explain_release` and `get_upgrade_candidates`. Milestone 1
has no desired state, no releases and no acquisition.

## Decision

Reserve `internal/api/mcp/`. Ship MCP alongside DesiredItem and acquisition in
Milestone 3.

## Consequences

MCP's value is *semantic actions over desired state*, not a second read API.
Shipping it in Milestone 1 would mean shipping `list_works` and `get_asset`,
publishing that vocabulary, and then breaking it once the interesting verbs
exist. Tool vocabularies are harder to change than endpoints because agents are
built against them.

Meanwhile the JSON API and the CLI's `--json` output already give an agent
everything Milestone 1 knows.

Note the boundary this eventually runs into (§72): controller-side MCP cannot
decrypt user artifacts. Private playlists and reading state stay inaccessible to
it unless an authorised user device surfaces them through a separate Personal
MCP (§73).

## What shipping it actually looked like (M3-14)

Nine of §71's fourteen verbs shipped. Five were **absent rather than stubbed**:
`search_releases` and `acquire_release` (no search job or download client yet),
`sync_peer` (one peer by design, ADR-0010), and `play_content` /
`transfer_playback`.

`sync_peer` shipped in Milestone 4 (M4-11), and its deferral is the one this
mechanism was for: the reason recorded against it was a missing CAPABILITY, so
when the second Full Peer arrived it was obvious what the deferral had been
waiting on and that the wait was over. A deferral phrased as "unfinished" would
have given nobody that signal.

The last pair is the interesting one, because the capability for `play_content`
arguably exists — Milestone 2 ships DIRECT playback. It is deferred anyway, and
the reasoning is this ADR's own: playback is **device-mediated**, returning a
credentialed URL scoped to a registered device, and §71 pairs it with
`transfer_playback`, which is not built. Shipping half of a device-control
vocabulary is the thing this ADR waited to avoid. Milestone 2 also refuses every
plan that is not DIRECT, so the verb would spend most of its life refusing —
which is a poor first impression of a vocabulary that cannot then be changed.

An agent that asks for a deferred verb is told **which milestone brings it**,
rather than that it mistyped. A name that is not a §71 verb at all gets no
milestone, so a typo and a not-yet are distinguishable — an agent told to wait
for something that is never coming will wait forever.

Two consequences that were not obvious when this was written:

**The write intents had to be extracted, not wrapped.** `want_content` is not
`POST /desired` with a different envelope, but it must not be a second
implementation of it either — so the intent moved into an exported method that
both doors call. There is precedent in the same package: the API once wrote an
acquisition row directly while the catalog's own path emitted, and an acceptance
assertion, not review, found that the two had silently diverged.

**"Not a second read API" also meant declining MCP features.** The server
advertises `tools` and neither `resources` nor `prompts`. A resource list is
precisely the browsable surface this ADR argued against, and it would have been
the easiest thing in the world to add.
