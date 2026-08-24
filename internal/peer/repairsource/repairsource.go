// Package repairsource fetches replacement chunks for a damaged blob from the
// peers that hold it.
//
// It is the concrete half of [integrity.ChunkSource], which repair declares
// for itself and deliberately does not name an implementation of (ADR-0036).
// The two halves meet here, in the peer layer, because this is the only side
// that may know about both: the Storage Fabric must stay extractable and may
// not reach up into the domain or the peer fabric (invariant 3), so a repair
// that dials a peer cannot live inside it.
//
// # It ranks and it tries, and neither is a routing decision
//
// The candidate list is [catalog.Catalog.BlobSources] — the peers whose
// reported inventory says they hold these bytes, ranked by
// [replication.RankSources] — which is a list of BELIEFS. A peer may have
// dropped the blob since it last reported. So this walks the list rather than
// picking one, and a peer that answers 404 is an ordinary next-candidate
// rather than an error, exactly as the replication puller treats it.
//
// # "Nowhere to fetch from" is ordinary, and "the wrong bytes" is not
//
// ADR-0038 makes each peer authoritative for its own site: a peer that cannot
// reach anywhere holding these bytes is having a NORMAL day, and repair turns
// that into OutcomeUnreachable and leaves the blob alone. So every failure of
// reach or of absence collapses to [integrity.ErrNoSource].
//
// A peer that answered and served bytes that do not match the manifest is a
// different thing entirely. It is a peer that cannot back its own inventory,
// and it is the failure mode most easily lost by a loop that treats every
// error as "try the next one".
//
// # The walk continues past it, and it is REPORTED — both, not either
//
// The first version of this stopped the walk and returned the fault, on the
// argument that routing around a lying peer silently leaves the lie in place.
// That was half right. It made a repairable blob stay damaged because ONE peer
// lied, when a good peer was next in the list — availability sacrificed for
// observability, and the two are not actually in tension here.
//
// So: the walk continues, and every fault is handed to [Options.OnFault] on
// the way past. The repair succeeds from the next peer that can supply the
// bytes, AND the operator is told which peer served what it could not back —
// on the command's own output and in its exit status, not only in a log line
// somebody has to go looking for. See internal/cli's fsck.
package repairsource

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/peer/transfer"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/chunking"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
)

// Sources lists the peers believed to hold a blob, best candidate first.
//
// Declared here rather than taking *catalog.Catalog so that this package's
// tests do not need a database to state what a peer list is.
type Sources interface {
	BlobSources(ctx context.Context, blobHash string) ([]replication.Source, error)
}

// Fetcher reads one verified chunk from one named source.
//
// *transfer.Puller is the implementation. It is an interface here for the same
// reason Sources is, and because the thing worth asserting about this package
// is which candidates it tries and in what order — which a fake can answer and
// a real mTLS dial cannot.
type Fetcher interface {
	FetchChunk(ctx context.Context, src replication.Source,
		blob hashing.Hash, c chunking.Chunk) ([]byte, error)
}

// A SourceFault is one peer failing to back its own inventory: it answered,
// it served bytes, and the bytes are not what the manifest names.
//
// It is not an error value, because it does not stop anything. It is a report
// about a peer, produced on the way past it, and the repair it happened during
// may well have succeeded from somebody else.
type SourceFault struct {
	// PeerID and PeerName are the peer that served the bytes.
	PeerID   string
	PeerName string
	// Blob is what was being fetched, and Offset which chunk of it.
	Blob   string
	Offset int64
	// Err is what was wrong, carrying transfer's sentinel so a caller can tell
	// corrupt bytes from a redirect out of the fabric.
	Err error
}

func (f SourceFault) Error() string {
	name := f.PeerName
	if name == "" {
		name = f.PeerID
	}
	return fmt.Sprintf("peer %s served bytes at offset %d of %s that are not what the manifest "+
		"names: %v", name, f.Offset, f.Blob, f.Err)
}

// Options are a Source's dependencies.
type Options struct {
	Sources Sources
	Fetcher Fetcher
	// OnFault is called for every peer that serves bytes it cannot back,
	// whether or not the fetch then succeeds from somebody else. Optional, and
	// a Source without one still logs — but a log line is not an operator
	// telling you something went wrong, it is an operator having to go and
	// look.
	//
	// It is called from the fetching goroutine, so an implementation that
	// wants to accumulate must do its own locking.
	OnFault func(SourceFault)
	Logger  *slog.Logger
}

// Source is an [integrity.ChunkSource] backed by the peer fabric.
type Source struct {
	sources Sources
	fetcher Fetcher
	onFault func(SourceFault)
	log     *slog.Logger
}

// New builds a Source, or says what is missing.
func New(opts Options) (*Source, error) {
	if opts.Sources == nil {
		return nil, errors.New("repairsource: a source list is required — a repair with no way to " +
			"name a peer cannot fetch anything")
	}
	if opts.Fetcher == nil {
		return nil, errors.New("repairsource: a chunk fetcher is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Source{
		sources: opts.Sources, fetcher: opts.Fetcher, onFault: opts.OnFault,
		log: log.With("component", "repairsource"),
	}, nil
}

// FetchChunk returns the bytes of one chunk of blob, from the first peer that
// can supply them.
//
// The bytes are verified against the manifest's digest for this chunk before
// they are returned — by [transfer.Puller.FetchChunk], and then AGAIN by
// repair before they are written. That is not redundancy worth removing: this
// package's contract is "bytes or an error", and a caller that trusted the
// contract instead of the digest would be trusting a peer with extra steps
// (invariant 1).
func (s *Source) FetchChunk(
	ctx context.Context, blob hashing.Hash, c chunking.Chunk,
) ([]byte, error) {
	candidates, err := s.sources.BlobSources(ctx, blob.String())
	if err != nil {
		return nil, fmt.Errorf("repairsource: listing the peers that hold %s: %w", blob, err)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%w: no peer's inventory reports holding %s", integrity.ErrNoSource, blob)
	}

	var tried []error
	for _, src := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Refused before a connection is opened, and it is not an error about
		// this chunk: a candidate with no pinned key or no endpoint is one
		// membership cannot authenticate (ADR-0012).
		if err := src.Usable(); err != nil {
			tried = append(tried, err)
			continue
		}
		got, err := s.fetcher.FetchChunk(ctx, src, blob, c)
		if err == nil {
			s.log.Debug("fetched a replacement chunk from a peer",
				"blob_hash", blob.String(), "offset", c.Offset, "length", c.Length,
				"source_peer_id", src.PeerID, "source_peer_name", src.Name)
			return got, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		// A peer that served the WRONG BYTES is REPORTED and then walked past.
		// Reach and absence are ordinary; a source that cannot back its own
		// inventory is a fault, and the operator has to be told which peer it
		// was — but not at the cost of leaving a blob damaged that the next
		// candidate could have repaired.
		if errors.Is(err, transfer.ErrChunkCorrupt) || errors.Is(err, transfer.ErrRedirected) {
			fault := SourceFault{
				PeerID: src.PeerID, PeerName: src.Name,
				Blob: blob.String(), Offset: c.Offset, Err: err,
			}
			s.log.Warn("a peer served bytes that are not what the manifest names",
				"blob_hash", blob.String(), "offset", c.Offset,
				"source_peer_id", src.PeerID, "source_peer_name", src.Name, "error", err)
			if s.onFault != nil {
				s.onFault(fault)
			}
			tried = append(tried, fault)
			continue
		}
		tried = append(tried, fmt.Errorf("peer %s: %w", src.PeerID, err))
	}

	// Everything that is left is reach or absence, which ADR-0038 makes an
	// ordinary answer. Joined rather than summarised: an operator asking why a
	// repair could not happen wants each peer's reason, not a count.
	return nil, fmt.Errorf("%w: %d peer(s) report holding %s and none could serve it: %w",
		integrity.ErrNoSource, len(candidates), blob, errors.Join(tried...))
}
