// Package encryption holds the Milestone 9 key material: the X25519 device and
// recovery encryption keys that space keys are wrapped for, and (in later
// deliverables) the wrap/unwrap seal itself. Peers store wrapped keys and cannot
// unwrap them (§41, ADR-0049).
//
// It is deliberately the X25519 counterpart of internal/peer/identity, which
// owns the Ed25519 signing key ADR-0012 pins: the two are different primitives
// for different jobs — signing vs. key agreement — and ADR-0049 keeps them apart
// rather than deriving one from the other. A device therefore carries two keys,
// and each is rendered algorithm-prefixed so a log, a --json document or a cert
// names it unambiguously: an Ed25519 key is "ed25519:<hex>" (identity) and an
// encryption key is "x25519:<hex>" (here). Sharing the shape and not the code is
// the point.
package encryption
