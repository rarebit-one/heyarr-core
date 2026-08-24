package resources

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/domain/playback"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

// Consumption sessions (§67, ADR-0024).
//
// The state machine lives in internal/domain/playback and knows nothing about
// SQL, HTTP or the filesystem — depguard enforces that. This file is the port:
// it loads a session, asks the domain what a transition means, writes the
// result and emits the event, all inside one transaction.
//
// # The transaction boundary is the interesting part
//
// The row and its event are written together or not at all. Invariant 7 says
// every state transition emits an event, and an event emitted outside the
// transaction that wrote the row is an event that can exist for a transition
// that rolled back — or, worse, a transition that committed with no event,
// which is invisible. EmitTx is what makes "no exceptions" true rather than
// nearly true, and the job queue needed a follow-up fix (#62) to learn it.

// ConsumptionSession is the wire type.
type ConsumptionSession struct {
	ID       string `json:"id"`
	AssetID  string `json:"asset_id"`
	DeviceID string `json:"device_id"`
	Verb     string `json:"verb"`
	State    string `json:"state"`
	// Progress is absent until something has been recorded. Absent rather than
	// a zero locator, because position zero is a real position and "nothing
	// recorded" is not the same as "the very beginning".
	Progress  *SessionProgress `json:"progress,omitempty"`
	CreatedAt time.Time        `json:"created_at"`
	UpdatedAt time.Time        `json:"updated_at"`
	StartedAt *time.Time       `json:"started_at"`
	EndedAt   *time.Time       `json:"ended_at"`
}

// SessionProgress is where a session reached.
type SessionProgress struct {
	Locator string `json:"locator"`
	Unit    string `json:"unit"`
}

const sessionColumns = `id, asset_id, device_id, verb, state,
	progress_locator, progress_unit, created_at, updated_at, started_at, ended_at`

func scanSessionRow(row interface{ Scan(...any) error }) (playback.Session, error) {
	var s playback.Session
	var verb, state, locator, unit, created, updated string
	var started, ended sql.NullString
	if err := row.Scan(&s.ID, &s.AssetID, &s.DeviceID, &verb, &state,
		&locator, &unit, &created, &updated, &started, &ended); err != nil {
		return playback.Session{}, err
	}
	s.Verb = playback.Verb(verb)
	s.State = playback.State(state)
	s.Progress = playback.Progress{Locator: locator, Unit: playback.Unit(unit)}
	s.CreatedAt = parseTime(created)
	s.UpdatedAt = parseTime(updated)
	if started.Valid {
		t := parseTime(started.String)
		s.StartedAt = &t
	}
	if ended.Valid {
		t := parseTime(ended.String)
		s.EndedAt = &t
	}
	return s, nil
}

// renderSession maps the domain type onto the wire type. They are separate for
// the same reason every other wire type here is: the API is a contract with
// clients and the domain is not, and a struct that is both makes every internal
// rename a breaking API change.
func renderSession(s playback.Session) ConsumptionSession {
	out := ConsumptionSession{
		ID: s.ID, AssetID: s.AssetID, DeviceID: s.DeviceID,
		Verb: string(s.Verb), State: string(s.State),
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
		StartedAt: s.StartedAt, EndedAt: s.EndedAt,
	}
	if !s.Progress.Zero() {
		out.Progress = &SessionProgress{Locator: s.Progress.Locator, Unit: string(s.Progress.Unit)}
	}
	return out
}

// createSessionRequest is the POST /consumption/sessions body.
type createSessionRequest struct {
	AssetID  string `json:"asset_id"`
	DeviceID string `json:"device_id"`
	Verb     string `json:"verb"`
}

func (a *API) createSession(w http.ResponseWriter, r *http.Request) {
	var body createSessionRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	for _, f := range []struct{ name, value string }{
		{"asset_id", body.AssetID}, {"device_id", body.DeviceID},
	} {
		if err := required(f.name, f.value); err != nil {
			httpapi.Fail(w, r, problem.BadRequest(err.Error()))
			return
		}
	}
	verb, err := playback.ParseVerb(body.Verb)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	now := a.now().UTC()
	session := playback.Session{
		ID: a.newID(), AssetID: body.AssetID, DeviceID: body.DeviceID,
		Verb: verb, State: playback.StateCreated,
		CreatedAt: now, UpdatedAt: now,
	}

	var event events.Event
	err = a.db.InTx(r.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO consumption_sessions
				(id, asset_id, device_id, verb, state, progress_locator, progress_unit,
				 created_at, updated_at, started_at, ended_at)
			VALUES (?, ?, ?, ?, ?, '', '', ?, ?, NULL, NULL)`,
			session.ID, session.AssetID, session.DeviceID, string(session.Verb), string(session.State),
			now.Format(timeFormat), now.Format(timeFormat)); err != nil {
			return err
		}
		event, err = a.events.EmitTx(r.Context(), tx, eventSessionCreated, "consumption_session", session.ID,
			map[string]any{
				"session_id": session.ID, "asset_id": session.AssetID,
				"device_id": session.DeviceID, "verb": string(session.Verb),
			})
		return err
	})
	if err != nil {
		// A session naming an asset or a device that does not exist is the
		// client's mistake, not ours: it is a 400 with the reason, rather than
		// the 500 a raw foreign-key violation would otherwise become.
		if isForeignKeyViolation(err) {
			httpapi.Fail(w, r, problem.BadRequest(
				"asset_id and device_id must both name something that exists"))
			return
		}
		a.fail(w, r, "consumption session", err)
		return
	}
	a.events.Publish(event)

	w.Header().Set("Location", httpapi.APIPrefix+"/consumption/sessions/"+session.ID)
	a.write(w, r, http.StatusCreated, renderSession(session))
}

// eventSessionCreated announces a session that exists but has not started.
//
// It is not one of the domain's transitions — creation is not a transition,
// there is no prior state to move from — so it is spelled here rather than in
// playback.EventType, which maps transitions and nothing else.
const eventSessionCreated = "playback.session.created"

// transitionRequest is the POST /consumption/sessions/{id}/transitions body.
type transitionRequest struct {
	Transition string           `json:"transition"`
	Progress   *SessionProgress `json:"progress"`
}

// applyTransition is the single write path for a session's whole life.
//
// One endpoint rather than six (/start, /pause, /resume, …) because the state
// machine is one thing: six endpoints would each need to know the table, and
// the first one to disagree with it would be a session in a state the domain
// says is impossible.
// applySessionTransition moves one session and emits its event.
//
// Extracted so the HTTP handler and the renderer progress poller
// (renderer_progress.go) drive the state machine through ONE path. A poller
// with its own copy of this would be a second writer of session state, free to
// disagree about what "completed" means — and the disagreement would surface
// as a "continue watching" row nobody can explain.
func (a *API) applySessionTransition(ctx context.Context, id string, transition playback.Transition, progress *playback.Progress) (playback.Session, error) {
	var (
		updated playback.Session
		event   events.Event
	)
	err := a.db.InTx(ctx, func(tx *sql.Tx) error {
		// SELECT inside the transaction, so two clients transitioning one
		// session cannot both read "playing" and both act on it. The control
		// plane is single-writer (ADR-0003), so this is a short serialised
		// read-modify-write rather than a lock anyone waits on.
		current, err := scanSessionRow(tx.QueryRowContext(ctx,
			`SELECT `+sessionColumns+` FROM consumption_sessions WHERE id = ?`, id))
		if err != nil {
			return err
		}

		next, err := current.Apply(transition, a.now().UTC(), progress)
		if err != nil {
			return err
		}
		updated = next

		if _, err := tx.ExecContext(ctx, `
			UPDATE consumption_sessions
			SET state = ?, progress_locator = ?, progress_unit = ?,
				updated_at = ?, started_at = ?, ended_at = ?
			WHERE id = ?`,
			string(next.State), next.Progress.Locator, string(next.Progress.Unit),
			next.UpdatedAt.Format(timeFormat),
			nullableTime(next.StartedAt), nullableTime(next.EndedAt), next.ID); err != nil {
			return err
		}

		// Invariant 7, inside the transaction that made the change. Emitting
		// outside it would allow an event for a transition that rolled back,
		// or a committed transition with no event at all — and the second is
		// invisible, which is the failure mode that makes retrofitting events
		// expensive (ADR-0009).
		event, err = a.events.EmitTx(ctx, tx, playback.EventType(transition),
			"consumption_session", next.ID,
			map[string]any{
				"session_id": next.ID, "asset_id": next.AssetID, "device_id": next.DeviceID,
				"verb": string(next.Verb), "state": string(next.State),
				"transition": string(transition),
			})
		return err
	})
	if err != nil {
		return playback.Session{}, err
	}
	a.events.Publish(event)
	return updated, nil
}

func (a *API) applyTransition(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var body transitionRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	transition, err := parseTransition(body.Transition)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	var progress *playback.Progress
	if body.Progress != nil {
		progress = &playback.Progress{
			Locator: body.Progress.Locator,
			Unit:    playback.Unit(body.Progress.Unit),
		}
	}

	updated, err := a.applySessionTransition(r.Context(), id, transition, progress)
	switch {
	case errors.Is(err, playback.ErrIllegalTransition):
		// 409, not 400: the request is well-formed and would be legal against
		// a different state. A client cannot tell "you sent nonsense" from
		// "you are out of date" if both are 400, and only one of them is fixed
		// by re-reading the session.
		httpapi.Fail(w, r, problem.Conflict(err.Error()))
		return
	case errors.Is(err, playback.ErrInvalidProgress):
		// Typed, not matched on the message. A layer that branches on error
		// TEXT starts returning 500 the day someone rewords a sentence, and
		// nothing catches it.
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	case err != nil:
		a.fail(w, r, "consumption session", err)
		return
	}

	a.write(w, r, http.StatusOK, renderSession(updated))
}

func parseTransition(s string) (playback.Transition, error) {
	if s == "" {
		return "", errors.New("transition is required")
	}
	for _, t := range playback.Transitions() {
		if string(t) == s {
			return t, nil
		}
	}
	names := make([]string, 0, len(playback.Transitions()))
	for _, t := range playback.Transitions() {
		names = append(names, string(t))
	}
	return "", errors.New("transition must be one of " + strings.Join(names, ", ") + ", not " + s)
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format(timeFormat)
}

func (a *API) getSession(w http.ResponseWriter, r *http.Request) {
	session, err := scanSessionRow(a.reader.QueryRowContext(r.Context(),
		`SELECT `+sessionColumns+` FROM consumption_sessions WHERE id = ?`, chi.URLParam(r, "id")))
	if err != nil {
		a.fail(w, r, "consumption session", err)
		return
	}
	a.write(w, r, http.StatusOK, renderSession(session))
}

// listSessions pages sessions, newest first.
//
// ?state=resumable is the "continue watching" query — every session that has
// not reached a terminal state, most recently touched first. It is a named
// filter rather than three separate state values because "resumable" is the
// question clients actually ask, and expressing it as
// state=created,playing,paused would put the state machine's shape into every
// client (§67's "continuing").
func (a *API) listSessions(w http.ResponseWriter, r *http.Request) {
	q, err := parseQuery(r, "consumption_sessions", 2)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	state, err := oneOf(r, "state", "resumable", "created", "playing", "paused", "stopped", "completed")
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	where := []string{"1 = 1"}
	args := []any{}
	switch {
	case state == "resumable":
		where = append(where, "state IN ('created', 'playing', 'paused')")
	case state != "":
		where = append(where, "state = ?")
		args = append(args, state)
	}
	if v := r.URL.Query().Get("asset_id"); v != "" {
		where = append(where, "asset_id = ?")
		args = append(args, v)
	}
	if v := r.URL.Query().Get("device_id"); v != "" {
		where = append(where, "device_id = ?")
		args = append(args, v)
	}
	if q.cursor != nil {
		// Newest first, so the keyset boundary is "strictly older". The id is
		// in the sort key because updated_at is not unique — two transitions in
		// the same instant are ordinary under an injected clock.
		where = append(where, "(updated_at, id) < (?, ?)")
		args = append(args, q.cursor[0], q.cursor[1])
	}
	args = append(args, q.limit+1)

	//nolint:gosec // assembled only from the literal fragments above; every value is bound
	stmt := `SELECT ` + sessionColumns + ` FROM consumption_sessions WHERE ` +
		strings.Join(where, " AND ") + ` ORDER BY updated_at DESC, id DESC LIMIT ?`

	rows, err := a.reader.QueryContext(r.Context(), stmt, args...)
	if err != nil {
		a.fail(w, r, "consumption session", err)
		return
	}
	defer func() { _ = rows.Close() }()

	var sessions []ConsumptionSession
	for rows.Next() {
		s, err := scanSessionRow(rows)
		if err != nil {
			a.fail(w, r, "consumption session", err)
			return
		}
		sessions = append(sessions, renderSession(s))
	}
	if err := rows.Err(); err != nil {
		a.fail(w, r, "consumption session", err)
		return
	}
	a.write(w, r, http.StatusOK, newPage(sessions, q.limit,
		func(s ConsumptionSession) []string {
			return []string{s.UpdatedAt.Format(timeFormat), s.ID}
		}, "consumption_sessions"))
}

// isForeignKeyViolation reports whether an error is a FOREIGN KEY constraint
// failure, so a client naming something that does not exist gets a 400 rather
// than a 500.
func isForeignKeyViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "FOREIGN KEY")
}
