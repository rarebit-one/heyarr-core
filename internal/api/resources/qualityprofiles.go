package resources

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

// Quality profiles (§62).
//
// A profile says three different KINDS of thing, and the API's job is to keep
// them from looking like three degrees of one:
//
//	accept    a gate   — fail it and the candidate is rejected outright
//	prefer    a score  — never a gate; a candidate meeting none is still fine
//	terminal  a stop   — the point at which the upgrade workflow stops looking
//
// The domain refuses a weight on a gate, an ordering comparison on a codec
// name, and an attribute that does not exist — all at WRITE time, so the
// mistake is reported to whoever wrote the profile rather than becoming a
// rejection reason attached to every candidate for six months.
//
// # A profile is not a scorer
//
// Nothing here evaluates anything. §63's evaluation is M3-04 and lives in
// internal/domain/acquisition. Editing a profile can retroactively unsatisfy a
// DesiredItem, which is why every change emits.

// QualityProfile is the wire type.
type QualityProfile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`

	// Accept, Prefer and Terminal are never null on the wire — an absent
	// section and an empty one are the same statement, and a client should not
	// have to handle both spellings of "no terminal condition".
	Accept   []policy.Rule `json:"accept"`
	Prefer   []policy.Rule `json:"prefer"`
	Terminal []policy.Rule `json:"terminal"`

	// Seeded reports that this profile came from Heyarr's defaults rather than
	// from an operator. Seeding never overwrites, so an edited default stays
	// edited — this flag is how you can tell you are looking at one.
	Seeded    bool      `json:"seeded"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// profileRequest is the POST and PUT body.
//
// The sections are pointers so that a PUT can tell "leave this section alone"
// from "make this section empty". Without that, clearing a profile's terminal
// rules and forgetting to send them are the same request, and one of the two
// is a silent data loss.
type profileRequest struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Accept      *[]policy.Rule `json:"accept"`
	Prefer      *[]policy.Rule `json:"prefer"`
	Terminal    *[]policy.Rule `json:"terminal"`
}

const profileColumns = `id, name, description, accept, prefer, terminal, seeded, created_at, updated_at`

func scanQualityProfile(row interface{ Scan(...any) error }) (QualityProfile, error) {
	var p QualityProfile
	var accept, prefer, terminal string
	var seeded int
	var created, updated string
	if err := row.Scan(&p.ID, &p.Name, &p.Description,
		&accept, &prefer, &terminal, &seeded, &created, &updated); err != nil {
		return QualityProfile{}, err
	}
	p.Seeded = seeded == 1
	for _, pair := range []struct {
		raw  string
		dest *[]policy.Rule
	}{
		{accept, &p.Accept},
		{prefer, &p.Prefer},
		{terminal, &p.Terminal},
	} {
		rules, err := decodeRules(pair.raw)
		if err != nil {
			return QualityProfile{}, err
		}
		*pair.dest = rules
	}
	p.CreatedAt = parseTime(created)
	p.UpdatedAt = parseTime(updated)
	return p, nil
}

// decodeRules reads one of the rule columns, normalising null and an absent
// value to an empty slice rather than nil.
func decodeRules(raw string) ([]policy.Rule, error) {
	if strings.TrimSpace(raw) == "" {
		return []policy.Rule{}, nil
	}
	var out []policy.Rule
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("decoding quality profile rules: %w", err)
	}
	if out == nil {
		out = []policy.Rule{}
	}
	return out, nil
}

// domainProfile is the wire type as the domain sees it, for validation.
func (p QualityProfile) domainProfile() policy.Profile {
	return policy.Profile{
		ID: p.ID, Name: p.Name, Description: p.Description,
		Accept: p.Accept, Prefer: p.Prefer, Terminal: p.Terminal,
	}
}

func fromDomain(d policy.Profile, seeded bool, created, updated time.Time) QualityProfile {
	nonNil := func(r []policy.Rule) []policy.Rule {
		if r == nil {
			return []policy.Rule{}
		}
		return r
	}
	return QualityProfile{
		ID: d.ID, Name: d.Name, Description: d.Description,
		Accept: nonNil(d.Accept), Prefer: nonNil(d.Prefer), Terminal: nonNil(d.Terminal),
		Seeded: seeded, CreatedAt: created, UpdatedAt: updated,
	}
}

// listQualityProfiles pages by name, which is unique.
func (a *API) listQualityProfiles(w http.ResponseWriter, r *http.Request) {
	q, err := parseQuery(r, "quality-profiles", 1)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	where := []string{"1 = 1"}
	args := []any{}
	if q.cursor != nil {
		where = append(where, "name > ?")
		args = append(args, q.cursor[0])
	}
	args = append(args, q.limit+1)

	//nolint:gosec // assembled only from the literal fragments above; every value is bound
	stmt := `SELECT ` + profileColumns + ` FROM quality_profiles WHERE ` +
		strings.Join(where, " AND ") + ` ORDER BY name ASC LIMIT ?`

	rows, err := a.reader.QueryContext(r.Context(), stmt, args...)
	if err != nil {
		a.fail(w, r, "quality profile", err)
		return
	}
	defer func() { _ = rows.Close() }()

	var profiles []QualityProfile
	for rows.Next() {
		p, scanErr := scanQualityProfile(rows)
		if scanErr != nil {
			a.fail(w, r, "quality profile", scanErr)
			return
		}
		profiles = append(profiles, p)
	}
	if err := rows.Err(); err != nil {
		a.fail(w, r, "quality profile", err)
		return
	}
	a.write(w, r, http.StatusOK, newPage(profiles, q.limit,
		func(x QualityProfile) []string { return []string{x.Name} }, "quality-profiles"))
}

func (a *API) getQualityProfile(w http.ResponseWriter, r *http.Request) {
	p, err := qualityProfileByID(r.Context(), a.reader, chi.URLParam(r, "id"))
	if err != nil {
		a.fail(w, r, "quality profile", err)
		return
	}
	a.write(w, r, http.StatusOK, p)
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func qualityProfileByID(ctx context.Context, q rowQuerier, id string) (QualityProfile, error) {
	return scanQualityProfile(q.QueryRowContext(ctx,
		`SELECT `+profileColumns+` FROM quality_profiles WHERE id = ?`, id))
}

// createQualityProfile is POST /api/v1/quality-profiles.
//
// A create, not an upsert. Device registration is an upsert because a
// television announces itself on every launch and nobody chose to do it; a
// profile is authored deliberately, and silently replacing one because the
// name matched would discard whatever it said before.
func (a *API) createQualityProfile(w http.ResponseWriter, r *http.Request) {
	var body profileRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	profile := policy.Profile{
		ID:          a.newID(),
		Name:        body.Name,
		Description: body.Description,
		Accept:      deref(body.Accept),
		Prefer:      deref(body.Prefer),
		Terminal:    deref(body.Terminal),
	}
	// Validation is the domain's, so that the API and any other caller refuse
	// the same profiles for the same reasons.
	if err := profile.Validate(); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	now := a.now().UTC()
	out := fromDomain(profile, false, now, now)

	var ev events.Event
	err := a.db.InTx(r.Context(), func(tx *sql.Tx) error {
		if err := insertQualityProfile(r.Context(), tx, out); err != nil {
			return err
		}
		var emitErr error
		ev, emitErr = a.events.EmitTx(r.Context(), tx, events.TypeQualityProfileCreated,
			"quality_profile", out.ID,
			map[string]any{"quality_profile_id": out.ID, "name": out.Name, "seeded": false})
		return emitErr
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.Fail(w, r, problem.Conflict(
				fmt.Sprintf("a quality profile called %q already exists", profile.Name)))
			return
		}
		a.fail(w, r, "quality profile", err)
		return
	}
	a.events.Publish(ev)

	w.Header().Set("Location", httpapi.APIPrefix+"/quality-profiles/"+out.ID)
	a.write(w, r, http.StatusCreated, out)
}

// updateQualityProfile is PUT /api/v1/quality-profiles/{id}.
//
// A section the client omits is left alone; a section it sends as `[]` is
// cleared. Those are different intentions and the request body distinguishes
// them, because collapsing them means clearing a profile's terminal rules and
// forgetting to send them are the same request.
func (a *API) updateQualityProfile(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body profileRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	var (
		ev      events.Event
		emitted bool
		out     QualityProfile
	)
	err := a.db.InTx(r.Context(), func(tx *sql.Tx) error {
		existing, err := qualityProfileByID(r.Context(), tx, id)
		if err != nil {
			return err
		}

		merged := existing.domainProfile()
		if strings.TrimSpace(body.Name) != "" {
			merged.Name = body.Name
		}
		if body.Description != "" {
			merged.Description = body.Description
		}
		if body.Accept != nil {
			merged.Accept = *body.Accept
		}
		if body.Prefer != nil {
			merged.Prefer = *body.Prefer
		}
		if body.Terminal != nil {
			merged.Terminal = *body.Terminal
		}
		if err := merged.Validate(); err != nil {
			return &badRequest{err}
		}

		now := a.now().UTC()
		out = fromDomain(merged, existing.Seeded, existing.CreatedAt, now)
		out.ID = existing.ID

		if sameProfile(existing, out) {
			// Not a transition. A PUT that changes nothing must not emit, or
			// a client that re-sends its whole configuration on every start
			// turns the event log into a heartbeat — the same reasoning that
			// keeps device re-registration quiet.
			out = existing
			return nil
		}
		if err := updateQualityProfileRow(r.Context(), tx, out); err != nil {
			return err
		}
		var emitErr error
		ev, emitErr = a.events.EmitTx(r.Context(), tx, events.TypeQualityProfileUpdated,
			"quality_profile", out.ID,
			map[string]any{"quality_profile_id": out.ID, "name": out.Name})
		emitted = emitErr == nil
		return emitErr
	})
	if err != nil {
		var bad *badRequest
		if errors.As(err, &bad) {
			httpapi.Fail(w, r, problem.BadRequest(bad.err.Error()))
			return
		}
		if isUniqueViolation(err) {
			httpapi.Fail(w, r, problem.Conflict("a quality profile with that name already exists"))
			return
		}
		a.fail(w, r, "quality profile", err)
		return
	}
	if emitted {
		a.events.Publish(ev)
	}
	a.write(w, r, http.StatusOK, out)
}

// deleteQualityProfile is DELETE /api/v1/quality-profiles/{id}.
//
// Physical, not logical, and that is a deliberate departure from ADR-0018's
// stance for CONTENT. ADR-0018 is about bytes: deleting an asset must not
// unlink blobs inline because bytes are expensive, shared and unrecoverable. A
// profile is a page of configuration — cheap to retype, referenced by nothing
// but desired state, and a soft-deleted one would have to be filtered out of
// every read path forever to avoid an operator seeing a profile they deleted.
//
// A profile still referenced by a DesiredItem is refused rather than cascaded
// (M3-02 owns the reference). Deleting the standard by which desire is
// measured, and leaving the desire, would make satisfaction unanswerable.
func (a *API) deleteQualityProfile(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var (
		ev   events.Event
		name string
	)
	err := a.db.InTx(r.Context(), func(tx *sql.Tx) error {
		existing, err := qualityProfileByID(r.Context(), tx, id)
		if err != nil {
			return err
		}
		name = existing.Name
		if _, err := tx.ExecContext(r.Context(),
			`DELETE FROM quality_profiles WHERE id = ?`, id); err != nil {
			return err
		}
		var emitErr error
		ev, emitErr = a.events.EmitTx(r.Context(), tx, events.TypeQualityProfileDeleted,
			"quality_profile", id, map[string]any{"quality_profile_id": id, "name": existing.Name})
		return emitErr
	})
	if err != nil {
		if isForeignKeyViolation(err) {
			httpapi.Fail(w, r, problem.Conflict(
				fmt.Sprintf("the quality profile %q is still in use — "+
					"nothing can be desired against a standard that does not exist", name)))
			return
		}
		a.fail(w, r, "quality profile", err)
		return
	}
	a.events.Publish(ev)
	w.WriteHeader(http.StatusNoContent)
}

// badRequest carries a validation failure out of a transaction so it becomes a
// 400 rather than the 500 that any other error inside InTx becomes.
type badRequest struct{ err error }

func (b *badRequest) Error() string { return b.err.Error() }
func (b *badRequest) Unwrap() error { return b.err }

func deref(r *[]policy.Rule) []policy.Rule {
	if r == nil {
		return nil
	}
	return *r
}

// sameProfile reports whether a PUT is a no-op.
func sameProfile(a, b QualityProfile) bool {
	if a.Name != b.Name || a.Description != b.Description {
		return false
	}
	same := func(x, y []policy.Rule) bool {
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i].String() != y[i].String() || x[i].Weight != y[i].Weight {
				return false
			}
		}
		return true
	}
	return same(a.Accept, b.Accept) && same(a.Prefer, b.Prefer) && same(a.Terminal, b.Terminal)
}

func insertQualityProfile(ctx context.Context, tx *sql.Tx, p QualityProfile) error {
	accept, prefer, terminal, err := encodeProfileSections(p)
	if err != nil {
		return err
	}
	seeded := 0
	if p.Seeded {
		seeded = 1
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO quality_profiles
			(id, name, description, accept, prefer, terminal, seeded, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Name, p.Description, accept, prefer, terminal, seeded,
		p.CreatedAt.Format(time.RFC3339Nano), p.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

func updateQualityProfileRow(ctx context.Context, tx *sql.Tx, p QualityProfile) error {
	accept, prefer, terminal, err := encodeProfileSections(p)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE quality_profiles
		   SET name = ?, description = ?, accept = ?, prefer = ?, terminal = ?, updated_at = ?
		 WHERE id = ?`,
		p.Name, p.Description, accept, prefer, terminal,
		p.UpdatedAt.Format(time.RFC3339Nano), p.ID)
	return err
}

func encodeProfileSections(p QualityProfile) (accept, prefer, terminal string, err error) {
	enc := func(rules []policy.Rule) (string, error) {
		if rules == nil {
			rules = []policy.Rule{}
		}
		raw, marshalErr := json.Marshal(rules)
		return string(raw), marshalErr
	}
	if accept, err = enc(p.Accept); err != nil {
		return "", "", "", err
	}
	if prefer, err = enc(p.Prefer); err != nil {
		return "", "", "", err
	}
	if terminal, err = enc(p.Terminal); err != nil {
		return "", "", "", err
	}
	return accept, prefer, terminal, nil
}
