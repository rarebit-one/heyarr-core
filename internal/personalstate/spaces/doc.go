// Package spaces models the EncryptedSpace: the unit of personal-state
// isolation (spec §39, Milestone 9).
//
// A space is a boundary around a slice of encrypted personal state —
// "personal", "family", a shared playlist, a research collection — and §39's
// load-bearing property is that "each space has independent encryption keys."
// The confidentiality plane (ADR-0049) wraps ONE symmetric key per space for
// each authorised device and for the recovery key; nothing here is that key.
// This package is the space's IDENTITY and METADATA only — deliberately NOT the
// encryption, NOT persistence, and NOT the wrapped keys, which are their own
// deliverables that build on the id this mints.
//
// # The one thing this model must get right: the id is opaque
//
// §38 draws the line the whole plane is built to honour: a peer (or the
// controller-side MCP) sees a space's ID and the causal metadata that routes its
// changes, and sees NONE of its names, members, or contents. ADR-0049 restates
// it — "what the peer does see, stated plainly: the space id … §38's list of
// what it does not see holds." So the id this package mints is a UUIDv7
// (ADR-0017), drawn from time and randomness and NEVER derived from or
// containing the space's kind, its name, or any content. That is not a stylistic
// choice: an id computed from the name would leak the name to every peer that
// stores the space, breaking Invariant 6 at the identifier before a single byte
// of ciphertext moved. The opacity is asserted at the model level (see the
// tests): two spaces of the same kind get different, unrelated ids.
//
// # Where a name lives, and why not here
//
// A space plainly has a human-readable name in the product ("Road Trip 2026",
// "Kids"). That name is encrypted personal-state CONTENT — it lives in the
// space's CRDT state under the space key (§38, §42), readable only by a device
// that can unwrap it. It is therefore deliberately absent from this
// server-visible model. The model holds exactly what a peer is allowed to know
// about a space's existence — its opaque id, its kind, and when it was created —
// and not one field more.
package spaces
