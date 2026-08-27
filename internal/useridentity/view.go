package useridentity

import "time"

// View is the rendered shape of a user identity: what `identity --json` prints.
// There is no private-key field, and no code path that could add one: Identity
// never holds the seed to begin with.
type View struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Algorithm string `json:"algorithm"`
	// PublicKey is `ed25519:` followed by 64 lowercase hex characters — the
	// string a peer pins to trust this user (identity.FormatPublicKey, #135).
	PublicKey string `json:"public_key"`
	// EncryptionPublicKey is the recovery x25519 public key ("x25519:<hex>", §41)
	// a new space is wrapped for by default so it stays recoverable (#360).
	// Omitted on a pre-#360 identity that has none.
	EncryptionPublicKey string `json:"encryption_public_key,omitempty"`
	CreatedAt           string `json:"created_at"`
	// KeyPath is where the private key lives. The path, never the bytes.
	KeyPath string `json:"key_path"`
}

// NewView renders a user identity.
func NewView(i Identity) View {
	return View{
		ID:                  i.ID,
		Name:                i.Name,
		Algorithm:           i.Algorithm,
		PublicKey:           i.PublicKeyString(),
		EncryptionPublicKey: i.EncryptionKey,
		CreatedAt:           i.CreatedAt.UTC().Format(time.RFC3339Nano),
		KeyPath:             i.KeyPath,
	}
}
