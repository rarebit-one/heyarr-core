# 0014. Ingest materialisation: reflink, then hardlink, then copy

**Status:** Accepted
**Date:** 2026-08-19

## Context

Spec §60 retains "hardlink/reflink-friendly workflows" from the *arr ecosystem,
while §66 brings ingested bytes into the CAS. Those pull in opposite directions
if ingest means "copy".

## Decision

Ingest materialises a source file into the CAS by trying, in order:
copy-on-write reflink (`FICLONE`), then hardlink, then a byte copy. The mode
actually used is recorded. Configurable per library root; default reflink.

## Consequences

On a filesystem with block cloning — ZFS 2.2, XFS, btrfs — bringing an existing
60 GB remux under management costs metadata only. That is the difference between
Heyarr being adoptable against a real library and requiring you to double your
storage first.

A hardlink means the CAS and the original path share bytes, so an external tool
that writes in place would corrupt a blob. Integrity scanning (§57) is what
catches that, and it is why corrupt blobs are quarantined rather than deleted.

Cross-filesystem ingest degrades to a copy with a warning, never an error.

## The boundary is the mount, not the filesystem (#222)

`link(2)` returns `EXDEV` when its two paths are on different **mounts**,
whatever device they share. One filesystem bind-mounted twice therefore degrades
past the hardlink rung exactly as a genuinely separate disk would — and that is
precisely what `ProtectSystem=strict` does to a `ReadOnlyPaths` library and a
`ReadWritePaths` store.

#222 measured it: **63 of 63 files copied, ~22 GB consumed**, on a host where
`stat -c %d` reported one device for both paths. The warning this ADR promised
was implemented against `st_dev`, so it was structurally incapable of firing on
the one deployment where it mattered.

**So the check attempts the operation rather than predicting it.** It links a
real file from the library into the store's `tmp/`, reads the errno, and removes
it. A mount-id comparison would have been right about mounts and still a
prediction; the probe is right about whatever the kernel actually does, which is
how it also catches the `EPERM` from `fs.protected_hardlinks` that no mount
inference models. It falls back to inferring from the mount table only when
there is no file to link yet — the one case where nothing is at stake — and each
warning names which instrument answered, because a measurement and a prediction
are not the same claim.

The rule for anything added here later: **do not ask whether a cheap rung
*should* work.** Ask the kernel whether it just did.
