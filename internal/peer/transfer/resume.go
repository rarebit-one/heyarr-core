package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/chunking"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

// Resumable, reusing transfer (§15, §20, §84, ADR-0034, ADR-0035, M5-06, M5-07).
//
// # The resume unit is a chunk, never an offset
//
// [Puller.PullChunked] is the whole of the milestone's byte-moving half, and
// the shape it deliberately is not is the download manager's: record how many
// bytes arrived, reopen with a Range header, carry on. That shape retires
// invariant 1, because a whole-object BLAKE3 cannot be continued across a
// process restart without persisting hasher state, and a persisted hasher
// state is a serialised intermediate that nothing verifies (ADR-0035).
//
// So the unit is a chunk from a manifest this node fetched and verified
// ITSELF, and the sequence is:
//
//  1. re-read the partial staging file and re-hash each chunk it should
//     contain against the manifest, keeping only the contiguous verified
//     prefix and truncating at the first mismatch;
//  2. decide, for every remaining chunk, whether this node already holds those
//     bytes somewhere else — [manifests.PlanFor], pure and elsewhere;
//  3. carry the plan out in index order, re-hashing every chunk against the
//     manifest as it lands, whether it came off this disk or off the wire;
//  4. hash the ASSEMBLED file whole against the digest the caller asked for,
//     and publish only then.
//
// Step 4 is not redundant with step 3. A set of individually valid chunks
// assembled in the wrong order is a set of valid chunks and the wrong file,
// and the whole-object hash is the only thing that detects it (ADR-0034).
//
// # What it costs, and the 19-of-20-GB case
//
// The prefix scan is a local sequential read of everything kept, so a transfer
// interrupted at 19 GB of 20 re-reads 19 GB from its own disk before it asks
// the source for anything. That is the honest trade ADR-0035 records: resume
// saves NETWORK and spends DISK, and on the links this exists for — slow,
// flaky, metered — that is the trade worth making. The whole-object
// verification adds a second sequential read of the same file, which every
// receive in this system already pays.
//
// It is bounded, and the bound is what makes a crash loop diagnosable: a
// destination that keeps dying re-verifies its kept chunks each time and makes
// no progress past the chunk that is failing, visibly, and the work lost per
// crash is one chunk rather than one blob.
//
// # Reuse makes transfers cheaper and storage no smaller
//
// A chunk this node already holds is read out of the blob that holds it and
// re-verified against the manifest before a byte of it is used — the index is
// a claim about this disk, and claims and bytes diverge, which is the premise
// of the whole milestone. Two blobs sharing every chunk remain two blobs, both
// stored, both addressable (ADR-0034): nothing here consults a chunk list to
// answer "is this the same blob", and the only question this file asks of a
// chunk is "are these the bytes the manifest names", never "which blob is
// this".

// Refusals the chunked path makes.
var (
	// ErrManifestMismatch is a manifest that does not describe the blob this
	// transfer is for, or does not describe a whole blob.
	ErrManifestMismatch = errors.New("transfer: this manifest does not describe the blob being pulled")
	// ErrRangeRefused is a source that answered a ranged read with something
	// other than 206.
	//
	// Refused rather than accommodated, and 200 is the reason: a source that
	// ignores the Range header answers the whole blob, and the first chunk of
	// a blob is a prefix of the whole blob — so a chunk-0 read would VERIFY
	// against its manifest digest and every later chunk would land on bytes
	// nobody asked for. Requiring 206 is what makes that impossible rather
	// than merely unlikely.
	ErrRangeRefused = errors.New("transfer: the source did not answer a ranged read with 206")
	// ErrChunkCorrupt is a chunk that arrived from the source and does not
	// hash to what the manifest says it should.
	ErrChunkCorrupt = errors.New("transfer: a chunk this source served is not what the manifest names")
)

// Index is the local chunk index a reusing transfer consults (M5-03, M5-07).
//
// Narrow to the one question this package asks: where, if anywhere, does this
// node already hold these bytes. It is optional — a Puller built without one
// resumes but never reuses, which is exactly M5-06 without M5-07 — and its
// answer is a claim that is re-verified against the manifest before use.
type Index interface {
	Locate(ctx context.Context, digest hashing.Hash) ([]manifests.LocalChunk, error)
}

// Mode is how a completed transfer moved its bytes.
//
// An enum rather than a boolean because the acceptance conditions are about
// which PATH ran, and "it succeeded" is satisfied by either one. A test that
// infers the whole-pull path from a successful transfer is a test that would
// keep passing after the fallback stopped existing.
type Mode string

const (
	// ModeWhole — the M4 path: one unranged read, verified as it streams,
	// published by cas.PutExpecting. What a blob with no manifest gets, and
	// what a blob with no manifest must keep getting (§16, ADR-0035).
	ModeWhole Mode = "whole"
	// ModeChunked — the M5 path: verified prefix, planned reuse, ranged
	// fetches, whole-object verification, publish.
	ModeChunked Mode = "chunked"
)

// Valid reports whether m is one of the two.
func (m Mode) Valid() bool { return m == ModeWhole || m == ModeChunked }

func (m Mode) String() string { return string(m) }

// PullChunked resumes, reuses and completes one blob against one source.
//
// m must be a manifest this node verified itself — [Puller.FetchManifest]'s
// output, or one read back out of this node's own store. Nothing here trusts
// it because it arrived: it is checked against expected and revalidated before
// a byte moves, because a manifest is the description every subsequent
// decision is taken from.
//
// It is safe to call repeatedly for the same blob. That is the point: the
// first call may be killed at any instant, and the second re-derives what is
// true from the bytes on disk rather than from anything the first left behind
// (invariant 9).
func (p *Puller) PullChunked(
	ctx context.Context, src replication.Source, expected hashing.Hash, m manifests.Manifest,
) (Outcome, error) {
	if !m.BlobHash.Equal(expected) {
		return Outcome{}, fmt.Errorf("%w: pulling %s with a manifest for %s",
			ErrManifestMismatch, expected, m.BlobHash)
	}
	if err := m.Validate(); err != nil {
		return Outcome{}, fmt.Errorf("%w: %w", ErrManifestMismatch, err)
	}
	if len(m.Chunks) == 0 {
		// A manifest with no chunks describes no bytes. It cannot be the
		// description of a blob worth transferring, and treating it as "0% to
		// fetch" would publish an empty file under a digest that is not the
		// empty file's.
		return Outcome{}, fmt.Errorf("%w: the manifest for %s has no chunks", ErrManifestMismatch, expected)
	}

	origin, err := p.originFor(src)
	if err != nil {
		return Outcome{}, err
	}
	client, err := p.clientFor(src)
	if err != nil {
		return Outcome{}, err
	}

	partial, err := p.store.OpenPartial(ctx, expected)
	if err != nil {
		return Outcome{}, err
	}
	// Closed rather than discarded on every failure path: the bytes that did
	// land are the next attempt's verified prefix, and the reaper takes them
	// by age if there is no next attempt (ADR-0035).
	published := false
	defer func() {
		if !published {
			_ = partial.Close()
		}
	}()

	kept, err := p.verifiedPrefix(ctx, partial, m)
	if err != nil {
		return Outcome{}, err
	}

	held, err := p.locate(ctx, m, kept)
	if err != nil {
		return Outcome{}, err
	}
	plan := manifests.PlanFor(m, kept, held)
	if err := plan.Validate(m); err != nil {
		return Outcome{}, err
	}

	out := Outcome{SourcePeerID: src.PeerID, Mode: ModeChunked}
	for _, entry := range plan.Entries {
		if err := ctx.Err(); err != nil {
			return Outcome{}, err
		}
		switch entry.Availability {
		case manifests.AvailabilityKept:
			out.ChunksKept++
			out.BytesKept += entry.Chunk.Length
			continue
		case manifests.AvailabilityLocal:
			ok, err := p.appendLocal(ctx, partial, entry)
			if err != nil {
				return Outcome{}, err
			}
			if ok {
				out.ChunksReused++
				out.BytesReused += entry.Chunk.Length
				continue
			}
			// The index was a claim and the bytes disagreed. Fetching is the
			// answer, and it is the ordinary answer — an index entry going
			// stale is what happens when a blob is re-materialised or damaged
			// underneath it.
			out.ChunksIndexStale++
			fallthrough
		case manifests.AvailabilityFetch:
			if err := p.fetchChunk(ctx, client, origin, partial, expected, entry.Chunk); err != nil {
				return Outcome{}, err
			}
			out.ChunksFetched++
			out.BytesFetched += entry.Chunk.Length
		default:
			return Outcome{}, fmt.Errorf("transfer: chunk %d of %s has availability %q",
				entry.Index, expected, entry.Availability)
		}
	}

	// The whole-object verification, and the only publication on this path.
	// Everything above proved things about pieces; this proves the thing
	// invariant 1 is about (ADR-0034).
	desc, err := partial.Publish(ctx, expected)
	if err != nil {
		return Outcome{}, err
	}
	published = true
	out.Bytes = desc.Size
	out.Deduplicated = desc.Deduplicated

	p.log.Info("assembled a blob from chunks",
		"blob_hash", expected.String(), "source_peer_id", src.PeerID,
		"source_peer_name", src.Name, "bytes", desc.Size,
		"chunks_kept", out.ChunksKept, "bytes_kept", out.BytesKept,
		"chunks_reused", out.ChunksReused, "bytes_reused", out.BytesReused,
		"chunks_fetched", out.ChunksFetched, "bytes_fetched", out.BytesFetched,
		"chunks_index_stale", out.ChunksIndexStale, "deduplicated", desc.Deduplicated)
	return out, nil
}

// verifiedPrefix re-hashes what a previous attempt left and reports how many
// leading chunks survived.
//
// This is ADR-0035's rule as a loop. Every chunk the staging file is long
// enough to contain is read back and hashed against the manifest entry that
// names it; the first one that does not match ends the prefix, and the file is
// truncated to that boundary so nothing beyond it can be mistaken for received
// data later. A file LONGER than the prefix — the truncated-and-extended case,
// where plausible bytes were appended so the length lies — loses everything
// past the last verified chunk for exactly the same reason: length is not
// evidence, so it is not consulted except as a bound on what can be read.
//
// Nothing is quarantined here. A partial is not a blob and was never
// addressable, so bytes that failed are not evidence of a source serving the
// wrong thing — they are one of a killed process, a torn write, or somebody
// editing a staging file, and all three are answered by discarding them.
func (p *Puller) verifiedPrefix(
	ctx context.Context, partial cas.Partial, m manifests.Manifest,
) (int, error) {
	size := partial.Size()
	if size == 0 {
		return 0, nil
	}
	kept := 0
	var boundary int64
	for _, c := range m.Chunks {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if c.End() > size {
			// The staging file stops inside this chunk. A partially received
			// chunk is not a chunk: there is nothing to hash it against.
			break
		}
		got, err := hashRange(ctx, partial, c.Offset, c.Length)
		if err != nil {
			return 0, err
		}
		if !got.Equal(c.Digest) {
			break
		}
		kept++
		boundary = c.End()
	}
	if boundary != size {
		if err := partial.Truncate(boundary); err != nil {
			return 0, err
		}
		p.log.Info("discarded the unverifiable tail of a partial transfer",
			"blob_hash", m.BlobHash.String(), "was_bytes", size, "kept_bytes", boundary,
			"kept_chunks", kept, "chunk_count", m.ChunkCount())
	}
	return kept, nil
}

// hashRange hashes one range of a partial without holding it in memory.
func hashRange(ctx context.Context, ra io.ReaderAt, off, length int64) (hashing.Hash, error) {
	section := io.NewSectionReader(ra, off, length)
	h, n, err := hashing.HashReader(&ctxReader{ctx: ctx, r: section})
	if err != nil {
		return hashing.Hash{}, fmt.Errorf("transfer: re-reading %d bytes at %d of a partial "+
			"transfer: %w", length, off, err)
	}
	if n != length {
		// Short read with no error: the file is not as long as the caller
		// established it was, which means something else is writing to it.
		return hashing.Hash{}, fmt.Errorf("transfer: a partial transfer's staging file gave %d "+
			"bytes where %d were expected at offset %d", n, length, off)
	}
	return h, nil
}

// locate gathers this node's chunk index for the chunks still to come.
//
// One lookup per DISTINCT digest, not per chunk: a blob whose content repeats
// — and repetitive content is precisely what chunk reuse is good at — would
// otherwise ask the same question thousands of times.
func (p *Puller) locate(
	ctx context.Context, m manifests.Manifest, kept int,
) (map[hashing.Hash][]manifests.LocalChunk, error) {
	if p.index == nil {
		return nil, nil
	}
	held := make(map[hashing.Hash][]manifests.LocalChunk)
	for i := kept; i < len(m.Chunks); i++ {
		d := m.Chunks[i].Digest
		if _, done := held[d]; done {
			continue
		}
		where, err := p.index.Locate(ctx, d)
		if err != nil {
			return nil, fmt.Errorf("transfer: consulting this node's chunk index for %s: %w", d, err)
		}
		held[d] = where
	}
	return held, nil
}

// appendLocal reads a chunk out of a blob this node already holds, verifies it
// against the manifest, and appends it.
//
// It reports whether the bytes were usable. false is not an error: it is the
// index having been a claim, and the caller's answer is to fetch the chunk
// from the source instead. The bytes are written and then checked rather than
// buffered and then written, so memory stays flat whatever a manifest claims a
// chunk's length is; a chunk that fails is truncated away before anything else
// is appended.
//
// A donor whose bytes do not match gets the existing integrity path pointed at
// it — cas.Verify, which re-reads the blob and quarantines it if it no longer
// hashes to its own name (ADR-0018). That is what tells a stale index entry
// apart from a damaged blob, and neither is diagnosable from here without
// asking.
func (p *Puller) appendLocal(
	ctx context.Context, partial cas.Partial, entry manifests.PlanEntry,
) (bool, error) {
	before := partial.Size()
	rc, desc, err := p.store.Open(ctx, entry.Donor.BlobHash)
	if err != nil {
		if errors.Is(err, cas.ErrNotFound) {
			// The index names a blob this node no longer holds. Ordinary: a
			// deletion and a garbage collection both produce it.
			p.log.Debug("the chunk index names a blob this node no longer holds",
				"chunk_digest", entry.Chunk.Digest.String(),
				"donor_blob_hash", entry.Donor.BlobHash.String())
			return false, nil
		}
		return false, err
	}
	defer func() { _ = rc.Close() }()

	if entry.Donor.Offset+entry.Chunk.Length > desc.Size {
		p.log.Debug("the chunk index points past the end of the blob it names",
			"chunk_digest", entry.Chunk.Digest.String(),
			"donor_blob_hash", entry.Donor.BlobHash.String(),
			"donor_offset", entry.Donor.Offset, "blob_bytes", desc.Size)
		return false, nil
	}
	if _, err := rc.Seek(entry.Donor.Offset, io.SeekStart); err != nil {
		return false, fmt.Errorf("transfer: seeking to %d in %s: %w",
			entry.Donor.Offset, entry.Donor.BlobHash, err)
	}

	verifier := hashing.NewVerifyingReader(io.LimitReader(rc, entry.Chunk.Length), entry.Chunk.Digest)
	n, err := partial.Append(ctx, verifier)
	if err != nil {
		return false, err
	}
	if n != entry.Chunk.Length || verifier.Check() != nil {
		if err := partial.Truncate(before); err != nil {
			return false, err
		}
		p.log.Warn("a locally held chunk did not match the manifest and will be fetched",
			"chunk_digest", entry.Chunk.Digest.String(),
			"donor_blob_hash", entry.Donor.BlobHash.String(), "donor_offset", entry.Donor.Offset)
		p.reportDonor(ctx, entry.Donor.BlobHash)
		return false, nil
	}
	return true, nil
}

// reportDonor asks the store whether a donor blob still hashes to its own
// name, so that a damaged blob reaches quarantine on the path that already
// exists for it and a merely stale index entry does not.
func (p *Puller) reportDonor(ctx context.Context, blob hashing.Hash) {
	err := p.store.Verify(ctx, blob)
	switch {
	case err == nil:
		// The blob is intact, so the index entry was stale — the offsets moved
		// under it, or it was written for a different chunking. Nothing to
		// preserve and nothing to quarantine.
		p.log.Info("a chunk index entry is stale: the blob it names is intact and does not hold "+
			"those bytes at that offset", "donor_blob_hash", blob.String())
	case errors.Is(err, cas.ErrCorrupt):
		var corrupt *cas.Corruption
		if errors.As(err, &corrupt) {
			p.log.Warn("a blob this node holds is damaged and was quarantined while looking for a "+
				"chunk in it", "donor_blob_hash", blob.String(),
				"actual", corrupt.Actual.String(), "quarantined_at", corrupt.Path)
		}
	default:
		p.log.Warn("could not check the blob a chunk index entry names",
			"donor_blob_hash", blob.String(), "error", err)
	}
}

// fetchChunk reads one chunk's byte range from the source and appends it.
//
// This is the Range header the M4 package doc forbade, and it is here under
// the condition that made it forbidden: the range is a CHUNK BOUNDARY out of a
// manifest this node verified, and what arrives is hashed against that
// manifest's digest for that chunk before it counts as received. A range
// derived from "how much do I already have" would be the thing ADR-0035
// refuses, and nothing here can compute one — the loop's only input is the
// manifest.
func (p *Puller) fetchChunk(
	ctx context.Context, client *http.Client, origin string, partial cas.Partial,
	blob hashing.Hash, c chunking.Chunk,
) error {
	before := partial.Size()
	url := origin + peerapi.BlobContentPath(blob.String())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("transfer: building a ranged read of %s: %w", blob, err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", c.Offset, c.End()-1))

	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, ErrRedirected) {
			return fmt.Errorf("%w: reading bytes %d-%d of %s", ErrRedirected, c.Offset, c.End()-1, blob)
		}
		return fmt.Errorf("transfer: reading bytes %d-%d of %s: %w", c.Offset, c.End()-1, blob, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusPartialContent:
	case http.StatusNotFound:
		return fmt.Errorf("%w: the source answered 404 part-way through %s", ErrSourceLacksBlob, blob)
	default:
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, maxRefusalBody))
		return fmt.Errorf("%w: %d for bytes %d-%d of %s: %s", ErrRangeRefused,
			resp.StatusCode, c.Offset, c.End()-1, blob, strings.TrimSpace(string(detail)))
	}

	// Limited to the chunk's length regardless of what the source sends, and
	// verified against the manifest's digest for THIS chunk. A source that
	// answered 206 and then streamed something else gets its bytes truncated
	// away rather than assembled.
	verifier := hashing.NewVerifyingReader(io.LimitReader(resp.Body, c.Length), c.Digest)
	n, err := partial.Append(ctx, verifier)
	if err != nil {
		return err
	}
	if n != c.Length {
		if tErr := partial.Truncate(before); tErr != nil {
			return tErr
		}
		return fmt.Errorf("%w: %s served %d bytes for a chunk of %d at offset %d",
			ErrChunkCorrupt, blob, n, c.Length, c.Offset)
	}
	if err := verifier.Check(); err != nil {
		if tErr := partial.Truncate(before); tErr != nil {
			return tErr
		}
		return fmt.Errorf("%w: %s, chunk at %d: %w", ErrChunkCorrupt, blob, c.Offset, err)
	}

	// The response is read to its END before it is closed, and that is not
	// tidiness.
	//
	// A chunk fetch reads exactly the range it asked for, so the reader above
	// stops on a LIMIT rather than on the end of the body. Closing a body that
	// has not reached its end resets the stream on HTTP/2 and forfeits the
	// connection on HTTP/1.1 — and this path makes one request per chunk,
	// which is on the order of eighteen thousand of them for a 20 GB blob
	// (see chunking's default parameters). Ending each one cleanly is the
	// difference between reusing one connection and renegotiating thousands,
	// and it is why a source's accounting of what it sent agrees with what
	// arrived instead of being truncated by a reset it did not expect.
	//
	// It is bounded to one byte, and a source that sent MORE than the range it
	// was asked for is refused rather than tolerated. An over-long 206 is a
	// source answering a question it was not asked, and the extra bytes are
	// exactly what the next chunk's offset would otherwise land on.
	if extra, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, 1)); extra > 0 {
		if tErr := partial.Truncate(before); tErr != nil {
			return tErr
		}
		return fmt.Errorf("%w: %s sent more than the %d bytes at offset %d that it was asked for",
			ErrChunkCorrupt, blob, c.Length, c.Offset)
	}
	return nil
}

// ctxReader makes a long local read cancellable, the way the CAS's own does.
// Re-hashing a 19 GB verified prefix should stop when the job's lease is lost
// (ADR-0008), not run to completion first.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
