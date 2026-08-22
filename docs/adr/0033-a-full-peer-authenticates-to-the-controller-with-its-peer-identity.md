# 0033. A Full Peer authenticates to the controller with its ADR-0012 identity

**Status:** Accepted
**Date:** 2026-08-22

## Context

ADR-0011 covers how a *client* authenticates: scoped bearer tokens, loopback by
default. ADR-0012 covers how a peer authenticates to *another peer*: mTLS over
self-signed certificates whose Ed25519 public keys are pinned by membership.

Neither covers peer → controller, and ADR-0029 makes that link load-bearing. A
Full Peer is controller-attached: it runs no control plane and gets its
scheduling, authorisation and read routing from a controller it must now reach
across a network it does not control. Milestone 1 never had to answer the
question because `heyarr all` put every role in one process and the
split-process mode ran over loopback. A second site is what asks it.

## Decision

**A peer authenticates to the controller with the same identity it uses to
authenticate to a peer**: mTLS, self-signed, pinned against the membership
record, on the peer listener (`/peer/v1`). One mechanism, one credential, one
revocation path.

Three things follow, and they are the decision as much as the transport is.

**The controller's peer surface is a second listener, not a route group.** The
client API authenticates a bearer token; this surface authenticates a pinned
key. Two trust roots on one router would leave "which credential applies here"
to a per-route decision, and the failure mode of a per-route decision is the
one route that forgot.

**A peer is not an admin.** A peer certificate authenticates *as that peer* and
authorises only the peer surface — reporting its inventory, fetching its
snapshot, reading its jobs. It cannot create tokens, change policy, or write
catalog rows that are not about itself. A peer certificate presented on an
admin route of the client API is refused, and the refusal is central rather
than per-route so a route added tomorrow inherits it.

**The acting peer comes from the certificate and never from the request body.**
A body may *declare* which peer it believes it is; that declaration is
compared against the verified identity and a mismatch is refused. It is never
read as the answer. A surface that took the identity from the body would
authenticate every peer perfectly and then let any of them act as any other,
and every test in which both ends are honest would still pass.

## The alternative that was declined

A scoped bearer token per peer, extending ADR-0011, will look simpler. It is
not. It adds a second secret that has to be distributed to the remote site out
of band, stored at rest there, and revoked separately from membership — and
ADR-0012 already made *revocation is removing a membership record* the rule.
A per-peer token creates a second revocation path that nobody remembers to use:
the operator deletes the membership row, the peer's transport dies, and the
token is still valid the day someone re-enrols the key.

The peer already holds an Ed25519 keypair and the controller already pins it.
Reusing it costs one fewer credential to rotate, leak, or forget to revoke.

## Consequences

The controller is now a node with two listeners and two trust roots, and it must
keep serving deployments that have neither. `heyarr all`, a single-node install
and the split-process acceptance path need **no certificate configuration at
all**: the peer listener binds only when `peer.listen` is set. Loopback never
authenticates itself to itself, which is a requirement rather than a
convenience — the day it is not, a laptop install needs a PKI.

Membership becomes load-bearing for control-plane access as well as for byte
access. That is the intended concentration: one table to read to know who may
talk to this deployment, and one row to delete to stop them.

## What would make us revisit this

A peer class that has no Ed25519 identity — a browser-shaped client, a device
that enrols under Milestone 8's key model, an appliance that cannot generate or
hold a keypair. Or a controller that must accept peers it has **not** enrolled,
which is a different trust root and not a different transport, and would
supersede ADR-0012's pinning before it touched this record.
