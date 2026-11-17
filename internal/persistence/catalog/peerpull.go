package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// Peer-pull of non-swarm assets (ADR-0119 amendment, #833). The origin
// publishes a `hold` op for each asset of an opted-in library that no swarm
// covers; a sibling that opted the library in pulls the blob from the signer
// and records the asset under the origin's keys. This file is the catalog's
// half of both: what the origin may publish, where a sibling lands a hold, and
// what it did with each one.

// HoldOutcome is what this node did with one hold op.
type HoldOutcome string

const (
	// HoldOwn means this node signed the op; nothing to pull.
	HoldOwn HoldOutcome = "own"
	// HoldHeld means the asset is recorded here. Final: deleting it here is
	// respected and the blob is not pulled again.
	HoldHeld HoldOutcome = "held"
	// HoldPulling means the pull is queued, in flight, or waiting out the
	// spacing after a failure.
	HoldPulling HoldOutcome = "pulling"
	// HoldNoLibrary means no opted-in library here takes the op's content type,
	// or several do and none has the origin library's name.
	HoldNoLibrary HoldOutcome = "no_library"
	// HoldTooLarge means the blob is over this node's per-blob cap.
	HoldTooLarge HoldOutcome = "too_large"
	// HoldNoSiteSource means the signer is not a member of this node, so there
	// is nobody to pull from.
	HoldNoSiteSource HoldOutcome = "no_site_source"
	// HoldTombstoned means the work is tombstoned here.
	HoldTombstoned HoldOutcome = "tombstoned"
)

// Final reports whether an outcome is settled for its op.
func (o HoldOutcome) Final() bool { return o == HoldOwn || o == HoldHeld }

// HoldApplication is one recorded outcome.
type HoldApplication struct {
	OpHash    string
	BlobHash  string
	LibraryID string
	Outcome   HoldOutcome
	Detail    string
	AppliedAt time.Time
}

// HoldApplications reads every recorded hold outcome, by op hash.
func (c *Catalog) HoldApplications(ctx context.Context) (map[string]HoldApplication, error) {
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT op_hash, blob_hash, COALESCE(library_id, ''), outcome, detail, applied_at
		  FROM catalog_hold_applications`)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading hold applications: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]HoldApplication{}
	for rows.Next() {
		var a HoldApplication
		var at string
		if err := rows.Scan(&a.OpHash, &a.BlobHash, &a.LibraryID, &a.Outcome, &a.Detail, &at); err != nil {
			return nil, fmt.Errorf("catalog: reading hold applications: %w", err)
		}
		a.AppliedAt = parseStamp(at)
		out[a.OpHash] = a
	}
	return out, rows.Err()
}

// RecordHoldApplication upserts the outcome for a hold op and, when it is new
// or differs from the stored one, emits replication.hold_changed in the same
// transaction (invariant 7). An unchanged outcome writes nothing.
func (c *Catalog) RecordHoldApplication(ctx context.Context, a HoldApplication) error {
	var lib any
	if a.LibraryID != "" {
		lib = a.LibraryID
	}
	var pending []events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		pending = nil
		var prevLib, prevOutcome, prevDetail string
		err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(library_id, ''), outcome, detail FROM catalog_hold_applications WHERE op_hash = ?`,
			a.OpHash).Scan(&prevLib, &prevOutcome, &prevDetail)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		case prevLib == a.LibraryID && prevOutcome == string(a.Outcome) && prevDetail == a.Detail:
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO catalog_hold_applications (op_hash, blob_hash, library_id, outcome, detail, applied_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (op_hash) DO UPDATE SET
				blob_hash = excluded.blob_hash, library_id = excluded.library_id, outcome = excluded.outcome,
				detail = excluded.detail, applied_at = excluded.applied_at`,
			a.OpHash, a.BlobHash, lib, string(a.Outcome), a.Detail, sqlite.FormatTimestamp(c.clock.Now())); err != nil {
			return err
		}
		ev, err := c.events.EmitTx(ctx, tx, events.TypeHoldApplicationChanged, "catalog_op", a.OpHash,
			map[string]any{
				"op_hash": a.OpHash, "blob_hash": a.BlobHash, "library_id": a.LibraryID,
				"outcome": string(a.Outcome), "was": prevOutcome, "detail": a.Detail,
			})
		if err != nil {
			return err
		}
		pending = append(pending, ev)
		return nil
	})
	if err != nil {
		return fmt.Errorf("catalog: recording hold application %s: %w", a.OpHash, err)
	}
	c.events.Publish(pending...)
	return nil
}

// HoldCandidate is an asset this node may publish as a hold.
type HoldCandidate struct {
	AssetID     string
	LibraryName string
	ContentType string
	WorkKey     string
	WorkTitle   string
	Year        int
	EditionKey  string
	// ItemKey is the item the asset is linked to, "" when none: an `acquire`
	// scoped to that item covers it.
	ItemKey  string
	BlobHash string
	Size     int64
	// RelPath is the asset's path relative to the library root it was found
	// under, slash-separated; its file name when it lies under none (an
	// acquisition's download path).
	RelPath string
	Role    string
	HeldAt  time.Time
}

// HoldCandidates lists the managed assets of the named libraries that this node
// holds present and that are no larger than maxBytes, in asset order. Assets
// this node itself pulled from a sibling's hold are left out: publishing them
// would echo the origin's op back to it as this site's own.
//
// Whether a swarm covers an asset is the op-log's to say, not the catalog's,
// so the caller filters on that.
func (c *Catalog) HoldCandidates(ctx context.Context, libraries []string, maxBytes int64) ([]HoldCandidate, error) {
	if len(libraries) == 0 {
		return nil, nil
	}
	self, err := c.SelfPeer(ctx)
	if err != nil {
		return nil, err
	}
	roots, err := c.libraryRootPaths(ctx, libraries)
	if err != nil {
		return nil, err
	}
	args := []any{self, maxBytes, identification.SourceSiblingHold}
	for _, l := range libraries {
		args = append(args, l)
	}
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT a.id, l.name, w.content_type, w.work_key, w.title, COALESCE(w.year, 0),
		       e.edition_key, COALESCE(i.item_key, ''), a.blob_hash, b.size,
		       COALESCE(a.source_path, ''), COALESCE(a.filename, ''), a.role, a.created_at
		  FROM assets a
		  JOIN editions e ON e.id = a.edition_id
		  JOIN works w ON w.id = e.work_id
		  JOIN libraries l ON l.id = a.library_id
		  JOIN blobs b ON b.hash = a.blob_hash
		  JOIN replicas r ON r.blob_hash = a.blob_hash AND r.peer_id = ? AND r.state = 'present'
		  LEFT JOIN items i ON i.id = a.item_id
		 WHERE a.source_class = 'managed' AND a.missing_since IS NULL
		   AND b.size <= ? AND a.identification_source <> ?
		   AND l.name IN (?`+strings.Repeat(",?", len(libraries)-1)+`)
		 ORDER BY a.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing hold candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []HoldCandidate
	for rows.Next() {
		var (
			h                  HoldCandidate
			source, file, held string
		)
		if err := rows.Scan(&h.AssetID, &h.LibraryName, &h.ContentType, &h.WorkKey, &h.WorkTitle, &h.Year,
			&h.EditionKey, &h.ItemKey, &h.BlobHash, &h.Size, &source, &file, &h.Role, &held); err != nil {
			return nil, fmt.Errorf("catalog: listing hold candidates: %w", err)
		}
		h.RelPath = relativeTo(roots[h.LibraryName], source, file)
		h.HeldAt = parseStamp(held)
		if h.RelPath == "" {
			continue
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// libraryRootPaths is every root path of the named libraries, longest first so
// a nested root wins over its parent.
func (c *Catalog) libraryRootPaths(ctx context.Context, libraries []string) (map[string][]string, error) {
	args := make([]any, 0, len(libraries))
	for _, l := range libraries {
		args = append(args, l)
	}
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT l.name, r.path FROM library_roots r JOIN libraries l ON l.id = r.library_id
		 WHERE l.name IN (?`+strings.Repeat(",?", len(libraries)-1)+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading library roots: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]string{}
	for rows.Next() {
		var name, path string
		if err := rows.Scan(&name, &path); err != nil {
			return nil, err
		}
		out[name] = append(out[name], filepath.Clean(path))
	}
	for name := range out {
		sort.Slice(out[name], func(i, j int) bool { return len(out[name][i]) > len(out[name][j]) })
	}
	return out, rows.Err()
}

// relativeTo is source's path under the first root that contains it,
// slash-separated, or the file name when no root does.
func relativeTo(roots []string, source, file string) string {
	if source != "" {
		clean := filepath.Clean(source)
		for _, root := range roots {
			if rel, err := filepath.Rel(root, clean); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
				return filepath.ToSlash(rel)
			}
		}
		if file == "" {
			file = filepath.Base(clean)
		}
	}
	if file == "" || file == "." || file == "/" {
		return ""
	}
	return file
}

// PeerPullLibrary is an opted-in library a hold may land in.
type PeerPullLibrary struct {
	ID          string
	Name        string
	ContentType string
	// RootID is the library's first enabled root, which the adopted asset is
	// recorded under; "" when it has none.
	RootID string
}

// PeerPullLibraries returns the enabled libraries among names, by name order,
// each with its first enabled root. A name with no library is simply absent.
func (c *Catalog) PeerPullLibraries(ctx context.Context, names []string) ([]PeerPullLibrary, error) {
	if len(names) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(names))
	for _, n := range names {
		args = append(args, n)
	}
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT l.id, l.name, l.content_type,
		       COALESCE((SELECT r.id FROM library_roots r
		                  WHERE r.library_id = l.id AND r.enabled = 1
		                  ORDER BY r.created_at, r.id LIMIT 1), '')
		  FROM libraries l
		 WHERE l.enabled = 1 AND l.name IN (?`+strings.Repeat(",?", len(names)-1)+`)
		 ORDER BY l.name`, args...)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading peer-pull libraries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PeerPullLibrary
	for rows.Next() {
		var l PeerPullLibrary
		if err := rows.Scan(&l.ID, &l.Name, &l.ContentType, &l.RootID); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// LibraryHoldsBlob reports whether the library already has an asset of blob:
// a hold for it there is held without a transfer.
func (c *Catalog) LibraryHoldsBlob(ctx context.Context, libraryID, blobHash string) (bool, error) {
	var one int
	err := c.db.Reader().QueryRowContext(ctx,
		`SELECT 1 FROM assets WHERE library_id = ? AND blob_hash = ? LIMIT 1`, libraryID, blobHash).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("catalog: looking for %s in library %s: %w", blobHash, libraryID, err)
	}
	return true, nil
}

// LastFailedPull is when this node's own replica of blob was last recorded
// missing or corrupt, the zero time when it was not: a failed transfer leaves
// one of those (RecordBlobTransferFailed), and its time spaces the retry.
func (c *Catalog) LastFailedPull(ctx context.Context, blobHash string) (time.Time, error) {
	self, err := c.SelfPeer(ctx)
	if err != nil {
		return time.Time{}, err
	}
	var at string
	err = c.db.Reader().QueryRowContext(ctx, `
		SELECT updated_at FROM replicas
		 WHERE blob_hash = ? AND peer_id = ? AND state IN ('missing', 'corrupt')`, blobHash, self).Scan(&at)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return time.Time{}, nil
	case err != nil:
		return time.Time{}, fmt.Errorf("catalog: reading the last failed pull of %s: %w", blobHash, err)
	}
	return parseStamp(at), nil
}
