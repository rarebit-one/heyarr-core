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
| Device enrolment (`/enrol`, `/membership`, `/api/v1/devices`) | Always |
| Token and auth (`/api/v1/tokens`, `/api/v1/session`) | Always |
| Peer mTLS surface (replication, blob replication) | When `peer.listen` is set |
| Health, metrics, events (`/healthz`, `/readyz`, `/metrics`, `/api/v1/events`) | Always |
| Backup beat | Always |
| GC | Always |

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

```
# Minimal: built-in defaults, no config file
mnemosyne serve

# With a config file
mnemosyne --config /etc/mnemosyne/config.yaml serve

# Inspect the fully resolved configuration
mnemosyne config print

# Manage tokens
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
- A **combined node** runs both on different ports. The two processes replicate
  vault blobs to each other through the peer fabric.

## Phase plan

This document tracks Phase 1 (ADR-0107). Future phases are tracked in the epic
and the architecture notes; they are not committed here until the spec is
settled.

- **Phase 1 (this PR):** Second binary, personal profile, vault.enabled flag,
  goreleaser packaging, profile-routing tests.
- **Phase 2:** Mnemosyne-specific Docker image; CI acceptance for the personal
  profile; `mnemosyne fsck` reports vault completeness.
- **Phase 3:** Operator guide for a combined personal+media node; peer pairing
  between a Mnemosyne node and a heyarr node.
