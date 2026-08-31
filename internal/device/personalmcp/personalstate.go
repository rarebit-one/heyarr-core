package personalmcp

// personalstate.go is the §72/§73 half of the Personal MCP: the tools that read
// DECRYPTED personal state — a playlist, listening history, starred items and
// reading positions — on an authorised device. The state is unwrapped and
// decrypted HERE, on the device, and never moves onto the controller: the
// controller-side MCP (internal/api/mcp) exposes NO tool that returns this
// plaintext (§72), which a boundary test on both surfaces asserts. This is what
// M8's empty device MCP shell was built to be populated with, "before there is
// any personal state to argue about" (ADR-0032) — and now there is.
//
// Each read verb names its space AND its CRDT type by which tool it is: a space
// holds one CRDT, and which one is a property the caller decides, never a tag on
// the wire (statesync's bridge doc; #386). personal_playlist decodes a space as
// a playlist, personal_starred as a starred set, and so on — the controller,
// holding only ciphertext, could not and must not learn which is which.

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
	// Starred returns a space's starred item ids, most-recently-starred first.
	Starred(spaceID string) ([]string, error)
	// History returns a space's decrypted listening history — recent items,
	// most-played items, and the single now-playing item.
	History(spaceID string) (PlayHistory, error)
	// ReadingPositions returns a space's per-publication reading positions,
	// ordered by publication id.
	ReadingPositions(spaceID string) ([]ReadingPosition, error)
}

// PlayHistory is a device-decrypted listening history view — the three reads a
// stock client asks of play history, each a pure function of the play-event set.
type PlayHistory struct {
	// Recent is the distinct item ids ordered most-recently-played first.
	Recent []string
	// Frequent is the distinct items with their play counts, most-played first.
	Frequent []ItemCount
	// NowPlaying is the item of the single most recent play, or "" when the
	// history is empty.
	NowPlaying string
}

// ItemCount is one item and how many times it was played.
type ItemCount struct {
	ID    string
	Count int
}

// ReadingPosition is one publication's decrypted reading position.
type ReadingPosition struct {
	PubID    string
	Position string
}

// spaceIDSchema is the one input every read verb takes: the opaque space id.
func spaceIDSchema() map[string]any {
	return obj(map[string]any{
		"space_id": map[string]any{
			"type":        "string",
			"description": "The opaque space id (a UUIDv7), as `space list` reports it.",
		},
	}, "space_id")
}

// decryptedHere is the shared caveat on every personal-state tool's description:
// the plaintext exists only here, on this device, under a key the controller
// never holds.
const decryptedHere = " It is decrypted HERE, on this device, under a key the controller never holds " +
	"— the controller stores ciphertext and can read none of it (spec §72, §73). This is the only " +
	"place this plaintext exists; the controller-side MCP has no tool that returns it."

// registerPersonalStateTools adds the read-over-real-state tools. It is called
// only when a reader is wired, so a device MCP with no personal-state access
// (the M8 shell) does not advertise tools it cannot serve.
func (s *Server) registerPersonalStateTools() {
	s.register(Tool{
		Name:  "personal_playlist",
		Title: "Read a space's playlist (decrypted on this device)",
		Description: "Return the items of an encrypted personal-state playlist, in their converged " +
			"order." + decryptedHere,
		ReadOnly:    true,
		InputSchema: spaceIDSchema(),
		Handler:     s.personalPlaylist,
	})
	s.register(Tool{
		Name:  "personal_starred",
		Title: "Read a space's starred items (decrypted on this device)",
		Description: "Return the item ids a user has starred/favourited in an encrypted personal-state " +
			"space, most-recently-starred first." + decryptedHere,
		ReadOnly:    true,
		InputSchema: spaceIDSchema(),
		Handler:     s.personalStarred,
	})
	s.register(Tool{
		Name:  "personal_history",
		Title: "Read a space's listening history (decrypted on this device)",
		Description: "Return an encrypted personal-state space's play history: the recently-played items, " +
			"the most-played items with their counts, and the single now-playing item." + decryptedHere,
		ReadOnly:    true,
		InputSchema: spaceIDSchema(),
		Handler:     s.personalHistory,
	})
	s.register(Tool{
		Name:  "personal_reading_position",
		Title: "Read a space's reading positions (decrypted on this device)",
		Description: "Return the per-publication reading positions held in an encrypted personal-state " +
			"space, each an opaque locator (a CFI, a percentage, a page)." + decryptedHere,
		ReadOnly:    true,
		InputSchema: spaceIDSchema(),
		Handler:     s.personalReadingPosition,
	})
}

// spaceArg decodes the one argument every read verb takes.
func spaceArg(args json.RawMessage) (string, error) {
	var in struct {
		SpaceID string `json:"space_id"`
	}
	if err := decode(args, &in); err != nil {
		return "", err
	}
	return in.SpaceID, nil
}

func (s *Server) personalPlaylist(args json.RawMessage) (any, error) {
	spaceID, err := spaceArg(args)
	if err != nil {
		return nil, err
	}
	items, err := s.personalState.Playlist(spaceID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"space_id": spaceID, "items": nonNil(items)}, nil
}

func (s *Server) personalStarred(args json.RawMessage) (any, error) {
	spaceID, err := spaceArg(args)
	if err != nil {
		return nil, err
	}
	items, err := s.personalState.Starred(spaceID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"space_id": spaceID, "starred": nonNil(items)}, nil
}

func (s *Server) personalHistory(args json.RawMessage) (any, error) {
	spaceID, err := spaceArg(args)
	if err != nil {
		return nil, err
	}
	h, err := s.personalState.History(spaceID)
	if err != nil {
		return nil, err
	}
	frequent := make([]map[string]any, 0, len(h.Frequent))
	for _, f := range h.Frequent {
		frequent = append(frequent, map[string]any{"id": f.ID, "count": f.Count})
	}
	return map[string]any{
		"space_id":    spaceID,
		"recent":      nonNil(h.Recent),
		"frequent":    frequent,
		"now_playing": h.NowPlaying,
	}, nil
}

func (s *Server) personalReadingPosition(args json.RawMessage) (any, error) {
	spaceID, err := spaceArg(args)
	if err != nil {
		return nil, err
	}
	positions, err := s.personalState.ReadingPositions(spaceID)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(positions))
	for _, p := range positions {
		out = append(out, map[string]any{"pub_id": p.PubID, "position": p.Position})
	}
	return map[string]any{"space_id": spaceID, "positions": out}, nil
}

// nonNil renders a nil slice as an empty JSON array rather than null, so a client
// reads "nothing starred" as [] and not as a missing field.
func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}
