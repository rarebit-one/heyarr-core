package personalmcp

// personalstate.go is the §72/§73 half of the Personal MCP: the tools that read
// DECRYPTED personal state — a playlist and, later, listening history, ratings,
// annotations — on an authorised device. The state is unwrapped and decrypted
// HERE, on the device, and never moves onto the controller: the controller-side
// MCP (internal/api/mcp) exposes NO tool that returns this plaintext (§72), which
// a boundary test on both surfaces asserts. This is what M8's empty device MCP
// shell was built to be populated with, "before there is any personal state to
// argue about" (ADR-0032) — and now there is.

import "encoding/json"

// PersonalStateReader reads a device's decrypted personal state. Its
// implementation lives on the device (the CLI's `device mcp`, and a phone app):
// it fetches the opaque ciphertext from the controller, unwraps the space key
// with this device's key, decrypts and merges the CRDT — all locally. The
// controller sees only ciphertext; this interface returns the plaintext the
// device alone can produce.
type PersonalStateReader interface {
	// Playlist returns a space's playlist items in their converged order.
	Playlist(spaceID string) ([]string, error)
}

// registerPersonalStateTools adds the read-over-real-state tools. It is called
// only when a reader is wired, so a device MCP with no personal-state access
// (the M8 shell) does not advertise tools it cannot serve.
func (s *Server) registerPersonalStateTools() {
	s.register(Tool{
		Name:  "personal_playlist",
		Title: "Read a space's playlist (decrypted on this device)",
		Description: "Return the items of an encrypted personal-state playlist, in their converged " +
			"order. The playlist is decrypted HERE, on this device, under a key the controller " +
			"never holds — the controller stores ciphertext and can read none of it (spec §72, §73). " +
			"This is the only place this plaintext exists; the controller-side MCP has no tool that " +
			"returns it.",
		ReadOnly: true,
		InputSchema: obj(map[string]any{
			"space_id": map[string]any{
				"type":        "string",
				"description": "The opaque space id (a UUIDv7), as `space list` reports it.",
			},
		}, "space_id"),
		Handler: s.personalPlaylist,
	})
}

func (s *Server) personalPlaylist(args json.RawMessage) (any, error) {
	var in struct {
		SpaceID string `json:"space_id"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	items, err := s.personalState.Playlist(in.SpaceID)
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []string{}
	}
	return map[string]any{"space_id": in.SpaceID, "items": items}, nil
}
