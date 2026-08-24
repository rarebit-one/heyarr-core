package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

// Release candidates and their evaluations (§63, M3-12).
//
// # The edge, again
//
// The decision — which candidate is best, and why each was or was not
// acceptable — is a pure function in internal/domain/acquisition. What lives
// here is the write that makes the answer durable, because §63's
// "inspectable" is a claim about after the fact and nothing in memory is.
//
// # Storing the evaluation verbatim is the point
//
// The evaluation column holds exactly what json.Marshal produced from what
// M3-04 returned. Re-deriving it on read, or decomposing it into columns that
// this file would then have to keep in step with the evaluator, would make the
// stored explanation drift from the explanation that was actually used — and
// an explanation that might not be the real one is worse than none, because it
// will be believed.

// Candidate is one release, as offered and as judged.
type Candidate struct {
	ID             string
	DesiredItemID  string
	SearchID       string
	Provider       string
	CandidateID    string
	Title          string
	Attributes     acquisition.Attributes
	Evaluation     acquisition.Evaluation
	Selected       bool
	Overridden     bool
	OverrideDetail string
	SearchedAt     time.Time
}

// SearchOutcome is what one search concluded.
type SearchOutcome struct {
	SearchID string
	// Found is how many candidates every indexer offered between them, after
	// the registry deduplicated across them.
	Found int
	// Acceptable is how many passed every gate. Zero with Found > 0 is the
	// twelve-rejections case, and it is a normal, explainable outcome rather
	// than a failure.
	Acceptable int
	// SelectedCandidateID is the provider's id for the chosen release, empty
	// when nothing was acceptable.
	SelectedCandidateID string
}

// ErrNoCandidate is returned when a want has no candidate with that id.
var ErrNoCandidate = errors.New("catalog: no such candidate for that desired item")

// ErrNotAcceptable is returned when an override names a candidate the scorer
// rejected outright.
//
// A distinct error because it is the one refusal an operator will actually
// meet, and it needs to reach them as a 409 with a reason rather than as a 500.
var ErrNotAcceptable = errors.New("catalog: that candidate was rejected by the quality profile")

const candidateCols = `id, desired_item_id, search_id, provider, candidate_id, title,
	attributes, evaluation, selected, overridden, override_detail, searched_at`

func scanCandidate(row interface{ Scan(...any) error }) (Candidate, error) {
	var c Candidate
	var attrs, eval, searchedAt string
	var selected, overridden int
	if err := row.Scan(&c.ID, &c.DesiredItemID, &c.SearchID, &c.Provider, &c.CandidateID,
		&c.Title, &attrs, &eval, &selected, &overridden, &c.OverrideDetail,
		&searchedAt); err != nil {
		return Candidate{}, err
	}
	c.Selected = selected == 1
	c.Overridden = overridden == 1
	if err := json.Unmarshal([]byte(attrs), &c.Attributes); err != nil {
		return Candidate{}, fmt.Errorf("catalog: decoding candidate attributes: %w", err)
	}
	if err := json.Unmarshal([]byte(eval), &c.Evaluation); err != nil {
		return Candidate{}, fmt.Errorf("catalog: decoding a stored evaluation: %w", err)
	}
	c.SearchedAt = parseStamp(searchedAt)
	return c, nil
}

func parseStamp(s string) time.Time {
	t, err := time.Parse(timestampFormat, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// RecordSearch replaces a want's candidates with what a search just found.
//
// REPLACES, in one transaction. A search supersedes its predecessor — the
// previous answer was computed against an indexer's state that no longer
// exists — and doing it atomically means a reader never sees half of one
// search and half of another, which would make a ranking that never existed
// look like the one that was used.
//
// It emits exactly once, for the search, including when nothing was found. An
// empty search is the outcome that most needs a record: it leaves no candidate
// rows behind to explain itself, so without the event a want that found
// nothing is indistinguishable from a want nobody searched.
// incumbent is what the want already holds, and a zero AssetID means nothing
// acceptable is. It is a PARAMETER rather than a read inside this method
// because the caller has already gathered it with the rest of the search
// context, and a second read here could disagree with the one the handler
// reasoned about.
func (c *Catalog) RecordSearch(
	ctx context.Context, desiredItemID string, ranked []acquisition.Ranked,
	incumbent acquisition.Incumbent,
) (SearchOutcome, error) {
	searchID := uuid.Must(uuid.NewV7()).String()
	now := c.clock.Now()
	stamp := now.Format(timestampFormat)

	outcome := SearchOutcome{SearchID: searchID, Found: len(ranked)}
	// BestOver, not Best: for a satisfied want this is an upgrade search, and
	// an upgrade must be strictly better than what is held (#229).
	if best, ok := acquisition.BestOver(ranked, incumbent); ok {
		outcome.SelectedCandidateID = best.Candidate.ID
	}
	for _, r := range ranked {
		if r.Evaluation.Accepted {
			outcome.Acceptable++
		}
	}

	var ev events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM release_candidates WHERE desired_item_id = ?`, desiredItemID); err != nil {
			return fmt.Errorf("catalog: clearing previous candidates: %w", err)
		}

		for _, r := range ranked {
			attrs, err := json.Marshal(r.Candidate.Attributes)
			if err != nil {
				return fmt.Errorf("catalog: encoding candidate attributes: %w", err)
			}
			// VERBATIM. Whatever the evaluator produced is what is stored; see
			// the file comment.
			eval, err := json.Marshal(r.Evaluation)
			if err != nil {
				return fmt.Errorf("catalog: encoding an evaluation: %w", err)
			}
			// The best ACCEPTABLE candidate is selected. Not ranked[0]: with
			// nothing acceptable the first row is merely the least bad, and
			// selecting it is exactly what §62's gates exist to prevent.
			selected := 0
			if r.Candidate.ID == outcome.SelectedCandidateID && r.Evaluation.Accepted {
				selected = 1
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO release_candidates
					(id, desired_item_id, search_id, provider, candidate_id, title,
					 attributes, evaluation, accepted, score, terminal, selected,
					 overridden, override_detail, searched_at, created_at, source)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '', ?, ?, ?)`,
				uuid.Must(uuid.NewV7()).String(), desiredItemID, searchID,
				r.Candidate.Provider, r.Candidate.ID, r.Candidate.Title,
				string(attrs), string(eval),
				boolToInt(r.Evaluation.Accepted), r.Evaluation.Score,
				boolToInt(r.Evaluation.Terminal), selected,
				stamp, stamp, r.Candidate.Source.Reveal()); err != nil {
				return fmt.Errorf("catalog: recording candidate %s: %w", r.Candidate.ID, err)
			}
		}

		var emitErr error
		ev, emitErr = c.events.EmitTx(ctx, tx, events.TypeSearchCompleted,
			"desired_item", desiredItemID, map[string]any{
				"desired_item_id": desiredItemID,
				"search_id":       searchID,
				"found":           outcome.Found,
				"acceptable":      outcome.Acceptable,
				"selected":        outcome.SelectedCandidateID,
			})
		return emitErr
	})
	if err != nil {
		return SearchOutcome{}, err
	}
	c.events.Publish(ev)
	return outcome, nil
}

// CandidatesFor lists a want's candidates, best first.
//
// Accepted before rejected, then by score descending, then by the provider's
// candidate id — the same total order M3-04 ranks by, so the listing and the
// selection cannot disagree about which is best.
func (c *Catalog) CandidatesFor(ctx context.Context, desiredItemID string) ([]Candidate, error) {
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT `+candidateCols+` FROM release_candidates
		WHERE desired_item_id = ?
		ORDER BY accepted DESC, score DESC, candidate_id ASC`, desiredItemID)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing candidates for %s: %w", desiredItemID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Candidate
	for rows.Next() {
		cand, err := scanCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, cand)
	}
	return out, rows.Err()
}

// SelectedCandidate returns the candidate a want is acquiring, if any.
func (c *Catalog) SelectedCandidate(ctx context.Context, desiredItemID string) (Candidate, error) {
	cand, err := scanCandidate(c.db.Reader().QueryRowContext(ctx,
		`SELECT `+candidateCols+` FROM release_candidates
		 WHERE desired_item_id = ? AND selected = 1`, desiredItemID))
	if errors.Is(err, sql.ErrNoRows) {
		return Candidate{}, ErrNoCandidate
	}
	return cand, err
}

// OverrideSelection records a PERSON choosing a candidate against the scorer.
//
// It records what the scorer had said instead, because an override that left no
// trace would look exactly like an ordinary selection — and "why did it take
// that one" would then have no answer, which is the property §60's
// deterministic scoring exists to give.
//
// It refuses a candidate the profile REJECTED. That is a real refusal rather
// than a nicety: the gates in §62 are the operator's own statement of what is
// acceptable, and an override that could ignore them would make "accept" a
// suggestion. Wanting something outside the profile is expressible — by
// changing the profile, which is a visible, durable act.
func (c *Catalog) OverrideSelection(
	ctx context.Context, desiredItemID, candidateID string,
) (Candidate, error) {
	now := c.clock.Now().Format(timestampFormat)

	var (
		chosen Candidate
		ev     events.Event
	)
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		var err error
		chosen, err = scanCandidate(tx.QueryRowContext(ctx,
			`SELECT `+candidateCols+` FROM release_candidates
			 WHERE desired_item_id = ? AND candidate_id = ?`, desiredItemID, candidateID))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrNoCandidate, candidateID)
		}
		if err != nil {
			return err
		}
		if !chosen.Evaluation.Accepted {
			return fmt.Errorf("%w: %s", ErrNotAcceptable, candidateID)
		}

		// What the scorer would have chosen, so the disagreement is recorded
		// rather than merely the departure.
		previous, prevErr := scanCandidate(tx.QueryRowContext(ctx,
			`SELECT `+candidateCols+` FROM release_candidates
			 WHERE desired_item_id = ? AND selected = 1`, desiredItemID))
		// Every branch below sets this, and there is deliberately no default
		// value: a half-written override detail would be worse than none,
		// because it would look like a record of a decision nobody made.
		var detail string
		switch {
		case errors.Is(prevErr, sql.ErrNoRows):
			detail = fmt.Sprintf("chosen by hand; the scorer had selected nothing, "+
				"and this candidate scores %d", chosen.Evaluation.Score)
		case prevErr != nil:
			return prevErr
		case previous.CandidateID == chosen.CandidateID:
			// Overriding to the candidate already chosen is not a
			// disagreement. Recording it as one would put a departure in the
			// audit trail that never happened.
			detail = ""
		default:
			detail = fmt.Sprintf("chosen by hand over %s from %s, which scored %d; "+
				"this candidate scores %d",
				previous.CandidateID, previous.Provider,
				previous.Evaluation.Score, chosen.Evaluation.Score)
		}

		// Clear first: the partial unique index permits exactly one selected
		// row per want, so setting the new one before clearing the old would
		// be refused by the database.
		if _, err := tx.ExecContext(ctx, `
			UPDATE release_candidates SET selected = 0, overridden = 0, override_detail = ''
			WHERE desired_item_id = ? AND selected = 1`, desiredItemID); err != nil {
			return err
		}
		overridden := 1
		if detail == "" {
			// Re-selecting what was already selected. Still a selection, not
			// an override.
			overridden = 0
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE release_candidates
			   SET selected = 1, overridden = ?, override_detail = ?
			 WHERE id = ?`, overridden, detail, chosen.ID); err != nil {
			return err
		}
		chosen.Selected = true
		chosen.Overridden = overridden == 1
		chosen.OverrideDetail = detail

		if overridden == 0 {
			return nil
		}
		var emitErr error
		ev, emitErr = c.events.EmitTx(ctx, tx, events.TypeCandidateOverridden,
			"desired_item", desiredItemID, map[string]any{
				"desired_item_id": desiredItemID,
				"candidate_id":    chosen.CandidateID,
				"provider":        chosen.Provider,
				"score":           chosen.Evaluation.Score,
				"detail":          detail,
				"at":              now,
			})
		return emitErr
	})
	if err != nil {
		return Candidate{}, err
	}
	if chosen.Overridden {
		c.events.Publish(ev)
	}
	return chosen, nil
}

// SearchContext is what the search job needs to build a query.
type SearchContext struct {
	DesiredItemID string
	Title         string
	Year          int
	ContentType   string
	Profile       policy.Profile
	State         acquisition.State
	// Incumbent is the evaluation of what this want ALREADY holds, and its
	// AssetID is empty when it holds nothing acceptable.
	//
	// Read here because a search over a SATISFIED want is an upgrade search,
	// and an upgrade search that does not know what is held cannot tell an
	// improvement from the release it already has. Without it, the search beat
	// re-selected the byte-identical incumbent and dragged the want backwards
	// out of satisfaction (#229).
	//
	// Recomputed rather than cached, for the same reason ScanForUpgrades
	// recomputes it: a profile edit changes the answer, and a stored score
	// would measure against a standard nobody is using any more.
	Incumbent acquisition.Evaluation
	// IncumbentID is the asset that satisfies the want, empty when none does.
	//
	// Carried separately because Evaluation describes a RELEASE and this names
	// an ASSET; folding the asset id into the evaluation would make the two
	// interchangeable, which is exactly the confusion between "what we could
	// get" and "what we have" that this whole comparison exists to keep clear.
	IncumbentID string
}

// SearchContextFor gathers everything one search needs, in one read.
func (c *Catalog) SearchContextFor(ctx context.Context, desiredItemID string) (SearchContext, error) {
	var (
		out       SearchContext
		year      sql.NullInt64
		profileID string
	)
	err := c.db.Reader().QueryRowContext(ctx, `
		SELECT d.id, w.title, w.year, w.content_type, d.quality_profile_id
		FROM desired_items d
		JOIN works w ON w.id = d.work_id
		WHERE d.id = ?`, desiredItemID).
		Scan(&out.DesiredItemID, &out.Title, &year, &out.ContentType, &profileID)
	if errors.Is(err, sql.ErrNoRows) {
		return SearchContext{}, fmt.Errorf("catalog: no desired item %s: %w", desiredItemID, err)
	}
	if err != nil {
		return SearchContext{}, fmt.Errorf("catalog: reading the search context: %w", err)
	}
	if year.Valid {
		out.Year = int(year.Int64)
	}

	profile, err := c.profileForReconcile(ctx, profileID)
	if err != nil {
		return SearchContext{}, err
	}
	out.Profile = profile

	rec, err := c.Acquisition(ctx, desiredItemID)
	if err != nil {
		return SearchContext{}, err
	}
	out.State = rec.State

	// What is already held, when anything is. Only for a satisfied want: for
	// any other the evaluation is empty by construction and the extra queries
	// would be spent proving it.
	if rec.State.Content == acquisition.SatisfactionSatisfied {
		incumbent, incumbentID, err := c.incumbentEvaluation(ctx, desiredItemID)
		if err != nil {
			return SearchContext{}, err
		}
		out.Incumbent, out.IncumbentID = incumbent, incumbentID
	}
	return out, nil
}

// PruneCandidates removes candidate sets older than a cutoff.
//
// GLOBAL rather than per want, and that is the whole reason it exists.
// Replacement already bounds the table in the normal path — a new search
// replaces the previous set — so the only rows that accumulate belong to wants
// nobody searches any more, and a per-want prune can by definition never reach
// them.
//
// A SELECTED candidate is never pruned however old it is: it explains what a
// want is currently acquiring, and removing it would leave an acquisition in
// flight with nothing to say why.
func (c *Catalog) PruneCandidates(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := c.db.Writer().ExecContext(ctx, `
		DELETE FROM release_candidates
		WHERE searched_at < ? AND selected = 0`, olderThan.Format(timestampFormat))
	if err != nil {
		return 0, fmt.Errorf("catalog: pruning candidates: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}

// DesiredWork is the Work a want points at — its identity, not its contents.
//
// Separate from SearchContext because the two are read at different moments for
// different reasons: a search needs a title to ask an indexer about, and an
// ingest needs the identity of the Work an arriving asset must attach to.
type DesiredWork struct {
	ID          string
	ContentType string
	WorkKey     string
	Title       string
	SortTitle   string
	Year        int
}

// WorkForDesired is the Work a want is about.
//
// # Why an acquisition needs this and a scan does not
//
// A library scan has no source of truth but the path, so it parses one — that
// is what identification.Registry is for, and its guesses are honest guesses.
//
// An acquisition is the opposite situation: something asked for this, by Work
// id, and that is authoritative. Re-deriving identity from the downloaded
// filename discards the one fact that was certain, and the two disagree
// ROUTINELY — release titles carry extensions, scene tags and the indexer's own
// normalisation, so the case where they match is the lucky one (#224).
func (c *Catalog) WorkForDesired(ctx context.Context, desiredItemID string) (DesiredWork, error) {
	var (
		out  DesiredWork
		year sql.NullInt64
	)
	err := c.db.Reader().QueryRowContext(ctx, `
		SELECT w.id, w.content_type, w.work_key, w.title, w.sort_title, w.year
		FROM desired_items d
		JOIN works w ON w.id = d.work_id
		WHERE d.id = ?`, desiredItemID).
		Scan(&out.ID, &out.ContentType, &out.WorkKey, &out.Title, &out.SortTitle, &year)
	if errors.Is(err, sql.ErrNoRows) {
		return DesiredWork{}, fmt.Errorf("catalog: no desired item %s: %w", desiredItemID, err)
	}
	if err != nil {
		return DesiredWork{}, err
	}
	out.Year = int(year.Int64)
	return out, nil
}

// ErrNoSelection is returned when a want has no selected candidate.
//
// Distinct from ErrNoCandidate — which is "you named one that is not there" —
// because this one is an ordinary race rather than a mistake: a grab job is
// durable and re-runnable (invariant 9), and by the time it runs the search
// that selected the release may have been superseded by another that selected
// nothing. The grab treats it as work that no longer applies, not as a failure.
var ErrNoSelection = errors.New("catalog: this want has no selected candidate")

// SelectedSource is where the want's chosen release is fetched from.
//
// # Why this is its own read and not a column on Candidate
//
// The value is a credential — on a private tracker the magnet carries a
// passkey — and Candidate is what the API returns and what §63's explanations
// are built from. Adding a field there would put the credential one accidental
// serialisation away from every candidate listing, and the redaction would be
// the only thing standing between it and a response body.
//
// So `source` is deliberately absent from candidateCols, and the ONE caller
// that needs it asks for it by name. That makes `grep -rn SelectedSource` the
// complete list of places this value is read, which is the same property
// secret.Value's Reveal gives inside the process.
//
// Returns the candidate id alongside it so the caller can record WHICH release
// it grabbed without a second query — and so a grab can tell that the selection
// changed under it rather than silently fetching a different release than the
// one whose id it was enqueued with.
func (c *Catalog) SelectedSource(ctx context.Context, desiredItemID string) (
	candidateID string, source secret.Value, err error,
) {
	var raw string
	err = c.db.Reader().QueryRowContext(ctx,
		`SELECT candidate_id, source FROM release_candidates
		 WHERE desired_item_id = ? AND selected = 1`, desiredItemID).
		Scan(&candidateID, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("%w: %s", ErrNoSelection, desiredItemID)
	}
	if err != nil {
		return "", "", err
	}
	return candidateID, secret.Value(raw), nil
}

// Held is what this want already holds, in the shape the domain compares
// against. A zero AssetID means nothing acceptable is held.
func (sc SearchContext) Held() acquisition.Incumbent {
	return acquisition.Incumbent{AssetID: sc.IncumbentID, Evaluation: sc.Incumbent}
}
