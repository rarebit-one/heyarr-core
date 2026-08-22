// Package catalog is the SQLite-backed implementation of the content domain's
// repository ports (spec §79).
//
// It is a sibling of the sqlite package rather than part of it because the two
// answer different questions. sqlite owns connection policy, pragmas and
// migrations — how the database behaves. This owns what the rows mean, which
// requires knowing about the domain and about the event log. Folding it into
// sqlite makes the persistence package depend on events, and the event log's
// own tests already depend on catalog: the import cycle is the design telling
// you they are two packages.
package catalog

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/domain/ingest"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// Catalog records what exists.
//
// It lives outside the domain because it is the half that knows how things are
// stored: the domain states what must be true after an ingest, this decides
// which statements make it so and holds them in one transaction (ADR-0006,
// ADR-0007).
type Catalog struct {
	db       *sqlite.DB
	events   *events.Log
	peerName string
	peerSite string
	clock    Clock
	log      *slog.Logger

	mu       sync.Mutex
	selfPeer string

	fault func(stage string) error
}

// Clock is injected so recorded timestamps are testable (ADR-0017).
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// Options configure a Catalog.
type Options struct {
	DB     *sqlite.DB
	Events *events.Log
	// PeerName and PeerSite describe this node, and are used only when the
	// self peer row does not exist yet (ADR-0010).
	PeerName string
	PeerSite string
	Clock    Clock
	Logger   *slog.Logger

	// RecordFault runs at each named stage inside Record's transaction, and
	// returning an error from it aborts that transaction. It exists so the
	// property Milestone 1 actually cares about — a fault after the CAS write
	// and before the commit leaves an orphan and NO partial database state —
	// can be asserted rather than reasoned about. Nothing in production sets
	// it; a green suite nobody has watched go red is not evidence.
	//
	// Stages, in order: blob, work, edition, asset, replica, commit.
	RecordFault func(stage string) error
}

// New constructs a Catalog.
func New(opts Options) (*Catalog, error) {
	if opts.DB == nil {
		return nil, errors.New("catalog: a database is required")
	}
	if opts.Events == nil {
		return nil, errors.New("catalog: an event log is required — every state transition emits an event (ADR-0009)")
	}
	if opts.PeerName == "" {
		return nil, errors.New("catalog: a peer name is required")
	}
	clock := opts.Clock
	if clock == nil {
		clock = systemClock{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Catalog{
		db:       opts.DB,
		events:   opts.Events,
		peerName: opts.PeerName,
		peerSite: opts.PeerSite,
		clock:    clock,
		log:      logger,
		fault:    opts.RecordFault,
	}, nil
}

var _ ingest.Catalog = (*Catalog)(nil)

const timestampFormat = time.RFC3339Nano

// SelfPeer returns this node's peer id, creating the row on first use.
//
// The peer model exists from Milestone 1 with exactly one peer (ADR-0010), so
// that Milestone 4 is a protocol addition rather than a schema migration plus a
// rewrite of every read-path query. The unique index on is_self is what makes
// this safe to race: a loser re-reads rather than creating a second self.
func (c *Catalog) SelfPeer(ctx context.Context) (string, error) {
	c.mu.Lock()
	cached := c.selfPeer
	c.mu.Unlock()
	if cached != "" {
		return cached, nil
	}

	id, err := c.selfPeerFromDB(ctx)
	if err != nil {
		return "", err
	}
	if id == "" {
		if id, err = c.createSelfPeer(ctx); err != nil {
			return "", err
		}
	}
	c.mu.Lock()
	c.selfPeer = id
	c.mu.Unlock()
	return id, nil
}

func (c *Catalog) selfPeerFromDB(ctx context.Context) (string, error) {
	var id string
	err := c.db.Reader().QueryRowContext(ctx, `SELECT id FROM peers WHERE is_self = 1`).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("catalog: reading the self peer: %w", err)
	}
	return id, nil
}

func (c *Catalog) createSelfPeer(ctx context.Context) (string, error) {
	id := uuid.Must(uuid.NewV7()).String()
	now := c.clock.Now()

	var ev events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		// enrolled_at is deliberately NOT written here, even though 00020 adds
		// it. This statement runs in the worker and the peer as well as the
		// controller, and those roles start against the LOWEST schema they can
		// work at rather than the newest — so a column from the newest
		// migration is a column that may not exist yet when this runs. Readers
		// treat a NULL enrolled_at as created_at, which for the self peer is
		// the same instant: the row appearing IS when this node joined.
		res, err := tx.ExecContext(ctx, `
			INSERT INTO peers (id, name, site, mode, is_self, created_at)
			VALUES (?, ?, ?, 'full', 1, ?)
			ON CONFLICT (name) DO NOTHING`,
			id, c.peerName, c.peerSite, now.Format(timestampFormat))
		if err != nil {
			return fmt.Errorf("catalog: registering this peer: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("catalog: registering this peer: %w", err)
		}
		if n == 0 {
			// A peer with this name already exists but is not marked self.
			// That is a configuration mistake worth naming rather than
			// silently adopting.
			return fmt.Errorf("catalog: a peer named %q already exists and is not this node — "+
				"peer.name must be unique within an instance", c.peerName)
		}
		// The same payload shape peer enrolment emits (M4-04). This is the
		// self peer, but a subscriber asking "what peers does this system know
		// about" watches one type and must not have to parse two shapes of it
		// depending on which code path wrote the row. public_key is empty
		// here by construction: the row exists before the keypair does, which
		// is why peer.identity_established is a separate transition.
		ev, err = c.events.EmitTx(ctx, tx, events.TypePeerRegistered, "peer", id,
			map[string]any{
				"transition": events.PeerTransitionEnrolled,
				"peer_id":    id,
				"name":       c.peerName,
				"site":       c.peerSite,
				"mode":       "full",
				"endpoint":   "",
				"public_key": "",
				"is_self":    true,
			})
		return err
	})
	if err != nil {
		// Losing the race is ordinary: another role created the row first.
		if existing, readErr := c.selfPeerFromDB(ctx); readErr == nil && existing != "" {
			return existing, nil
		}
		return "", err
	}
	c.events.Publish(ev)
	c.log.Info("registered this peer", "peer_id", id, "name", c.peerName, "site", c.peerSite)
	return id, nil
}

// SelfIdentity reports the self peer's id and the public key recorded for it,
// creating the peer row on first use.
//
// The public key is nil when this node has not generated a keypair yet, which
// is every database migrated before 00019 and every fresh one before the
// controller's first identity check (M4-03).
func (c *Catalog) SelfIdentity(ctx context.Context) (string, []byte, error) {
	id, err := c.SelfPeer(ctx)
	if err != nil {
		return "", nil, err
	}
	var pub []byte
	err = c.db.Reader().QueryRowContext(ctx,
		`SELECT public_key FROM peers WHERE id = ?`, id).Scan(&pub)
	if err != nil {
		return "", nil, fmt.Errorf("catalog: reading this peer's public key: %w", err)
	}
	return id, pub, nil
}

// RecordSelfPublicKey stores this node's public key, once.
//
// It writes only where no key is recorded yet, and re-reads on a no-op write:
// two roles racing to establish the same identity must converge rather than
// one of them silently replacing the other's key. A stored key that differs
// from the one offered is not a race, it is the failure ADR-0010 is about, and
// it is refused here rather than overwritten — the private key on disk is the
// only thing that can prove which of the two is real, and overwriting the
// public key destroys the evidence.
//
// The private key is never passed to this method and never enters the
// database: backups stream to peers, and a restored backup carrying a private
// key produces two machines able to authenticate as one peer.
func (c *Catalog) RecordSelfPublicKey(ctx context.Context, algo string, pub []byte) error {
	if algo == "" {
		return errors.New("catalog: a key algorithm is required")
	}
	if len(pub) == 0 {
		return errors.New("catalog: refusing to record an empty public key")
	}
	id, err := c.SelfPeer(ctx)
	if err != nil {
		return err
	}
	now := c.clock.Now()

	var (
		ev      events.Event
		emitted bool
	)
	err = c.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE peers SET public_key = ?, key_algo = ?, key_generated_at = ?
			WHERE id = ? AND public_key IS NULL`,
			pub, algo, now.Format(timestampFormat), id)
		if err != nil {
			return fmt.Errorf("catalog: recording this peer's public key: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("catalog: recording this peer's public key: %w", err)
		}
		if n == 0 {
			var existing []byte
			if err := tx.QueryRowContext(ctx, `SELECT public_key FROM peers WHERE id = ?`, id).
				Scan(&existing); err != nil {
				return fmt.Errorf("catalog: recording this peer's public key: %w", err)
			}
			if !bytes.Equal(existing, pub) {
				return fmt.Errorf("catalog: peer %s already has a different public key recorded — "+
					"refusing to overwrite it", id)
			}
			return nil
		}
		emitted = true
		ev, err = c.events.EmitTx(ctx, tx, events.TypePeerIdentityEstablished, "peer", id,
			map[string]any{"algo": algo, "public_key": hex.EncodeToString(pub)})
		return err
	})
	if err != nil {
		return err
	}
	if emitted {
		c.events.Publish(ev)
		c.log.Info("established this peer's identity", "peer_id", id, "algo", algo)
	}
	return nil
}

// Root returns a library root's ingest configuration.
func (c *Catalog) Root(ctx context.Context, rootID string) (ingest.Root, error) {
	var (
		root            ingest.Root
		mode            string
		rootEnabled     int
		libraryEnabled  int
		libraryContent  string
		libraryID, path string
	)
	err := c.db.Reader().QueryRowContext(ctx, `
		SELECT r.id, r.library_id, r.path, r.ingest_mode, r.enabled, l.content_type, l.enabled
		FROM library_roots r JOIN libraries l ON l.id = r.library_id
		WHERE r.id = ?`, rootID).
		Scan(&root.ID, &libraryID, &path, &mode, &rootEnabled, &libraryContent, &libraryEnabled)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ingest.Root{}, fmt.Errorf("%w: %s", ingest.ErrRootNotFound, rootID)
	case err != nil:
		return ingest.Root{}, fmt.Errorf("catalog: reading library root %s: %w", rootID, err)
	}
	root.LibraryID = libraryID
	root.Path = path
	root.Mode = ingest.Materialisation(mode)
	root.LibraryContentType = libraryContent
	// A disabled library disables its roots. Recording that as the root being
	// disabled is what the caller can act on.
	root.Enabled = rootEnabled == 1 && libraryEnabled == 1
	return root, nil
}

// Record commits one ingest atomically: blob, work, edition, asset, replica and
// every resulting event.
//
// One transaction, because the alternative is a catalog that can be observed
// half-written. The events go in the same transaction (events.EmitTx) and fan
// out only after it commits — a subscriber that acts on an event whose
// transaction later rolls back has acted on something that did not happen
// (§76, ADR-0009).
//
// Every statement here is a get-or-create or an upsert, because the job queue
// will re-run this (ADR-0008): a lease can expire under a slow hash of a 60 GB
// remux and the reaper hands the job straight back.
func (c *Catalog) Record(ctx context.Context, rec ingest.Recording) (ingest.Result, error) {
	if rec.Blob.Hash == "" {
		return ingest.Result{}, errors.New("catalog: a recording needs a blob hash")
	}
	if rec.PeerID == "" {
		return ingest.Result{}, errors.New("catalog: a recording needs the recording peer")
	}

	res := ingest.Result{
		BlobHash:     rec.Blob.Hash,
		BlobSize:     rec.Blob.Size,
		Materialised: rec.Blob.Materialised,
	}
	var pending []events.Event

	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		now := rec.Now.Format(timestampFormat)

		blobCreated, err := c.recordBlob(ctx, tx, rec, now)
		if err != nil {
			return err
		}
		res.BlobCreated = blobCreated
		res.Deduplicated = !blobCreated
		if err := c.injectFault("blob"); err != nil {
			return err
		}

		// Beside the blob, because it describes the bytes rather than this use
		// of them (§69, M2-08). It goes in the same transaction as everything
		// else: a publication row for a blob whose asset rolled back would be
		// metadata about something the catalog does not think exists.
		if err := c.recordPublication(ctx, tx, rec, now); err != nil {
			return err
		}

		workID, workCreated, err := c.resolveWork(ctx, tx, rec.Candidate, now)
		if err != nil {
			return err
		}
		res.WorkID, res.WorkCreated = workID, workCreated
		if err := c.injectFault("work"); err != nil {
			return err
		}

		editionID, editionCreated, err := c.resolveEdition(ctx, tx, workID, rec.Candidate, now)
		if err != nil {
			return err
		}
		res.EditionID, res.EditionCreated = editionID, editionCreated
		if err := c.injectFault("edition"); err != nil {
			return err
		}

		assetID, assetCreated, err := c.recordAsset(ctx, tx, rec, editionID, now)
		if err != nil {
			return err
		}
		res.AssetID, res.AssetCreated = assetID, assetCreated
		if err := c.injectFault("asset"); err != nil {
			return err
		}

		replicaCreated, err := c.recordReplica(ctx, tx, rec, now)
		if err != nil {
			return err
		}
		res.ReplicaCreated = replicaCreated
		if err := c.injectFault("replica"); err != nil {
			return err
		}

		pending, err = c.recordEvents(ctx, tx, rec, res)
		if err != nil {
			return err
		}
		return c.injectFault("commit")
	})
	if err != nil {
		return ingest.Result{}, err
	}

	c.events.Publish(pending...)
	return res, nil
}

func (c *Catalog) injectFault(stage string) error {
	if c.fault == nil {
		return nil
	}
	return c.fault(stage)
}

// recordBlob is the whole of deduplication. The hash is the primary key
// (ADR-0005), so "have we seen these bytes" is a constraint rather than a
// lookup the handler could forget to do.
func (c *Catalog) recordBlob(ctx context.Context, tx *sql.Tx, rec ingest.Recording, now string) (bool, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO blobs (hash, size, mime, chunked, first_seen_at)
		VALUES (?, ?, ?, 0, ?)
		ON CONFLICT (hash) DO NOTHING`,
		rec.Blob.Hash, rec.Blob.Size, nullString(rec.MIME), now)
	if err != nil {
		return false, fmt.Errorf("catalog: recording blob %s: %w", rec.Blob.Hash, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("catalog: recording blob %s: %w", rec.Blob.Hash, err)
	}
	return n == 1, nil
}

func (c *Catalog) resolveWork(ctx context.Context, tx *sql.Tx, cand identification.Candidate, now string) (string, bool, error) {
	attrs, err := encodeAttributes(cand.WorkAttributes)
	if err != nil {
		return "", false, err
	}
	id := uuid.Must(uuid.NewV7()).String()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO works (id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (content_type, work_key) DO NOTHING`,
		id, cand.ContentType, cand.WorkKey, cand.Title, cand.SortTitle, nullYear(cand.Year), attrs, now, now)
	if err != nil {
		return "", false, fmt.Errorf("catalog: resolving work %s/%s: %w", cand.ContentType, cand.WorkKey, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", false, fmt.Errorf("catalog: resolving work %s/%s: %w", cand.ContentType, cand.WorkKey, err)
	}
	if n == 1 {
		return id, true, nil
	}

	// It already existed. Re-reading rather than trusting the generated id is
	// the point: the WorkKey is what converges, not the identifier we invented.
	var existing string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM works WHERE content_type = ? AND work_key = ?`,
		cand.ContentType, cand.WorkKey).Scan(&existing); err != nil {
		return "", false, fmt.Errorf("catalog: re-reading work %s/%s: %w", cand.ContentType, cand.WorkKey, err)
	}
	return existing, false, nil
}

// resolveEdition gets or creates the edition. Its attributes stay empty in
// Milestone 1 on purpose: the identifier's EditionAttributes describe where one
// FILE sits (season, episode, disc, track), and every file in a season resolves
// to the same edition row — so they go on the asset, not here. The edition's
// own identity is carried by edition_key, label, edition_type and language.
// Milestone 3's real identifier is what fills this in with edition-scoped facts.
func (c *Catalog) resolveEdition(ctx context.Context, tx *sql.Tx, workID string, cand identification.Candidate, now string) (string, bool, error) {
	const attrs = "{}"
	id := uuid.Must(uuid.NewV7()).String()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO editions (id, work_id, edition_key, label, edition_type, language, attributes, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (work_id, edition_key) DO NOTHING`,
		id, workID, cand.EditionKey, cand.EditionLabel, cand.EditionType, nullString(cand.Language), attrs, now)
	if err != nil {
		return "", false, fmt.Errorf("catalog: resolving edition %s/%s: %w", workID, cand.EditionKey, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", false, fmt.Errorf("catalog: resolving edition %s/%s: %w", workID, cand.EditionKey, err)
	}
	if n == 1 {
		return id, true, nil
	}
	var existing string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM editions WHERE work_id = ? AND edition_key = ?`, workID, cand.EditionKey).Scan(&existing); err != nil {
		return "", false, fmt.Errorf("catalog: re-reading edition %s/%s: %w", workID, cand.EditionKey, err)
	}
	return existing, false, nil
}

// recordAsset upserts on (library_id, source_path).
//
// The key is where the bytes were found, not the blob hash: two paths holding
// identical bytes are two assets sharing one blob (§13), and keying on the hash
// would collapse them into one. A file replaced in place keeps its asset and
// gains a new blob — the old one falls out of reference and the GC reclaims it
// after its grace window (ADR-0018).
func (c *Catalog) recordAsset(ctx context.Context, tx *sql.Tx, rec ingest.Recording, editionID, now string) (string, bool, error) {
	attrs, err := encodeAttributes(rec.Candidate.EditionAttributes)
	if err != nil {
		return "", false, err
	}
	var existing string
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM assets
		WHERE library_id = ? AND source_path = ? AND source_class = 'managed'`,
		rec.Root.LibraryID, rec.SourcePath).Scan(&existing)
	switch {
	case err == nil:
		if _, err := tx.ExecContext(ctx, `
			UPDATE assets SET edition_id = ?, blob_hash = ?, role = ?, filename = ?, mime = ?,
				identification_source = ?, identification_rule = ?, attributes = ?,
				missing_since = NULL, updated_at = ?
			WHERE id = ?`,
			editionID, rec.Blob.Hash, rec.Candidate.AssetRole, rec.Filename, nullString(rec.MIME),
			rec.Candidate.Source, rec.Candidate.Rule, attrs, now, existing); err != nil {
			return "", false, fmt.Errorf("catalog: updating asset %s: %w", existing, err)
		}
		return existing, false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", false, fmt.Errorf("catalog: reading asset for %s: %w", rec.SourcePath, err)
	}

	id := uuid.Must(uuid.NewV7()).String()
	// Milestone 1 only ever writes managed assets (ADR-0020). linked and vault
	// exist in the schema so that Milestone 4's read routing can express "this
	// exists on one peer by design" rather than "replication failed".
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash, source_path,
			role, filename, mime, identification_source, identification_rule, attributes,
			created_at, updated_at)
		VALUES (?, ?, ?, 'managed', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, editionID, rec.Root.LibraryID, rec.Blob.Hash, rec.SourcePath,
		rec.Candidate.AssetRole, rec.Filename, nullString(rec.MIME),
		rec.Candidate.Source, rec.Candidate.Rule, attrs, now, now); err != nil {
		return "", false, fmt.Errorf("catalog: creating asset for %s: %w", rec.SourcePath, err)
	}
	return id, true, nil
}

func (c *Catalog) recordReplica(ctx context.Context, tx *sql.Tx, rec ingest.Recording, now string) (bool, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, updated_at)
		VALUES (?, ?, 'present', ?, ?)
		ON CONFLICT (blob_hash, peer_id) DO UPDATE SET
			state = 'present', bytes_present = excluded.bytes_present, updated_at = excluded.updated_at
		WHERE replicas.state <> 'present' OR replicas.bytes_present <> excluded.bytes_present`,
		rec.Blob.Hash, rec.PeerID, rec.Blob.Size, now)
	if err != nil {
		return false, fmt.Errorf("catalog: recording replica of %s: %w", rec.Blob.Hash, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("catalog: recording replica of %s: %w", rec.Blob.Hash, err)
	}
	return n == 1, nil
}

// recordEvents appends the transitions this ingest caused. Every state
// transition emits an event, with no exceptions — retrofitting is what makes
// this expensive (§76, ADR-0009).
//
// The order is causal: the bytes exist, then the catalog entry that names them,
// then the replica that holds them, and finally the ingest as a whole. A
// consumer replaying the log in seq order never sees an asset before its blob.
func (c *Catalog) recordEvents(ctx context.Context, tx *sql.Tx, rec ingest.Recording, res ingest.Result) ([]events.Event, error) {
	var out []events.Event
	emit := func(eventType, subjectType, subjectID string, payload any) error {
		e, err := c.events.EmitTx(ctx, tx, eventType, subjectType, subjectID, payload)
		if err != nil {
			return err
		}
		out = append(out, e)
		return nil
	}

	if res.BlobCreated {
		if err := emit(events.TypeBlobCreated, "blob", res.BlobHash, map[string]any{
			"size":         res.BlobSize,
			"mime":         rec.MIME,
			"materialised": string(res.Materialised),
		}); err != nil {
			return nil, err
		}
	}
	if res.AssetCreated {
		if err := emit(events.TypeAssetCreated, "asset", res.AssetID, map[string]any{
			"work_id":               res.WorkID,
			"edition_id":            res.EditionID,
			"library_id":            rec.Root.LibraryID,
			"blob_hash":             res.BlobHash,
			"role":                  rec.Candidate.AssetRole,
			"content_type":          rec.Candidate.ContentType,
			"identification_source": rec.Candidate.Source,
			"identification_rule":   rec.Candidate.Rule,
		}); err != nil {
			return nil, err
		}
	}
	if res.ReplicaCreated {
		if err := emit(events.TypeReplicaPresent, "blob", res.BlobHash, map[string]any{
			"peer_id":       rec.PeerID,
			"bytes_present": res.BlobSize,
		}); err != nil {
			return nil, err
		}
	}
	// Always, including on a re-run that changed nothing: "this file is under
	// management" is the fact a consumer waits for, and a repeat ingest must
	// still answer it.
	if err := emit(events.TypeIngestCompleted, "asset", res.AssetID, map[string]any{
		"blob_hash":     res.BlobHash,
		"size":          res.BlobSize,
		"work_id":       res.WorkID,
		"edition_id":    res.EditionID,
		"library_id":    rec.Root.LibraryID,
		"root_id":       rec.Root.ID,
		"source_path":   rec.SourcePath,
		"materialised":  string(res.Materialised),
		"deduplicated":  res.Deduplicated,
		"asset_created": res.AssetCreated,
	}); err != nil {
		return nil, err
	}
	return out, nil
}

func encodeAttributes(attrs map[string]any) (string, error) {
	if len(attrs) == 0 {
		return "{}", nil
	}
	// encoding/json sorts map keys, so the same attributes always produce the
	// same bytes — which is what lets a re-scan be a no-op rather than a diff
	// (ADR-0017).
	b, err := json.Marshal(attrs)
	if err != nil {
		return "", fmt.Errorf("catalog: encoding attributes: %w", err)
	}
	return string(b), nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullYear(y int) any {
	if y == 0 {
		return nil
	}
	return y
}

// recordPublication stores what a publication container declared about itself.
//
// It is an upsert on the blob hash and therefore safely re-runnable, which
// matters because ingest jobs are re-run by design (invariant 9). Re-examining
// an unchanged archive writes the same numbers; re-examining one whose index
// has become readable since — a truncated download completed, say — improves
// them.
func (c *Catalog) recordPublication(ctx context.Context, tx *sql.Tx, rec ingest.Recording, now string) error {
	if rec.Publication == nil {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO publications (blob_hash, format, page_count, chapter_count, examined_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (blob_hash) DO UPDATE SET
			format = excluded.format,
			page_count = excluded.page_count,
			chapter_count = excluded.chapter_count,
			examined_at = excluded.examined_at`,
		rec.Blob.Hash, string(rec.Publication.Format),
		nullableCount(rec.Publication.PageCount),
		nullableCount(rec.Publication.ChapterCount),
		now)
	if err != nil {
		return fmt.Errorf("catalog: recording the publication for %s: %w", rec.Blob.Hash, err)
	}
	return nil
}

// nullableCount keeps "not read" distinct from zero all the way to the column.
func nullableCount(n *int) any {
	if n == nil {
		return nil
	}
	return *n
}
