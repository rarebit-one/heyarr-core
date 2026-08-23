package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/chunking"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

// Chunk manifests, the local chunk index, and §16's three-state question
// (M5-03, ADR-0034).
//
// # This file is the implementation of a port the fabric declares
//
// internal/storagefabric/manifests declares [manifests.Store] and
// [manifests.Index]; this satisfies them against SQLite. The dependency runs
// fabric <- persistence, never the other way, and the content domain does not
// appear in either direction: internal/domain has no reason to know chunks
// exist and depguard makes sure it does not learn (Invariant 2).
//
// # The rule the whole file is arranged around
//
// Asking whether a blob has a manifest must never generate one. Every read
// here goes to the reader pool, writes nothing and enqueues nothing.
// [Catalog.ChunkManifestState] is the sharpest case: it answers "undecided"
// and stops, because the alternative — producing a manifest so it can answer
// "present" — is the exact convenience ADR-0034 exists to forbid.

// ErrManifestBlobUnknown is returned when a manifest is written for a blob the
// catalog has never seen.
//
// Typed, because a manifest is keyed BY the blob's identity (ADR-0034) and a
// manifest for bytes nothing records is a manifest that can never be reached.
// The foreign key would refuse it anyway; this says why.
var ErrManifestBlobUnknown = errors.New("catalog: no blob is recorded with that hash")

var _ manifests.Store = (*Catalog)(nil)

var _ manifests.Index = (*Catalog)(nil)

// SaveChunkManifest writes a manifest, replacing whatever was held for the
// blob (§15, §16).
//
// Idempotent: the chunk rows are deleted and rewritten inside one transaction,
// so a re-run of the job that produced it converges rather than accumulating.
// Invariant 9 — this handler will be re-run.
//
// The manifest is validated BEFORE it is stored, digest included, so a
// malformed or mis-digested manifest never reaches the table a reader trusts.
func (c *Catalog) SaveChunkManifest(ctx context.Context, m manifests.Manifest) error {
	if err := m.Validate(); err != nil {
		return fmt.Errorf("catalog: refusing to store a manifest that does not check out: %w", err)
	}
	return c.db.InTx(ctx, func(tx *sql.Tx) error {
		var known int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM blobs WHERE hash = ?`, m.BlobHash.String()).Scan(&known); err != nil {
			return err
		}
		if known == 0 {
			return fmt.Errorf("%w: %s", ErrManifestBlobUnknown, m.BlobHash)
		}

		generated := m.GeneratedAt
		if generated.IsZero() {
			generated = c.clock.Now()
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO chunk_manifests
				(blob_hash, algorithm, min_size, avg_size, max_size,
				 chunk_count, covered_size, digest, generated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (blob_hash) DO UPDATE SET
				algorithm = excluded.algorithm,
				min_size = excluded.min_size,
				avg_size = excluded.avg_size,
				max_size = excluded.max_size,
				chunk_count = excluded.chunk_count,
				covered_size = excluded.covered_size,
				digest = excluded.digest,
				generated_at = excluded.generated_at`,
			m.BlobHash.String(), m.Algorithm, m.Params.Min, m.Params.Avg, m.Params.Max,
			m.ChunkCount(), m.CoveredSize, m.Digest.String(),
			generated.UTC().Format(timestampFormat)); err != nil {
			return fmt.Errorf("catalog: storing the manifest for %s: %w", m.BlobHash, err)
		}

		// Replace rather than merge. A manifest recomputed under different
		// parameters shares no boundaries with the old one, so merging would
		// leave rows from two incomparable chunkings under one blob_hash and
		// the idx sequence would describe neither.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM manifest_chunks WHERE blob_hash = ?`, m.BlobHash.String()); err != nil {
			return err
		}
		for i, ch := range m.Chunks {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO manifest_chunks (blob_hash, idx, byte_offset, byte_length, digest)
				VALUES (?, ?, ?, ?, ?)`,
				m.BlobHash.String(), i, ch.Offset, ch.Length, ch.Digest.String()); err != nil {
				return fmt.Errorf("catalog: storing chunk %d of %s: %w", i, m.BlobHash, err)
			}
		}

		// A blob that has a manifest is not also exempt from having one. The
		// two facts are independent columns of one answer and letting both be
		// true would make the three states two-and-a-half.
		if _, err := tx.ExecContext(ctx,
			`UPDATE blobs SET chunking_exempt_reason = NULL, chunking_exempt_at = NULL
			 WHERE hash = ?`, m.BlobHash.String()); err != nil {
			return err
		}
		return nil
	})
}

// Save satisfies [manifests.Store].
func (c *Catalog) Save(ctx context.Context, m manifests.Manifest) error {
	return c.SaveChunkManifest(ctx, m)
}

// ChunkManifest reads a blob's manifest back, in index order, verified.
//
// # ORDER BY idx, and it is not decoration
//
// ADR-0034: "a set of individually valid chunks assembled in the wrong order
// is a set of valid chunks and the wrong file." SQLite is free to return rows
// in any order it likes; the primary key happens to make idx order likely,
// which is worse than it being unlikely — it means an unordered read passes in
// development and corrupts a reassembly under a different query plan.
//
// The digest is recomputed over what came back and compared with what was
// stored, so a tampered manifest_chunks row is caught here rather than at
// reassembly.
//
// found is false when the blob has no manifest. That is an ordinary answer.
// Nothing is generated.
func (c *Catalog) ChunkManifest(
	ctx context.Context, blob hashing.Hash,
) (manifests.Manifest, bool, error) {
	var (
		m        manifests.Manifest
		digest   string
		gen      string
		count    int
		hashText = blob.String()
	)
	err := c.db.Reader().QueryRowContext(ctx, `
		SELECT algorithm, min_size, avg_size, max_size, chunk_count, covered_size,
		       digest, generated_at
		FROM chunk_manifests WHERE blob_hash = ?`, hashText).
		Scan(&m.Algorithm, &m.Params.Min, &m.Params.Avg, &m.Params.Max,
			&count, &m.CoveredSize, &digest, &gen)
	if errors.Is(err, sql.ErrNoRows) {
		return manifests.Manifest{}, false, nil
	}
	if err != nil {
		return manifests.Manifest{}, false, fmt.Errorf("catalog: reading the manifest for %s: %w", blob, err)
	}
	m.BlobHash = blob
	if m.Digest, err = hashing.Parse(digest); err != nil {
		return manifests.Manifest{}, false, fmt.Errorf("catalog: manifest digest for %s: %w", blob, err)
	}
	if t, err := time.Parse(timestampFormat, gen); err == nil {
		m.GeneratedAt = t.UTC()
	}

	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT idx, byte_offset, byte_length, digest
		FROM manifest_chunks WHERE blob_hash = ?
		ORDER BY idx`, hashText)
	if err != nil {
		return manifests.Manifest{}, false, fmt.Errorf("catalog: reading the chunks of %s: %w", blob, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			idx        int
			ch         chunking.Chunk
			chunkHash  string
			wantedNext = len(m.Chunks)
		)
		if err := rows.Scan(&idx, &ch.Offset, &ch.Length, &chunkHash); err != nil {
			return manifests.Manifest{}, false, err
		}
		// The stored index is asserted against the position it came back in.
		// Without this the ORDER BY is the only thing making the sequence
		// right, and a missing row would shift every chunk after it while
		// still reassembling something.
		if idx != wantedNext {
			return manifests.Manifest{}, false, fmt.Errorf(
				"%w: manifest for %s: chunk index %d arrived at position %d — the sequence has a hole",
				manifests.ErrMalformed, blob, idx, wantedNext)
		}
		if ch.Digest, err = hashing.Parse(chunkHash); err != nil {
			return manifests.Manifest{}, false, fmt.Errorf("catalog: chunk %d of %s: %w", idx, blob, err)
		}
		m.Chunks = append(m.Chunks, ch)
	}
	if err := rows.Err(); err != nil {
		return manifests.Manifest{}, false, err
	}
	if count != len(m.Chunks) {
		return manifests.Manifest{}, false, fmt.Errorf(
			"%w: manifest for %s records %d chunks, %d rows came back",
			manifests.ErrMalformed, blob, count, len(m.Chunks))
	}
	if err := m.Validate(); err != nil {
		return manifests.Manifest{}, false, err
	}
	return m, true, nil
}

// Load satisfies [manifests.Store].
func (c *Catalog) Load(ctx context.Context, blob hashing.Hash) (manifests.Manifest, bool, error) {
	return c.ChunkManifest(ctx, blob)
}

// ChunkManifestState answers §16's question for one blob, in ONE read.
//
// # It generates nothing
//
// This is a SELECT on the reader pool. It creates no manifest, records no
// decision and enqueues no job — a `chunk_blob` job appearing as a consequence
// of somebody asking this question would make the question unaskable, because
// asking it is exactly what a caller does when it is trying to decide whether
// the work is worth doing. ADR-0034 states the rule; the test named
// TestAskingForTheStateGeneratesNothing is what holds it.
//
// # The state is derived, not stored
//
// 'present' is the existence of a chunk_manifests row and nothing else, so
// deleting every manifest — which ADR-0034 makes a supported recovery action —
// moves those blobs back to 'undecided' automatically. A stored enum would
// keep saying 'present' after the recovery and would be the only record that
// disagreed with the tables.
func (c *Catalog) ChunkManifestState(
	ctx context.Context, blob hashing.Hash,
) (manifests.State, error) {
	states, err := c.chunkManifestStates(ctx, blob.String())
	if err != nil {
		return "", err
	}
	s, ok := states[blob.String()]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrManifestBlobUnknown, blob)
	}
	return s, nil
}

// StateOf satisfies [manifests.Store].
func (c *Catalog) StateOf(ctx context.Context, blob hashing.Hash) (manifests.State, error) {
	return c.ChunkManifestState(ctx, blob)
}

// ChunkManifestStates answers the same question for the whole catalog, in one
// read, so that a listing does not become one query per blob.
func (c *Catalog) ChunkManifestStates(ctx context.Context) (map[string]manifests.State, error) {
	return c.chunkManifestStates(ctx)
}

// chunkManifestStateSQL is the one place the three-way answer is computed.
//
// One CASE, one LEFT JOIN, one pass. The order of the branches matters: a blob
// with a manifest is 'present' even if an exemption was recorded earlier, so
// that a stale decision can never hide bytes that have actually been chunked.
// SaveChunkManifest clears the exemption for the same reason, from the other
// side.
const chunkManifestStateSQL = `
	SELECT b.hash,
	       CASE
	           WHEN m.blob_hash IS NOT NULL          THEN 'present'
	           WHEN b.chunking_exempt_reason IS NOT NULL THEN 'not_required'
	           ELSE 'undecided'
	       END
	FROM blobs b
	LEFT JOIN chunk_manifests m ON m.blob_hash = b.hash`

func (c *Catalog) chunkManifestStates(
	ctx context.Context, only ...string,
) (map[string]manifests.State, error) {
	query := chunkManifestStateSQL
	var args []any
	if len(only) > 0 {
		clause, values := inList(only)
		query += " WHERE b.hash IN " + clause
		args = values
	}
	rows, err := c.db.Reader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading chunk-manifest state: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]manifests.State)
	for rows.Next() {
		var hash, state string
		if err := rows.Scan(&hash, &state); err != nil {
			return nil, err
		}
		s, err := manifests.ParseState(state)
		if err != nil {
			return nil, err
		}
		out[hash] = s
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// RecordChunkingNotRequired writes down the decision that a blob will never
// need a manifest (§16).
//
// This is the ONLY route to StateNotRequired. It is a decision somebody took,
// with a reason, and it is stored as such rather than inferred — an inference
// from "the blob is small" would be recomputed differently the day the
// threshold moves, and every blob's recorded history would change with it.
//
// Idempotent, and it refuses a blob that already has a manifest: "these bytes
// never need chunking" and "here are their chunks" cannot both be true, and
// silently accepting the second would put the two facts in a race.
func (c *Catalog) RecordChunkingNotRequired(
	ctx context.Context, blob hashing.Hash, reason string,
) error {
	if reason == "" {
		return errors.New("catalog: recording that a blob needs no manifest needs a reason — " +
			"a decision with no grounds cannot be reviewed when the threshold moves")
	}
	now := c.clock.Now().UTC().Format(timestampFormat)
	return c.db.InTx(ctx, func(tx *sql.Tx) error {
		var manifested int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM chunk_manifests WHERE blob_hash = ?`, blob.String()).
			Scan(&manifested); err != nil {
			return err
		}
		if manifested > 0 {
			return fmt.Errorf(
				"catalog: %s already has a chunk manifest, so it cannot also be recorded as never needing one",
				blob)
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE blobs SET chunking_exempt_reason = ?, chunking_exempt_at = ? WHERE hash = ?`,
			reason, now, blob.String())
		if err != nil {
			return fmt.Errorf("catalog: recording the chunking decision for %s: %w", blob, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: %s", ErrManifestBlobUnknown, blob)
		}
		return nil
	})
}

// RecordNotRequired satisfies [manifests.Store].
func (c *Catalog) RecordNotRequired(ctx context.Context, blob hashing.Hash, reason string) error {
	return c.RecordChunkingNotRequired(ctx, blob, reason)
}

// DiscardChunkManifest removes one blob's manifest.
//
// Supported rather than exceptional. ADR-0034 makes "delete every manifest" a
// legitimate recovery action and the cheapest possible answer to a suspected
// chunker bug, so the operation exists as a first-class method rather than as
// something an operator has to reach into SQLite to do.
//
// The blob returns to 'undecided', which is the truth: nobody has decided
// anything about it any more.
func (c *Catalog) DiscardChunkManifest(ctx context.Context, blob hashing.Hash) error {
	return c.db.InTx(ctx, func(tx *sql.Tx) error {
		// manifest_chunks cascades from chunk_manifests, so this is the whole
		// delete. The cascade is asserted rather than assumed — see
		// TestDeletingAManifestTakesItsChunksAndNothingElse.
		_, err := tx.ExecContext(ctx, `DELETE FROM chunk_manifests WHERE blob_hash = ?`, blob.String())
		return err
	})
}

// Discard satisfies [manifests.Store].
func (c *Catalog) Discard(ctx context.Context, blob hashing.Hash) error {
	return c.DiscardChunkManifest(ctx, blob)
}

// RecordLocal replaces this node's chunk-index entries for one blob.
//
// Replace rather than merge, for the same reason SaveChunkManifest replaces:
// re-chunking under different parameters produces different boundaries, and
// leaving the old rows would leave the index claiming bytes at offsets nothing
// cuts at any more — a reuse index whose entries point at nothing is worse
// than no index, because a transfer acts on it.
func (c *Catalog) RecordLocal(
	ctx context.Context, blob hashing.Hash, chunks []manifests.LocalChunk,
) error {
	now := c.clock.Now().UTC().Format(timestampFormat)
	return c.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM local_chunks WHERE blob_hash = ?`, blob.String()); err != nil {
			return err
		}
		for _, ch := range chunks {
			if ch.BlobHash.IsZero() {
				ch.BlobHash = blob
			}
			if !ch.BlobHash.Equal(blob) {
				return fmt.Errorf(
					"catalog: a local chunk index entry for %s claims to belong to %s",
					blob, ch.BlobHash)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO local_chunks (digest, blob_hash, byte_offset, byte_length, recorded_at)
				VALUES (?, ?, ?, ?, ?)
				ON CONFLICT (digest, blob_hash, byte_offset) DO UPDATE SET
					byte_length = excluded.byte_length,
					recorded_at = excluded.recorded_at`,
				ch.Digest.String(), ch.BlobHash.String(), ch.Offset, ch.Length, now); err != nil {
				return fmt.Errorf("catalog: indexing a local chunk of %s: %w", blob, err)
			}
		}
		return nil
	})
}

// Locate reports every place this node holds a chunk.
//
// # It returns places, never an identity
//
// The result is a list of (blob, offset, length), and the plural is the point:
// a chunk recurring in two blobs is the ordinary case chunk-level
// deduplication exists for, so "which blob is this chunk" has no answer and
// this method deliberately cannot be used to ask it. Identity is the
// whole-object BLAKE3 and Milestone 5 does not add a second (ADR-0034,
// ADR-0005).
func (c *Catalog) Locate(
	ctx context.Context, digest hashing.Hash,
) ([]manifests.LocalChunk, error) {
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT digest, blob_hash, byte_offset, byte_length
		FROM local_chunks WHERE digest = ?
		ORDER BY blob_hash, byte_offset`, digest.String())
	if err != nil {
		return nil, fmt.Errorf("catalog: locating chunk %s: %w", digest, err)
	}
	defer func() { _ = rows.Close() }()

	var out []manifests.LocalChunk
	for rows.Next() {
		var d, b string
		var lc manifests.LocalChunk
		if err := rows.Scan(&d, &b, &lc.Offset, &lc.Length); err != nil {
			return nil, err
		}
		if lc.Digest, err = hashing.Parse(d); err != nil {
			return nil, err
		}
		if lc.BlobHash, err = hashing.Parse(b); err != nil {
			return nil, err
		}
		out = append(out, lc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// LocalChunksOf lists this node's index entries for one blob, in offset order.
func (c *Catalog) LocalChunksOf(
	ctx context.Context, blob hashing.Hash,
) ([]manifests.LocalChunk, error) {
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT digest, blob_hash, byte_offset, byte_length
		FROM local_chunks WHERE blob_hash = ?
		ORDER BY byte_offset`, blob.String())
	if err != nil {
		return nil, fmt.Errorf("catalog: listing the local chunks of %s: %w", blob, err)
	}
	defer func() { _ = rows.Close() }()

	var out []manifests.LocalChunk
	for rows.Next() {
		var d, b string
		var lc manifests.LocalChunk
		if err := rows.Scan(&d, &b, &lc.Offset, &lc.Length); err != nil {
			return nil, err
		}
		if lc.Digest, err = hashing.Parse(d); err != nil {
			return nil, err
		}
		if lc.BlobHash, err = hashing.Parse(b); err != nil {
			return nil, err
		}
		out = append(out, lc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// inList builds an IN (?, ?, ...) clause. A small local helper rather than a
// shared one because the shared one (inClause) lives in the jobs package.
func inList(values []string) (string, []any) {
	if len(values) == 0 {
		return "(NULL)", nil
	}
	out := make([]byte, 0, 2*len(values)+1)
	out = append(out, '(')
	args := make([]any, 0, len(values))
	for i, v := range values {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, '?')
		args = append(args, v)
	}
	out = append(out, ')')
	return string(out), args
}
