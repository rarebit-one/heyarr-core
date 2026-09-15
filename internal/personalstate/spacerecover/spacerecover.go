// Package spacerecover recovers vault space keys from the paper recovery secret,
// offline (ADR-0022, ADR-0049) — the load-bearing half of the vault gate that
// ADR-0021 forbids shipping a vault without: "key loss is total data loss, so
// recovery must reconstruct the ability to unwrap the wrapped keys the peers
// already hold, offline, with Heyarr not running."
//
// The peers hold, for every space, a copy of that space's key sealed for the
// user's recovery ENCRYPTION key (a permanent wrap target, ADR-0049). This
// package re-derives that key's private half from the recovery secret under the
// distinct label heyarr/recovery/v1/user-encryption-x25519-seed — a different key
// from the identity signing seed — and unwraps those copies. Derivation and
// unwrapping are pure functions of the secret and the ciphertext: no process, no
// network, and no disk beyond reading the wrapped bytes (which the caller supplies).
//
// This is deliberately NOT routed through the device-side client.Unwrapper: that
// abstraction exists for device keys that may live in a non-exportable keystore,
// whereas the recovery key is inherently a software key re-derived from the paper
// secret. Keeping it a one-shot offline path is the ADR-0022 recovery chain
// (secret -> root key -> unwrap the copies the peers hold -> re-wrap for new
// devices); the re-wrap tail is #545, and lives with the caller.
package spacerecover

import (
	"fmt"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
	"github.com/rarebit-one/heyarr-core/internal/recovery"
)

// RecipientID returns the wrapped_keys recipient id ("x25519:<hex>") a space key
// is sealed for when it is wrapped for the user's recovery key. A caller uses it
// to select, from a peer's stored wrapped copies, the ones this secret can open.
func RecipientID(secret recovery.Secret) (string, error) {
	seed := recovery.DeriveUserEncryptionSeed(secret)
	defer clear(seed)
	priv, err := encryption.NewPrivateKey(seed)
	if err != nil {
		return "", fmt.Errorf("spacerecover: deriving the recovery encryption key: %w", err)
	}
	return encryption.FormatPublicKey(priv.PublicKey().Bytes()), nil
}

// UnwrapAll re-derives the recovery private key from the paper secret and unwraps
// every wrapped copy in wrapped (keyed by an id the caller chooses, e.g. a space
// id), returning the recovered space keys under the same ids. It is offline and
// pure — derivation and unwrapping touch no process, network or disk (ADR-0022).
//
// The transient derived seed is zeroized before return. The *ecdh.PrivateKey it
// produces is managed by the runtime and cannot be zeroized here; the seed, which
// this package owns, is.
//
// It fails on the FIRST copy that does not open (a wrong secret, a truncated or
// tampered blob), because a partial recovery that silently drops spaces is worse
// than a loud one — Unwrap never says which of those it was (ADR-0049), so neither
// does this.
func UnwrapAll(secret recovery.Secret, wrapped map[string][]byte) (map[string]encryption.SpaceKey, error) {
	seed := recovery.DeriveUserEncryptionSeed(secret)
	defer clear(seed)
	priv, err := encryption.NewPrivateKey(seed)
	if err != nil {
		return nil, fmt.Errorf("spacerecover: deriving the recovery encryption key: %w", err)
	}
	out := make(map[string]encryption.SpaceKey, len(wrapped))
	for id, w := range wrapped {
		sk, err := encryption.Unwrap(w, priv)
		if err != nil {
			return nil, fmt.Errorf("spacerecover: unwrapping the key for space %q: %w", id, err)
		}
		out[id] = sk
	}
	return out, nil
}
