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

---

# The API contract (what `heyarr-mobile` integrates against)

> The section above records the *constraints* a phone forces onto the planes. This
> section is the *contract*: the concrete endpoints, verbs and headers the
> separate `heyarr-mobile` repo (KMP, #330/#341) builds against, so its scaffold
> can be written against real routes rather than a sketch. Every path below exists
> in `heyarr-core` today unless marked. Client strategy — why this client is
> first-party and device-shaped rather than a stock Subsonic app — is ADR-0054;
> the QR login is ADR-0053.

All authenticated routes are under `/api/v1` and require at least the `read`
scope; the public login, streaming-capability and compat surfaces sit outside it
(see below). Errors are RFC-9457 problem+json (`internal/api/problem`).

## 1. Getting a session: two credential shapes

A `heyarr-mobile` install is a Voidbind **device** and should hold its own key —
so its primary credential is the Device scheme, and QR login is the fallback / the
first-run bootstrap before it is paired.

### 1a. Device credential (the product path, ADR-0048)

The client generates on-device Ed25519 (signing) + X25519 (encryption) keypairs
(the `voidbind-kmp` `DeviceIdentity`), is enrolled under the owner's user identity
by pairing (§4 above, #336), and then presents on every request:

```
Authorization: Device <enrolment-cert>~<possession-proof>
```

- `<enrolment-cert>` is the long-lived user-signed cert (`internal/enrolment`).
- `<possession-proof>` is a **short-lived** signature by the device key over the
  cert, **re-minted on wake** — never fail hard on `expired`/`not_yet_valid`,
  re-sign and retry (constraint 2 above; `enrolment.SignPossession`).

Enrolment itself: `POST /api/v1/devices` registers a device (write scope);
`GET /api/v1/devices`, `GET /api/v1/devices/{id}` list/inspect. Pairing exchanges
public values through the dumb relay at `PUT|GET /pair/sessions/{session}/slots/{slot}`
(ADR-0022, unauthenticated — it carries only commitments and public keys).

### 1b. QR web-login → session token (bootstrap / credential-less fallback, ADR-0053)

Used before the device is paired, and by any surface that should not hold a
standing key (a browser, a TV). Public routes, no credential to start:

| Verb & path | Purpose |
|---|---|
| `POST /login` | mint a login → `{ "id", "qr" }` where `qr` = `voidbind:login?rp=<origin>&id=<id>` |
| `GET /login/{id}` | poll → `{ "status": "pending\|approved\|expired", "token"?, "user"? }` |
| `GET /login/{id}/challenge` | the *approving* device fetches what to sign → `{ id, nonce, audience, expires_at }` |
| `POST /login/{id}/approve` | the approving device submits `{ "cert", "sig" }` |
| `GET /signin` | a static browser page that drives the above |

On `approved`, carry the minted token as `Authorization: Bearer <token>` on
`/api/v1`. It is **read-scoped** and short-lived. The **approving** half (fetch
challenge → hardware-gated sign → approve) is the `voidbind-kmp` authenticator's
existing `LoginApproval` flow — `heyarr-mobile` reuses it verbatim.

## 2. Library + playback (reach; a stock client also gets this)

- Browse the authenticated catalogue: `GET /api/v1/works`, `/works/{id}`,
  `/editions/{id}`, `/assets`, `/libraries`, `/publications`, `/consumption/sessions`.
- Plan/start playback: `POST /api/v1/playback/plan` (read), `POST /api/v1/playback`
  (write) → a `ContentURL` (+ optionally an ADR-0040 render capability URL for a TV).
- Stream bytes with Range/206/progressive-partial (ADR-0013, M10):
  `GET|HEAD /api/v1/blobs/{hash}/content` (bytes identified by BLAKE3 digest,
  Invariant 1 — never a path or id).
- **Compat, for reach and the credential-less TV** (bearer-as-password, never
  personal state, §72): OpenSubsonic `GET /rest/{method}` (`ping`, `getArtists`,
  `getAlbumList2`, `stream`, `download`, …); OPDS `GET /opds` (HTTP Basic); DLNA
  `/dlna` (capability `res` URLs, no auth). A first-party client SHOULD prefer the
  `/api/v1` surface; these exist so a stock app works and so a TV can browse.

## 3. Personal state — device-side key-holder ONLY (the hard, valuable part)

This is why the client is first-party (ADR-0054, Invariant 6). The controller
stores personal state (playlists, starred, reading position, history) **only as
ciphertext** and can neither decrypt nor merge it. Two orthogonal gates:

- a **grant/scope** authorises *fetching* the ciphertext, and
- a **wrapped key** (unwrapped in-enclave, constraint 1) authorises *reading* it.

Fetch/store the opaque state over the personal-state plane (`internal/api/personalstate`,
mounted under `/api/v1`, ADR-0049):

| Verb & path | Scope | Purpose |
|---|---|---|
| `GET /api/v1/spaces` | read | list the spaces this identity holds |
| `GET /api/v1/spaces/{id}/keys` | read | the **wrapped** key copies (the client unwraps its own with its X25519 device key — in-enclave) |
| `GET /api/v1/spaces/{id}/changes` | read | pull opaque CRDT changes by causal head (incremental/resumable, constraint 3) |
| `GET /api/v1/spaces/{id}/snapshot` | read | a snapshot for a fresh/long-offline device (constraint 3, #325) |
| `POST /api/v1/spaces` · `POST /spaces/{id}/changes` · `POST /spaces/{id}/snapshots` | write | store ciphertext the client encrypted on-device |
| `POST /api/v1/spaces/{id}/keys` (rewrap) · `DELETE /spaces/{id}/keys/{recipient}` (revoke) | write / admin | key rotation on device add/remove (#361) |

**The controller never sees plaintext.** Decryption and CRDT merge happen only in
the client, under a key it unwrapped in its secure element. On the device this is
surfaced through the **device-side Personal MCP** (`internal/device/personalmcp`,
#372/#387) — a **local stdio JSON-RPC** server (never a network endpoint a peer
could reach); its read tools return decrypted personal state that the
controller-side MCP has *no* tool to return. A stock Subsonic app that wants
playlists can instead point at the **device gateway** (`internal/device/gateway`,
ADR-0051) — a local `/rest` Subsonic server the *device* runs, composing
controller-library + device-decrypted personal state — but the first-party client
composes these itself and supersedes the gateway for the product.

## 4. TV: cast, don't log in (ADR-0054 / ADR-0040)

The phone is the key-holder and controller; the TV is a credential-less renderer.
The client resolves "continue watching" locally (personal state, §3) and hands the
TV an ADR-0040 signed capability URL to fetch bytes from — the TV holds no key, no
login, no personal state. Renderer control lives at
`POST /api/v1/renderers/{udn}/play|pause|resume|stop|seek` (write). SSDP
advertisement so a TV is discoverable is deferred (#202/#382); browse+serve exist.

## What is concrete enough to scaffold against, and what is not yet

- **Concrete today:** QR login (§1b), Device auth (§1a), library/playback/blob
  streaming (§2), the personal-state plane and the local Personal MCP surface (§3),
  renderer control (§4). `heyarr-mobile` can build its auth, browse, stream and
  device-side-decrypt layers against these routes now.
- **Deferred (do not scaffold against yet):** the **push** login channel (needs
  `voidbind-go` v0.5.0's notify plane + number-matching — QR is the shipping
  channel); a rendered **QR image** on `/signin`; **SSDP** TV discovery. Each is a
  tracked follow-up and none changes the routes above.
