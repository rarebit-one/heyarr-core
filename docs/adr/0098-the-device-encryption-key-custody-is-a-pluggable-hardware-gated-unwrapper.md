# 0098. The device encryption key's custody is a pluggable `Unwrapper`, hardware-gated per platform

**Status:** Proposed
**Date:** 2026-09-16
**Milestone:** M9 — Encrypted personal state (vault device-key custody)

## Context

ADR-0049 gave each device an X25519 **encryption** key and made the one operation
that touches it — turning a wrapped space key into a space key by an
ephemeral-static ECDH — the sole point of contact. It listed, under *what would
make us revisit*, "a hardware root for the device encryption key … Secure Enclave
/ TPM / passkey store." Two things make now the time to decide it:

1. **The seam already exists and is used in exactly two places.** Opening a space
   takes an `Unwrapper` interface, not a raw key:
   ```go
   // internal/personalstate/client/client.go
   type Unwrapper interface { Unwrap(wrapped []byte) (encryption.SpaceKey, error) }
   ```
   Its own doc says the design intent outright: *"the desktop CLI's exportable key
   (`KeyUnwrapper`) and a phone's enclave-backed one implement the same interface
   … a phone drops in an enclave-backed Unwrapper and nothing else changes."* The
   only callers are `mgr.Open` (device gateway) and the `vault` CLI.

2. **The fleet is moving to hardware with a reachable root.** The household
   laptops are migrating from Apple-Silicon/Intel Macs running Linux — whose
   Secure Enclave / T2 Linux cannot reach — to **Framework 13 (AMD fTPM) and
   Framework 12 (Intel PTT)**, which expose a **TPM 2.0** to Linux. The phone
   (cruciform) has StrongBox/TEE. A YubiKey is on hand.

voidbind already decided the analogous question for the **signing** key
(voidbind ADR-0001): keep Ed25519/X25519 as the wire primitives and achieve
device-binding through **hardware-*gated* storage, not enclave-native signing**,
because StrongBox/SEP/TPM speak P-256, not Curve25519. This record applies the
same model to heyarr's X25519 **encryption** key, through the `Unwrapper`.

## Decision

**The device encryption key is reached only through the `Unwrapper` interface, so
its custody is a pluggable backend chosen per platform with no change to any
caller. The backends, in ascending strength, are: an in-process software key; a
TPM-*gated* key; a YubiKey that does the ECDH on-card; and offload to cruciform on
the phone. The fingerprint reader is an authorization *gate* in front of a backend,
never a key holder. None of this changes the wrapped-key wire format (ADR-0049) or
recovery (ADR-0022): it changes only where the recipient private key lives and who
computes the ECDH.**

### The backends

1. **Software — `KeyUnwrapper` (default, shipped).** The key on disk (protected by
   LUKS / filesystem permissions), ECDH in-process. ADR-0032's first device. It is
   extractable by local root, and that is acceptable: the vault's guarantee is
   against the **peer/operator** (ADR-0021), not the device's own owner. This is
   what the `vault` CLI and the device gateway use today.

2. **TPM-gated (Framework fTPM/PTT, any TPM 2.0 Linux box).** The X25519 key is
   **sealed to the TPM** under a policy — PCR state plus a PIN — and released only
   after the gate; the ECDH then runs in RAM. This is voidbind ADR-0001's model
   exactly. 🔴 **The TPM does not compute the ECDH.** TPM 2.0 has no Curve25519 in
   its ECC set, so it *gates* the software key, it is not enclave-native for
   X25519. Do not describe it as if the key never exists in RAM — it does, briefly,
   after the gate. → #570.

3. **YubiKey on-card (the strongest *local* option).** The OpenPGP applet's
   cv25519 decipher runs the X25519 key-agreement **on the token**; the private
   key never leaves it. This is the only genuinely enclave-native X25519 custody
   available on these machines, and it needs no accessible platform enclave — it
   works on the Macs-on-Linux too. → #569 (testable now).

4. **cruciform-offload (the desktop holds no key at all).** On unwrap the desktop
   wakes cruciform over the voidbind **notify plane** (ADR-0005, self-hosted
   ntfy/UnifiedPush) and exchanges the wrapped space key over the **pairing relay**
   (ADR-0002, stateless/opaque); cruciform hardware-gates (its StrongBox/TEE key,
   biometric) and performs the ECDH, returning the space key. The phone is the root
   of trust; the desktop is a terminal. 🔴 **Consulted once per space-key unwrap**
   (per space, per session, per rotation) — **not per file**: the space key
   decrypts every frame locally, so offload is a rare round-trip, not a per-byte
   one, which is what makes it usable. → #571 (supersedes the closed #330).

### 🔴 The fingerprint reader gates, it does not hold

On Linux the Framework/Goodix reader is `fprintd`/libfprint, **match-on-host**,
feeding PAM/polkit. So a fingerprint authorizes the *release* of a gated key (the
TPM PIN step, or unlocking the keyring) — a "touch to open the vault for this
session" front-end. It is **not** a hardware-attested biometric-to-key binding
(that lives inside a phone's TEE, not here). Stated plainly so the ergonomics are
not mistaken for a cryptographic property they do not have.

### 🔴 Hardware custody adds no key-loss mode

The **recovery** encryption key (ADR-0049/ADR-0022), derived from the paper
secret, is a permanent wrap target independent of any device's hardware key. A
lost or destroyed TPM / YubiKey / phone is recovered from the paper secret and
re-wrapped for a fresh device — the `space recover [--rewrap]` path already shipped
(P0.1/P0.2). So binding a device key to hardware is never a way to lose access; it
only narrows who can read *that device's* copy.

### Per-platform custody (the plan)

| Device | Reachable root | Backend |
|---|---|---|
| Framework 13 / 12 (Linux) | fTPM / PTT | **TPM-gated** + fingerprint gate; YubiKey optional-stronger |
| Asahi / Intel-Mac Omarchy | none (enclave not reachable) | **software**; YubiKey optional-stronger |
| Nothing phone (cruciform) | StrongBox / TEE | hardware-gated in-app (voidbind ADR-0001) |
| Any desktop, maximum security | — | **cruciform-offload** (phone is the root) |

## Consequences

- **One interface, N backends.** Each backend is a single `Unwrap` implementation;
  the two callers are untouched. The vault client and gateway select a backend by
  configuration/flag, not by code change.
- **Signing and encryption keys stay separate but should share the gate.** voidbind
  holds the Ed25519 identity key; heyarr holds the X25519 encryption key. They are
  different keys (ADR-0049) but should sit behind the *same* platform gate — one
  TPM policy, one YubiKey, one biometric — so the user unlocks once. Coordinate the
  gate, not the keys.
- **No wire change, no coordinated release.** The wrapped-key format and the ECDH
  seal (ADR-0049) are unchanged; this is purely custody. A backend can land in
  heyarr alone.

## Alternatives rejected

- **Switch the device key to P-256 so the TPM/StrongBox do it natively.** voidbind
  ADR-0001 already kept X25519 for the wire; changing the curve is a coordinated
  break across voidbind, heyarr and All Thing for a marginal gain, when the YubiKey
  already does X25519 on-card and TPM-gating covers the rest.
- **Per-file offload to the phone.** The space key decrypts frames locally, so
  offload is per-unwrap; per-file would be unusable and needless.
- **Treating the fingerprint as a key store.** It is match-on-host; it authorizes,
  it does not hold or compute.

## What would make us revisit

- A Linux userspace that exposes a **25519-capable** secure element (a plug-in
  module beyond the TPM), which would upgrade the TPM-gated backend to
  enclave-native.
- P-256 becoming the wire primitive (a voidbind decision), which would let the TPM
  and StrongBox compute the agreement natively.

## Addendum (2026-09-17): the cruciform-offload unwrap protocol (#571)

Backend 4 needs a desktop↔phone protocol that the other three do not. This
addendum records its shape; the desktop-side backend and its authenticated
protocol codec land in `internal/personalstate/client/cruciform` (unit-tested
against a fake transport), with the live wiring deferred as noted below.

### Shape: the phone returns the space key, not a raw agreement

Two shapes were considered:

1. **Remote `AgreementFunc`** — the desktop runs `encryption.UnwrapWithAgreement`
   and offloads only the ECDH: it sends the ephemeral public point to the phone,
   which computes `ECDH(phone_priv, ephPub)` in its enclave and returns the raw
   32-byte shared secret; the desktop assembles the key. This mirrors the YubiKey
   backend most literally.
2. **Phone returns the space key** (chosen) — the phone unwraps against its own
   key and returns a fully-formed space key, sealed to a per-unwrap ephemeral key.

Shape 1 is **rejected**: it turns the phone into a general X25519 decryption
oracle — `agree(X)` returns `ECDH(phone_priv, X)` for *any* X, so anyone who
reached the phone past its gate could decrypt anything ever wrapped to it. A
PIN-gated card in a USB port is a safe oracle (backend 3); a phone reachable over
a network relay is not. Shape 1 gains no confidentiality either — the shared
secret on the wire is as sensitive as the space key, needing the same envelope
anyway. Shape 2 narrows the phone to "return a space key for a blob that unwraps
against my own key," a far smaller capability. So this backend implements
`client.Unwrapper` via a wake + relay round-trip, **not** via
`UnwrapWithAgreement` — the agreement *and* the assembly run on the phone.

### The exchange is mutually authenticated over an untrusted relay

The pairing relay (ADR-0002) is opaque but untrusted: it forwards bytes it cannot
open and could try to substitute them. Two signed, nonce-bound messages close
that:

- **Request** (desktop → phone): `{wrapped, ephPub, nonce}`, signed by the
  desktop **transport** key. It carries no secret — `wrapped` is already opaque,
  `ephPub`/`nonce` are public — so it needs authenticity, not sealing. The
  signature is the confused-deputy gate a wake-with-no-QR reopens: an attacker who
  reaches the relay cannot make the phone act as an unwrap oracle.
- **Response** (phone → desktop): the space key sealed to `ephPub` (the same
  `encryption.Seal` wrap the controller uses), plus the nonce, **signed by the
  phone's device key**. The signature is load-bearing: `Seal` is anonymous-sender,
  so without it a malicious relay could swap in `Seal(attackerKey, ephPub)` and
  the desktop would trust an attacker-chosen key. The desktop verifies the phone
  signature and the nonce *before* unsealing, so substitution is caught at the
  door, not by a later failed frame decrypt. The ephemeral private key is
  discarded per unwrap, so each exchange is forward-secret against a later relay
  compromise. The space key lands in desktop RAM — that is this ADR's §4 by
  design; what offload protects is the long-term *device* key, which never leaves
  the phone.

### Requester trust: one-time transport pairing, then biometric

The desktop authenticates as a paired **terminal** via a persistent *transport*
signing key — **not** a device encryption key (this backend holds none). The
phone pins that transport key once, via a voidbind pairflow SAS number-match
(ADR-0002); after that, each unwrap needs only the phone's biometric gate, not a
per-unwrap number-match. The transport key is not a custody key: stolen alone it
is useless — an attacker still needs the phone and its biometric to get any space
key.

### Built now vs. deferred

- **Built:** `internal/personalstate/client/cruciform` — the `Unwrapper` backend,
  the authenticated request/response codec, and the envelope, unit-tested against
  a fake transport + a reference "phone" that runs the real unwrap/seal. The
  `Transport` seam (wake + relay round-trip) is an interface here.
- **Deferred (follow-ups):** extending voidbind-go's relay to accept unwrap
  message types and its notify plane to carry an opaque `voidbind:unwrap?…` ping
  (both **voidbind-go-first**, source of truth); the concrete notify+relay
  `Transport`; the one-time pairing ceremony that pins the transport key; the
  phone half in **voidbind-kmp** (UnifiedPush receive, biometric gate, in-enclave
  unwrap, seal); backend **selection** at the two callers (`mgr.Open`, the vault
  CLI still hardcode `NewKeyUnwrapper` — a prerequisite shared with backends 2/3);
  and a live phone round-trip, gated on a reachable paired phone (as the YubiKey
  backend gated its on-card round-trip).

## Addendum (2026-09-17): backend selection is wired

The "select a backend by configuration, not by code change" this ADR called for
is now real for the two wired backends. Opening a space takes a `client.Custody`
— the `Unwrapper` plus the `RecipientID()` a space key is wrapped to — so create
(the `--self` wrap target) and open (the copy it looks up and the key it unwraps
with) stay consistent under any backend. `internal/personalstate/custody.Select`
maps `vault.unwrapper` (config; default `software`, or `yubikey`) to a
`client.Custody`; `tpm` and `cruciform` are refused at config validation until
they are wired. Both callers honour it: the vault CLI builds it per command
(`selectCustody`, reading the same config), and the device gateway takes it via
`SpaceLibrary.WithCustody`. The YubiKey PIN comes from a pin file or
`HEYARR_VAULT_YUBIKEY_PIN`, read lazily and never persisted. So the card-proven
YubiKey backend is now usable end to end by setting one config key; the TPM
(#570) and cruciform (#571) backends slot into the same selector when they wire.

## Addendum (2026-09-17): the TPM-gated backend is built and selectable (#570)

Backend 2 is now wired — `internal/personalstate/client/tpm`. The device's
X25519 seed is sealed to the TPM (pure-Go `go-tpm`, no cgo) under a policy that is
**PolicyPCR(selection) AND PolicyAuthValue(PIN)**; the sealed object is
policy-only (UserWithAuth clear), so unsealing requires both the PCR state and the
PIN. 🔴 The TPM **gates** the seed — after the unseal the seed is briefly in RAM
and the ECDH runs there (TPM 2.0 has no Curve25519), exactly as this ADR's §2
states; do not describe it as key-never-in-RAM. `RecipientID` reads the public
point recorded in the sealed-key blob, so selecting the wrapped copy never
triggers the gate; only `Unwrap` does. Selectable via `vault.unwrapper: tpm`
(+ `vault.tpm.{sealed_key_file,device,pin_file}` / `HEYARR_VAULT_TPM_PIN`).

Testing: the seal/unseal core is proven against the **TPM 2.0 reference
simulator** (go-tpm-tools, in-process), behind a `tpmsim` build tag and run in CI
by a single `CGO_ENABLED=1` step — the only cgo in the build; the whole matrix
stays `CGO_ENABLED=0` and the pure-Go legs never compile it. (go-tpm's transport
targets that reference simulator; swtpm's socket control channel speaks a
different protocol and is not a drop-in.) The hardware-free unit tests (blob
codec, RecipientID, input validation) run everywhere.

Provisioning: **`heyarr device seal-tpm`** seals the EXISTING device encryption
key to the local TPM (so the public point — the wrap target the controller holds
— is unchanged and already-wrapped spaces keep opening) and writes the sealed-key
blob (`Blob.WriteFile`, 0600). It is non-destructive: it prints how to select the
backend and then remove the plaintext seed to complete the hardening, but never
removes it — recovery via the paper secret is unchanged, so this adds no loss
mode. **Deferred:** the on-real-fTPM/PTT validation, gated on the Framework
laptops arriving. Only cruciform (#571) remains unwired in the selector now.

## Addendum (2026-09-17): the cruciform-offload live path — rendezvous and approval UX

The "unwrap protocol" addendum above settled the *message-level* exchange (who
signs what, and how the space key comes back sealed). This settles the layer
*above* it — how the desktop and phone find each other, and what the approval
feels like — so the deferred pieces (the notify unwrap-ping, the `Transport`, the
pairing ceremony, and the phone half) are built to one shape. The offload backend
landed (#582); this is its live path.

### Two moments, opposite tools

- **Pairing is once, and uses a QR.** Establishing desktop↔phone trust is a
  one-time act, and voidbind pairing already does it well (ADR-0002 pairflow): the
  desktop renders a `voidbind:pair?…` QR, the phone scans it off the screen, and a
  short number-compare (SAS) confirms it. The visual channel is what authenticates
  the rendezvous, so a network attacker cannot substitute the relay/session/salt.
  This pins the desktop's **transport key** on the phone — a pairing identity, NOT
  a custody key (stolen alone it opens nothing without the phone + biometric).
- **Unwrapping is recurring, and never scans.** A QR per unwrap is unacceptable
  UX. The recurring path is push/discover + biometric, below.

### Recurring unwrap: LAN-direct first, relay+wake as the away fallback

Two transports behind the STEP-4 `Transport` seam — the same signed message-level
protocol rides either:

- **Same network (the common homelab case): direct, via mDNS.** heyarr already
  advertises on the LAN (mDNS); the desktop discovers the paired phone and connects
  to it directly — no relay, no push, lowest latency. This is the everyday path
  when the owner is home.
- **Away (phone on cellular, laptop off-net): relay + wake.** The desktop wakes
  the phone over the notify plane (ADR-0005) with an opaque **unwrap ping** that
  carries only the relay session pointer, and the two exchange over the pairing
  relay (ADR-0002). The ping wakes a sleeping phone; the relay carries the opaque,
  mutually-signed request/response.

### Binding the approval without a QR: number-matching

A scanned QR proves "this is my desktop in front of me"; a push scans nothing, so
it loses that binding. voidbind's number-matching (ADR-0006) restores it on the
relay path: the desktop shows a short code, the phone shows candidates, the human
taps the match, then approves with biometric — two taps, no scan, and it proves it
is the owner's desktop. On the LAN-direct path the pinned transport key plus the
biometric already bind it; whether to also require a number-match there is an open
knob (below).

### Cadence: once per space-key unwrap, not per file

The phone is consulted once per space / per session / per rotation — after that the
space key decrypts every frame locally. So the human approves when they sit down to
work, not on every action. This low frequency is what makes hardware-offload
usable, and it is why a per-unwrap biometric (rather than a cached credential on the
desktop) is affordable.

### What each deferred piece is therefore built to

- **notify unwrap-ping (voidbind-go):** a new opaque tuple
  `voidbind:unwrap?relay=&session=` + an enqueue path, mirroring the login ping —
  carries only the unguessable relay-session pointer, never a key or challenge.
- **relay message-types (voidbind-go, PR #39):** the configurable slot names that
  let the unwrap request/response ride the same dumb relay as pairing.
- **the heyarr `Transport`:** mDNS LAN-direct with relay+wake as fallback, behind
  the seam the offload backend already takes.
- **the pairing ceremony:** the one-time QR pairflow that pins the desktop
  transport key on the phone.
- **the voidbind-kmp/cruciform phone half:** scan-to-pair once; then receive a
  wake (or accept a LAN connection), number-match, biometric, in-enclave unwrap,
  and reply.

### Open knobs (flagged, not locked)

- Whether the LAN-direct path also requires a number-match, or biometric-only is
  enough given the pinned transport key.
- Whether the desktop may cache the space key for a bounded window after an
  approval, or must re-consult the phone every session — a security/UX trade-off
  that weakens the "desktop holds nothing" property if cached, so the default is
  no cache.

## Relationship to existing records

- **ADR-0049** — the device X25519 key and the wrap/seal format this leaves
  unchanged; this record is the "hardware root" it parked.
- **ADR-0032** — the CLI-first device and its exportable software key (backend 1).
- **ADR-0022 / P0.1–P0.2** — recovery, which makes hardware binding safe (no new
  loss mode).
- **voidbind ADR-0001** — the signing-key custody model this mirrors for the
  encryption key; **voidbind ADR-0005 / ADR-0002** — the notify wake and pairing
  relay the offload backend rides.
- **Backends:** #569 (YubiKey), #570 (TPM-gated), #571 (cruciform-offload).
