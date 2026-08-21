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
	"github.com/rarebit-one/heyarr-core/internal/domain/desired"
	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/events"
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

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// createDesiredRequest is the POST body.
//
// Exactly one of WorkID and Work is required. The descriptor exists so that
// wanting something absent does not require cataloguing it first.
type createDesiredRequest struct {
	Scope     string `json:"scope"`
	WorkID    string `json:"work_id"`
	EditionID string `json:"edition_id"`
	// Work names content that may not exist yet. Resolved through the same
	// get-or-create as the scanner, so a want and a later scan converge.
	Work *workDescriptor `json:"work"`

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

// workDescriptor names content semantically, whether or not it exists.
type workDescriptor struct {
	ContentType string `json:"content_type"`
	Title       string `json:"title"`
	Year        int    `json:"year"`
}

// updateDesiredRequest is the PATCH body. Every field is a pointer: absent
// means "leave it alone", which is what makes a PATCH a PATCH.
type updateDesiredRequest struct {
	QualityProfileID *string `json:"quality_profile_id"`
	QualityProfile   *string `json:"quality_profile"`
	Monitor          *bool   `json:"monitor"`
	Reason           *string `json:"reason"`
}

const desiredColumns = `id, scope, work_id, edition_id, quality_profile_id, monitor,
	reason, created_at, updated_at`

func scanDesiredItem(row interface{ Scan(...any) error }) (DesiredItem, error) {
	var d DesiredItem
	var edition sql.NullString
	var monitor int
	var created, updated string
	if err := row.Scan(&d.ID, &d.Scope, &d.WorkID, &edition, &d.QualityProfileID,
		&monitor, &d.Reason, &created, &updated); err != nil {
		return DesiredItem{}, err
	}
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
		where = append(where, "scope = ?")
		args = append(args, v)
	}
	if v, err := oneOf(r, "monitor", "true", "false"); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	} else if v != "" {
		where = append(where, "monitor = ?")
		args = append(args, map[string]int{"true": 1, "false": 0}[v])
	}
	if workID := r.URL.Query().Get("work_id"); workID != "" {
		where = append(where, "work_id = ?")
		args = append(args, workID)
	}
	if profile := r.URL.Query().Get("quality_profile_id"); profile != "" {
		where = append(where, "quality_profile_id = ?")
		args = append(args, profile)
	}
	if q.cursor != nil {
		where = append(where, "(created_at, id) > (?, ?)")
		args = append(args, q.cursor[0], q.cursor[1])
	}
	args = append(args, q.limit+1)

	//nolint:gosec // assembled only from the literal fragments above; every value is bound
	stmt := `SELECT ` + desiredColumns + ` FROM desired_items WHERE ` +
		strings.Join(where, " AND ") + ` ORDER BY created_at ASC, id ASC LIMIT ?`

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
		`SELECT `+desiredColumns+` FROM desired_items WHERE id = ?`, id))
}

// createDesired is POST /api/v1/desired.
func (a *API) createDesired(w http.ResponseWriter, r *http.Request) {
	var body createDesiredRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if body.WorkID != "" && body.Work != nil {
		httpapi.Fail(w, r, problem.BadRequest(
			"name the work with either work_id or work, not both"))
		return
	}
	if body.WorkID == "" && body.Work == nil {
		httpapi.Fail(w, r, problem.BadRequest(
			"a desired item must name a work — by work_id, or by a work descriptor "+
				"if it does not exist yet"))
		return
	}
	if body.QualityProfileID != "" && body.QualityProfile != "" {
		httpapi.Fail(w, r, problem.BadRequest(
			"name the quality profile with either quality_profile_id or quality_profile, not both"))
		return
	}

	monitor := true
	if body.Monitor != nil {
		monitor = *body.Monitor
	}

	var (
		ev      events.Event
		out     DesiredItem
		created bool
	)
	err := a.db.InTx(r.Context(), func(tx *sql.Tx) error {
		profileID, err := a.resolveProfile(r.Context(), tx, body.QualityProfileID, body.QualityProfile)
		if err != nil {
			return err
		}
		workID := body.WorkID
		if body.Work != nil {
			workID, err = a.resolveWorkDescriptor(r.Context(), tx, *body.Work)
			if err != nil {
				return err
			}
		}

		item := desired.Item{
			ID:               a.newID(),
			Scope:            desired.Scope(body.Scope),
			WorkID:           workID,
			EditionID:        body.EditionID,
			QualityProfileID: profileID,
			Monitor:          monitor,
			Reason:           body.Reason,
		}
		if err := item.Validate(); err != nil {
			return &badRequest{err}
		}

		now := a.now().UTC()
		out = DesiredItem{
			ID: item.ID, Scope: string(item.Scope), WorkID: item.WorkID,
			EditionID: item.EditionID, QualityProfileID: item.QualityProfileID,
			Monitor: item.Monitor, Reason: item.Reason,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := insertDesired(r.Context(), tx, out); err != nil {
			return err
		}
		created = true

		kind, target := item.Target()
		var emitErr error
		ev, emitErr = a.events.EmitTx(r.Context(), tx, events.TypeDesiredCreated,
			"desired_item", out.ID, map[string]any{
				"desired_item_id":    out.ID,
				"scope":              out.Scope,
				"target_type":        kind,
				"target_id":          target,
				"quality_profile_id": out.QualityProfileID,
				"monitor":            out.Monitor,
			})
		return emitErr
	})
	if err != nil {
		a.failDesiredWrite(w, r, err)
		return
	}
	if created {
		a.events.Publish(ev)
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
func (a *API) resolveWorkDescriptor(ctx context.Context, tx *sql.Tx, d workDescriptor) (string, error) {
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

// updateDesired is PATCH /api/v1/desired/{id}.
//
// The target is deliberately NOT changeable. Repointing a want at different
// content is not an edit, it is a different want, and allowing it would make
// the acquisition history attached to this row describe something else.
func (a *API) updateDesired(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body updateDesiredRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	var (
		ev      events.Event
		emitted bool
		out     DesiredItem
	)
	err := a.db.InTx(r.Context(), func(tx *sql.Tx) error {
		existing, err := desiredByID(r.Context(), tx, id)
		if err != nil {
			return err
		}
		out = existing

		if body.QualityProfileID != nil || body.QualityProfile != nil {
			var wantID, wantName string
			if body.QualityProfileID != nil {
				wantID = *body.QualityProfileID
			}
			if body.QualityProfile != nil {
				wantName = *body.QualityProfile
			}
			profileID, err := a.resolveProfile(r.Context(), tx, wantID, wantName)
			if err != nil {
				return err
			}
			out.QualityProfileID = profileID
		}
		if body.Monitor != nil {
			out.Monitor = *body.Monitor
		}
		if body.Reason != nil {
			out.Reason = *body.Reason
		}

		item := out.domainItem()
		if err := item.Validate(); err != nil {
			return &badRequest{err}
		}
		out.Reason = item.Reason

		if sameDesired(existing, out) {
			// Not a transition. A PATCH that changes nothing must not emit.
			out = existing
			return nil
		}
		out.UpdatedAt = a.now().UTC()
		if err := updateDesiredRow(r.Context(), tx, out); err != nil {
			return err
		}
		var emitErr error
		ev, emitErr = a.events.EmitTx(r.Context(), tx, events.TypeDesiredUpdated,
			"desired_item", out.ID, map[string]any{
				"desired_item_id":    out.ID,
				"quality_profile_id": out.QualityProfileID,
				"monitor":            out.Monitor,
			})
		emitted = emitErr == nil
		return emitErr
	})
	if err != nil {
		a.failDesiredWrite(w, r, err)
		return
	}
	if emitted {
		a.events.Publish(ev)
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

// failDesiredWrite maps the write failures that are the client's fault onto
// the right status, so an ordinary operator mistake is not a 500.
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

func insertDesired(ctx context.Context, tx *sql.Tx, d DesiredItem) error {
	var edition any
	if d.EditionID != "" {
		edition = d.EditionID
	}
	monitor := 0
	if d.Monitor {
		monitor = 1
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO desired_items
			(id, scope, work_id, edition_id, quality_profile_id, monitor, reason,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.Scope, d.WorkID, edition, d.QualityProfileID, monitor, d.Reason,
		d.CreatedAt.Format(time.RFC3339Nano), d.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

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
