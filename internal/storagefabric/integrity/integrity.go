package integrity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// Job types this package provides handlers for (§75).
const (
	// VerifyJobType re-hashes one blob.
	VerifyJobType = "verify_blob"
	// GCJobType reclaims unreferenced bytes.
	GCJobType = "gc_blobs"
)

// VerifyPayload is the verify_blob job payload.
type VerifyPayload struct {
	// Hash is the blob to re-read, in canonical form (ADR-0005).
	Hash string `json:"hash"`
}

// GCPayload is the gc_blobs job payload. The zero value is a dry run with the
// default grace window, which is the safe reading of an empty payload.
type GCPayload struct {
	// Apply flips the sweep from reporting to reclaiming. Absent means dry run,
	// because ADR-0018 makes that the default everywhere, and a job payload
	// that forgot the field must not be the one that deletes.
	Apply bool `json:"apply,omitempty"`
	// GraceSeconds overrides DefaultGrace. Zero means the default.
	GraceSeconds int64 `json:"grace_seconds,omitempty"`
}

// ErrUnknownBlob means the catalog has no row for that hash. A verify_blob job
// naming one is stale rather than wrong: the blob may have been reclaimed
// between the enqueue and the claim.
var ErrUnknownBlob = errors.New("integrity: the catalog has no such blob")

// Clock is injected so grace windows are asserted with a clock rather than a
// sleep (ADR-0017). A test that sleeps past a seven-day window does not exist.
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// Blob is what the catalog knows about a blob for integrity purposes.
type Blob struct {
	Hash hashing.Hash
	// Size is what the catalog recorded at ingest, which is the expectation a
	// shallow check tests the file against.
	Size int64
	// References is how many assets point at this blob. Zero is what makes a
	// blob a garbage collection candidate, and the only thing that does.
	References int
	// UnreferencedSince is when a sweep first observed References == 0. Zero
	// means either that the blob is referenced or that no sweep has seen it
	// unreferenced yet — the two cases are distinguished by References.
	UnreferencedSince time.Time
}

// Corruption is a blob whose bytes no longer hash to their own name, together
// with where those bytes were preserved.
type Corruption struct {
	Hash hashing.Hash
	// Actual is what the bytes hash to now.
	Actual hashing.Hash
	Size   int64
	// Path is where the bytes were moved. They are evidence and are never
	// deleted (ADR-0018).
	Path   string
	Detail string
}

// Catalog is what integrity needs from the control plane.
//
// It is an interface declared here and implemented in persistence for the same
// reason ingest's ports are: the Storage Fabric must stay extractable and may
// not depend on Heyarr's content domain (§18, ADR-0007). Everything below is
// phrased in blob hashes and times, never in works, editions or assets.
//
// Every method takes the time to record rather than reading a clock, so one
// injected clock governs a whole sweep and two rows written by the same pass
// cannot disagree about when it happened.
type Catalog interface {
	// Blobs lists every blob the catalog knows about, with its reference count.
	Blobs(ctx context.Context) ([]Blob, error)

	// Blob reads one. It returns ErrUnknownBlob when there is no such row.
	Blob(ctx context.Context, h hashing.Hash) (Blob, error)

	// Known reports which of these hashes still have a blobs row. Garbage
	// collection re-asks immediately before unlinking bytes it believes are
	// untracked, because the listing it started from is a snapshot and an
	// ingest may have committed since.
	Known(ctx context.Context, hashes []hashing.Hash) (map[string]bool, error)

	// MarkVerified stamps a successful verification and, if the replica was
	// previously anything other than present, records its return.
	MarkVerified(ctx context.Context, h hashing.Hash, at time.Time) error

	// MarkCorrupt records the replica as corrupt and appends the quarantine
	// ledger entry. The bytes themselves are already quarantined by the store.
	MarkCorrupt(ctx context.Context, c Corruption, at time.Time) error

	// MarkMissing records that this peer does not hold the bytes at all, and
	// marks the assets that referenced them missing.
	MarkMissing(ctx context.Context, h hashing.Hash, at time.Time) error

	// MarkUnreferenced starts the grace window for blobs observed with no
	// referencing asset. It never deletes anything.
	MarkUnreferenced(ctx context.Context, hashes []hashing.Hash, at time.Time) error

	// ClearUnreferenced ends the grace window for blobs that regained a
	// reference, so a returning blob gets a fresh full window rather than a
	// partly spent one.
	ClearUnreferenced(ctx context.Context, hashes []hashing.Hash) error

	// Reclaim removes the catalog's record of a blob and emits blob.reclaimed.
	//
	// tracked is false for bytes that never had a row — the orphan an ingest
	// fault between the CAS write and the commit leaves behind (M1-10). The
	// event is emitted either way: bytes leaving the store is a state
	// transition whether or not the catalog had noticed them (ADR-0009).
	//
	// Implementations must let the database's ON DELETE RESTRICT do the final
	// refcount check rather than trusting the caller's arithmetic. A garbage
	// collector that deletes a referenced blob has destroyed user data with no
	// way back, so it is worth being told twice.
	Reclaim(ctx context.Context, h hashing.Hash, size int64, tracked bool, at time.Time) error

	// Peers lists every peer other than this node (M4-12).
	//
	// The self row is excluded here rather than by every caller, because the
	// question this port exists to answer is "is this blob somewhere ELSE",
	// and a self row silently satisfying it is the exact failure ADR-0018
	// describes. An empty result is therefore a real answer — this deployment
	// has no elsewhere — and not an absence to be papered over.
	Peers(ctx context.Context) ([]Peer, error)

	// Replicas is what the catalog BELIEVES other peers hold of one blob,
	// joined to the peers it names: who claims it, how that claim is doing,
	// and how fresh it is (00023's reported_at).
	//
	// The interface had nothing peer-shaped in it until this method, which is
	// why garbage collection could not answer ADR-0018's deferred question.
	// Note what it returns: beliefs. Verifying them is Durability's job, and
	// keeping those two things in different ports is what stops a future edit
	// from quietly treating a row as an answer.
	Replicas(ctx context.Context, h hashing.Hash) ([]Replica, error)

	// MarkReplicaMissing corrects a row a peer contradicted — the catalog said
	// present, the peer answered that it does not hold the bytes.
	//
	// The correction is not tidiness. An uncorrected lying row keeps offering
	// the same false assurance to every later sweep, to read routing, and to
	// replication, and each of them would have to rediscover it. It is the
	// same transition an inventory report makes (§20), reached by a different
	// road.
	MarkReplicaMissing(ctx context.Context, h hashing.Hash, peerID string, at time.Time) error

	// RecordDurability writes down why a blob was believed durable elsewhere,
	// BEFORE the reclaim that relies on it.
	//
	// Before, because replicas.blob_hash is ON DELETE CASCADE: the transaction
	// that deletes the blobs row destroys every record of who else held it, so
	// evidence written afterwards would have nothing to say. Migration 00028
	// keeps this table free of foreign keys for the same reason.
	RecordDurability(ctx context.Context, e Evidence) error

	// DurabilityEvidence reads the evidence back, by hash, for a blob whose
	// row may well no longer exist. That it still answers after the reclaim is
	// the property the whole table was added for.
	DurabilityEvidence(ctx context.Context, h hashing.Hash) ([]Evidence, error)
}

// Store is the subset of the content-addressed store integrity uses.
type Store interface {
	Stat(ctx context.Context, h hashing.Hash) (cas.Descriptor, error)
	Verify(ctx context.Context, h hashing.Hash) error
	Delete(ctx context.Context, h hashing.Hash) error
	Walk(ctx context.Context, fn func(cas.Descriptor) error) error
}

// TempReaper is implemented by stores that keep partial writes on local disk.
//
// It is a separate, optional interface rather than part of Store because a
// remote store has no tmp/ directory of ours to sweep, and widening Store would
// force every implementation to answer a question that does not apply to it.
type TempReaper interface {
	TempFiles() ([]cas.TempFile, error)
	RemoveTemp(name string) error
}

// Options are the shared dependencies of the checker and the collector.
type Options struct {
	Store   Store
	Catalog Catalog
	Clock   Clock
	Logger  *slog.Logger

	// Durability is how another machine is asked whether it really holds a
	// blob, and how the controller is reached (ADR-0018, M4-12).
	//
	// It may be nil, and a nil one REFUSES rather than permits: in a
	// deployment with another peer, a collector with no way to check is a
	// collector with no business unlinking anything. See durability.go. A
	// single-peer deployment needs none, because there is nothing to ask.
	Durability Durability

	// Freshness is how recently another peer must have confirmed a replica for
	// that claim to count. Zero means DefaultFreshness; negative is refused
	// rather than read as "any age will do", which is the same argument the
	// grace window makes about a negative value.
	Freshness time.Duration
}

func (o Options) resolve() (Options, error) {
	if o.Store == nil {
		return Options{}, errors.New("integrity: a store is required")
	}
	if o.Catalog == nil {
		return Options{}, errors.New("integrity: a catalog is required")
	}
	if o.Clock == nil {
		o.Clock = systemClock{}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	switch {
	case o.Freshness == 0:
		o.Freshness = DefaultFreshness
	case o.Freshness < 0:
		return Options{}, fmt.Errorf("integrity: a negative freshness bound (%s) would accept a "+
			"replica claim of any age as evidence, which is the failing-open shape ADR-0018's "+
			"placement precondition exists to close", o.Freshness)
	}
	return o, nil
}
