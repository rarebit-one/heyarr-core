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
	CreatedAt string `json:"created_at"`
	// KeyPath is where the private key lives. The path, never the bytes.
	KeyPath string `json:"key_path"`
	// EnrolmentStatus is enum-like: today always "not_enrolled".
	EnrolmentStatus string `json:"enrolment_status"`
	// Unproven says this key has proved nothing to anything. It is the same
	// word, meaning the same thing, as placement's `unproven`.
	Unproven bool `json:"unproven"`
	// Authorises spells the caveat out for a reader who did not come here
	// looking for it.
	Authorises string `json:"authorises"`
}

// NewView renders a device.
func NewView(d Device) View {
	return View{
		ID:              d.ID,
		Name:            d.Name,
		Algorithm:       d.Algorithm,
		PublicKey:       d.PublicKeyString(),
		CreatedAt:       d.CreatedAt.UTC().Format(time.RFC3339Nano),
		KeyPath:         d.KeyPath,
		EnrolmentStatus: d.EnrolmentStatus(),
		Unproven:        d.Unproven(),
		Authorises:      NotYetAuthorising,
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
