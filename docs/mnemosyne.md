# Mnemosyne

Mnemosyne is a second binary built from the heyarr-core repository. It is the
personal-plane half of ADR-0107 ("personal media is a second service that
mounts only the personal plane"): a focused service for encrypted vaults, drive
CRDT state and placement pins, with no media library, no scanner and no
playback surface.

## Why a second binary?

ADR-0107 records the decision. The short version: personal media and mass media
have different trust models, different scaling profiles and different operator
concerns. A node that holds only encrypted vaults has no reason to run a
library scanner, an ingest scheduler or a compat adapter. A separate binary
makes that clear in the process table, the firewall and the systemd unit,
rather than hiding the distinction inside a feature flag.

## What Mnemosyne mounts

| Surface | Condition |
|---------|-----------|
| Blob content serving (GET/HEAD `/api/v1/blobs/{hash}/content`) | Always |
| Vault blob upload (PUT `/api/v1/vault/blobs/{hash}`) | Always (`vault.enabled` is unconditionally true) |
| Vault placement pins (`/api/v1/vault/placements`) | Always |
| Personal-state plane (`/api/v1/spaces`, `/api/v1/state/replicate`) | Always |
| Shared-space membership (`GET/POST /api/v1/spaces/{id}/members`, `DELETE /api/v1/spaces/{id}/members/{member}`, ADR-0108) | Always (part of the personal-state plane; the media profile mounts it too) |
| Service recipients (`/api/v1/service-recipients`) | Always |
| Peer membership (`/api/v1/peers`, `/api/v1/peers/{id}`, `/api/v1/peers/{id}/reconcile`) | Always (#762) |
| Device enrolment (`/enrol`, `/membership`) | Always |
| Health and metrics (`/healthz`, `/readyz`, `/metrics`) | Always |
| Backup beat | Always |
| Encrypted-state replication beat | Always |
| Discovery (mDNS / DNS-SD `_mnemosyne._tcp`, instance `mnemosyne`, SRV target `mnemosyne.local.`) | Same conditions as heyarr: a LAN-reachable port, a trusted multicast-capable interface (`http.guest.trusted_nets`), `http.discovery.disabled` unset. heyarr clients browsing `_heyarr._tcp` never see it; a Mnemosyne client browses `_mnemosyne._tcp` |
| `gc_blobs` job type (reclaim orphaned vault bytes) | Worker role only (`mnemosyne worker` / `mnemosyne all`) |
| GC beat (every 6 hours, no startup run) | Worker role only |
| `reconcile_peer` job type (plan vault-blob convergence toward placement-pin desired set) | Worker role only |
| Convergence beat (at startup, then every 5 minutes) | Worker role only |
| `replicate_blob` job type (mTLS blob transfer to named peers, bounded concurrency) | Worker role only |
| `chunk_blob` job type (produce chunk manifests for resumable large-blob transfers) | Worker role only |

**Not mounted on the HTTP surface:** the rest of `resources.API` (the route
function that registers works, assets, libraries, jobs, tokens, devices, events
and sessions) is not mounted on the personal profile. The one piece of it that
is mounted is peer membership (`MountPeers`): `mnemosyne peers add/list/show/
remove/ping/attach/report-inventory` are thin clients over `/api/v1/peers`,
exactly as heyarr's are, and without that route no membership record could ever
be written — so the peer surface, the encrypted-state replication beat,
`reconcile_peer` and `replicate_blob` would all be mounted with no sibling to
reach (#762). Enrolment is admin-scoped in both directions (ADR-0012); the list
is `read`.

Tokens are different: `mnemosyne token create` opens the database directly
rather than going through HTTP, so `/api/v1/tokens` stays unmounted. The CLI
loads the same mnemosyne profile as the server, so it opens the same
`mnemosyne.db` — a token minted on the command line is one the server knows.

User pins are the same. `/api/v1/identities/users` is not mounted, so the
first user is pinned on the host with `mnemosyne admin user pin <name>
<ed25519:public-key> --recovery-key <x25519:public-key>` (ADR-0032), which also
opens `mnemosyne.db` directly. A pin is local to one controller: the encrypted
state replication beat carries spaces, changes and wrapped keys, never pinned
identities, so pin the user on each sibling its devices enrol at.

A device paired on the command line enrols itself the way a phone does, with
`mnemosyne device enrol --peer <url>`: it posts its admitting op, a fresh
possession proof and the membership ops it knows to `POST /enrol` (ADR-0067).
No admin token is involved, and the user must already be pinned. A space key
can only be wrapped for an enrolled device (enrol-before-wrap, ADR-0049), so
enrol at the controller the device creates or opens spaces through. A wrapped
key replicated to a sibling is not gated on enrolment there.

Mnemosyne **does not** mount: the rest of the resource API, library/scanner/
ingest, search, acquisition, the MCP surface, render, relay, DLNA, OPDS,
Subsonic, pair relay or any provider integration.

## Sharing a space (ADR-0108)

The space is the unit of sharing. Only the owner's device changes who a
`personal`, `shared` or `research` space is shared with (any
management-authorised device on a `family` space, or on a space with no
recorded owner); members may not re-share. Adding a member is two steps, and
the CLI does both:

```
# From the owner's enrolled device: record the membership, then wrap the
# space's current key for each of the member's enrolled devices.
mnemosyne space members add <space-id> --user <principal>
mnemosyne space members list <space-id>
```

From the first membership row on, a wrap for an enrolled device whose user is
not a current member is refused with `wrap_recipient_not_member`. Recovery keys
and granted service recipients are not member devices and pass as before; a
recipient the space was created for keeps being re-wrapped.

Removing a member records a tombstone and names the rotation that completes
it. The server cannot rotate (it holds no key), so the removed member's devices
keep the copies they hold until the owner's device re-keys the space without
them — and that rotation is refused for every space that is not a playlist
until older clients understand key epochs (#698, #706):

```
mnemosyne space members remove <space-id> --user <principal>
# then, with an admin token, the rotation it prints:
mnemosyne space rotate <space-id> --revoke <device-key> [...]
```

## Default configuration

| Setting | Default |
|---------|---------|
| Data directory | `/var/lib/mnemosyne` |
| Listen address | `127.0.0.1:7779` (7778 is heyarr's conventional render-listener port, ADR-0079) |
| Unix socket | `/var/lib/mnemosyne/mnemosyne.sock` |
| Database | `/var/lib/mnemosyne/mnemosyne.db` |
| Profile | `personal` (fixed; cannot be changed) |
| Vault enabled | `true` (fixed; Mnemosyne is the vault service) |

All settings can be overridden via the config file or `MNEMOSYNE_*` environment
variables. The config file format is identical to heyarr's; unused fields are
silently ignored.

## Running

`mnemosyne all` is the operator default. It runs the controller (personal
profile) and the PersonalWorker in one process, which is what the systemd unit
and the homelab-ops Nix module both use. Run `serve` and `worker` as separate
processes only when they need to run on different machines or at different
resource limits.

```
# Operator default: controller + worker in one process (the systemd ExecStart)
mnemosyne --config /etc/mnemosyne/config.yaml all

# Controller only (no GC, no peer convergence, no blob replication)
mnemosyne --config /etc/mnemosyne/config.yaml serve

# Worker only (pair with a separately running serve process)
mnemosyne --config /etc/mnemosyne/config.yaml worker

# Minimal: built-in defaults, no config file
mnemosyne all

# Inspect the fully resolved configuration
mnemosyne config print

# Manage tokens (opens the database directly; no HTTP server needed)
mnemosyne token create --scope read
mnemosyne token list

# Enrol the sibling at the other site (over the API, with an admin token;
# the key is what `mnemosyne peers list` prints there)
mnemosyne peers add --name peer-b --site site-b --endpoint <host>:<peer-port> --public-key ed25519:<hex>
mnemosyne peers ping peer-b
```

## systemd

The release archive includes `deploy/systemd/mnemosyne.service`. Copy it to
`/etc/systemd/system/`, place a config file at `/etc/mnemosyne/config.yaml`,
create the `mnemosyne` user and group, and enable:

```
useradd --system --home-dir /var/lib/mnemosyne --shell /usr/sbin/nologin mnemosyne
install -d -o mnemosyne -g mnemosyne -m 0700 /var/lib/mnemosyne
install -d -o root -g root -m 0755 /etc/mnemosyne
systemctl daemon-reload
systemctl enable --now mnemosyne
```

The unit writes only to `/var/lib/mnemosyne`. It holds no capabilities, no
supplementary groups, no filesystem access outside its state directory.

## Running alongside heyarr

Mnemosyne and heyarr are independent processes. They share no database, no
socket and no data directory by default. A node can run both, though most
deployments will run one or the other:

- A **personal node** (phone backup, drive sync, vault replication) runs
  Mnemosyne only. It is a small, quiet process with no media library.
- A **media node** (library, ingest, transcode, playback) runs heyarr only.
  If vault.enabled is true (the default), it also serves vault routes.
- A **combined node** runs both on different ports. Each service holds its own
  vault pins; cross-site blob replication between them arrives in Phase 1b
  once the worker subcommand is available.

On the client side the rule is one app, one device: a phone running both the
heyarr app and the Mnemosyne app is two Voidbind devices, each paired, admitted
and enrolled (`POST /enrol`) on its own, because the two app sandboxes cannot
share a private key (ADR-0107 amendment, #765).

## Phase plan

This document tracks Phase 1 (ADR-0107). Future phases are tracked in the epic
and the architecture notes; they are not committed here until the spec is
settled.

- **Phase 1 (merged):** Second binary, personal profile, vault.enabled flag,
  goreleaser packaging, profile-routing tests. Personal-state plane, vault
  upload/pins, blob reads, enrolment, backup and encrypted-state replication
  are live.
- **Phase 1b (this PR):** `mnemosyne worker` and `mnemosyne all` subcommands;
  reconciliation sweep (GC + pin-driven peer convergence via
  `PlanPeerConvergence`); cross-site blob replication via `replicate_blob`;
  chunk-manifest generation via `chunk_blob`. The systemd unit now defaults to
  `mnemosyne all`.
- **Phase 2 (in progress):** ADR-0108 shared-space membership —
  `space_members` record, `GET/POST /api/v1/spaces/{id}/members` and
  `DELETE /api/v1/spaces/{id}/members/{member}`, the membership gate on
  enrol-before-wrap (`wrap_recipient_not_member`), and
  `mnemosyne space members list|add|remove` (shipped). Still open: the
  membership record replicating with the space (ADR-0101 path), member removal
  completing with a rotation on non-playlist spaces (gated on #698/#706), the
  two-device acceptance scene. Also planned: Mnemosyne-specific Docker image;
  CI acceptance for the personal profile; `mnemosyne fsck` reports vault
  completeness.
- **Phase 3:** Operator guide for a combined personal+media node; peer pairing
  between a Mnemosyne node and a heyarr node. **Album CRDT (merged, #756):** the
  first Mnemosyne-only kind — `internal/personalstate/crdt/album.go` — an
  ordered, deduplicated collection of vault photos inside a space (create, title,
  caption, cover, add / remove / move item, delete), each item a `PhotoManifest`
  (sealed manifest id, thumbnail and sidecar blob ids, plaintext BLAKE3,
  dimensions, capture time). Parity vectors for the Kotlin client are minted into
  `internal/personalstate/crdt/testdata/vectors/album.json`. The server is
  unchanged: it stores opaque changes and snapshots keyed by space and never
  learns the kind (Invariant 6).
