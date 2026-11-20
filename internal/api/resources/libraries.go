package resources

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
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
	if !decodeOr400(w, r, &body) {
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

// updateLibraryRequest is the PATCH /libraries/{id} body.
//
// Name is a pointer so a patch that names no field is refused rather than read
// as "rename to the empty string". It is the only field: content_type cannot be
// changed in place (it would re-identify every work under the library), and the
// strict decoder refuses it as an unknown field. The one exception, a type
// Heyarr does not understand, is its own action: retypeLibrary.
type updateLibraryRequest struct {
	Name *string `json:"name"`
}

// updateLibrary renames a library in place (ADR-0124).
//
// The id is the library's identity and the name is a label: assets, roots,
// scanned files and jobs all reference the id, so a rename touches one column
// of one row and nothing that hangs off it. The name is validated the way
// createLibrary validates it — trimmed, non-empty, unique, with a duplicate a
// 409 rather than a 500 — so a rename cannot produce a library that `library
// add` would have refused.
//
// Renaming to the name it already has is a 200 with no event: nothing
// transitioned, so there is nothing to put on the log.
func (a *API) updateLibrary(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var body updateLibraryRequest
	if !decodeOr400(w, r, &body) {
		return
	}
	if body.Name == nil {
		httpapi.Fail(w, r, problem.BadRequest("a library patch must change something — name"))
		return
	}
	if err := required("name", *body.Name); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	name := strings.TrimSpace(*body.Name)

	var (
		ev      events.Event
		emitted bool
	)
	err := a.db.InTx(r.Context(), func(tx *sql.Tx) error {
		// A missing library is ErrNoRows here, which a.fail turns into a 404.
		var previous string
		if err := tx.QueryRowContext(r.Context(),
			`SELECT name FROM libraries WHERE id = ?`, id).Scan(&previous); err != nil {
			return err
		}
		if previous == name {
			return nil
		}
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE libraries SET name = ? WHERE id = ?`, name, id); err != nil {
			return err
		}
		var emitErr error
		ev, emitErr = a.events.EmitTx(r.Context(), tx, events.TypeLibraryRenamed, "library", id,
			map[string]any{"library_id": id, "name": name, "previous_name": previous})
		emitted = emitErr == nil
		return emitErr
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.Fail(w, r, problem.Conflict("a library named "+name+" already exists"))
			return
		}
		a.fail(w, r, "library", err)
		return
	}
	if emitted {
		a.events.Publish(ev)
	}

	lib, err := a.loadLibrary(r.Context(), id)
	if err != nil {
		a.fail(w, r, "library", err)
		return
	}
	a.write(w, r, http.StatusOK, lib)
}

// retypeLibraryRequest is the POST /libraries/{id}/retype body.
type retypeLibraryRequest struct {
	ContentType string `json:"content_type"`
}

// retypeLibrary corrects a library whose stored content type Heyarr does not
// understand to one it does (#896, ADR-0127). The rules are the catalog's
// (catalog.RetypeLibrary); this maps them onto statuses:
//
//   - a target that is not a known type is a 400, worded as createLibrary
//     words it;
//   - a library whose type is already known, and different, is a 409 — that is
//     a real content-type change, which nothing does in place;
//   - a library that already has the target type is a 200 with no event, so a
//     second run of the same command is not an error.
func (a *API) retypeLibrary(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var body retypeLibraryRequest
	if !decodeOr400(w, r, &body) {
		return
	}
	if err := required("content_type", body.ContentType); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	contentType := strings.TrimSpace(body.ContentType)

	if _, err := a.catalog.RetypeLibrary(r.Context(), id, contentType); err != nil {
		a.failRetype(w, r, contentType, err)
		return
	}

	lib, err := a.loadLibrary(r.Context(), id)
	if err != nil {
		a.fail(w, r, "library", err)
		return
	}
	a.write(w, r, http.StatusOK, lib)
}

// failRetype maps catalog.RetypeLibrary's refusals onto statuses.
func (a *API) failRetype(w http.ResponseWriter, r *http.Request, contentType string, err error) {
	var known *catalog.KnownContentTypeError
	switch {
	case errors.Is(err, catalog.ErrUnknownContentType):
		httpapi.Fail(w, r, problem.BadRequest(fmt.Sprintf(
			"content_type %q is not one Heyarr understands — use one of: %s",
			contentType, strings.Join(identification.ContentTypes(), ", "))))
	case errors.As(err, &known):
		httpapi.Fail(w, r, problem.Conflict(known.Reason()))
	case errors.Is(err, catalog.ErrNoLibrary):
		httpapi.Fail(w, r, problem.NotFound("no library with that identifier"))
	default:
		a.fail(w, r, "library", err)
	}
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
	if !decodeOr400(w, r, &body) {
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

// updateRootRequest is the PATCH /libraries/{id}/roots/{rootID} body.
//
// IngestMode is a pointer so "absent" and "empty" are distinguishable: a patch
// that names no field changes nothing and is refused, rather than silently
// resetting the root to a default.
type updateRootRequest struct {
	IngestMode *string `json:"ingest_mode"`
}

// updateLibraryRoot changes an existing root's ingest configuration (#222).
//
// # Why this exists at all
//
// A root's materialisation mode was settable only at creation, so a store that
// shipped on the `reflink` default and later needed `hardlink` — the exact case
// #222 pins, where a ZFS pool without block cloning degrades reflink to a full
// byte copy — could be corrected only by deleting the root or reaching past the
// API into SQLite. Neither is a supported surface, and the CLI writes through
// the API precisely so it works against a controller on another host. POST and
// DELETE a root but never amend it was an asymmetry, not a decision.
//
// # It touches no bytes
//
// The mode governs how the NEXT ingest materialises; blobs already in the store
// keep the inode they arrived with. So this is safe to run at any time and on a
// live root — the worst case is that already-copied files stay copied until
// they are re-adopted, which is a separate operation and not this one's job.
func (a *API) updateLibraryRoot(w http.ResponseWriter, r *http.Request) {
	libraryID := chi.URLParam(r, "id")
	rootID := chi.URLParam(r, "rootID")

	var body updateRootRequest
	if !decodeOr400(w, r, &body) {
		return
	}
	if body.IngestMode == nil {
		httpapi.Fail(w, r, problem.BadRequest(
			"a root patch must change something — ingest_mode"))
		return
	}
	// An explicit empty or unknown mode is an error here, not a silent reset to
	// reflink the way an omitted field is at creation. required() rejects the
	// empty string, so inSet's empty-defaulting branch is never reached and its
	// fallback is inert.
	if err := required("ingest_mode", *body.IngestMode); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	mode, err := inSet("ingest_mode", *body.IngestMode, *body.IngestMode,
		"reflink", "hardlink", "copy", "link")
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	var (
		updated LibraryRoot
		ev      events.Event
	)
	err = a.db.InTx(r.Context(), func(tx *sql.Tx) error {
		// Scoped to the library named in the path: a root id belonging to
		// another library reads as a 404 here, never a cross-library edit.
		before, err := scanRootRow(tx.QueryRowContext(r.Context(),
			`SELECT `+rootColumns+` FROM library_roots WHERE id = ? AND library_id = ?`,
			rootID, libraryID))
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE library_roots SET ingest_mode = ? WHERE id = ?`, mode, rootID); err != nil {
			return err
		}
		updated = before
		updated.IngestMode = mode
		ev, err = a.events.EmitTx(r.Context(), tx, events.TypeLibraryRootUpdated, "library", libraryID,
			map[string]any{
				"library_id": libraryID, "root_id": rootID, "path": before.Path,
				"ingest_mode": mode, "previous_ingest_mode": before.IngestMode,
			})
		return err
	})
	if err != nil {
		a.fail(w, r, "library root", err)
		return
	}
	a.events.Publish(ev)

	a.write(w, r, http.StatusOK, updated)
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

// errLibraryNotEmpty is the refusal a library that still holds assets gets. It
// is a sentinel rather than a database error because the database would not
// refuse the delete: assets.library_id is ON DELETE SET NULL, so a raw delete
// silently orphans a whole library's content from the library it belonged to.
var errLibraryNotEmpty = errors.New("resources: the library still holds assets")

// deleteLibrary removes an empty library and its roots (#228).
//
// The library must hold no assets. That is a deliberate refusal, not a cascade:
// "remove the library, keep the content" (the schema's ON DELETE SET NULL) and
// "remove the library and everything it brought in" are different operations,
// and overloading one verb with both is how content disappears by surprise
// (ADR-0083). The operation this route performs is the safe, unambiguous one —
// an empty library, whose content was already removed per-work via
// DELETE /works/{id}, goes together with its roots (library_roots is ON DELETE
// CASCADE). Logical in ADR-0018's sense: catalog rows go, not one byte is
// unlinked, and the event says so.
func (a *API) deleteLibrary(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var (
		ev    events.Event
		name  string
		roots int
	)
	err := a.db.InTx(r.Context(), func(tx *sql.Tx) error {
		// A missing library is ErrNoRows here, which a.fail turns into a 404.
		if err := tx.QueryRowContext(r.Context(),
			`SELECT name FROM libraries WHERE id = ?`, id).Scan(&name); err != nil {
			return err
		}
		var assets int
		if err := tx.QueryRowContext(r.Context(),
			`SELECT count(*) FROM assets WHERE library_id = ?`, id).Scan(&assets); err != nil {
			return err
		}
		if assets > 0 {
			return errLibraryNotEmpty
		}
		if err := tx.QueryRowContext(r.Context(),
			`SELECT count(*) FROM library_roots WHERE library_id = ?`, id).Scan(&roots); err != nil {
			return err
		}
		// One statement. The library's roots are ON DELETE CASCADE, so the
		// database removes them; the empty-check above proved there are no
		// assets for the SET NULL to orphan.
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM libraries WHERE id = ?`, id); err != nil {
			return err
		}
		var emitErr error
		ev, emitErr = a.events.EmitTx(r.Context(), tx, events.TypeLibraryDeleted, "library", id,
			map[string]any{
				"library_id": id,
				"name":       name,
				"roots":      roots,
				// The first question anyone reading the log will have; ADR-0018.
				"bytes_removed": false,
			})
		return emitErr
	})
	if err != nil {
		if errors.Is(err, errLibraryNotEmpty) {
			httpapi.Fail(w, r, problem.Conflict(fmt.Sprintf(
				"the library %q still holds content — remove its works first "+
					"(DELETE /api/v1/works/{id}), then the empty library can go", name)))
			return
		}
		a.fail(w, r, "library", err)
		return
	}
	a.events.Publish(ev)
	w.WriteHeader(http.StatusNoContent)
}

// deleteLibraryRoot removes one root from a library (#228): Heyarr stops
// scanning that directory.
//
// Unconditional and always safe. An asset references its library, never a root
// — there is no root_id on assets — so removing a root orphans no ingested
// content; only the root's scanned_files records go with it (ON DELETE
// CASCADE), which a rescan of a remaining root would rebuild anyway. Logical in
// ADR-0018's sense: no byte is unlinked.
func (a *API) deleteLibraryRoot(w http.ResponseWriter, r *http.Request) {
	libraryID := chi.URLParam(r, "id")
	rootID := chi.URLParam(r, "rootID")

	var (
		ev   events.Event
		path string
	)
	err := a.db.InTx(r.Context(), func(tx *sql.Tx) error {
		// Scoped to the library named in the path: a root id belonging to
		// another library reads as a 404 here, never a cross-library delete.
		if err := tx.QueryRowContext(r.Context(),
			`SELECT path FROM library_roots WHERE id = ? AND library_id = ?`, rootID, libraryID).
			Scan(&path); err != nil {
			return err
		}
		if _, err := tx.ExecContext(r.Context(),
			`DELETE FROM library_roots WHERE id = ?`, rootID); err != nil {
			return err
		}
		var emitErr error
		ev, emitErr = a.events.EmitTx(r.Context(), tx, events.TypeLibraryRootRemoved, "library", libraryID,
			map[string]any{
				"library_id": libraryID,
				"root_id":    rootID,
				"path":       path,
			})
		return emitErr
	})
	if err != nil {
		a.fail(w, r, "library root", err)
		return
	}
	a.events.Publish(ev)
	w.WriteHeader(http.StatusNoContent)
}
