package manifests

import (
	"context"
	"fmt"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// State is what is known about a blob's chunk manifest (§16, ADR-0034).
//
// # Why this is not a boolean
//
// `blobs.chunked` was one, from Milestone 1 until migration 00029, and it was
// 0 on every row in every deployment because nothing ever wrote it. §16 makes
// chunking lazy and says small blobs may never require a manifest at all, so
// the question has three answers and a boolean can carry two. The one it
// cannot carry is the one replication branches on: `false` conflates "we
// decided these bytes never need chunking" with "nobody has looked yet", and a
// caller that cannot tell those apart will generate a manifest to find out.
//
// # Asking must never generate
//
// That is the whole reason this type exists. Reading a State is a read: it
// touches the manifest tables and the blob row and nothing else, it enqueues
// no work, and StateUndecided is a legitimate final answer rather than a
// prompt to go and produce one. Deciding to chunk is a separate call by a
// caller that wanted to (M5-04), never a side effect of asking.
type State string

// The three states. Deliberately chosen so that none is a substring of
// another: `assert_contains` on "not_satisfied" matching "satisfied" shipped
// here once, and these are compared with equality everywhere for that reason.
const (
	// StatePresent — a manifest exists for these bytes.
	StatePresent State = "present"
	// StateNotRequired — a decision was recorded that these bytes will never
	// need one. §16: "small Blobs may never require chunk manifests."
	StateNotRequired State = "not_required"
	// StateUndecided — nobody has decided. Not "no", and not an error.
	StateUndecided State = "undecided"
)

// Valid reports whether s is one of the three.
func (s State) Valid() bool {
	switch s {
	case StatePresent, StateNotRequired, StateUndecided:
		return true
	default:
		return false
	}
}

func (s State) String() string { return string(s) }

// HasManifest is the honest reading of the old boolean: true only for
// StatePresent.
//
// It exists so the compatibility field the OpenAPI still requires has exactly
// one definition, rather than each caller re-deriving it and one of them
// deciding StateNotRequired ought to count. It is NOT a substitute for reading
// the State — a caller that branches on this alone has thrown the third state
// away again.
func (s State) HasManifest() bool { return s == StatePresent }

// ParseState turns a stored or transported value back into a State, refusing
// anything else rather than defaulting.
func ParseState(s string) (State, error) {
	if st := State(s); st.Valid() {
		return st, nil
	}
	return "", fmt.Errorf("manifests: %q is not a chunk-manifest state", s)
}

// Store is the manifest repository the fabric depends on (Invariant 2, §18).
//
// The fabric declares what it needs; internal/persistence/catalog implements
// it against SQLite. The content domain never sees this interface at all — it
// has no reason to know that chunks exist, and depguard enforces that it does
// not learn.
type Store interface {
	// Load returns the manifest for a blob, verified.
	//
	// found is false when there is none — that is an ordinary answer, not an
	// error, and it does not cause one to be produced. A manifest that fails
	// its own digest check is an error, not a missing manifest: silently
	// treating a tampered manifest as absent would hide the tampering behind a
	// slow path that still works.
	Load(ctx context.Context, blob hashing.Hash) (m Manifest, found bool, err error)

	// Save writes a manifest, replacing any manifest already held for the
	// blob. Idempotent, because the job that calls it will be re-run
	// (Invariant 9).
	Save(ctx context.Context, m Manifest) error

	// StateOf answers §16's three-way question for one blob.
	//
	// A READ. It writes nothing, it enqueues nothing, and it never produces
	// the manifest whose absence it is reporting.
	StateOf(ctx context.Context, blob hashing.Hash) (State, error)

	// RecordNotRequired records the policy decision that a blob will never
	// need a manifest, with the reason it was taken. This is the only way a
	// blob reaches StateNotRequired: it is a decision somebody made, written
	// down, not an inference from the absence of a row.
	RecordNotRequired(ctx context.Context, blob hashing.Hash, reason string) error

	// Discard removes a blob's manifest. Supported, not exceptional: ADR-0034
	// makes "delete every manifest" a legitimate recovery action and the
	// cheapest answer to a suspected chunker bug.
	Discard(ctx context.Context, blob hashing.Hash) error
}

// LocalChunk is one entry of the local chunk index: bytes this node holds, and
// where they sit inside a blob it holds.
//
// It is an answer to "where can I get these bytes from", and it is never an
// answer to "which blob is this" (ADR-0034). One digest maps to many of these
// by design — a chunk recurring across blobs is the case deduplication exists
// for.
type LocalChunk struct {
	Digest   hashing.Hash
	BlobHash hashing.Hash
	Offset   int64
	Length   int64
}

// Index is the local chunk index the reuse question is asked of (M5-07).
type Index interface {
	// RecordLocal replaces this node's index entries for one blob.
	RecordLocal(ctx context.Context, blob hashing.Hash, chunks []LocalChunk) error
	// Locate reports where this node holds a chunk, if anywhere. The result is
	// every place it occurs, not one — and never a blob identity.
	Locate(ctx context.Context, digest hashing.Hash) ([]LocalChunk, error)
}
