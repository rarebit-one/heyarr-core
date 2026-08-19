# 0019. MCP lands in Milestone 3, not Milestone 1

**Status:** Proposed
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
