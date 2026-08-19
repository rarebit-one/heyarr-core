# Contributing to Heyarr

Heyarr is pre-alpha. The most useful contributions right now are issues: real
homelab topologies, content types we have modelled badly, and places where the
[technical specification](docs/spec/heyarr-spec.md) is wrong rather than merely
unimplemented.

## Sign-off, not a CLA

Contributions are accepted under the [Developer Certificate of Origin](https://developercertificate.org/).
Sign each commit:

```
git commit -s
```

There is no CLA and no copyright assignment. Your contribution stays yours,
licensed under [AGPL-3.0-or-later](LICENSE) like the rest of the project.

## Before you open a PR

```
make lint test
```

## Scope discipline

Spec §83: every feature belongs clearly to exactly one of the **control plane**,
the **content storage fabric**, the **personal state plane**, or an **external
specialist**. A change that does not fit one of those cleanly is usually a
change that wants to be two changes.

Two boundaries are enforced by `golangci-lint` rather than by review — see
`.golangci.yml`:

- `internal/domain/**` may not import `os`, `path/filepath`, `database/sql`, or
  the persistence and CAS packages. The content domain does not know how or
  where its bytes are stored.
- `internal/storagefabric/**` may not import the domain or API layers, so the
  Storage Fabric stays extractable as its own module.

If you need to cross one of those lines, the answer is a new interface, not an
exclusion.

## Architecture decisions

Anything that changes an architectural stance needs an ADR in
[`docs/adr/`](docs/adr/). Copy the shape of an existing one; keep it to a page.
An ADR that merely describes the code is not worth having — record the decision,
the alternatives, and what would make us revisit it.

## Testing

- Table-driven tests; no mocking frameworks.
- Real filesystems (`t.TempDir()`) for storage tests — the reflink, hardlink and
  fsync behaviour *is* the thing under test.
- Golden files for API and CLI `--json` output, so response-shape changes are
  visible in review.
- An injected clock, never `time.Sleep`, for anything involving leases or backoff.
- `-race` always.

`scripts/acceptance.sh` is the end-to-end gate and the real signal that a
milestone is done. Coverage percentage is not a target.

## Commit messages

Conventional Commits (`feat:`, `fix:`, `docs:`, `chore:`, `refactor:`, `test:`).
Reference the issue in the body or use a closing keyword in the PR description.
