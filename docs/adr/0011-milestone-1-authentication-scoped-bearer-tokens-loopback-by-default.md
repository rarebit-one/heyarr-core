# 0011. Milestone 1 authentication: scoped bearer tokens, loopback by default

**Status:** Accepted
**Date:** 2026-08-19

## Context

Self-sovereign identity — device keys, delegations, grants, pairing — is
Milestone 8 (§84). Milestone 1 nonetheless serves every byte of the library over
HTTP with range requests.

## Decision

Opaque bearer tokens, hashed at rest with argon2id, compared in constant time,
carrying one of three scopes: `read`, `write`, `admin`. The server binds
`127.0.0.1` and a unix socket by default, and **refuses to start** if configured
to bind a non-loopback address with authentication disabled.

The `principals` table exists now so Milestone 8 is additive.

## Consequences

Shipping an unauthenticated media server "just for one milestone" is how a
homelab ends up with an open library on a routable address. The refusal-to-start
rule is the part that matters: a warning in a log is not a control.

Explicitly not built: users, sessions, password login, OIDC. Those are Milestone
8's problem and would be thrown away.
