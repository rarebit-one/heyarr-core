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
