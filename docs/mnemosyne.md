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
| Service recipients (`/api/v1/service-recipients`) | Always |
| Device enrolment (`/enrol`, `/membership`) | Always |
| Health and metrics (`/healthz`, `/readyz`, `/metrics`) | Always |
| Backup beat | Always |
| Encrypted-state replication beat | Always |
| `gc_blobs` job type (reclaim orphaned vault bytes) | Worker role only (`mnemosyne worker` / `mnemosyne all`) |
| GC beat (every 6 hours, no startup run) | Worker role only |
| `reconcile_peer` job type (plan vault-blob convergence toward placement-pin desired set) | Worker role only |
| Convergence beat (at startup, then every 5 minutes) | Worker role only |
| `replicate_blob` job type (mTLS blob transfer to named peers, bounded concurrency) | Worker role only |
| `chunk_blob` job type (produce chunk manifests for resumable large-blob transfers) | Worker role only |

**Not mounted on the HTTP surface:** `resources.API` (the monolithic route
function that registers works, assets, libraries, jobs, tokens, devices, peers,
events and sessions) is not mounted on the personal profile. Tokens, devices and
peers are managed through the CLI (`mnemosyne token create`, `mnemosyne device
list`, etc.), which opens the database directly rather than through HTTP.

Mnemosyne **does not** mount: the resource API, library/scanner/ingest, search,
acquisition, the MCP surface, render, relay, DLNA, OPDS, Subsonic, pair relay
or any provider integration.

## Default configuration

| Setting | Default |
|---------|---------|
| Data directory | `/var/lib/mnemosyne` |
| Listen address | `127.0.0.1:7778` |
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
- **Phase 2:** Mnemosyne-specific Docker image; CI acceptance for the personal
  profile; `mnemosyne fsck` reports vault completeness.
- **Phase 3:** Operator guide for a combined personal+media node; peer pairing
  between a Mnemosyne node and a heyarr node.
