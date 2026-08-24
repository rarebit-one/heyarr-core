package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
)

// Content and peer reconciliation (§56, §57).
//
// # This file is the EDGE, and the thinking is in the domain
//
// Everything here is a query. The two questions — "do we hold bytes the profile
// accepts" and "are those bytes everywhere they should be" — are answered by
// pure functions in internal/domain/acquisition, which is what makes their
// interesting cases testable without a library, a peer or a profile.
//
// What lives here is the mapping FROM rows TO the values those functions take,
// and that mapping is where the milestone's honest limitations show up: an
// asset's quality attributes come from a probe that may not exist, an edition
// whose type may be empty, and a blob whose size is the only thing always
// known.

// ReconcileResult is what one want's reconciliation concluded.
type ReconcileResult struct {
	DesiredItemID string
	Content       acquisition.ContentVerdict
	Placement     acquisition.PlacementVerdict
	// PlacementUnproven reports that the placement answer is true by
	// construction rather than by replication having worked: the Full Peer
	// target set is this node alone, so placement is satisfied the moment
	// content is and `converging` is unreachable (§56, ADR-0010, ADR-0027).
	//
	// It is a fact about the DEPLOYMENT rather than about this want, and it is
	// carried on the result because the API reports it per response — see
	// resources.PlacementSatisfaction.Unproven. It is computed on every pass,
	// including passes where content is unsatisfied and placement is therefore
	// unknown: the shape of the fabric is the same question either way, and a
	// field that went quiet when the answer was unknown would be a caveat that
	// disappears exactly when the reader has least to go on.
	PlacementUnproven bool
	// Changed reports whether anything actually moved. A pass over a steady
	// library must change nothing and emit nothing.
	Changed bool
	State   acquisition.State
}

// ReconcileDesired evaluates one want's two axes and records the answers.
//
// Idempotent by construction: it reads, computes and writes the conclusion. Run
// twice over an unchanged library it writes the same values the second time,
// which SetSatisfaction recognises as a no-op and does not emit for.
func (c *Catalog) ReconcileDesired(ctx context.Context, desiredItemID string) (ReconcileResult, error) {
	want, err := c.desiredForReconcile(ctx, desiredItemID)
	if err != nil {
		return ReconcileResult{}, err
	}

	profile, err := c.profileForReconcile(ctx, want.qualityProfileID)
	if err != nil {
		return ReconcileResult{}, err
	}

	assets, err := c.assetsForWant(ctx, want)
	if err != nil {
		return ReconcileResult{}, err
	}
	content := acquisition.EvaluateContent(assets, profile)

	// The Full Peer target set is read whether or not placement is evaluated.
	// "Is this deployment one peer talking to itself?" is a question about the
	// fabric, and the answer travels back on every result so the API can say
	// so even when the placement answer itself is unknown.
	required, soleSelf, err := c.requiredPeers(ctx)
	if err != nil {
		return ReconcileResult{}, err
	}

	// Placement is a question about THE BYTES THAT SATISFY, not about every
	// asset under the work. Asking it of a 480p rip that the profile rejected
	// would report a want as fully satisfied because the wrong file is well
	// replicated.
	placement := acquisition.PlacementVerdict{Satisfaction: acquisition.SatisfactionUnknown}
	if content.Satisfaction == acquisition.SatisfactionSatisfied {
		var satisfying acquisition.AssetView
		for _, a := range assets {
			if a.ID == content.SatisfiedBy {
				satisfying = a
				break
			}
		}
		replicas, err := c.replicasOf(ctx, satisfying.BlobHash)
		if err != nil {
			return ReconcileResult{}, err
		}
		placement = acquisition.EvaluatePlacement(satisfying.BlobHash, required, replicas)
	}

	before, err := c.Acquisition(ctx, desiredItemID)
	if err != nil {
		return ReconcileResult{}, err
	}

	// Content satisfaction is a statement about managed bytes, so a want that
	// holds nothing cannot be satisfied — and the `managed` fact has to move
	// with it, or the state fails its own validation.
	if err := c.setManaged(ctx, desiredItemID, len(assets) > 0); err != nil {
		return ReconcileResult{}, err
	}

	after, err := c.SetSatisfaction(ctx, desiredItemID, content.Satisfaction, placement.Satisfaction)
	if err != nil {
		return ReconcileResult{}, err
	}

	return ReconcileResult{
		DesiredItemID:     desiredItemID,
		Content:           content,
		Placement:         placement,
		PlacementUnproven: soleSelf,
		Changed:           after.State != before.State,
		State:             after.State,
	}, nil
}

// setManaged records whether Heyarr holds bytes for this want.
//
// It is written before the axes because Validate refuses "content satisfied
// while holding nothing", and the two have to move together or the write is
// rejected by the state machine that exists to catch exactly that.
func (c *Catalog) setManaged(ctx context.Context, desiredItemID string, managed bool) error {
	flag := 0
	if managed {
		flag = 1
	}
	now := c.clock.Now().Format(timestampFormat)
	return c.db.InTx(ctx, func(tx *sql.Tx) error {
		// Only clears content when it has to. Setting managed = 0 while
		// content says satisfied would violate the CHECK, so the two move in
		// one statement.
		_, err := tx.ExecContext(ctx, `
			UPDATE acquisition_state
			   SET managed = ?,
			       content = CASE WHEN ? = 0 AND content = 'satisfied' THEN 'not_satisfied' ELSE content END,
			       placement = CASE WHEN ? = 0 AND content = 'satisfied' THEN 'unknown' ELSE placement END,
			       updated_at = ?
			 WHERE desired_item_id = ?`,
			flag, flag, flag, now, desiredItemID)
		return err
	})
}

// desiredWant is the slice of a DesiredItem reconciliation needs.
type desiredWant struct {
	id               string
	scope            string
	workID           string
	editionID        string
	qualityProfileID string
}

func (c *Catalog) desiredForReconcile(ctx context.Context, id string) (desiredWant, error) {
	var w desiredWant
	var edition sql.NullString
	err := c.db.Reader().QueryRowContext(ctx, `
		SELECT id, scope, work_id, edition_id, quality_profile_id
		FROM desired_items WHERE id = ?`, id).
		Scan(&w.id, &w.scope, &w.workID, &edition, &w.qualityProfileID)
	if errors.Is(err, sql.ErrNoRows) {
		return desiredWant{}, fmt.Errorf("catalog: no desired item %s: %w", id, err)
	}
	if edition.Valid {
		w.editionID = edition.String
	}
	return w, err
}

func (c *Catalog) profileForReconcile(ctx context.Context, id string) (policy.Profile, error) {
	var p policy.Profile
	var accept, prefer, terminal string
	err := c.db.Reader().QueryRowContext(ctx, `
		SELECT id, name, description, accept, prefer, terminal
		FROM quality_profiles WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &p.Description, &accept, &prefer, &terminal)
	if err != nil {
		return policy.Profile{}, fmt.Errorf("catalog: reading the quality profile: %w", err)
	}
	for _, pair := range []struct {
		raw  string
		dest *[]policy.Rule
	}{{accept, &p.Accept}, {prefer, &p.Prefer}, {terminal, &p.Terminal}} {
		if err := json.Unmarshal([]byte(pair.raw), pair.dest); err != nil {
			return policy.Profile{}, fmt.Errorf("catalog: decoding profile rules: %w", err)
		}
	}
	return p, nil
}

// assetsForWant gathers the assets under a want's target, with the attributes
// the evaluator scores.
//
// # Where the attributes come from, and what is missing
//
// resolution, video_codec, audio_codec, audio_channels and hdr come from the
// PROBE, which may not exist — a node with no ffprobe leaves probes pending by
// design (ADR-0023). size_bytes comes from the blob and is always known.
// source and language come from the edition, and are empty when identification
// could not tell.
//
// Anything unknown is LEFT OUT rather than defaulted, so §63 reports
// "undetermined" instead of confidently reporting a zero. That is what makes a
// node with no toolchain report "I cannot tell whether this satisfies you"
// rather than "this does not satisfy you", which are different problems.
func (c *Catalog) assetsForWant(ctx context.Context, w desiredWant) ([]acquisition.AssetView, error) {
	where := "e.work_id = ?"
	args := []any{w.workID}
	if w.scope == "edition" {
		where = "a.edition_id = ?"
		args = []any{w.editionID}
	}

	//nolint:gosec // assembled only from the literal fragments above; every value is bound
	stmt := `
		SELECT a.id, a.source_class, a.blob_hash,
		       b.size,
		       e.edition_type, e.language,
		       p.container, p.bitrate_bps, p.streams
		FROM assets a
		JOIN editions e ON e.id = a.edition_id
		LEFT JOIN blobs b ON b.hash = a.blob_hash
		LEFT JOIN blob_probes p ON p.blob_hash = a.blob_hash
		WHERE ` + where + `
		  AND a.missing_since IS NULL
		ORDER BY a.id`

	rows, err := c.db.Reader().QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading the assets for %s: %w", w.id, err)
	}
	defer func() { _ = rows.Close() }()

	var out []acquisition.AssetView
	for rows.Next() {
		var (
			id, sourceClass        string
			blobHash               sql.NullString
			size, bitrate          sql.NullInt64
			editionType, language  sql.NullString
			container, streamsJSON sql.NullString
		)
		if err := rows.Scan(&id, &sourceClass, &blobHash, &size,
			&editionType, &language, &container, &bitrate, &streamsJSON); err != nil {
			return nil, err
		}

		view := acquisition.AssetView{
			ID: id, SourceClass: sourceClass,
			Attributes: acquisition.Attributes{},
		}
		if blobHash.Valid {
			view.BlobHash = blobHash.String
		}
		if size.Valid {
			view.Attributes[policy.AttrSizeBytes] = policy.Num(size.Int64)
		}
		if editionType.Valid && strings.TrimSpace(editionType.String) != "" {
			view.Attributes[policy.AttrSource] = policy.Text(editionType.String)
		}
		if language.Valid && strings.TrimSpace(language.String) != "" {
			view.Attributes[policy.AttrLanguage] = policy.Text(language.String)
		}
		if streamsJSON.Valid {
			applyProbeAttributes(view.Attributes, streamsJSON.String)
		}
		out = append(out, view)
	}
	return out, rows.Err()
}

// probeStream is the fragment of a probe result the quality attributes come
// from. Declared here rather than imported from the API layer so persistence
// does not depend on the HTTP package for a shape.
type probeStream struct {
	Type     string `json:"type"`
	Codec    string `json:"codec"`
	Width    int64  `json:"width"`
	Height   int64  `json:"height"`
	Channels int64  `json:"channels"`
	Profile  string `json:"profile"`
}

// applyProbeAttributes fills in what the probe knows.
//
// A stream the probe did not describe leaves its attribute ABSENT, which §63
// reports as undetermined. That is the difference between "this file is not
// HDR" and "nobody looked", and the whole reason an attribute map is sparse
// rather than a struct of zero values.
func applyProbeAttributes(attrs acquisition.Attributes, streamsJSON string) {
	var streams []probeStream
	if err := json.Unmarshal([]byte(streamsJSON), &streams); err != nil {
		// A malformed probe row tells us nothing, which is exactly what an
		// absent attribute means. Not an error: one bad row must not stop a
		// library-wide reconciliation.
		return
	}
	for _, s := range streams {
		switch s.Type {
		case "video":
			if _, seen := attrs[policy.AttrVideoCodec]; seen {
				continue
			}
			if s.Codec != "" {
				attrs[policy.AttrVideoCodec] = policy.Text(s.Codec)
			}
			// The class, not the frame's pixel height: a 2.35:1 1080p
			// master is 1920x816, and taking the height rejected it as
			// sub-1080 (#231). policy.ResolutionClass explains the ladder.
			if class, ok := policy.ResolutionClass(s.Width, s.Height); ok {
				attrs[policy.AttrResolution] = policy.Num(class)
			}
			// HDR is a substring match on the stream profile, and it is a
			// KNOWN WEAKNESS carried over from Milestone 2 rather than a
			// finding. It is applied only when the profile string is present
			// at all: with no profile the attribute stays absent, so a profile
			// preferring HDR reports "could not determine" rather than
			// confidently reporting false.
			if s.Profile != "" {
				attrs[policy.AttrHDR] = policy.Flag(
					strings.Contains(strings.ToLower(s.Profile), "hdr"))
			}
		case "audio":
			if _, seen := attrs[policy.AttrAudioCodec]; seen {
				continue
			}
			if s.Codec != "" {
				attrs[policy.AttrAudioCodec] = policy.Text(s.Codec)
			}
			if s.Channels > 0 {
				attrs[policy.AttrAudioChannels] = policy.Num(s.Channels)
			}
		}
	}
}

// requiredPeers is §56's Full Peer target set, and whether that set is this
// node alone.
//
// Every peer in `full` mode, which is the whole of placement policy until
// §34's placement policies land. Until then "every full peer holds everything"
// is the §19 full-replica default, and saying so here is more honest than
// inventing a policy table nothing writes.
//
// ## The single-peer answer, and why it is read here
//
// This returned exactly one peer in every deployment that existed before
// Milestone 4, and it still does in most of them: a Heyarr on one machine is a
// supported, permanent configuration, not a stage on the way to something. In
// that deployment placement is satisfied the moment content is and `converging`
// is unreachable — not because replication worked, but because there is nowhere
// for bytes to converge to.
//
// `soleSelf` is that condition, and it is the value the API's `unproven` field
// is computed from (ADR-0027, M4-11). It is read from the same row set as the
// target set, in the same query, because the two answers have to be about the
// same fabric: computing them from separate reads would let a peer enrolled
// between them make the pair disagree.
func (c *Catalog) requiredPeers(ctx context.Context) (peers []string, soleSelf bool, err error) {
	rows, err := c.db.Reader().QueryContext(ctx,
		`SELECT id, is_self FROM peers WHERE mode = 'full' ORDER BY id`)
	if err != nil {
		return nil, false, fmt.Errorf("catalog: reading the full peers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	var selves int
	for rows.Next() {
		var id string
		var isSelf int
		if err := rows.Scan(&id, &isSelf); err != nil {
			return nil, false, err
		}
		out = append(out, id)
		selves += isSelf
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	// One member, and it is this node. Both halves matter: a target set of one
	// peer that is NOT this node is a real replication question with a real
	// answer, and calling that unproven would be the same lie in the other
	// direction.
	return out, len(out) == 1 && selves == 1, nil
}

// replicasOf reports which peers hold verified bytes for a blob.
//
// `present` only. A pending or corrupt replica is not a replica for placement
// purposes: §56 asks whether the content is replicated, and bytes that failed
// verification are not replicated, they are quarantined (ADR-0018).
func (c *Catalog) replicasOf(ctx context.Context, blobHash string) ([]acquisition.PeerReplica, error) {
	if blobHash == "" {
		return nil, nil
	}
	rows, err := c.db.Reader().QueryContext(ctx,
		`SELECT peer_id, state FROM replicas WHERE blob_hash = ? ORDER BY peer_id`, blobHash)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading the replicas of %s: %w", blobHash, err)
	}
	defer func() { _ = rows.Close() }()

	var out []acquisition.PeerReplica
	for rows.Next() {
		var peerID, state string
		if err := rows.Scan(&peerID, &state); err != nil {
			return nil, err
		}
		out = append(out, acquisition.PeerReplica{PeerID: peerID, Present: state == "present"})
	}
	return out, rows.Err()
}

// DesiredItemsToReconcile lists every want, oldest first.
//
// The whole library, on every pass. A DesiredItem's satisfaction can change
// without the want being touched — a profile edit, a deleted asset, a peer
// going away (§57) — so a sweep that only looked at recently-changed wants
// would miss exactly the cases reconciliation exists for.
func (c *Catalog) DesiredItemsToReconcile(ctx context.Context, limit int) ([]string, error) {
	rows, err := c.db.Reader().QueryContext(ctx,
		`SELECT id FROM desired_items ORDER BY created_at, id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing desired items: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
