# 0011. Milestone 1 authentication: scoped bearer tokens, loopback by default

**Status:** Accepted
**Date:** 2026-08-19
**Amended:** 2026-08-20 — "opaque" made precise; see *Token shape*.

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

## Token shape

"Opaque" here means *opaque to the client*: the token carries no claims, no
expiry, no scope, and nothing a client can read, parse or rely on. Everything
that authorises is a row in `api_tokens`, so revocation is immediate and total.
It does **not** mean the token is undifferentiated bytes.

A token is `heyarr_<id>_<secret>`: a public selector and a secret verifier.

The selector exists because argon2id is deliberately expensive. Without it,
verifying a presented credential means running an argon2 verification against
every unrevoked row in the table until one matches — at any cost setting worth
choosing, that is not a slow lookup, it is a denial of service you have built
against yourself, and it gets worse with every token ever issued. The selector
makes verification a single row read and a single argon2 verify. It is not
secret and grants nothing: it is the same id `heyarr token list` prints and
`heyarr token revoke` takes.

The `heyarr_` prefix is there so the credential announces itself. Secret
scanning only works on secrets that are recognisable, and a leaked token nobody
can identify is a leaked token nobody revokes.

Both halves must be **canonically** encoded, not merely decodable. Unpadded
base32 leaves spare bits in its final character, so several distinct strings
decode to the same secret — one credential with many spellings. That breaks
anything keyed on the presented string: the verified-token cache would hold a
separate entry per spelling, and a future rate limit or audit trail would count
them as different tokens.

## Consequences

Shipping an unauthenticated media server "just for one milestone" is how a
homelab ends up with an open library on a routable address. The refusal-to-start
rule is the part that matters: a warning in a log is not a control.

Verification is cached, authorisation is not. Argon2id at RFC 9106 parameters
costs tens of milliseconds, which no API can pay per request; the *verification*
of a presented secret is therefore memoised, while the `api_tokens` row is read
on every request. Revocation and expiry bite immediately, which is the property
that actually matters — a cache that also cached the answer would keep a revoked
token working for as long as it lived.

Explicitly not built: users, sessions, password login, OIDC. Those are Milestone
8's problem and would be thrown away.

## What would make us revisit this

Milestone 8 replaces the scheme, not the refusal-to-start rule. If a peer-facing
API ever needs to authenticate without a round trip to the controller database,
the selector/verifier split is the wrong shape and a signed, verifiable
credential is the right one — but that is a different milestone's problem, and
buying it now would mean carrying claim expiry and key rotation through seven
milestones that do not need them.
