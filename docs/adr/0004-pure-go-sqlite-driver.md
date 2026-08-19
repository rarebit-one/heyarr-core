# 0004. Pure-Go SQLite driver

**Status:** Accepted
**Date:** 2026-08-19

## Context

The two credible Go SQLite options are `mattn/go-sqlite3` (cgo, the real
library) and `modernc.org/sqlite` (SQLite transpiled to Go).

## Decision

`modernc.org/sqlite`, built with `CGO_ENABLED=0`.

## Consequences

Static binaries, trivial cross-compilation for the release matrix, no glibc
coupling, and no cgo in the race detector's path. Self-hosters get one file that
runs.

It is measurably slower than the cgo driver. That is acceptable because the
controller database holds thousands of rows, not millions, and Milestone 1's
cost is dominated by BLAKE3 hashing and disk I/O by two orders of magnitude.

## Revisit if

Profiling shows database time exceeding ~5% of a library scan. The successor to
evaluate first is `ncruces/go-sqlite3` (real SQLite compiled to WASM, run under
wazero): faithful to upstream and faster, with less `database/sql` mileage
behind it.
