# Mobile client requirements for Milestone 9

> Shape M9's sync and wrapping against a real phone *now*, so they are not
> retrofitted when one appears. This note does **not** build the app — the app is
> a separate repo, and its *contract* is heyarr-core's device-auth, Personal MCP
> and M9 sync protocols. What this records is the set of constraints a phone forces
> onto M9, and how each is honoured, so the M9 issues can be checked against them.
> See issue #330.

The architecture has pointed at a phone from the start: §40 draws Phone/Laptop/
Tablet under one identity, §46 is local-first, §73 puts the Personal MCP
device-side, and ADR-0032 says outright that the desktop CLI is a stand-in
"because there is no first-party mobile app." The recurring failure mode this
note exists to prevent is a plane **designed without the client it serves** — the
`unproven` label, the mechanism with no caller. A sync protocol and a wrapping
scheme designed CLI-shaped (co-located, always-connected, exportable keys) get
retrofitted the day a phone shows up.

Each constraint below is stated, then resolved or assigned to the M9 issue it
shapes.

## 1. The device encryption key is non-exportable, and unwrap happens in-enclave

**Constraint.** Android Keystore and the iOS Secure Enclave hold a private key the
app **cannot export**; the ECDH happens *inside* the secure element. "Load the
private key and decrypt" is not an operation a phone can perform. ADR-0022's
*hardware-root-of-trust* revisit-clause fires here.

**Resolved.** ADR-0049 already wraps a space key to a device's **public** X25519
key; nothing about wrapping needs the private key. The unwrap side is the part
that could have been designed CLI-shaped, and was not: the device-side client
(`internal/personalstate/client`) takes an **`Unwrapper` interface**, not a raw
`*ecdh.PrivateKey`:

```go
type Unwrapper interface {
    Unwrap(wrapped []byte) (encryption.SpaceKey, error)
}
```

The desktop CLI supplies `KeyUnwrapper` (an exportable key, ECDH in-process,
ADR-0032's first device); a phone supplies a keystore-backed unwrapper that does
the ECDH in-enclave and never yields the private key. Same interface, no code
change. A test already drives an enclave-style unwrapper that never exposes the
key. **This constraint is discharged.** (Wrapping, `internal/personalstate/client`.)

## 2. Possession over mobile tolerates a slept, clock-drifted device

**Constraint.** A phone wakes from sleep with a **drifted clock** and a possession
proof / cert whose short window may read as `not_yet_valid` or `expired` — the
same skew asymmetry that bit M8's possession window. The auth flow must **re-mint,
not fail hard**, and background-refresh before the window lapses.

**Decision, assigned.** The direction is already set by ADR-0048: skew fails
*toward refusing a valid credential*, never toward honouring an expired one, with
a fixed margin that only shortens the honoured window. A phone must therefore
treat a `not_yet_valid`/`expired` on wake as "re-mint and retry", not "fail" — the
possession proof is cheap to re-sign locally (`internal/enrolment.SignPossession`
already takes an injected clock). **What M9 owes:** the possession/refresh flow
must be on M9's list as re-mint-on-wake with a background refresh cadence, not
discovered when the app is written. It rides the device-auth work, not a new
mechanism. (Possession/auth flow.)

## 3. Sync is shaped by an intermittent, metered, battery-bound link

**Constraint.** A phone syncs **opaque** CRDT changes and merges **client-side**
(§42, §44) over a link that drops, costs money, and drains a battery — not a
co-located CLI that never disconnects. The wire protocol must assume
disconnection is normal (ADR-0038's "a week of silence is Tuesday", applied to a
device), must let a client fetch only what it is missing by causal metadata, and
must lean on **snapshots and compaction** so a phone that has been offline does
not replay an unbounded change log.

**Assigned to #322 (sync protocol) and #325 (snapshots/compaction).** The sync
route must:
- be **resumable and incremental** — offer causal heads, pull only missing
  changes by id, never require a full-log replay on reconnect;
- carry **opaque** changes only (the peer never decrypts — Invariant 6), so the
  protocol is bytes-and-causal-metadata, nothing content-shaped;
- be a **separate protocol** from CAS sync (§44), so its framing is tuned for many
  small encrypted changes, not large blobs;
- reach a fresh or long-offline device via a **snapshot + tail** (#325), not the
  whole history.
These are constraints the #322/#325 issues must be checked against, not a CLI's
always-connected convenience.

## 4. Pairing works with a phone as the new device

**Constraint.** The SAS is 7-digit / QR-ready (#312, ADR-0022) and the commit-
before-reveal relay flow exists (#317). The phone is where "the old device shows a
QR, the phone scans it, the strings match" becomes real, and where the phone, as
the **new** device, generates both its signing and encryption keys in the keystore
and proves them over the relay.

**Assigned to #336.** The pairing SAS **primitive** already binds both of a
device's keys (v2, #337). The remaining work — folding the encryption key into the
relay's **commit-reveal** so a relay cannot substitute the phone's keystore
encryption key during pairing — is #336. That issue must be checked against a phone
generating a **non-exportable** encryption key: the relay carries only public keys
and the SAS, which is already the model (every pairing input is public).

## 5. The Personal MCP runs on the phone, over real encrypted state

**Constraint.** §73 puts the Personal MCP **device-side**. On a phone, an on-device
agent reaches private state (playlists, history, ratings, annotations) that must
**never leave the device** or be readable by any peer — the state is decrypted
only in the app, under a key unwrapped in-enclave (constraint 1).

**Assigned to #326.** The Personal MCP reads state only after the device unwraps
the space key (via the `Unwrapper`, constraint 1) and decrypts locally; it shares
no code or transport with the controller MCP (ADR-0032), and it never moves onto a
peer. #326 must be checked against a phone: the decrypted state lives only in the
app's memory, and the MCP surface is a **local** one (on-device stdio / IPC), never
a network endpoint a peer could reach.

## What this note obliges

- The M9 **wrapping** is done against a non-exportable keystore key (constraint 1,
  discharged in `internal/personalstate/client`).
- The M9 **sync protocol** (#322) and **compaction** (#325) issues cite constraints
  3 — resumable, incremental, opaque, snapshot-reachable — and are checked against
  an intermittently-connected client.
- The **possession/clock-skew** handling for a slept device is on M9's list
  (constraint 2), not discovered when the app is written.
- **Pairing** (#336) and the **Personal MCP** (#326) are checked against a phone as
  the new device and the on-device agent, respectively.

No mobile code is written here. The deliverable is the design that keeps M9 from
being CLI-shaped — higher-leverage than the app itself, because it lands *while*
M9 is being built.
