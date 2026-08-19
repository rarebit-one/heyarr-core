# 0001. Module path and repository identity

**Status:** Accepted
**Date:** 2026-08-19

## Context

Heyarr needs an import path before the first line of code. The project owns
`heyarr.com`, so a vanity import path (`heyarr.com/heyarr`) is available.

## Decision

The module path is `github.com/rarebit-one/heyarr-core`. `heyarr.com` is for
documentation and never appears in an import path.

## Consequences

A vanity path permanently couples every downstream `go get`, the module proxy,
and the checksum database to our DNS and a live `go-import` meta-tag endpoint,
served from a homelab. A lapsed or misconfigured domain then breaks builds for
everyone who ever depended on us, retroactively. The convenience is not worth
that failure mode for a one-maintainer project.

The repository name carries the `-core` suffix because the project expects
siblings: a documentation site, and eventually the extracted Storage Fabric
(§18). Module paths are breaking to change, so this is a day-1 decision.

## Revisit if

The project grows an organisation of its own, at which point a *new* module path
under it is a deliberate major-version move, not a rename.
