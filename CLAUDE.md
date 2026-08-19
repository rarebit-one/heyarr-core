# CLAUDE.md

Guidance for Claude Code working in **heyarr-core**.

## What this is

Heyarr: a headless, self-hosted content lifecycle, replication and consumption
platform for homelabs. Plain Go, single binary, no build step beyond `go build`.
Public and AGPL-3.0 — this is the org's first open-source Go project and the
first repo here that outside contributors may read.

**Read [`docs/spec/heyarr-spec.md`](docs/spec/heyarr-spec.md) before designing
anything.** It is the authority, section-numbered, and referenced as `§n`
throughout the code, the ADRs and the issues. Where the code and the spec
disagree, the spec is the intent — unless an [ADR](docs/adr/) records a
deliberate departure.

## Quick reference

```bash
make build          # ./bin/heyarr
make test           # go test -race ./...
make lint           # go vet + golangci-lint
make gen            # regenerate committed generated code; CI asserts no drift
make demo           # scripts/acceptance.sh — the milestone gate
```

## Invariants — these are not style preferences

1. **Bytes are identified by their BLAKE3 digest; nothing else is identity.**
   Not paths, not filenames, not database IDs. A destination always verifies
   bytes itself and never trusts a claimed hash (§21, ADR-0005).
2. **The content domain does not know how or where bytes are stored.**
   `internal/domain/**` cannot import `os`, `path/filepath`, `database/sql`,
   persistence, or the CAS — enforced by `depguard`, not by review. Crossing the
   line means an interface is missing (§18, ADR-0006/0007).
3. **The Storage Fabric stays extractable.** `internal/storagefabric/**` cannot
   import the domain or the API.
4. **Roles communicate only through the job table and HTTP.** Never a shared
   in-process pointer, even inside `heyarr all`. If a call could not survive
   being a network hop, it is not allowed (§4, ADR-0002).
5. **Never active-active SQLite.** The control plane is single-writer; resilience
   comes from backup streams to peers, not replication (§48, ADR-0003).
6. **Encrypted personal state is opaque to the server.** No server-side
   plaintext, no server-side CRDT merge, ever. Peers store wrapped keys they
   cannot unwrap (§38, §41).
7. **Every state transition emits an event.** No exceptions — retrofitting is
   what makes this expensive (§76, ADR-0009).
8. **Deleting never unlinks bytes inline.** Logical delete, then a refcounted GC
   sweep with a grace window. Corrupt blobs are quarantined, never deleted
   (ADR-0018).
9. **Jobs are durable, leased and idempotent.** A handler that cannot be safely
   re-run is a bug; it *will* be re-run (§75, ADR-0008).
10. **Scope discipline (§83).** Every feature belongs clearly to the control
    plane, the storage fabric, the personal state plane, or an external
    specialist. Something that fits none of them cleanly usually wants to be two
    changes.

## Conventions

- Worktree-only workflow, as everywhere in this org — create a worktree, don't
  edit a main checkout.
- Table-driven tests, no mocking frameworks, real `t.TempDir()` filesystems for
  storage tests, golden files for API and CLI `--json` shapes, injected clock
  instead of `time.Sleep`, `-race` always.
- Conventional Commits, and every commit signed off (`git commit -s`) — this repo
  takes contributions under DCO.
- An architectural stance change needs an ADR. Keep it to a page; record the
  decision and what would make us revisit it, not a description of the code.

## Milestone discipline

The roadmap (§84) is eleven milestones and the order is load-bearing — each one
assumes the abstractions the previous laid down. Work assigned to a later
milestone should not be smuggled into an earlier one because it seemed easy;
that is how the peer model, the job queue and the event log end up needing to be
retrofitted.
