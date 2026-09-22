# 0097. A vault blob is fixed-frame ciphertext with an encrypted manifest, and each frame authenticates its own place

**Status:** Proposed
**Date:** 2026-09-15
**Milestone:** M9 — Encrypted personal state (the vault content plane, W1)

## Context

ADR-0021 decided *that* a vault's bytes are ciphertext Blobs the infrastructure
cannot read, encrypted in "independently decryptable frames" so a client can fetch
and decrypt a byte range without the whole object, with the client encrypting
before upload and the storage fabric unchanged. It deliberately left the wire
format open: it named no frame size, no manifest shape, and no framing/AAD scheme.
Those become a wire format the instant one vault blob is written (the same
reasoning ADR-0049 applied to its key labels), so they are pinned here before any
byte is sealed.

What already exists (M9 crypto lives in `voidbind-go`, re-exported):
- `encryption.EncryptChange(sk, plaintext) []byte` / `DecryptChange` —
  XChaCha20-Poly1305 under the space key, output `nonce ‖ ciphertext+tag`, a fresh
  random 24-byte nonce per call. It is single-shot and **takes no additional
  authenticated data** (`aead.Seal(nonce, nonce, pt, nil)`), and the `SpaceKey`'s
  bytes are unexported — so a caller outside `voidbind-go` cannot build its own
  AEAD over the space key.
- The CAS ingests opaque bytes (`cas.FS.PutExpecting`, `cas.Stage`) and serves
  byte ranges (`http.ServeContent`); a blob id is `blake3:<hex>` of its bytes.

What is absent: a frame codec, an encrypted manifest, and the client-upload ingest
path. The manifest optimisation of ADR-0034 (`manifests.Manifest`) is the wrong
tool — it is FastCDC, content-defined, keyed on a *plaintext* whole-object digest,
and ADR-0021 rules CDC out for vaults ("CDC boundaries do not survive
encryption"). This record decides the codec and manifest; the ingest route and the
client range logic are their own deliverables (W1b, W1c).

## Decision

**A vault object is stored as two ciphertext Blobs: a *content blob*, the
concatenation of independently-sealed fixed-size frames, and a *manifest blob*, the
object's geometry sealed under the space key. Each frame's plaintext is prefixed
with an authenticated header binding the object and the frame's position, so a
reordered, spliced, dropped or truncated frame is rejected on open without any AAD.
Both blobs are content-addressed by the BLAKE3 of their ciphertext (never
plaintext, ADR-0021). The first implementation of the codec lives in heyarr,
composing the existing per-frame `EncryptChange`; it is lifted into `voidbind-go`
once the wire format has settled.**

### 🔴 Fixed 1 MiB frames, not content-defined chunks

The plaintext is split into frames of a fixed **1 MiB** data payload (the last
frame short). Fixed, because CDC boundaries do not survive encryption (ADR-0021),
and a constant frame size makes the ciphertext offset of frame *i* a pure function
of *i* — no per-frame offset table is needed in the manifest, and a logical byte
range maps to a frame range by division. 1 MiB balances range-read over-fetch (at
most ~1 MiB fetched to read one byte) against manifest size and per-frame overhead
(61 bytes/frame ≈ 0.006%). The size is recorded in the manifest and versioned, so
a future change is a new version, never a silent reinterpretation.

A full frame's ciphertext length is therefore constant:
`L = 24 (nonce) + 21 (header) + 1 MiB (data) + 16 (tag)`. Frame *i* occupies
`[i·L, (i+1)·L)` for every frame but the last, whose length follows from the
plaintext total. The manifest records the total, so the client computes every
frame's byte range itself.

### 🔴 Each frame authenticates its own place, in the plaintext, not via AAD

`EncryptChange` takes no AAD and the space-key bytes are not reachable outside
`voidbind-go`, so position is bound **inside** the sealed plaintext instead. Each
frame seals `header ‖ data`, where the fixed 21-byte header is:

```
version (1) ‖ file_id (16) ‖ frame_index (uint32 BE, 4)
```

`file_id` is a fresh random 16 bytes minted per object. On open, the client
recovers the header and checks it against what it expected — the frame it asked
for, of the object it is reading. Because the header is inside the authenticated
plaintext, this is exactly as strong as an AAD binding for the attacks that matter
during a *range read*, when the whole-blob id is not verified: a frame reordered
within the object fails the index check, and a frame spliced from another object
fails the `file_id` check. The **count** is not in the header — it is unknown
during single-pass streaming and is redundant, because the manifest is itself
authenticated (sealed under the space key) and records `frame_count` and
`plaintext_size`; a dropped or truncated frame is caught by reconciling the
fetched bytes against those, and by the content blob id the manifest names. The
header costs 21 plaintext bytes per frame and needs no new primitive.

A full frame's ciphertext length is therefore
`L = 24 (nonce) + 21 (header) + frame_size (data) + 16 (tag)`.

### 🔴 The manifest is its own encrypted blob, and the drive entry points at it

The manifest is JSON — `{version, file_id, frame_size, frame_count,
plaintext_size, content}` where `content` is the content blob's `blake3:<hex>` id —
sealed with `EncryptChange` under the space key and stored as its own ciphertext
Blob. A drive entry (ADR-0095) references the **manifest** blob id: reading a file
is "fetch and decrypt the manifest, then range-read the content blob it names."
Keeping the manifest a separate blob (rather than inlining it in the drive change)
keeps the drive CRDT change small and lets the manifest be replicated and
range-fetched like any other blob. Its own bytes leak nothing — they are
ciphertext, and a peer holds no key.

### 🔴 The codec starts in heyarr and is lifted to voidbind-go later

Composing `EncryptChange` per frame keeps W1's first slice **single-repo and
dependency-free**: no AAD-accepting primitive, no access to the space-key bytes, no
`voidbind-go` release. The codec is a small package in
`internal/personalstate/…`. Once the frame/header/manifest wire format has been
exercised, it is lifted beside the other M9 crypto in `voidbind-go` — the
architecturally consistent home ("the personal-state plane growing large objects",
ADR-0021) — behind the *same* wire format this record pins, so the lift is a move,
not a reinterpretation. If the lift adopts a real AEAD AAD in place of the
in-plaintext header, that is a manifest **version** bump, not a change to a shipped
one.

## Consequences

- **The storage fabric is unchanged** (ADR-0021 holds): `PutExpecting`/`Stage`
  ingest the ciphertext, `http.ServeContent` serves its byte ranges. The frame↔range
  translation is entirely client-side (W1c) and touches no server code.
- **Two blobs per object**, both GC-reference-counted (ADR-0018) through the drive
  CRDT: the manifest entry keeps the manifest blob, the manifest keeps the content
  blob. Dropping a file's last drive reference makes both reclaimable.
- **Range reads over-fetch at most one frame** at each end and trim after decrypt —
  the cost ADR-0021 accepted for losing `http.ServeContent`'s free ranging over
  plaintext.
- **A malformed blob id must not enter the drive CRDT.** W1/W2 validate that a
  drive entry's `Blob` is a well-formed `blake3:` id (it is an unchecked string in
  the skeleton today).
- **No cross-user dedup**, by construction: two users sealing the same file produce
  different ciphertext (different keys, per-object `file_id`, random nonces). That
  is the price ADR-0021 chose — dedup here would be the leak.

## Alternatives rejected

- **Reuse `manifests.Manifest` (ADR-0034).** FastCDC, plaintext-digest-keyed, an
  internal transfer optimisation — wrong on every axis for a vault (above).
- **One-stream encryption.** Defeats range reads; ADR-0021 already rejected it.
- **AAD-based frame binding now.** Needs an AEAD over the space key, whose bytes
  `voidbind-go` does not export — so it forces the codec into `voidbind-go` and a
  release before W1 can move. The in-plaintext header is equivalent for the threat
  model and defers that cross-repo step; the AAD form is a clean later version.
- **Inlining the manifest in the drive change.** Bloats the CRDT change and couples
  the namespace to the content geometry; a separate blob replicates and fetches
  uniformly.

## What would make us revisit

- **A different frame size** for a workload dominated by tiny files or by huge
  ones — a manifest version bump, not a redesign.
- **Lifting the codec into `voidbind-go`** with an AAD-based header — a version
  bump and a shim update, planned, not a break.
- **Streaming very large objects** whose plaintext will not fit in memory — the
  codec is defined over `io.Reader`/`io.Writer` for exactly this; only the naive
  buffered helpers would change.

## Relationship to existing records

- **ADR-0021** owns the vault's existence and the frames/ciphertext-hash/
  client-encrypt/fabric-unchanged decisions this makes concrete.
- **ADR-0049** — frames are sealed under the space key its wrap protects; the same
  "a format is a wire format the instant one is written" discipline governs this.
- **ADR-0095** — the drive entry references the manifest blob id; a file's two
  blobs are retained/GC'd through the drive CRDT.
- **ADR-0096** — both blobs are placed across sites by the blind placement pins.
- **ADR-0005 / ADR-0013 / ADR-0018** — blob identity is BLAKE3 of the (ciphertext)
  bytes, served by the standard range contract, reclaimed by GC.
- **ADR-0034** — the chunk manifest this deliberately does not reuse.
