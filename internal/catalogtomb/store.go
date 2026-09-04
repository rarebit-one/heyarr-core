// Package catalogtomb persists the catalog tombstone op-log and materialises it
// into the work_tombstones view (ADR-0073 Phase 1, migration 00043). It is the
// storage half of internal/catalogop, modelled on deviceauth's RecordOps /
// reconcile idiom for membership_ops (ADR-0068): one write path appends ops
// idempotently and re-materialises the view from the WHOLE log afterwards, so
// the view is always a pure function of the ops the node has recorded — never a
// second writer reaching across a network (ADR-0003, Invariant 5 untouched).
//
// A delete op recorded here (whether written locally or learned from a peer
// sync) does two things in one transaction: it materialises the work_tombstones
// row the scanner will consult, and it removes the live `works` row for that
// natural key if one exists — the remove-wins application that makes a delete
// at one site converge at the other. A causal restore lifts the tombstone by
// removing the materialised row again; the base spine then re-derives the work
// from CAS on the next scan, which is exactly the intent.
package catalogtomb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/catalogop"
)

const timeFormat = time.RFC3339Nano

// Clock is injected so materialisation timestamps are unit facts, not sleeps —
// the deviceauth.Clock shape.
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// ErrMalformedOp is a token that does not verify. The callers that feed the log
// — a local delete and a peer sync — hand over only tokens catalogop.Verify
// accepted, so reaching this is a caller bug, not a client's.
var ErrMalformedOp = errors.New("catalogtomb: malformed catalog op")

// Store is the catalog_ops log and its work_tombstones materialised view.
type Store struct {
	writer *sql.DB
	reader *sql.DB
	clock  Clock
}

// Options configure a Store.
type Options struct {
	// Writer is the single-writer pool (ADR-0003).
	Writer *sql.DB
	// Reader serves the tombstone lookup off the write path; defaults to Writer.
	Reader *sql.DB
	Clock  Clock
}

// New constructs a Store.
func New(opts Options) (*Store, error) {
	if opts.Writer == nil {
		return nil, errors.New("catalogtomb: a writer database is required")
	}
	reader := opts.Reader
	if reader == nil {
		reader = opts.Writer
	}
	clock := opts.Clock
	if clock == nil {
		clock = systemClock{}
	}
	return &Store{writer: opts.Writer, reader: reader, clock: clock}, nil
}

// Ops returns every op token the node has recorded, oldest-issued first — the
// state a peer replicates and merges (catalogop.Merge). A node with no ops is
// nil, not an error.
func (s *Store) Ops(ctx context.Context) ([]string, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT token FROM catalog_ops ORDER BY iat, op_hash`)
	if err != nil {
		return nil, fmt.Errorf("catalogtomb: reading catalog ops: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var tok string
		if err := rows.Scan(&tok); err != nil {
			return nil, fmt.Errorf("catalogtomb: reading catalog op: %w", err)
		}
		out = append(out, tok)
	}
	return out, rows.Err()
}

// Heads returns the current frontier of the recorded DAG — what a caller cites
// as prev when it signs the next op (a restore over a delete). It is the
// evaluated View.Heads over everything recorded.
func (s *Store) Heads(ctx context.Context) ([]string, error) {
	tokens, err := s.Ops(ctx)
	if err != nil {
		return nil, err
	}
	return catalogop.Evaluate(tokens).Heads, nil
}

// Tombstoned reports whether a work (content_type, work_key) is currently
// tombstoned — the read a scanner's get-or-create makes before creating a work,
// to suppress re-materialising something a peer deleted. It reads the
// materialised view, so it is a single indexed lookup, not a re-evaluation.
func (s *Store) Tombstoned(ctx context.Context, contentType, workKey string) (bool, error) {
	var one int
	err := s.reader.QueryRowContext(ctx,
		`SELECT 1 FROM work_tombstones WHERE content_type = ? AND work_key = ?`,
		contentType, workKey).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("catalogtomb: reading tombstone: %w", err)
	default:
		return true, nil
	}
}

// RecordOps appends ops to the log, idempotently by op hash, and re-materialises
// work_tombstones from the whole log afterwards. It is the one write path into
// catalog_ops: a local delete/restore records the op it just signed, and a peer
// sync records what the other site pushed. An op that does not verify is
// ErrMalformedOp and nothing is written.
//
// Recording and reconciliation share one transaction, so a crash mid-way leaves
// neither the op nor a half-updated view — the log and its materialisation
// never disagree.
func (s *Store) RecordOps(ctx context.Context, ops []string) error {
	if len(ops) == 0 {
		return nil
	}
	parsed := make([]catalogop.Op, 0, len(ops))
	for _, tok := range ops {
		op, err := catalogop.Verify(tok)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrMalformedOp, err.Error())
		}
		parsed = append(parsed, op)
	}
	now := s.clock.Now().UTC()

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("catalogtomb: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	fresh := false
	for _, op := range parsed {
		prev, err := json.Marshal(normalise(op.Prev))
		if err != nil {
			return fmt.Errorf("catalogtomb: encoding prev: %w", err)
		}
		res, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO catalog_ops (op_hash, content_type, work_key, op, signer, prev, iat, token, received_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			op.Hash, op.Target.ContentType, op.Target.WorkKey, string(op.Kind), op.By,
			string(prev), op.IssuedAt.UTC().Format(timeFormat), op.Token, now.Format(timeFormat))
		if err != nil {
			return fmt.Errorf("catalogtomb: recording catalog op: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			fresh = true
		}
	}
	if !fresh {
		return tx.Commit()
	}
	if err := s.reconcileTx(ctx, tx, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("catalogtomb: committing: %w", err)
	}
	return nil
}

// Reconcile re-materialises work_tombstones from the whole log. It is what
// RecordOps does after every append, exposed for tests and for a startup
// backfill. Idempotent.
func (s *Store) Reconcile(ctx context.Context) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("catalogtomb: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.reconcileTx(ctx, tx, s.clock.Now().UTC()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("catalogtomb: committing: %w", err)
	}
	return nil
}

// reconcileTx evaluates the whole log and brings work_tombstones into line with
// the view, inside the caller's transaction. For every target the view
// tombstones it upserts a row and removes the live `works` row for that natural
// key (the remove-wins application). For every target the view NO LONGER
// tombstones — a causal restore has lifted it — it deletes the materialised
// row, letting the base spine re-derive the work from CAS on the next scan.
func (s *Store) reconcileTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	tokens, err := tokensTx(ctx, tx)
	if err != nil {
		return err
	}
	view := catalogop.Evaluate(tokens)

	existing, err := tombstoneRowsTx(ctx, tx)
	if err != nil {
		return err
	}

	ts := now.Format(timeFormat)
	for key, target := range view.Tombstoned {
		if _, ok := existing[key]; ok {
			delete(existing, key) // still tombstoned — keep it
			continue
		}
		// The op the tombstone is attributed to: the earliest un-overridden
		// delete for this target, so the provenance column is stable across
		// re-materialisations rather than whichever map order won.
		opHash := earliestDeleteHash(view, target)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO work_tombstones (content_type, work_key, op_hash, tombstoned_at)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT (content_type, work_key) DO UPDATE SET op_hash = excluded.op_hash`,
			target.ContentType, target.WorkKey, opHash, ts); err != nil {
			return fmt.Errorf("catalogtomb: materialising tombstone: %w", err)
		}
		// Remove-wins: the live work row (if this site ever created it) loses to
		// the tombstone. ON DELETE CASCADE takes its editions, assets and the
		// rows beneath them; the blobs stay (ADR-0018, logical delete).
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM works WHERE content_type = ? AND work_key = ?`,
			target.ContentType, target.WorkKey); err != nil {
			return fmt.Errorf("catalogtomb: suppressing tombstoned work: %w", err)
		}
	}
	// Whatever is left in `existing` is a target the view no longer tombstones:
	// a restore lifted it. Drop the materialised row so the scan re-derives it.
	for key := range existing {
		ct, wk := splitKey(key)
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM work_tombstones WHERE content_type = ? AND work_key = ?`, ct, wk); err != nil {
			return fmt.Errorf("catalogtomb: lifting tombstone: %w", err)
		}
	}
	return nil
}

// earliestDeleteHash picks the earliest un-overridden delete op for a target,
// by (issued-at, hash), as the tombstone's provenance. Every delete in the view
// for a tombstoned target is un-overridden (Evaluate tombstones the target
// precisely when one exists), so it is a stable, deterministic pick.
func earliestDeleteHash(view catalogop.View, target catalogop.Target) string {
	var candidates []catalogop.Op
	for _, op := range view.Accepted {
		if op.Kind == catalogop.OpDelete && op.Target.Key() == target.Key() {
			candidates = append(candidates, op)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].IssuedAt.Equal(candidates[j].IssuedAt) {
			return candidates[i].IssuedAt.Before(candidates[j].IssuedAt)
		}
		return candidates[i].Hash < candidates[j].Hash
	})
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0].Hash
}

// tombstoneRow keys a materialised row for reconciliation.
type tombstoneRow struct{ contentType, workKey string }

// tombstoneRowsTx reads the materialised view, by natural key.
func tombstoneRowsTx(ctx context.Context, tx *sql.Tx) (map[string]tombstoneRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT content_type, work_key FROM work_tombstones`)
	if err != nil {
		return nil, fmt.Errorf("catalogtomb: reading tombstones: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]tombstoneRow{}
	for rows.Next() {
		var r tombstoneRow
		if err := rows.Scan(&r.contentType, &r.workKey); err != nil {
			return nil, fmt.Errorf("catalogtomb: reading tombstone: %w", err)
		}
		out[catalogop.Target{ContentType: r.contentType, WorkKey: r.workKey}.Key()] = r
	}
	return out, rows.Err()
}

// tokensTx reads every recorded op token inside a transaction.
func tokensTx(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT token FROM catalog_ops`)
	if err != nil {
		return nil, fmt.Errorf("catalogtomb: reading catalog ops: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var tok string
		if err := rows.Scan(&tok); err != nil {
			return nil, fmt.Errorf("catalogtomb: reading catalog op: %w", err)
		}
		out = append(out, tok)
	}
	return out, rows.Err()
}

// splitKey inverts catalogop.Target.Key(): the two halves either side of the
// 0x1f unit separator.
func splitKey(key string) (contentType, workKey string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '\x1f' {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}

// normalise mirrors catalogop's prev normalisation for the denormalised column:
// sorted, de-duplicated, never null.
func normalise(prev []string) []string {
	if len(prev) == 0 {
		return []string{}
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(prev))
	for _, p := range prev {
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
