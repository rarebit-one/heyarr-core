package spaces

import "time"

// View is the rendered shape of a space: what `--json` prints and what a peer
// or the controller-side MCP is allowed to learn about a space's existence.
//
// It mirrors [internal/device.View]'s job — one rendering, so there is one place
// that decides what leaves the process. Crucially it carries ONLY §38-safe
// metadata: the opaque id, the kind, and the created-at. There is no name field
// and no content field, and there is no code path that could add one, because
// [EncryptedSpace] does not hold them to begin with — the same discipline
// device's View keeps by never holding a private key.
type View struct {
	// ID is the opaque UUIDv7 handle (§38). Safe to log and to hand a peer.
	ID string `json:"id"`
	// Kind is the §39 category, rendered as its lowercase string.
	Kind string `json:"kind"`
	// CreatedAt is RFC 3339 (nanosecond) UTC, matching device's rendering.
	CreatedAt string `json:"created_at"`
}

// NewView renders a space.
func NewView(s EncryptedSpace) View {
	return View{
		ID:        s.ID,
		Kind:      string(s.Kind),
		CreatedAt: s.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// NewViews renders a list, empty rather than nil so a JSON client gets `[]` and
// not `null` — the same reason [internal/device.NewViews] does.
func NewViews(spaces []EncryptedSpace) []View {
	out := make([]View, 0, len(spaces))
	for _, s := range spaces {
		out = append(out, NewView(s))
	}
	return out
}
