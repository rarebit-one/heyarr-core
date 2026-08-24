package resources

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/scanner"
)

const libraryColumns = `id, name, content_type, enabled, created_at`

func scanLibraryRow(row interface{ Scan(...any) error }) (Library, error) {
	var l Library
	var enabled int
	var created string
	if err := row.Scan(&l.ID, &l.Name, &l.ContentType, &enabled, &created); err != nil {
		return Library{}, err
	}
	l.Enabled = enabled == 1
	l.CreatedAt = parseTime(created)
	l.Roots = []LibraryRoot{}
	return l, nil
}

func scanRootRow(row interface{ Scan(...any) error }) (LibraryRoot, error) {
	var rt LibraryRoot
	var enabled int
	var created string
	if err := row.Scan(&rt.ID, &rt.LibraryID, &rt.Path, &rt.IngestMode, &enabled, &created); err != nil {
		return LibraryRoot{}, err
	}
	rt.Enabled = enabled == 1
	rt.CreatedAt = parseTime(created)
	return rt, nil
}

const rootColumns = `id, library_id, path, ingest_mode, enabled, created_at`

// attachRoots loads the roots for a page of libraries in one query rather than
// one per library. A library has a handful of roots, so they belong inline —
// but N+1 queries for a handful of rows is still N+1.
func (a *API) attachRoots(ctx context.Context, libs []Library) error {
	if len(libs) == 0 {
		return nil
	}
	placeholders := make([]string, len(libs))
	args := make([]any, len(libs))
	index := make(map[string]int, len(libs))
	for i, l := range libs {
		placeholders[i] = "?"
		args[i] = l.ID
		index[l.ID] = i
	}
	//nolint:gosec // the only interpolation is a run of ? placeholders
	stmt := `SELECT ` + rootColumns + ` FROM library_roots WHERE library_id IN (` +
		strings.Join(placeholders, ", ") + `) ORDER BY path ASC, id ASC`
	rows, err := a.reader.QueryContext(ctx, stmt, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		rt, err := scanRootRow(rows)
		if err != nil {
			return err
		}
		if i, ok := index[rt.LibraryID]; ok {
			libs[i].Roots = append(libs[i].Roots, rt)
		}
	}
	return rows.Err()
}

// listLibraries pages by name, which is unique, so the sort key needs no tie
// breaker.
func (a *API) listLibraries(w http.ResponseWriter, r *http.Request) {
	q, err := parseQuery(r, "libraries", 1)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	where := []string{"1 = 1"}
	args := []any{}
	if ct := r.URL.Query().Get("content_type"); ct != "" {
		where = append(where, "content_type = ?")
		args = append(args, ct)
	}
	if term := r.URL.Query().Get("q"); term != "" {
		where = append(where, `name LIKE ? ESCAPE '\'`)
		args = append(args, likePattern(term))
	}
	if q.cursor != nil {
		where = append(where, "name > ?")
		args = append(args, q.cursor[0])
	}
	args = append(args, q.limit+1)

	//nolint:gosec // the query is assembled only from the literal fragments above; every value is bound
	stmt := `SELECT ` + libraryColumns + ` FROM libraries WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY name ASC LIMIT ?`

	rows, err := a.reader.QueryContext(r.Context(), stmt, args...)
	if err != nil {
		a.fail(w, r, "library", err)
		return
	}
	defer func() { _ = rows.Close() }()

	var libs []Library
	for rows.Next() {
		l, err := scanLibraryRow(rows)
		if err != nil {
			a.fail(w, r, "library", err)
			return
		}
		libs = append(libs, l)
	}
	if err := rows.Err(); err != nil {
		a.fail(w, r, "library", err)
		return
	}

	result := newPage(libs, q.limit, func(x Library) []string { return []string{x.Name} }, "libraries")
	if err := a.attachRoots(r.Context(), result.Items); err != nil {
		a.fail(w, r, "library", err)
		return
	}
	a.write(w, r, http.StatusOK, result)
}

func (a *API) getLibrary(w http.ResponseWriter, r *http.Request) {
	lib, err := a.loadLibrary(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		a.fail(w, r, "library", err)
		return
	}
	a.write(w, r, http.StatusOK, lib)
}

func (a *API) loadLibrary(ctx context.Context, id string) (Library, error) {
	row := a.reader.QueryRowContext(ctx, `SELECT `+libraryColumns+` FROM libraries WHERE id = ?`, id)
	lib, err := scanLibraryRow(row)
	if err != nil {
		return Library{}, err
	}
	one := []Library{lib}
	if err := a.attachRoots(ctx, one); err != nil {
		return Library{}, err
	}
	return one[0], nil
}

// createLibraryRequest is the POST /libraries body.
type createLibraryRequest struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Enabled     *bool  `json:"enabled"`
}

func (a *API) createLibrary(w http.ResponseWriter, r *http.Request) {
	var body createLibraryRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if err := required("name", body.Name); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if err := required("content_type", body.ContentType); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	// And it must be one Heyarr KNOWS, not merely non-empty (#227).
	//
	// The column is TEXT with no CHECK and this was the only gate, so any
	// string got through — and `show` did. That is not cosmetic: Identify uses
	// the library's type to choose which rules may fire, a type no rule
	// declares matches nothing, and the fallback is every rule in registration
	// order with the movie rules first. A television library declared as
	// `show` had its artwork identified by `movie/title-year` and grew a movie
	// Work that does not exist.
	//
	// Refused here rather than normalised: guessing that `show` meant `series`
	// would be right this time and wrong for `films`, `tv`, or a typo — and a
	// silent correction is how a library ends up holding something other than
	// what its owner asked for. The refusal names the vocabulary.
	body.ContentType = strings.TrimSpace(body.ContentType)
	if !identification.IsContentType(body.ContentType) {
		httpapi.Fail(w, r, problem.BadRequest(fmt.Sprintf(
			"content_type %q is not one Heyarr understands — use one of: %s",
			body.ContentType, strings.Join(identification.ContentTypes(), ", "))))
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}

	lib := Library{
		ID:          a.newID(),
		Name:        strings.TrimSpace(body.Name),
		ContentType: body.ContentType,
		Enabled:     enabled,
		CreatedAt:   a.now().UTC(),
		Roots:       []LibraryRoot{},
	}

	var event events.Event
	err := a.db.InTx(r.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.Context(),
			`INSERT INTO libraries (id, name, content_type, enabled, created_at) VALUES (?, ?, ?, ?, ?)`,
			lib.ID, lib.Name, lib.ContentType, boolToInt(lib.Enabled), lib.CreatedAt.Format(timeFormat))
		if err != nil {
			return err
		}
		event, err = a.events.EmitTx(r.Context(), tx, events.TypeLibraryCreated, "library", lib.ID,
			map[string]any{"library_id": lib.ID, "name": lib.Name, "content_type": lib.ContentType})
		return err
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.Fail(w, r, problem.Conflict("a library named "+lib.Name+" already exists"))
			return
		}
		a.fail(w, r, "library", err)
		return
	}
	a.events.Publish(event)

	w.Header().Set("Location", httpapi.APIPrefix+"/libraries/"+lib.ID)
	a.write(w, r, http.StatusCreated, lib)
}

// createRootRequest is the POST /libraries/{id}/roots body.
type createRootRequest struct {
	Path       string `json:"path"`
	IngestMode string `json:"ingest_mode"`
	Enabled    *bool  `json:"enabled"`
}

func (a *API) createLibraryRoot(w http.ResponseWriter, r *http.Request) {
	libraryID := chi.URLParam(r, "id")

	var body createRootRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if err := required("path", body.Path); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	mode, err := inSet("ingest_mode", body.IngestMode, "reflink", "reflink", "hardlink", "copy", "link")
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}

	// The library has to exist before the root does, and saying so as a 404 is
	// the honest answer — the foreign key would refuse it anyway, as a 500.
	if err := a.libraryExists(r.Context(), libraryID); err != nil {
		a.fail(w, r, "library", err)
		return
	}

	root := LibraryRoot{
		ID:         a.newID(),
		LibraryID:  libraryID,
		Path:       body.Path,
		IngestMode: mode,
		Enabled:    enabled,
		CreatedAt:  a.now().UTC(),
	}

	var event events.Event
	err = a.db.InTx(r.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.Context(),
			`INSERT INTO library_roots (id, library_id, path, ingest_mode, enabled, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			root.ID, root.LibraryID, root.Path, root.IngestMode,
			boolToInt(root.Enabled), root.CreatedAt.Format(timeFormat))
		if err != nil {
			return err
		}
		event, err = a.events.EmitTx(r.Context(), tx, events.TypeLibraryRootAdded, "library", libraryID,
			map[string]any{
				"library_id": libraryID, "root_id": root.ID, "path": root.Path,
				"ingest_mode": root.IngestMode,
			})
		return err
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.Fail(w, r, problem.Conflict("this library already has a root at "+root.Path))
			return
		}
		a.fail(w, r, "library root", err)
		return
	}
	a.events.Publish(event)

	a.write(w, r, http.StatusCreated, root)
}

func (a *API) libraryExists(ctx context.Context, id string) error {
	var found string
	return a.reader.QueryRowContext(ctx, `SELECT id FROM libraries WHERE id = ?`, id).Scan(&found)
}

// scanResponse is what POST /libraries/{id}/scan returns.
//
// It is a list because a scan is per root (issue #13's job is
// scan_library(root_id)) and a library may have several. A single job id would
// be a lie for every library with two roots, and the CLI's --wait needs to
// watch all of them or it exits 0 while half the scan is still running.
type scanResponse struct {
	LibraryID string `json:"library_id"`
	Jobs      []Job  `json:"jobs"`
}

func (a *API) scanLibrary(w http.ResponseWriter, r *http.Request) {
	libraryID := chi.URLParam(r, "id")
	if err := a.libraryExists(r.Context(), libraryID); err != nil {
		a.fail(w, r, "library", err)
		return
	}

	rows, err := a.reader.QueryContext(r.Context(),
		`SELECT id FROM library_roots WHERE library_id = ? AND enabled = 1 ORDER BY path ASC, id ASC`,
		libraryID)
	if err != nil {
		a.fail(w, r, "library", err)
		return
	}
	defer func() { _ = rows.Close() }()
	var rootIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			a.fail(w, r, "library", err)
			return
		}
		rootIDs = append(rootIDs, id)
	}
	if err := rows.Err(); err != nil {
		a.fail(w, r, "library", err)
		return
	}
	if len(rootIDs) == 0 {
		// Enqueueing nothing and returning 202 would let `heyarr scan lib
		// --wait` exit 0 having scanned nothing at all.
		httpapi.Fail(w, r, problem.Conflict(
			"this library has no enabled roots, so there is nothing to scan; add one with POST /libraries/{id}/roots"))
		return
	}

	out := scanResponse{LibraryID: libraryID, Jobs: []Job{}}
	for _, rootID := range rootIDs {
		// The dedupe key makes a second scan request while the first is still
		// live return the running job rather than queueing a duplicate walk of
		// the same tree (ADR-0008).
		job, err := a.jobs.Enqueue(r.Context(), jobs.EnqueueOptions{
			Type:      scanner.JobType,
			Payload:   scanner.Payload{RootID: rootID},
			DedupeKey: scanner.DedupeKey(rootID),
		})
		if err != nil {
			a.fail(w, r, "job", err)
			return
		}
		out.Jobs = append(out.Jobs, jobFromQueue(job))
	}

	a.write(w, r, http.StatusAccepted, out)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// isUniqueViolation reports a duplicate-key error without importing the driver.
//
// modernc's sqlite returns a plain error whose text carries the SQLite message.
// Matching on the message is unpleasant; the alternative is a SELECT before
// every INSERT, which is both a race and slower. The unique constraint stays
// the authority — this only decides whether the client sees 409 or 500.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint failed") ||
		strings.Contains(msg, "constraint failed: unique")
}
