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
