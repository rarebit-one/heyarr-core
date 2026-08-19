# 0012. Peer-to-peer authentication: mTLS over Ed25519 peer identity

**Status:** Accepted
**Date:** 2026-08-19

## Context

Spec §26 requires peer discovery to come from authenticated Heyarr membership
rather than public discovery. Milestone 4 is when peers first talk.

## Decision

Each peer holds an Ed25519 keypair generated at first start. Peers authenticate
to each other with mTLS using self-signed certificates whose public keys are
pinned by the controller-issued peer membership record. No CA, no PKI, no
public certificate authority in the inter-peer path.

Decided now; implemented in Milestone 4. Milestone 1's only cost is a
`peers.public_key` column and generating the local keypair.

## Consequences

Heyarr is responsible for its own transport security and must be safe over a
hostile network. In particular it must **not** treat an existing site-to-site
VPN as its security boundary — tunnels get reconfigured, and their crypto ages.

Pinning rather than a CA means peer membership is the only trust root, which is
what §26 asks for. Revocation is removing a membership record.
