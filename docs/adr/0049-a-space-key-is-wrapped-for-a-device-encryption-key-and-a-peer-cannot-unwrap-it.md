# 0049. A space key is wrapped for a device encryption key, and a peer cannot unwrap it

**Status:** Proposed
**Date:** 2026-08-26

## Context

Milestone 9 builds the third plane — encrypted personal state — and it is the
one Heyarr must never be able to read (Invariant 6, §38, `SECURITY.md`). Nothing
in this record ships behaviour. It answers the questions everything else in M9
wraps against, in writing, because they are cheap now and expensive to reverse:
a wrapping scheme is a wire format the moment one space key is sealed to one
device, exactly as ADR-0048 was a wire format the moment one grant was signed and
ADR-0044 the moment one backup was written. M8's decision issue (#302 → ADR-0048)
and M7's (#281 → ADR-0044) both worked this way, and this is the M9 counterpart.

Four facts frame the whole decision, and the first is a gap M8 left open on
purpose.

**M8's device identity is a *signing* key, and §41 needs an *encryption* key.**
`enrolment.UserIdentity` and `enrolment.Cert` bind an Ed25519 key — the device
key that *authenticates* ("this device is user U's") and *authorises nothing on
its own" (ADR-0048). §41 wraps a space key **for a device**, which is
key-agreement, not signing: it needs an X25519 (ECDH) key, a different primitive
that exists nowhere yet. `device_identities` (migration 00034) stores one
`device_key` and it is the signing key. So the very first thing M9 owes is: where
does a device's *encryption* key come from, and how is it enrolled and pinned
before anything is wrapped for it? Everything below wraps against the answer.

**The encryption root is already teed up, under a reserved label.**
`internal/recovery` derives the user identity's Ed25519 signing seed from the
recovery secret via HKDF-SHA256 under the versioned label
`heyarr/recovery/v1/user-identity-ed25519-seed`, and its doc comment reserved
"the SAME recovery secret … under a DIFFERENT label" for exactly this milestone's
encryption root (RFC 5869's `info` is the domain-separation tag; two labels are
two independent keys). ADR-0022 makes this load-bearing: key loss is total data
loss, so recovery must reconstruct the ability to unwrap the wrapped keys the
peers already hold, offline, with Heyarr not running. M9 must state the label and
confirm it does not collide with the signing-seed label.

**ADR-0032 already fixed the ordering, and it applies to the new key.** Device
keys land before they authorise anything; "a key issued and immediately trusted
is a key nobody had to enrol." The device's encryption public key is subject to
the same rule: it must be enrolled and pinned before any space key is wrapped for
it, so "a key issued and immediately wrapped-for" is unspellable the way ADR-0048
made "a key issued and immediately honoured" unspellable.

**ADR-0048's verify path is the authorisation layer this reuses, not rebuilds.**
A grant binds a principal, a resource, a capability set and an expiry, verified
against any pinned issuer key (`grant.TrustStore`). Reading encrypted state is two
independent gates that must not be confused: *may I fetch these ciphertext
changes* (an ADR-0048 grant over the space as a resource — authorisation) and
*can I turn them into plaintext* (do I hold a device key the space key was wrapped
for — confidentiality). This ADR decides the second. The first is ADR-0048
already working, and the two are deliberately orthogonal: a peer serves ciphertext
to any principal a grant authorises, and that principal reads nothing unless a
space key was also wrapped for one of its devices.

The acceptance sentence this milestone owes is two halves, and each is a trap:

> A space's contents sync across peers and merge correctly after an offline
> concurrent edit — and at no point can a peer, an operator, or the
> controller-side MCP read one byte of the plaintext, only the ciphertext and the
> causal metadata that routes it.

Encrypted state that a peer can merge server-side has broken the invariant. A
CRDT that cannot converge after a partition has not delivered §43. This record
decides the crypto that makes the second half true by construction; the sync
protocol (a later deliverable, its own decomposition) makes the first true.

## Decision

**A space has one symmetric key. That key is sealed — "wrapped" — separately for
each authorised device's X25519 encryption public key and for the user's
recovery encryption key, using an ephemeral-static ECDH seal. Peers store every
wrapped copy and hold no X25519 private key, so they cannot unwrap any of them.
The device encryption key is a *separate* X25519 keypair, generated on the device
and bound into the enrolment cert beside the Ed25519 signing key. The recovery
encryption key derives from the recovery secret under a new HKDF label. Content
and CRDT changes are encrypted under the space key with an AEAD; a peer stores
them as opaque, content-addressed ciphertext with causal metadata and never
decrypts or merges them.**

Five parts, one invariant.

### The key hierarchy, and what each key can and cannot do

| Key | Primitive | Held by | Wraps / does |
|---|---|---|---|
| **Space key** `K_space` | 256-bit AEAD key | nobody at rest — it exists only unwrapped, in an authorised client's memory | encrypts the space's CRDT changes and snapshots; one per EncryptedSpace (§39); rotated on revocation |
| **Device encryption key** | X25519 | one device, client-side, private half never leaves it | a wrap *target* — `K_space` is sealed to its public half so that device can unwrap |
| **Recovery encryption key** | X25519, HKDF-derived from the recovery secret | the user (the paper restores it) | a permanent wrap target, so a user with no surviving device recovers `K_space` offline (ADR-0022) |
| **User identity key** (M8) | Ed25519 | the user | signs the enrolment cert that binds the device encryption key; unchanged by this record |
| **Device signing key** (M8) | Ed25519 | one device | authenticates the device; authorises nothing; unchanged |

The load-bearing separations are two: **authentication (Ed25519) from
key-agreement (X25519)** — different keys, below — and **authorisation (an
ADR-0048 grant) from confidentiality (a wrapped key)** — a grant lets you fetch
ciphertext, a wrapped key lets you read it, and neither implies the other.

### 🔴 A device's encryption key is a *separate* X25519 keypair, not derived from its Ed25519 key

The choice M8 left at M9's feet. A device needs an X25519 key to be a wrap
target, and it already has an Ed25519 signing key. The two options are a separate
X25519 keypair, or an X25519 key *derived* from the Ed25519 key by the birational
Edwards→Montgomery map (the `crypto_sign_ed25519_pk_to_curve25519` construction
Signal and libsodium expose). **We choose a separate keypair, and bind it into
the enrolment cert.**

The case for deriving is fewer keys: one keypair per device, and pairing/recovery
produce and transmit one thing. It is rejected because **crossing a signing key
and a key-agreement key on one keypair is a documented footgun**, and the saving
is small:

- The security proofs for Ed25519 (EUF-CMA signatures) and X25519 (the ECDH
  problem) each assume the key is used in *that* scheme only. A key used in both
  invites cross-protocol interactions the proofs do not cover; the map has edge
  cases (sign-bit handling, low-order points) that are easy to get subtly wrong
  and were the source of real CVEs in libraries that shipped it. Invariant 6 is
  the one guarantee we least want resting on a clever key-reuse trick.
- The saving the derivation buys is *one X25519 keypair per device* — 32 bytes of
  public key in the cert and one column in `device_identities`. It does not save
  anything in recovery, because a device's encryption key is generated *on the
  device* at enrolment, not derived from the recovery secret (only the user-level
  recovery encryption key is; see below). So "pairing and recovery must produce
  both" reduces to "pairing carries two device public keys instead of one," which
  is the same signed cert and the same short-authentication-string transcript over
  a few more bytes.

So a device generates **two** keypairs at enrolment: its Ed25519 signing key (M8)
and its X25519 encryption key (this record). Both public halves go into **one
enrolment cert**, signed once by the user identity: *"device D — signing key S,
encryption key E — is mine."* The cert format gains an encryption-key field and
its `Version` bumps to 2; a v1 cert (M8, signing key only) authenticates a device
that has nothing wrapped for it, which is correct — it can act on the control
plane and read nothing of personal state until it re-enrols with an encryption
key. The device encryption key renders `x25519:<hex>`, the algorithm-prefixed
form `identity.FormatPublicKey` established, so a log and a `--json` document name
it the way they name every other key.

### 🔴 The wrap is an ephemeral-static ECDH seal, and a peer cannot unwrap it

Wrapping `K_space` for a device public key `E` is a sealed-box construction over
X25519:

1. draw an ephemeral X25519 keypair `(e_priv, e_pub)`;
2. `shared = ECDH(e_priv, E)`;
3. `wrap_key = HKDF-SHA256(shared, salt = e_pub ‖ E, info = "heyarr/space-key-wrap/v1")`;
4. `wrapped = AEAD_seal(wrap_key, K_space)`; the stored wrapped key is
   `e_pub ‖ nonce ‖ wrapped`.

The recipient recovers `shared = ECDH(E_priv, e_pub)` and unwraps. This is a
standard seal (NaCl `box_seal` in shape) built on stdlib `crypto/ecdh` and an
AEAD, so it adds no dependency and no bespoke arithmetic. It is *not* ADR-0040's
HMAC capability and *not* ADR-0048's Ed25519 grant — those are symmetric-node-local
and asymmetric-signed-authority respectively; this is asymmetric key-agreement, a
third construction for a third job, and the three do not share a key.

**A peer cannot unwrap because it holds no X25519 private key.** Peers store the
set of wrapped copies (§79's `wrapped_keys`) and the ephemeral publics, and every
private half lives only on a device or is derivable only from the recovery secret
— neither of which is ever in the server's `data_dir` (ADR-0032). This is the
whole invariant: a space replicated to three peers is readable at none of them,
which is exactly what makes it safe to add a peer somewhere you would not
otherwise trust (ADR-0021).

**What the peer *does* see, stated plainly**: the space id, the set of device
public keys a space is wrapped for (so it learns *which devices can read a space*
— co-membership at the device level, not content), opaque change ids, causal
metadata, and ciphertext. §38's list of what it does not see — playlist names,
playlist *members* (those are content), ratings, annotations, history — holds.
Device-level wrap membership is structural to storing per-device wrapped keys and
is acknowledged, not hidden: it is "who may read," never "what they read."

### 🔴 The recovery encryption key derives under a new label that does not collide

The recovery secret already derives the user identity signing seed under
`heyarr/recovery/v1/user-identity-ed25519-seed`. The M9 encryption root derives
the user's **recovery encryption key** (an X25519 seed) under a distinct label:

> **`heyarr/recovery/v1/user-encryption-x25519-seed`**

Because HKDF's `info` is the domain-separation tag (RFC 5869), a different label
over the same secret yields an independent key: the signing seed and the
encryption seed cannot coincide, and a white-box test asserts two labels give two
different 32-byte outputs, the way `internal/recovery` already tests the signing
label. The recovery encryption key is modelled as a **permanent authorised wrap
target** — *the paper is a device that never breaks*: its public half is enrolled
at account creation alongside the user identity, every space key is wrapped for it
in addition to each device, and recovery is "derive the secret's encryption seed,
unwrap the copies the peers already hold, re-wrap for the new devices." That is
ADR-0022's chain exactly — secret → root key → unwrap `wrapped_keys` → space keys
— offline at every step, because derivation and unwrapping are pure functions of
the secret and the ciphertext, touching no process, network or disk.

### 🔴 Revocation re-wraps forward, and it is a real mechanism with a caller

Losing a device is **forward-looking only** (ADR-0022), and this record makes the
re-wrap concrete rather than a promise:

Revoking device D over the spaces it could read does three things, in order:
1. **rotate** — mint a fresh `K_space'` for each affected space;
2. **re-wrap** — seal `K_space'` for every *remaining* authorised device and the
   recovery key, and for **not** D;
3. **re-encrypt forward** — every CRDT change and snapshot from now on is under
   `K_space'`; a compaction snapshot under the new key lets remaining devices
   drop the old-key history they no longer need.

**It does not retroactively protect what D already read** — past changes D
downloaded under the old key stay readable to D, and any change still encrypted
only under the old key stays readable to D until a new snapshot supersedes it.
Say so plainly; the guarantee is forward secrecy of *future* content, not erasure
of the past. The caller is explicit: `heyarr device revoke <device>` (and the
device-side personal-state client) drives rotation over the revoked device's
spaces, and the demo exercises it — a mechanism no caller invokes cannot be
proven (#187), and re-wrap-with-no-caller is precisely the gap ADR-0022 warned
would be found late.

### 🔴 No server-side merge, ever — the peer moves opaque changes and reads none

A CRDT change is stored and transported as an opaque record —
`{space_id, change_id, causal_metadata, ciphertext}` — where `causal_metadata` is
what orders and de-duplicates changes without reading them (parent change ids /
causal heads / a version vector, §44). The peer has **no** code path that
decrypts a change or interprets its contents; the semantic merge happens
**client-side**, after a device unwraps `K_space` and decrypts, per §42. This is
enforced structurally, the way `internal/domain` cannot import persistence
(Invariant 2, `depguard`): the peer-side replication and protocol packages move
opaque changes and **cannot import the plaintext CRDT model**, so the easy failure
— a "merge helper" on the peer that peeks at a decrypted field — does not compile.
The state-sync protocol is a *separate* protocol from CAS sync (§44), rides the
peer surface beside the existing `/peer/v1/...` routes, and shares transport and
authentication with it but nothing of its blob-optimised shape.

### The primitives, pinned (no new dependency)

Every choice is stdlib or an existing dependency (`golang.org/x/crypto`,
`github.com/zeebo/blake3`), so this adds nothing to `go.mod`:

- **Key agreement / wrap**: X25519 via stdlib `crypto/ecdh`.
- **KDF**: HKDF-SHA256 via stdlib `crypto/hkdf` (already the recovery derivation).
- **AEAD** (content and wrap sealing): **XChaCha20-Poly1305** via
  `x/crypto/chacha20poly1305`. Its 192-bit nonce makes a random per-message nonce
  safe without a counter, which matters for a multi-master plane where two peers
  mint changes for one space during a partition and no shared nonce counter
  exists (§43) — AES-GCM's 96-bit nonce would force nonce coordination the
  leaderless model cannot provide.
- **Ciphertext identity**: a vault/large-object blob is content-addressed by the
  **BLAKE3 digest of its ciphertext**, never its plaintext (Invariant 1,
  ADR-0021 — hashing plaintext would hand the server a confirmation oracle).

## Consequences — the assertions this milestone now owes

This ADR produces no code; it produces the list of assertions the rest of M9 must
make provable in `make demo` (#187: an assertion whose subject is absent is not
coverage), and each decomposition issue carries "what would make it provable."

- **A peer's stored bytes are ciphertext** → the demo asserts that a peer's
  stored personal-state change is *not* the plaintext, and that a decrypt without
  the space key fails. This is the sabotage target: store plaintext (or a nil
  cipher) and a test must go red.
- **A peer cannot unwrap** → a peer holds the wrapped keys and no X25519 private
  key; an attempt to unwrap on the peer has nothing to unwrap with, asserted, not
  asserted-by-absence.
- **Converge after a partition, client-side** → two devices make concurrent
  offline edits; on reconnect the changes replicate as opaque ciphertext and the
  clients converge to one CRDT state — with an assertion that the *server* never
  held plaintext during it.
- **Enrol-before-wrap** → a space key wrapped for a device whose encryption key
  is not pinned is unspellable; the wrap target is resolved from a pinned
  `device_identities` row, and the ordering is ADR-0032's, tested.
- **Recovery is offline and derives a distinct key** → the encryption seed
  derives from the recovery secret under the new label, differs from the signing
  seed, and unwraps a space key the peers hold — with no Heyarr process running.
- **Revocation is forward-only, with a caller** → revoke rotates and re-wraps for
  the remaining set and not the revoked device; a test asserts the revoked device
  cannot read post-rotation content, and states that it can still read what it
  already held.
- **Every transition emits an event** (Invariant 7) → a space created, a key
  wrapped, a device revoked and re-wrapped, a change accepted for replication —
  the *metadata* transitions, never the plaintext.

Two things stated as numbers/labels the milestone owes: the **encryption-root
label** `heyarr/recovery/v1/user-encryption-x25519-seed`, and the **wrap KDF
info** `heyarr/space-key-wrap/v1` — both versioned, because each is a wire format
the instant one key is wrapped, and a change to either is a new label, never a
reinterpretation of the old one.

## Relationship to existing records

- **ADR-0021** (encrypted vault content) stays `Proposed`. This record is the
  encryption foundation both the CRDT personal state and (later) vaults build on;
  a vault is "the personal-state plane growing large objects," so it wraps space
  keys exactly this way. ADR-0021 moves to `Accepted` when vault *behaviour*
  ships, which is downstream of core M9, not part of this record.
- **ADR-0022** (enrolment and recovery) is the recovery half this depends on and
  extends: its recovery secret now derives the encryption root as well as the
  signing seed, and its "device revocation is forward-looking only" is the re-wrap
  mechanism made concrete here.
- **ADR-0032** (device keys land before they authorise) is the ordering this
  record honours for the *encryption* key; its "keys land before they authorise"
  becomes "keys are pinned before they are wrapped-for."
- **ADR-0048** (cross-site grant) is the authorisation layer, reused unchanged.
  Fetching ciphertext is a grant over the space as a resource; this record adds
  the orthogonal confidentiality layer and does not touch grant verification.
- **ADR-0040** (HMAC render capability) and ADR-0048's Ed25519 grant are the two
  existing trust roots; the X25519 wrap is a third construction for a third job,
  sharing no key with either.

## What would make us revisit this

- **Cross-user shared spaces (§47).** This record decides a user wrapping a space
  for its *own* devices and its recovery key. §47's "wrapped for User A and User
  B" adds one user wrapping a space key for *another user's* devices, plus the
  authorisation to do so — which ADR-0048 already flagged as M9's, its verify path
  already accepting any pinned issuer key. The confidentiality side fits here
  unchanged (seal `K_space` to B's pinned device encryption keys), but the
  *policy* of who may share whose space, and whether a shared capability may
  itself be re-delegated, is a separate decision. **It is its own ADR when the
  shared-spaces deliverable is built** — landed one at a time, after this one, as
  `docs/adr/README.md`'s conflict history advises.
- **A hardware root for the device encryption key.** If a device key can live in a
  Secure Enclave / TPM / passkey store, the X25519 key becomes non-extractable and
  enrolment changes shape — an improvement this record's software-key model does
  not preclude.
- **Post-quantum key agreement.** X25519 is classically secure; a store-now-
  decrypt-later adversary against long-lived personal state is the case that would
  push toward a hybrid X25519+ML-KEM wrap. That is a new label
  (`…/space-key-wrap/v2`) and a wrapped-key format bump, not a redesign — which is
  the reason the wrap KDF info is versioned from the first byte.
- **A per-device wrap becoming a scaling problem.** Wrapping every space key for
  every device is O(devices × spaces). For a household this is tiny; a deployment
  with hundreds of devices per user might want an intermediate per-user key the
  devices share. That trades the clean "peer sees exactly who can read" property
  for fewer wraps, and would be argued then, not now.
