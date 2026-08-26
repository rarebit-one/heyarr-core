package device

import "time"

// View is the rendered shape of a device: what `--json` prints and what the
// Personal MCP returns.
//
// One type, used by both doors, because the honesty labelling below is the
// deliverable. Two renderings would be two places to forget it, and the one
// that forgot would look exactly like the one that had nothing to say.
//
// There is no private-key field, and there is no code path that could add one:
// [Device] never holds the seed to begin with.
type View struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Algorithm string `json:"algorithm"`
	// PublicKey is `ed25519:` followed by 64 lowercase hex characters —
	// identity.FormatPublicKey, the convention #135 established.
	PublicKey string `json:"public_key"`
	// EncryptionPublicKey is `x25519:` followed by 64 lowercase hex characters —
	// the device's key-agreement key, what space keys are wrapped for (§41,
	// ADR-0049). Omitted for a pre-Milestone-9 device that has none.
	EncryptionPublicKey string `json:"encryption_public_key,omitempty"`
	CreatedAt           string `json:"created_at"`
	// KeyPath is where the private key lives. The path, never the bytes.
	KeyPath string `json:"key_path"`
	// EnrolmentStatus is enum-like: "not_enrolled" or "enrolled".
	EnrolmentStatus string `json:"enrolment_status"`
	// EnrolledUser is the user identity this device authenticates as, when
	// enrolled — "ed25519:<hex>", omitted otherwise.
	EnrolledUser string `json:"enrolled_user,omitempty"`
	// Unproven says this key has proved nothing to anything. It is the same
	// word, meaning the same thing, as placement's `unproven`. It is false once
	// a valid enrolment cert is held.
	Unproven bool `json:"unproven"`
	// Authorises spells the caveat out for a reader who did not come here
	// looking for it. It changes with enrolment so it never claims something
	// that has stopped being true.
	Authorises string `json:"authorises"`
}

// NewView renders a device.
func NewView(d Device) View {
	return View{
		ID:                  d.ID,
		Name:                d.Name,
		Algorithm:           d.Algorithm,
		PublicKey:           d.PublicKeyString(),
		EncryptionPublicKey: d.EncryptionKeyString(),
		CreatedAt:           d.CreatedAt.UTC().Format(time.RFC3339Nano),
		KeyPath:             d.KeyPath,
		EnrolmentStatus:     d.EnrolmentStatus(),
		EnrolledUser:        d.EnrolledUser(),
		Unproven:            d.Unproven(),
		Authorises:          d.AuthorisationNote(),
	}
}

// NewViews renders a list, empty rather than nil.
func NewViews(devices []Device) []View {
	out := make([]View, 0, len(devices))
	for _, d := range devices {
		out = append(out, NewView(d))
	}
	return out
}
