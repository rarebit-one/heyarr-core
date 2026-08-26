package spaces

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Kind is the sort of personal state a space isolates (§39). It is a distinct
// string type, not a bare string, so a caller cannot pass an unchecked literal
// where a validated kind is meant: the compiler makes `NewSpace("famly", …)`
// (a typo) a type error at the call site rather than a bad row discovered later.
// The known kinds are the §39 examples; [Kind.Validate] is the gate that rejects
// anything else.
type Kind string

// The kinds §39 names. They are the values a peer is allowed to see (a kind is
// structural, not content — unlike a space's name, which is encrypted state):
//
//   - KindPersonal — one person's private state.
//   - KindFamily — the household's shared state.
//   - KindShared — an explicitly shared slice, e.g. a shared playlist.
//   - KindResearch — a research/collection space.
//
// They are lowercase and stable: a kind is written into a stored space record,
// so its spelling is a wire value, changed only by adding a new kind, never by
// re-spelling an existing one.
const (
	KindPersonal Kind = "personal"
	KindFamily   Kind = "family"
	KindShared   Kind = "shared"
	KindResearch Kind = "research"
)

// ErrUnknownKind is a space kind this package does not recognise. It is a
// distinct sentinel so a caller can tell a bad-kind refusal from any other
// failure with errors.Is, rather than string-matching a message.
var ErrUnknownKind = errors.New("spaces: unknown space kind")

// knownKinds is the closed set [Kind.Validate] checks against. A kind absent
// from it is rejected: an open-ended "anything goes" kind would let a caller
// invent categories the rest of the plane has no meaning for.
var knownKinds = map[Kind]bool{
	KindPersonal: true,
	KindFamily:   true,
	KindShared:   true,
	KindResearch: true,
}

// Validate reports whether k is one of the known §39 kinds, returning
// [ErrUnknownKind] (wrapped with the offending value) when it is not. It is the
// single gate every entry point runs a kind through, so an unknown kind cannot
// reach a stored space.
func (k Kind) Validate() error {
	if !knownKinds[k] {
		return fmt.Errorf("%w: %q — want one of personal, family, shared, research", ErrUnknownKind, string(k))
	}
	return nil
}

// EncryptedSpace is the server-visible identity of a personal-state space (§39).
//
// It holds exactly what a peer is permitted to know that a space EXISTS — its
// opaque id, its kind, and when it was created — and nothing that §38 marks as
// content. In particular there is no name field: a space's name is encrypted
// CRDT state under the space key (see the package doc), not metadata a peer may
// store. The zero value is not a valid space; construct one with [NewSpace].
type EncryptedSpace struct {
	// ID is the opaque handle a peer and the controller-side MCP see (§38). It is
	// a UUIDv7 (ADR-0017), minted from time and randomness and NEVER derived from
	// the kind, a name, or any content — deriving it from anything the space
	// contains would leak that thing to every peer holding the space. It is the
	// one field the whole confidentiality argument rests on being opaque.
	ID string

	// Kind is the §39 category. It is structural — a peer may see it — and is
	// always one of the known kinds, because [NewSpace] refuses to mint a space
	// with any other.
	Kind Kind

	// CreatedAt is when the space was minted, in UTC. It comes from a caller-
	// injected clock (ADR-0017), never time.Now() reached for inside the
	// constructor, so a test can pin it and two spaces are not forced to differ
	// only because real time moved between them.
	CreatedAt time.Time
}

// NewSpace mints a space of the given kind, stamped at now.
//
// It validates the kind (an unknown kind is refused with [ErrUnknownKind] and no
// space is returned) and mints a fresh opaque UUIDv7 id. The timestamp is
// injected rather than read from the wall clock here (ADR-0017): the caller
// passes clock.Now(), which is what lets a test hold time still and still get
// distinct spaces — because the id's uniqueness comes from the UUID, not from
// the clock. now is normalised to UTC so a stored space never carries a local
// offset.
func NewSpace(kind Kind, now time.Time) (EncryptedSpace, error) {
	if err := kind.Validate(); err != nil {
		return EncryptedSpace{}, err
	}
	// SABOTAGE NOTE: the id MUST NOT be derived from the kind (or a name, or any
	// content). If this line were, say, `uuid.NewSHA1(ns, []byte(kind))`, every
	// space of the same kind would collide on one id — and that is exactly what
	// the uniqueness/opacity test asserts against, so the shortcut fails loudly
	// rather than silently leaking the kind into the handle a peer stores.
	return EncryptedSpace{
		ID:        uuid.Must(uuid.NewV7()).String(),
		Kind:      kind,
		CreatedAt: now.UTC(),
	}, nil
}
