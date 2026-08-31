package resources

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/desired"
	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
)

// Desired items (§55) — the endpoint that makes Heyarr more than a catalogue.
//
// # Wanting has to work when you have nothing
//
// The requirement most easily lost here is that a want must be expressible for
// content with no Asset, no Blob and no bytes anywhere. Every fixture in this
// repository has assets, so a design that only works once something exists
// passes every test and fails the first real use.
//
// That is why creation accepts a work DESCRIPTOR as well as a work id: asking
// an operator to first create a Work by hand, so they can then want it, is
// asking them to catalogue something they do not have. The descriptor resolves
// through the same (content_type, work_key) get-or-create the scanner uses
// (M1-11), so wanting a film and later scanning it converge on one Work rather
// than producing two.

// DesiredItem is the wire type.
type DesiredItem struct {
	ID    string `json:"id"`
	Scope string `json:"scope"`

	WorkID string `json:"work_id"`
	// EditionID is absent at work scope rather than null, so a client cannot
	// read it without noticing the scope.
	EditionID string `json:"edition_id,omitempty"`

	QualityProfileID string `json:"quality_profile_id"`
	Monitor          bool   `json:"monitor"`
	Reason           string `json:"reason,omitempty"`

	// Acquisition is where this want has got to (§64).
	//
	// It carries the derived §64 NAME and both of §56's axes. Showing only the
	// name would reintroduce at the edge exactly the collapse the storage
	// model exists to prevent: a client that can see CONTENT_SATISFIED but not
	// which of the two questions was answered cannot tell "we have it" from
	// "we have it everywhere".
	Acquisition *AcquisitionView `json:"acquisition,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AcquisitionView is §64's state, presented as the four facts it is made of
// plus the name the spec gives their combination.
type AcquisitionView struct {
	// State is §64's name — derived, never stored (ADR-0027).
	State string `json:"state"`
	// Phase is the pipeline position. There is no "missing" or "available"
	// phase: both mean "nothing in flight" and differ only in `managed`.
	Phase string `json:"phase"`
	// Managed is whether Heyarr holds bytes for this want.
	Managed bool `json:"managed"`
	// Content answers "do we hold bytes the profile accepts?" (§56)
	Content string `json:"content"`
	// Placement answers "are those bytes on every peer that should hold them?"
	//
	// ## PROVEN as of Milestone 4
	//
	// `converging` was unreachable outside a test with a synthetic peer set for
	// as long as one peer was the only deployment that existed (ADR-0010). A
	// second Full Peer now exists and real bytes move between the two, so this
	// value is observed as `converging` mid-transfer and `satisfied` after.
	//
	// On a deployment whose Full Peer target set is this node alone it is still
	// `satisfied` the moment content is, because there is nowhere for bytes to
	// converge to. That case is reported explicitly — GET
	// /api/v1/desired/{id}/satisfaction answers `placement.unproven: true` for
	// it — rather than being left for a reader of this listing to infer.
	Placement string `json:"placement"`
	// Detail is why the last pipeline move happened, when it was a failure.
	Detail string `json:"detail,omitempty"`
}

// AcquisitionView carries only the STATE. The reasons behind the content axis —
// which assets were considered and why each was or was not good enough — are
// deliberately not inlined here: a listing of fifty wants would carry fifty
// evaluations of several rules each, and the answer to "why is this not
// satisfied" is wanted for one want at a time.
//
// It is reachable per want at GET /api/v1/desired/{id}/satisfaction, which is
// the same shape §63's evaluation endpoint takes for candidates.

// WantContentRequest is the intent behind POST /desired and MCP's
// want_content: make this content exist under these conditions.
//
// Exactly one of WorkID and Work is required. The descriptor exists so that
// wanting something absent does not require cataloguing it first.
type WantContentRequest struct {
	Scope     string `json:"scope"`
	WorkID    string `json:"work_id"`
	EditionID string `json:"edition_id"`
	// Work names content that may not exist yet. Resolved through the same
	// get-or-create as the scanner, so a want and a later scan converge.
	Work *WorkDescriptor `json:"work"`

	QualityProfileID string `json:"quality_profile_id"`
	// QualityProfile names a profile by NAME rather than id. A person writing
	// this by hand knows "living-room", not a UUID.
	QualityProfile string `json:"quality_profile"`

	// Monitor defaults to TRUE when absent. Wanting something and then
	// silently never looking for anything better is the surprising default;
	// §60 keeps monitoring as a first-class idea.
	Monitor *bool  `json:"monitor"`
	Reason  string `json:"reason"`
}

// WorkDescriptor names content semantically, whether or not it exists.
type WorkDescriptor struct {
	ContentType string `json:"content_type"`
	Title       string `json:"title"`
	Year        int    `json:"year"`
}

// UpdateDesiredRequest changes a want. Every field is a pointer: absent means
// "leave it alone", which is what makes a PATCH a PATCH — and what lets MCP's
// monitor_content set one field without restating the rest.
type UpdateDesiredRequest struct {
	QualityProfileID *string `json:"quality_profile_id"`
	QualityProfile   *string `json:"quality_profile"`
	Monitor          *bool   `json:"monitor"`
	Reason           *string `json:"reason"`
}

// desiredColumns selects a want and its acquisition state in one go.
//
// A LEFT JOIN rather than a per-item lookup: a page of fifty wants would
// otherwise be fifty-one queries, and that N+1 would arrive silently the first
// time somebody paged a real library. LEFT rather than INNER because a want
// whose acquisition row is missing must still be readable — the API says
// nothing about its state rather than hiding the want.
const desiredColumns = `d.id, d.scope, d.work_id, d.edition_id, d.quality_profile_id,
	d.monitor, d.reason, d.created_at, d.updated_at, ` + acquisitionSelect

const desiredFrom = ` FROM desired_items d
	LEFT JOIN acquisition_state a ON a.desired_item_id = d.id`

func scanDesiredItem(row interface{ Scan(...any) error }) (DesiredItem, error) {
	var d DesiredItem
	var edition sql.NullString
	var monitor int
	var created, updated string
	var phase, content, placement, detail sql.NullString
	var managed sql.NullInt64
	if err := row.Scan(&d.ID, &d.Scope, &d.WorkID, &edition, &d.QualityProfileID,
		&monitor, &d.Reason, &created, &updated,
		&phase, &managed, &content, &placement, &detail); err != nil {
		return DesiredItem{}, err
	}
	d.Acquisition = scanAcquisitionView(phase, managed, content, placement, detail)
	if edition.Valid {
		d.EditionID = edition.String
	}
	d.Monitor = monitor == 1
	d.CreatedAt = parseTime(created)
	d.UpdatedAt = parseTime(updated)
	return d, nil
}

func (d DesiredItem) domainItem() desired.Item {
	return desired.Item{
		ID:               d.ID,
		Scope:            desired.Scope(d.Scope),
		WorkID:           d.WorkID,
		EditionID:        d.EditionID,
		QualityProfileID: d.QualityProfileID,
		Monitor:          d.Monitor,
		Reason:           d.Reason,
	}
}

// listDesired pages by (created_at, id).
//
// A want has no name to sort by, and the interesting order is the order they
// were asked for. The id is in the sort key because created_at is not unique,
// and a non-unique keyset boundary skips and repeats rows exactly as OFFSET
// does.
func (a *API) listDesired(w http.ResponseWriter, r *http.Request) {
	q, err := parseQuery(r, "desired", 2)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	where := []string{"1 = 1"}
	args := []any{}
	if v, err := oneOf(r, "scope", "work", "edition"); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	} else if v != "" {
		where = append(where, "d.scope = ?")
		args = append(args, v)
	}
	if v, err := oneOf(r, "monitor", "true", "false"); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	} else if v != "" {
		where = append(where, "d.monitor = ?")
		args = append(args, map[string]int{"true": 1, "false": 0}[v])
	}
	if workID := r.URL.Query().Get("work_id"); workID != "" {
		where = append(where, "d.work_id = ?")
		args = append(args, workID)
	}
	if profile := r.URL.Query().Get("quality_profile_id"); profile != "" {
		where = append(where, "d.quality_profile_id = ?")
		args = append(args, profile)
	}
	// upgradable is the listing question §71's get_upgrade_candidates asks:
	// which wants are in a state where an upgrade COULD happen.
	//
	// It is answered from STATE alone — monitored, satisfied, and holding
	// bytes — and deliberately not by scoring anything. Whether a want has
	// actually reached its profile's terminal condition needs the incumbent
	// re-evaluated against the profile, which is a join and a scorer per row;
	// doing that here would make paging a library run the evaluator hundreds
	// of times to render one screen.
	//
	// So this filter is the CHEAP half of eligibility, and it is a superset:
	// a want that is monitored and satisfied but already terminal appears
	// here. The upgrade scan (M3-06) applies the terminal test, and
	// GET /desired/{id}/satisfaction reports it per want. That is stated in
	// the OpenAPI rather than left for a client to discover.
	if v, err := oneOf(r, "upgradable", "true", "false"); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	} else if v == "true" {
		where = append(where,
			"d.monitor = 1", "a.content = 'satisfied'", "a.managed = 1")
	} else if v == "false" {
		where = append(where,
			"(d.monitor = 0 OR a.content IS NULL OR a.content <> 'satisfied' OR a.managed = 0)")
	}
	if q.cursor != nil {
		where = append(where, "(d.created_at, d.id) > (?, ?)")
		args = append(args, q.cursor[0], q.cursor[1])
	}
	args = append(args, q.limit+1)

	//nolint:gosec // assembled only from the literal fragments above; every value is bound
	stmt := `SELECT ` + desiredColumns + desiredFrom + ` WHERE ` +
		strings.Join(where, " AND ") + ` ORDER BY d.created_at ASC, d.id ASC LIMIT ?`

	rows, err := a.reader.QueryContext(r.Context(), stmt, args...)
	if err != nil {
		a.fail(w, r, "desired item", err)
		return
	}
	defer func() { _ = rows.Close() }()

	var items []DesiredItem
	for rows.Next() {
		item, scanErr := scanDesiredItem(rows)
		if scanErr != nil {
			a.fail(w, r, "desired item", scanErr)
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		a.fail(w, r, "desired item", err)
		return
	}
	a.write(w, r, http.StatusOK, newPage(items, q.limit,
		func(x DesiredItem) []string {
			return []string{x.CreatedAt.Format(time.RFC3339Nano), x.ID}
		}, "desired"))
}

func (a *API) getDesired(w http.ResponseWriter, r *http.Request) {
	item, err := desiredByID(r.Context(), a.reader, chi.URLParam(r, "id"))
	if err != nil {
		a.fail(w, r, "desired item", err)
		return
	}
	a.write(w, r, http.StatusOK, item)
}

func desiredByID(ctx context.Context, q rowQuerier, id string) (DesiredItem, error) {
	return scanDesiredItem(q.QueryRowContext(ctx,
		`SELECT `+desiredColumns+desiredFrom+` WHERE d.id = ?`, id))
}

// acquisitionColumns is the acquisition half of a want's row, joined in rather
// than fetched per item: a list of fifty wants would otherwise be fifty-one
// queries, and the N+1 would arrive silently the first time somebody paged a
// real library.
const acquisitionSelect = `a.phase, a.managed, a.content, a.placement, a.detail`

func scanAcquisitionView(
	phase sql.NullString, managed sql.NullInt64,
	content, placement, detail sql.NullString,
) *AcquisitionView {
	if !phase.Valid {
		// No acquisition row. Possible only for a want created before this
		// migration, or one whose row was removed by hand; the API says
		// nothing rather than inventing a state.
		return nil
	}
	state := acquisition.State{
		Phase:     acquisition.Phase(phase.String),
		Managed:   managed.Int64 == 1,
		Content:   acquisition.Satisfaction(content.String),
		Placement: acquisition.Satisfaction(placement.String),
	}
	return &AcquisitionView{
		State:     state.Name(),
		Phase:     phase.String,
		Managed:   state.Managed,
		Content:   content.String,
		Placement: placement.String,
		Detail:    detail.String,
	}
}

// WantContent creates a desired item from an intent (§55).
//
// # Why this is exported, and why the handler is a shell around it
//
// Milestone 3 gives Heyarr a second front door: MCP (§71, ADR-0019). Wanting
// something is not "POST /desired with a different envelope" — it is an
// INTENT, and both doors express the same one. If each implemented it, the two
// would drift, and the drift would be silent: one door would emit the
// acquisition event and the other would not, or one would resolve a work
// descriptor with the scanner's normalisation and the other would not.
//
// That is not hypothetical in this package. The API once wrote an acquisition
// row directly while the catalog's own StartAcquisition emitted, and an
// acceptance assertion — not review — found that the two paths had silently
// diverged. One implementation, two callers, is the answer.
//
// It returns raw errors rather than problem documents. Mapping an error onto a
// status code is the HTTP layer's business, and MCP has to map the same errors
// onto JSON-RPC codes; a shared implementation that returned HTTP problems
// would have made the MCP door translate out of a vocabulary it does not speak.
func (a *API) WantContent(ctx context.Context, req WantContentRequest) (DesiredItem, error) {
	if req.WorkID != "" && req.Work != nil {
		return DesiredItem{}, &badRequest{errors.New(
			"name the work with either work_id or work, not both")}
	}
	if req.WorkID == "" && req.Work == nil {
		return DesiredItem{}, &badRequest{errors.New(
			"a desired item must name a work — by work_id, or by a work descriptor " +
				"if it does not exist yet")}
	}
	if req.QualityProfileID != "" && req.QualityProfile != "" {
		return DesiredItem{}, &badRequest{errors.New(
			"name the quality profile with either quality_profile_id or quality_profile, not both")}
	}

	monitor := true
	if req.Monitor != nil {
		monitor = *req.Monitor
	}

	// Resolving a profile name and getting-or-creating a work from a descriptor
	// are the API's business — the catalog's CreateDesiredItem takes ids already
	// resolved. Both happen first, in one transaction, so a want and its work
	// converge on the scanner's get-or-create (§55, and see resolveWorkDescriptor).
	var profileID, workID string
	if err := a.db.InTx(ctx, func(tx *sql.Tx) error {
		var err error
		profileID, err = a.resolveProfile(ctx, tx, req.QualityProfileID, req.QualityProfile)
		if err != nil {
			return err
		}
		workID = req.WorkID
		if req.Work != nil {
			workID, err = a.resolveWorkDescriptor(ctx, tx, *req.Work)
		}
		return err
	}); err != nil {
		return DesiredItem{}, err
	}

	item := desired.Item{
		ID:               a.newID(),
		Scope:            desired.Scope(req.Scope),
		WorkID:           workID,
		EditionID:        req.EditionID,
		QualityProfileID: profileID,
		Monitor:          monitor,
		Reason:           req.Reason,
	}
	// Validate here, wrapped as a client fault, so the two front doors classify
	// a malformed want identically (see ClientFault). CreateDesiredItem validates
	// again — it is the shared persistence path M12's poll worker also uses, and
	// it must not trust a hand-built caller — but by then a bad want is already
	// this door's 400 rather than the catalog's raw error.
	if err := item.Validate(); err != nil {
		return DesiredItem{}, &badRequest{err}
	}

	// The row, its resting acquisition state and both events, in one place
	// (invariant 7). This is the SAME path the poll_source worker projects an
	// episode through — one implementation, two callers, so the two cannot drift
	// about whether a create emits or leaves a want with no acquisition row.
	rec, err := a.catalog.CreateDesiredItem(ctx, item)
	if err != nil {
		return DesiredItem{}, err
	}
	out := DesiredItem{
		ID: rec.Item.ID, Scope: string(rec.Item.Scope), WorkID: rec.Item.WorkID,
		EditionID: rec.Item.EditionID, QualityProfileID: rec.Item.QualityProfileID,
		Monitor: rec.Item.Monitor, Reason: rec.Item.Reason,
		CreatedAt: rec.CreatedAt, UpdatedAt: rec.UpdatedAt,
		Acquisition: &AcquisitionView{
			State:     rec.State.Name(),
			Phase:     string(rec.State.Phase),
			Managed:   rec.State.Managed,
			Content:   string(rec.State.Content),
			Placement: string(rec.State.Placement),
		},
	}

	// Reconcile this want now rather than waiting for the beat (§57, M3-05).
	//
	// An operator who wants something they already own should see that
	// immediately; waiting up to a full sweep interval to be told "you have
	// this" makes a working system look broken. Scoped to the one want, so
	// wanting five things queues five quick jobs rather than five full sweeps.
	//
	// After the row is committed, and failure is not fatal: the beat will pick
	// this want up regardless, so a queue that is briefly unavailable costs
	// latency rather than correctness.
	if _, err := a.jobs.Enqueue(ctx, jobs.EnqueueOptions{
		Type:      acquisition.ReconcileJobType,
		Payload:   acquisition.ReconcilePayload{DesiredItemID: out.ID},
		DedupeKey: acquisition.ReconcileDedupeKey + ":" + out.ID,
	}); err != nil {
		a.log.Warn("could not enqueue reconciliation for a new want",
			"desired_item_id", out.ID, "error", err)
	}
	return out, nil
}

// createDesired is POST /api/v1/desired — a shell over WantContent.
func (a *API) createDesired(w http.ResponseWriter, r *http.Request) {
	var body WantContentRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	out, err := a.WantContent(r.Context(), body)
	if err != nil {
		a.failDesiredWrite(w, r, err)
		return
	}
	w.Header().Set("Location", httpapi.APIPrefix+"/desired/"+out.ID)
	a.write(w, r, http.StatusCreated, out)
}

// resolveProfile turns a profile id or name into an id, refusing both an
// unknown id and an unknown name by name.
func (a *API) resolveProfile(ctx context.Context, tx *sql.Tx, id, name string) (string, error) {
	switch {
	case id != "":
		var found string
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM quality_profiles WHERE id = ?`, id).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			return "", &badRequest{fmt.Errorf("there is no quality profile with the id %q", id)}
		}
		return found, err
	case name != "":
		var found string
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM quality_profiles WHERE name = ?`, name).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			return "", &badRequest{fmt.Errorf("there is no quality profile called %q", name)}
		}
		return found, err
	default:
		return "", &badRequest{errors.New("a desired item must name a quality profile — " +
			"\"this should exist\" with no statement of what would count as existing " +
			"cannot be evaluated (§56)")}
	}
}

// resolveWorkDescriptor finds or creates the Work a descriptor names.
//
// It converges on works(content_type, work_key) exactly as the scanner does
// (M1-11), so wanting a film and later scanning it produce ONE Work. Using a
// different key here would be the quiet kind of bug: everything works, and the
// library slowly fills with pairs of works that are the same thing.
func (a *API) resolveWorkDescriptor(ctx context.Context, tx *sql.Tx, d WorkDescriptor) (string, error) {
	contentType := strings.ToLower(strings.TrimSpace(d.ContentType))
	title := strings.TrimSpace(d.Title)
	if contentType == "" {
		return "", &badRequest{errors.New("a work descriptor needs a content_type")}
	}
	if title == "" {
		return "", &badRequest{errors.New("a work descriptor needs a title")}
	}
	if d.Year < 0 || d.Year > 9999 {
		return "", &badRequest{fmt.Errorf("a year of %d is not a year", d.Year)}
	}

	key, sortTitle := identification.Describe(contentType, title, d.Year)
	var id string
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM works WHERE content_type = ? AND work_key = ?`,
		contentType, key).Scan(&id)
	switch {
	case err == nil:
		return id, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", err
	}

	id = a.newID()
	now := a.now().UTC().Format(time.RFC3339Nano)
	var year any
	if d.Year > 0 {
		year = d.Year
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO works (id, content_type, work_key, title, sort_title, year,
			attributes, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, '{}', ?, ?)`,
		id, contentType, key, identification.DisplayTitle(title), sortTitle, year, now, now)
	if err != nil {
		return "", err
	}
	// A Work created because somebody wanted it is a state transition like any
	// other (invariant 7). It is emitted under content.* rather than desired.*
	// because a Work appearing is a fact about the catalog, and a subscriber
	// watching the catalog grow should see it however it was created.
	ev, err := a.events.EmitTx(ctx, tx, events.TypeWorkCreated, "work", id,
		map[string]any{
			"work_id": id, "content_type": contentType, "title": title,
			"created_by": "desired",
		})
	if err != nil {
		return "", err
	}
	a.events.Publish(ev)
	return id, nil
}

// UpdateDesired changes the conditions, the monitoring or the note on a want.
//
// Exported for the same reason as WantContent: MCP's monitor_content is the
// same intent as PATCH /desired/{id}, narrowed to one field. Two
// implementations of "stop looking for something better" would eventually
// disagree about whether that emits.
//
// The TARGET is deliberately not changeable through either door. Repointing a
// want at different content is not an edit, it is a different want, and
// allowing it would make the acquisition history attached to that row describe
// something else.
func (a *API) UpdateDesired(ctx context.Context, id string, req UpdateDesiredRequest) (DesiredItem, error) {
	var (
		ev      events.Event
		emitted bool
		out     DesiredItem
	)
	err := a.db.InTx(ctx, func(tx *sql.Tx) error {
		existing, err := desiredByID(ctx, tx, id)
		if err != nil {
			return err
		}
		out = existing

		if req.QualityProfileID != nil || req.QualityProfile != nil {
			var wantID, wantName string
			if req.QualityProfileID != nil {
				wantID = *req.QualityProfileID
			}
			if req.QualityProfile != nil {
				wantName = *req.QualityProfile
			}
			profileID, err := a.resolveProfile(ctx, tx, wantID, wantName)
			if err != nil {
				return err
			}
			out.QualityProfileID = profileID
		}
		if req.Monitor != nil {
			out.Monitor = *req.Monitor
		}
		if req.Reason != nil {
			out.Reason = *req.Reason
		}

		item := out.domainItem()
		if err := item.Validate(); err != nil {
			return &badRequest{err}
		}
		out.Reason = item.Reason

		if sameDesired(existing, out) {
			// Not a transition. A change that changes nothing must not emit.
			out = existing
			return nil
		}
		out.UpdatedAt = a.now().UTC()
		if err := updateDesiredRow(ctx, tx, out); err != nil {
			return err
		}
		var emitErr error
		ev, emitErr = a.events.EmitTx(ctx, tx, events.TypeDesiredUpdated,
			"desired_item", out.ID, map[string]any{
				"desired_item_id":    out.ID,
				"quality_profile_id": out.QualityProfileID,
				"monitor":            out.Monitor,
			})
		emitted = emitErr == nil
		return emitErr
	})
	if err != nil {
		return DesiredItem{}, err
	}
	if emitted {
		a.events.Publish(ev)
	}
	return out, nil
}

// updateDesired is PATCH /api/v1/desired/{id} — a shell over UpdateDesired.
func (a *API) updateDesired(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body UpdateDesiredRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	out, err := a.UpdateDesired(r.Context(), id, body)
	if err != nil {
		a.failDesiredWrite(w, r, err)
		return
	}
	a.write(w, r, http.StatusOK, out)
}

// deleteDesired is DELETE /api/v1/desired/{id}.
//
// Physical, matching quality profiles and for the same reason: a want is a
// statement of intent, not bytes, and ADR-0018's logical-delete stance exists
// to stop shared, expensive, unrecoverable bytes being unlinked inline. An
// operator who stops wanting something and says so should find it gone.
//
// The acquisition history attached to it is M3-03's to own; when that lands, a
// want with history is the case to revisit — which is why the event carries the
// target rather than only the row id, so the log still says what was wanted
// after the row is gone.
func (a *API) deleteDesired(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var ev events.Event
	err := a.db.InTx(r.Context(), func(tx *sql.Tx) error {
		existing, err := desiredByID(r.Context(), tx, id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(r.Context(),
			`DELETE FROM desired_items WHERE id = ?`, id); err != nil {
			return err
		}
		kind, target := existing.domainItem().Target()
		var emitErr error
		ev, emitErr = a.events.EmitTx(r.Context(), tx, events.TypeDesiredRemoved,
			"desired_item", id, map[string]any{
				"desired_item_id": id,
				"target_type":     kind,
				"target_id":       target,
			})
		return emitErr
	})
	if err != nil {
		a.fail(w, r, "desired item", err)
		return
	}
	a.events.Publish(ev)
	w.WriteHeader(http.StatusNoContent)
}

// ClientFault reports whether an error from a write intent is the CALLER's
// fault, and returns a message safe to hand back.
//
// It exists because there are now two front doors and they speak different
// error vocabularies: HTTP answers with problem documents and status codes,
// MCP with JSON-RPC codes. Both need the same classification — "you asked
// wrongly" versus "we broke" — and duplicating that judgement would mean one
// door eventually reporting a duplicate want as an internal error while the
// other reports a conflict.
//
// An unclassified error is OURS. Neither door may hand its detail to a caller:
// it is not useful to them and it is free reconnaissance for anyone else.
func ClientFault(err error) (string, bool) {
	var bad *badRequest
	switch {
	case errors.As(err, &bad):
		return bad.err.Error(), true
	case isUniqueViolation(err):
		// §61: two wants over one target with DIFFERENT profiles are legal and
		// are the point. This is the same target AND the same profile, which
		// is one want written twice.
		return "that content is already wanted under that quality profile — " +
			"to want a second copy, use a different profile", true
	case isForeignKeyViolation(err):
		return "the work, edition or quality profile named does not exist", true
	case errors.Is(err, sql.ErrNoRows):
		return "nothing here matches that identifier", true
	}
	return "", false
}

// failDesiredWrite renders a write failure as a problem document, using
// ClientFault's judgement so the HTTP door and the MCP door agree about whose
// fault an error is.
func (a *API) failDesiredWrite(w http.ResponseWriter, r *http.Request, err error) {
	var bad *badRequest
	switch {
	case errors.As(err, &bad):
		httpapi.Fail(w, r, problem.BadRequest(bad.err.Error()))
	case isUniqueViolation(err):
		// §61: two wants over one target with DIFFERENT profiles are legal and
		// are the point. This is the same target AND the same profile, which
		// is one want written twice.
		httpapi.Fail(w, r, problem.Conflict(
			"that content is already wanted under that quality profile — "+
				"to want a second copy, use a different profile"))
	case isForeignKeyViolation(err):
		httpapi.Fail(w, r, problem.BadRequest(
			"the work, edition or quality profile named does not exist"))
	default:
		a.fail(w, r, "desired item", err)
	}
}

// sameDesired reports whether a PATCH is a no-op.
func sameDesired(a, b DesiredItem) bool {
	return a.QualityProfileID == b.QualityProfileID &&
		a.Monitor == b.Monitor &&
		a.Reason == b.Reason
}

// insertDesired and insertAcquisition used to live here; a new want's row, its
// resting acquisition state and both events are now catalog.CreateDesiredItem,
// so the API door and M12's poll_source worker create a want through one path
// (see WantContent, and the catalog method's own note).

func updateDesiredRow(ctx context.Context, tx *sql.Tx, d DesiredItem) error {
	monitor := 0
	if d.Monitor {
		monitor = 1
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE desired_items
		   SET quality_profile_id = ?, monitor = ?, reason = ?, updated_at = ?
		 WHERE id = ?`,
		d.QualityProfileID, monitor, d.Reason,
		d.UpdatedAt.Format(time.RFC3339Nano), d.ID)
	return err
}

// mountDesired registers the routes. Wanting is ordinary operator traffic, not
// an admin action, so it needs `write` rather than `admin`.
func (a *API) mountDesired(r chi.Router) {
	r.Get("/desired", a.listDesired)
	r.Get("/desired/{id}", a.getDesired)
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Post("/desired", a.createDesired)
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Patch("/desired/{id}", a.updateDesired)
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Delete("/desired/{id}", a.deleteDesired)
}
