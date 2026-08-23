package integrity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/chunking"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

// Outcome is how a repair ended.
//
// Every one of these is a legitimate ending. Only OutcomeRepaired changed
// anything; the rest describe a store that is exactly as it was, and the value
// of the type is that a caller can tell WHICH — a refusal nobody can read is
// an outage nobody can diagnose (M4-12).
//
// The values are compared with equality everywhere, and none is a substring of
// another, for the reason manifests.State spells out: an `assert_contains` on
// an enum has shipped a passing test over a wrong value in this repo before.
type Outcome string

// The endings a repair can have.
const (
	// OutcomeRepaired — a replacement was assembled, verified, and published,
	// and the damaged original is in quarantine. The only outcome that writes.
	OutcomeRepaired Outcome = "repaired"
	// OutcomeHealthy — every chunk verified against the manifest and the
	// length agreed. Nothing was staged, quarantined or published.
	OutcomeHealthy Outcome = "healthy"
	// OutcomeNoManifest — there is no chunk manifest for these bytes, so
	// there is no such thing as a chunk-scoped repair of them. Stated
	// plainly rather than made a precondition: the remedy is a whole re-pull,
	// which is replication's job and not this package's (ADR-0030, ADR-0034).
	OutcomeNoManifest Outcome = "no_manifest"
	// OutcomeNoLocalBytes — this node holds nothing to repair, neither an
	// addressable blob nor a quarantined artefact. Also a whole re-pull.
	OutcomeNoLocalBytes Outcome = "no_local_bytes"
	// OutcomeUnreachable — no peer offered the replacement chunks. Under
	// ADR-0038 this is ORDINARY, not an alarm: a peer that cannot reach
	// another peer is a peer, not a fault. The damaged bytes are left exactly
	// as they were.
	OutcomeUnreachable Outcome = "unreachable"
	// OutcomeSourceCorrupt — the chunks a peer served did not hash to what
	// the manifest says they are, so nothing was assembled from them. This is
	// the case where a repairer that trusted its source would write garbage
	// over a file that was still recoverable (Invariant 1).
	OutcomeSourceCorrupt Outcome = "source_corrupt"
	// OutcomeAssemblyMismatch — the reconstruction was complete and did not
	// hash to the blob's identity. Nothing is published: a repair that cannot
	// produce those exact bytes is a failed repair (ADR-0036).
	OutcomeAssemblyMismatch Outcome = "assembly_mismatch"
	// OutcomeFailed — an I/O error stopped the repair. Distinct from every
	// outcome above, all of which are answers; this one is a question.
	OutcomeFailed Outcome = "failed"
)

// Repaired reports whether this outcome changed the store.
func (o Outcome) Repaired() bool { return o == OutcomeRepaired }

// ErrNoSource means no peer offered these bytes.
//
// A ChunkSource returns it for the ordinary case, and it is ordinary: ADR-0038
// makes each peer authoritative for its own site and makes "I cannot reach
// anywhere that has this" a normal answer rather than a failure. Repair turns
// it into OutcomeUnreachable and leaves the blob alone.
var ErrNoSource = errors.New("integrity: no reachable peer holds these bytes")

// ChunkSource fetches replacement chunks for a damaged blob from a peer.
//
// # Why this interface is declared here
//
// It is the consumer's statement of what it needs, in the shape ManifestSource
// and BlobServer are already declared in: repair needs the bytes of ONE chunk
// of ONE blob from somewhere that has them, and nothing else. The
// implementation is the M5-06/M5-07 ranged fetch — the resumable transfer path
// being written in internal/peer/transfer — and this package deliberately does
// not name its types, so the two can land independently.
//
// # Nothing here is trusted
//
// An implementation may return the wrong bytes, a peer's own corruption, or a
// truncated read. Repair verifies every chunk against the manifest digest that
// names it and then verifies the whole assembly against the blob's identity
// (Invariant 1, ADR-0035). The verification is the caller's, always.
type ChunkSource interface {
	// FetchChunk returns the bytes of one chunk of blob. The chunk carries
	// its offset, length and digest; an implementation may use the offset and
	// length to make a ranged request and must not assume the caller will
	// accept bytes it did not ask for.
	//
	// It returns ErrNoSource when no peer it can reach holds the blob.
	FetchChunk(ctx context.Context, blob hashing.Hash, chunk chunking.Chunk) ([]byte, error)
}

// RepairStore is what a repair needs from the content store.
//
// Wider than Store because repair is the only thing in Heyarr that changes
// what a blob's bytes are, and narrow even so: it can read, it can stage, it
// can quarantine, and it can publish what it staged. There is no method here
// that writes to an addressable file, because ADR-0036 forbids the operation,
// not merely the mistake.
type RepairStore interface {
	Has(ctx context.Context, h hashing.Hash) (bool, error)
	Open(ctx context.Context, h hashing.Hash) (cas.ReadSeekCloser, cas.Descriptor, error)
	Stage() (*cas.Staged, error)
	Quarantine(h hashing.Hash) (string, error)
	QuarantinedBlobs() ([]cas.Quarantined, error)
	OpenQuarantined(name string) (cas.ReadSeekCloser, error)
}

// RepairResult is what one repair did, and why.
type RepairResult struct {
	Hash    string  `json:"hash"`
	Outcome Outcome `json:"outcome"`
	// ChunksTotal is how many chunks the manifest describes.
	ChunksTotal int `json:"chunks_total"`
	// ChunksDamaged is how many of them did not verify locally.
	ChunksDamaged int `json:"chunks_damaged"`
	// ChunksFetched is how many were pulled from a peer. It is the number the
	// whole feature exists to keep small, so it is reported rather than
	// inferred.
	ChunksFetched int `json:"chunks_fetched"`
	// BytesFetched is the network cost of the repair, and BytesRead the local
	// cost. ADR-0036 is explicit that only the first is proportional to the
	// damage; reporting both is what stops the second being mistaken for a
	// regression.
	BytesFetched int64 `json:"bytes_fetched"`
	BytesRead    int64 `json:"bytes_read"`
	BlobSize     int64 `json:"blob_size"`
	// QuarantinePath is where the damaged original was preserved (ADR-0018).
	QuarantinePath string `json:"quarantine_path,omitempty"`
	// Detail says why, in a sentence an operator can act on.
	Detail string `json:"detail"`
}

// RepairOptions are a repairer's dependencies.
type RepairOptions struct {
	Store     RepairStore
	Manifests manifests.Store
	Catalog   Catalog
	// Source is where replacement chunks come from. It may be nil, and a nil
	// one REFUSES rather than permits, exactly as a nil Durability does: a
	// repairer with no way to fetch replacements reports every damaged blob
	// as unreachable and says so, rather than appearing to have looked.
	Source ChunkSource
	Clock  Clock
	Logger *slog.Logger
}

func (o RepairOptions) resolve() (RepairOptions, error) {
	if o.Store == nil {
		return RepairOptions{}, errors.New("integrity: a store is required to repair")
	}
	if o.Manifests == nil {
		return RepairOptions{}, errors.New("integrity: a manifest store is required to repair — " +
			"a chunk-scoped repair is a repair against a manifest (ADR-0034)")
	}
	if o.Catalog == nil {
		return RepairOptions{}, errors.New("integrity: a catalog is required to repair")
	}
	if o.Clock == nil {
		o.Clock = systemClock{}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return o, nil
}

// Repairer replaces the damaged parts of a blob by staging a whole replacement
// (§57, §84, ADR-0036).
type Repairer struct {
	opts RepairOptions
}

// NewRepairer constructs a Repairer.
func NewRepairer(opts RepairOptions) (*Repairer, error) {
	resolved, err := opts.resolve()
	if err != nil {
		return nil, err
	}
	return &Repairer{opts: resolved}, nil
}

// Repair restores one blob to the bytes its name says it holds.
//
// # The order is the design, and it is load-bearing
//
//  1. RECONSTRUCT into the store's private staging area — intact local chunks
//     plus replacements fetched from a peer, each verified against the
//     manifest entry that names it. Nothing addressable is touched. During
//     this whole window the blob's digest still resolves to the original
//     bytes, damage and all, which is correct: that is what this node
//     actually holds, and a caller that needs good bytes has a peer to read
//     from (ADR-0030, ADR-0036).
//  2. VERIFY the assembled whole-object digest against the blob's ADR-0005
//     identity. Before this point nothing may be published, because until it
//     passes the assembly is just bytes.
//  3. QUARANTINE the damaged original, BEFORE publication (ADR-0018). The two
//     orderings differ only in what a crash between them costs: quarantine
//     first and a crash loses the REPAIR, leaving the damaged bytes safe in
//     quarantine and the blob missing — which replication already knows how
//     to fix. Publish first and a crash loses the EVIDENCE, permanently. One
//     is recoverable and one is not.
//  4. PUBLISH by the existing atomic link, the same one every other write to
//     the store uses.
//
// There is no in-place write at any step, and no interval in which a partly
// assembled file answers to the blob's digest. The store is therefore in
// exactly one of two states at every instant — the pre-repair one or the
// post-repair one — plus a reapable staging file.
func (r *Repairer) Repair(ctx context.Context, h hashing.Hash) (RepairResult, error) {
	result := RepairResult{Hash: h.String(), Outcome: OutcomeFailed}

	m, found, err := r.opts.Manifests.Load(ctx, h)
	if err != nil {
		return result, err
	}
	if !found {
		result.Outcome = OutcomeNoManifest
		result.Detail = "no chunk manifest for these bytes, so there is nothing to repair chunk by " +
			"chunk; the remedy is a whole re-pull from a peer, which is replication's job (ADR-0034)"
		return result, nil
	}
	if err := m.Validate(); err != nil {
		return result, err
	}
	result.ChunksTotal = m.ChunkCount()
	result.BlobSize = m.CoveredSize

	src, err := r.localBytes(ctx, h)
	if err != nil {
		return result, err
	}
	if src == nil {
		result.Outcome = OutcomeNoLocalBytes
		result.Detail = "this node holds neither these bytes nor a quarantined artefact of them, " +
			"so there is nothing to repair; the remedy is a whole transfer from a peer"
		return result, nil
	}
	defer func() { _ = src.rc.Close() }()

	// Locate the damage first, as a pure read. It costs one pass over the
	// blob and it is what makes the FETCH proportional to the damage — and it
	// is also what makes a healthy blob cost nothing at all: no staging file
	// is created, so "repair" over an undamaged store writes nothing, and a
	// repairer that silently rewrote every blob would be visible immediately.
	damaged, read, err := locateDamage(ctx, src, m)
	result.BytesRead = read
	if err != nil {
		return result, err
	}
	result.ChunksDamaged = len(damaged)
	if len(damaged) == 0 && src.size == m.CoveredSize {
		result.Outcome = OutcomeHealthy
		result.Detail = "every chunk verified against the manifest and the length agreed; " +
			"nothing was staged, quarantined or published"
		return result, nil
	}
	if len(damaged) == 0 {
		// Every chunk verified but the file is the wrong length: the damage is
		// bytes past the end of what the manifest covers. The reconstruction
		// below drops them, which is the repair.
		result.Detail = fmt.Sprintf("every chunk verified but the file is %d bytes against a "+
			"manifest covering %d", src.size, m.CoveredSize)
	}

	if r.opts.Source == nil && len(damaged) > 0 {
		result.Outcome = OutcomeUnreachable
		result.Detail = "no peer transport is configured, so no replacement chunks can be " +
			"fetched; the damaged bytes are untouched (ADR-0038)"
		return result, nil
	}

	staged, err := r.opts.Store.Stage()
	if err != nil {
		return result, err
	}
	// Unconditional: a no-op after a successful publish, and the thing that
	// keeps a failed repair from leaving anything behind at all.
	defer func() { _ = staged.Discard() }()

	assembly, err := r.assemble(ctx, staged, src, m, damaged)
	result.ChunksFetched = assembly.fetched
	result.BytesFetched = assembly.bytesFetched
	result.BytesRead += assembly.bytesRead
	if err != nil {
		return result, err
	}
	if assembly.outcome != "" {
		result.Outcome = assembly.outcome
		result.Detail = assembly.detail
		return result, nil
	}

	// Verify. The assembly is bytes until this passes.
	got, err := staged.Digest()
	if err != nil {
		return result, err
	}
	if !got.Equal(h) {
		result.Outcome = OutcomeAssemblyMismatch
		result.Detail = fmt.Sprintf("the reconstruction hashes to %s, not %s — every chunk verified "+
			"individually, so the manifest describes different bytes than the blob's name does; "+
			"nothing was published and the original is untouched", got, h)
		return result, nil
	}

	// Quarantine before publishing, so a crash between the two loses the
	// repair rather than the evidence. Only when the original is still
	// addressable: when the repair is reading from an artefact the checker
	// already quarantined, the evidence is already preserved and there is
	// nothing at the blob path to move.
	if !src.quarantined {
		path, qErr := r.opts.Store.Quarantine(h)
		if qErr != nil {
			return result, qErr
		}
		result.QuarantinePath = path
	} else {
		result.QuarantinePath = src.path
	}

	if _, err := staged.Publish(h); err != nil {
		return result, err
	}

	// The transition, without a new event type. A repaired blob is
	// indistinguishable from a freshly replicated one — same bytes, same name
	// — so the state change is the one the catalog already emits for a replica
	// that has come back: replica.present, via MarkVerified (Invariant 7,
	// ADR-0009, ADR-0036). A repair-specific event would fire once per blob
	// and tell a subscriber nothing replica.present does not.
	if err := r.opts.Catalog.MarkVerified(ctx, h, r.opts.Clock.Now()); err != nil {
		return result, err
	}

	result.Outcome = OutcomeRepaired
	result.Detail = fmt.Sprintf("replaced %d of %d chunks (%d bytes fetched of %d); "+
		"the damaged original is in quarantine",
		result.ChunksFetched, result.ChunksTotal, result.BytesFetched, result.BlobSize)
	r.opts.Logger.Info("blob repaired",
		"hash", h.String(),
		"chunks_total", result.ChunksTotal,
		"chunks_fetched", result.ChunksFetched,
		"bytes_fetched", result.BytesFetched,
		"quarantine", result.QuarantinePath,
	)
	return result, nil
}

// assemble writes the whole replacement into the staging file, chunk by chunk
// in index order.
//
// Intact chunks are copied from the local bytes; damaged ones are fetched and
// verified against the manifest digest that names them before a single byte of
// them is written. A chunk that does not verify abandons the repair with
// nothing published — which is the difference between this and a repairer that
// writes a peer's corruption over a file that was still recoverable.
//
// It streams: one chunk is in memory at a time, bounded by the chunker's Max.
func (r *Repairer) assemble(
	ctx context.Context, staged *cas.Staged, src *localBytes, m manifests.Manifest, damaged map[int]bool,
) (assembly, error) {
	var a assembly
	buf := make([]byte, 0, m.Params.Max)
	for i, chunk := range m.Chunks {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return a, ctxErr
		}
		if !damaged[i] {
			n, readErr := src.readChunk(chunk, &buf)
			a.bytesRead += n
			if readErr != nil {
				return a, readErr
			}
			if _, wErr := staged.Write(buf); wErr != nil {
				return a, wErr
			}
			continue
		}

		got, fErr := r.opts.Source.FetchChunk(ctx, m.BlobHash, chunk)
		if errors.Is(fErr, ErrNoSource) {
			a.outcome = OutcomeUnreachable
			a.detail = fmt.Sprintf(
				"no reachable peer holds %s, so chunk %d of %d could not be replaced; the damaged "+
					"bytes are untouched, which is an ordinary outcome and not a fault (ADR-0038)",
				m.BlobHash, i, len(m.Chunks))
			return a, nil
		}
		if fErr != nil {
			return a, fmt.Errorf("integrity: fetching chunk %d of %s: %w", i, m.BlobHash, fErr)
		}
		a.fetched++
		a.bytesFetched += int64(len(got))

		// The digest before the write, every time. A peer is a source of
		// bytes and never a source of truth (Invariant 1, ADR-0035).
		if int64(len(got)) != chunk.Length {
			a.outcome = OutcomeSourceCorrupt
			a.detail = fmt.Sprintf(
				"the peer served %d bytes for chunk %d of %s, which the manifest says is %d bytes; "+
					"the repair was abandoned and the local bytes are untouched",
				len(got), i, m.BlobHash, chunk.Length)
			return a, nil
		}
		if h := sumBytes(got); !h.Equal(chunk.Digest) {
			a.outcome = OutcomeSourceCorrupt
			a.detail = fmt.Sprintf(
				"chunk %d of %s came back hashing to %s, not %s — the peer's copy is damaged too; "+
					"the repair was abandoned and the local bytes are untouched",
				i, m.BlobHash, h, chunk.Digest)
			return a, nil
		}
		if _, wErr := staged.Write(got); wErr != nil {
			return a, wErr
		}
	}
	return a, nil
}

// assembly is what one reconstruction cost and, if it stopped early, why.
type assembly struct {
	fetched      int
	bytesFetched int64
	bytesRead    int64
	// outcome is empty when the reconstruction completed. It is never
	// OutcomeRepaired: assembling is not publishing, and only Repair decides
	// that.
	outcome Outcome
	detail  string
}

// sumBytes is the digest of one chunk's bytes.
func sumBytes(p []byte) hashing.Hash {
	h := hashing.New()
	_, _ = h.Write(p)
	return h.Sum()
}

// localBytes is the local copy a repair reads its intact chunks from.
type localBytes struct {
	rc   cas.ReadSeekCloser
	size int64
	// quarantined is true when these bytes are already preserved as evidence
	// — the checker quarantined them when it found the damage — and so must
	// not be quarantined a second time.
	quarantined bool
	path        string
}

// readChunk reads one chunk's bytes into *buf, which is reused across chunks so
// a 20 GB repair does not allocate per chunk.
func (l *localBytes) readChunk(chunk chunking.Chunk, buf *[]byte) (int64, error) {
	if cap(*buf) < int(chunk.Length) {
		*buf = make([]byte, chunk.Length)
	}
	*buf = (*buf)[:chunk.Length]
	if _, err := l.rc.Seek(chunk.Offset, io.SeekStart); err != nil {
		return 0, fmt.Errorf("integrity: seeking to chunk at %d: %w", chunk.Offset, err)
	}
	n, err := io.ReadFull(l.rc, *buf)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			// Short: the file is truncated here. Not an error — it is the
			// damage, and locateDamage is what decides what to do about it.
			*buf = (*buf)[:n]
			return int64(n), errShortChunk
		}
		return int64(n), fmt.Errorf("integrity: reading chunk at %d: %w", chunk.Offset, err)
	}
	return int64(n), nil
}

// errShortChunk is a chunk that ran off the end of a truncated file.
var errShortChunk = errors.New("integrity: the local bytes end inside this chunk")

// localBytes finds something to repair from: the addressable blob first, then
// the most recent quarantined artefact of it.
//
// Quarantine second and not instead: a blob the checker has already quarantined
// (ADR-0018) is both the evidence and the only local source of the parts that
// are still intact, and a repair that ignored it would refetch a 20 GB blob to
// replace 64 KiB. It returns nil when there is nothing here at all, which is an
// answer and not an error.
func (r *Repairer) localBytes(ctx context.Context, h hashing.Hash) (*localBytes, error) {
	has, err := r.opts.Store.Has(ctx, h)
	if err != nil {
		return nil, err
	}
	if has {
		rc, desc, oErr := r.opts.Store.Open(ctx, h)
		if oErr != nil {
			if errors.Is(oErr, cas.ErrNotFound) {
				return nil, nil
			}
			return nil, oErr
		}
		return &localBytes{rc: rc, size: desc.Size}, nil
	}

	quarantined, err := r.opts.Store.QuarantinedBlobs()
	if err != nil {
		return nil, err
	}
	var newest *cas.Quarantined
	for i := range quarantined {
		if !quarantined[i].Hash.Equal(h) {
			continue
		}
		if newest == nil || quarantined[i].QuarantinedAt.After(newest.QuarantinedAt) {
			newest = &quarantined[i]
		}
	}
	if newest == nil {
		return nil, nil
	}
	rc, err := r.opts.Store.OpenQuarantined(newest.Name)
	if err != nil {
		if errors.Is(err, cas.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &localBytes{rc: rc, size: newest.Size, quarantined: true, path: newest.Name}, nil
}

// locateDamage reads the local bytes against the manifest and reports which
// chunks are not what the manifest says they are.
//
// This is the local half of "the fetch is proportional to the damage": without
// it the only honest repair is to refetch everything, because a whole-object
// mismatch says the bytes are wrong and nothing about where.
func locateDamage(ctx context.Context, src *localBytes, m manifests.Manifest) (map[int]bool, int64, error) {
	damaged := map[int]bool{}
	var read int64
	buf := make([]byte, 0, m.Params.Max)
	for i, chunk := range m.Chunks {
		if err := ctx.Err(); err != nil {
			return nil, read, err
		}
		n, err := src.readChunk(chunk, &buf)
		read += n
		switch {
		case errors.Is(err, errShortChunk):
			damaged[i] = true
			continue
		case err != nil:
			return nil, read, err
		}
		if !sumBytes(buf).Equal(chunk.Digest) {
			damaged[i] = true
		}
	}
	return damaged, read, nil
}
