package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

// LibrarySpec is a library as configuration declares it. It is deliberately not
// config.Library: the catalog does not import the configuration package, so
// that "what a library is" stays one thing whether it came from a file, the API
// (M1-14) or a test.
type LibrarySpec struct {
	Name        string
	ContentType string
	// Roots are absolute paths. Each becomes one library_roots row.
	Roots []string
}

// ReconciledRoot is a library root as it now exists in the database.
type ReconciledRoot struct {
	ID        string
	LibraryID string
	Path      string
	Created   bool
}

// ReconcileLibraries brings the libraries and library_roots tables into line
// with the declared configuration, and reports the roots that now exist.
//
// It is additive on purpose. A root that has disappeared from the config is
// left in place rather than deleted, because deleting it cascades to nothing
// visible but changes what a later scan considers vanished — and an operator
// who commented a line out of a YAML file has not asked for their catalog to
// forget a library. Removal is an explicit operation for M1-14's API.
//
// Idempotent: it runs at every controller start, and the second run must be a
// no-op rather than a second set of rows.
func (c *Catalog) ReconcileLibraries(ctx context.Context, specs []LibrarySpec) ([]ReconciledRoot, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	now := c.clock.Now()
	stamp := now.Format(timestampFormat)

	var (
		roots   []ReconciledRoot
		pending []events.Event
	)

	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		roots = roots[:0]
		pending = pending[:0]
		for _, spec := range specs {
			if spec.Name == "" {
				return errors.New("catalog: a library needs a name")
			}
			if spec.ContentType == "" {
				return fmt.Errorf("catalog: library %q needs a content_type", spec.Name)
			}
			// Known, not merely non-empty (#896): config.Validate checks the
			// same, and so does POST /libraries, but this is the write and the
			// catalog does not trust its callers to have asked.
			if !identification.IsContentType(spec.ContentType) {
				return fmt.Errorf("catalog: library %q has content_type %q, which is not one Heyarr understands — use one of: %s",
					spec.Name, spec.ContentType, strings.Join(identification.ContentTypes(), ", "))
			}

			libraryID, created, err := getOrCreateLibrary(ctx, tx, spec, stamp)
			if err != nil {
				return err
			}
			if created {
				ev, err := c.events.EmitTx(ctx, tx, events.TypeLibraryCreated, "library", libraryID,
					map[string]any{"name": spec.Name, "content_type": spec.ContentType})
				if err != nil {
					return err
				}
				pending = append(pending, ev)
			}

			for _, rootPath := range spec.Roots {
				if !filepath.IsAbs(rootPath) {
					return fmt.Errorf("catalog: library %q root %q must be an absolute path", spec.Name, rootPath)
				}
				clean := filepath.Clean(rootPath)
				rootID, created, err := getOrCreateRoot(ctx, tx, libraryID, clean, stamp)
				if err != nil {
					return err
				}
				roots = append(roots, ReconciledRoot{
					ID: rootID, LibraryID: libraryID, Path: clean, Created: created,
				})
				if created {
					ev, err := c.events.EmitTx(ctx, tx, events.TypeLibraryRootAdded, "library_root", rootID,
						map[string]any{"library_id": libraryID, "library": spec.Name, "path": clean})
					if err != nil {
						return err
					}
					pending = append(pending, ev)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	c.events.Publish(pending...)
	for _, r := range roots {
		if r.Created {
			c.log.Info("registered a library root", "root_id", r.ID, "library_id", r.LibraryID, "path", r.Path)
		}
	}
	return roots, nil
}

func getOrCreateLibrary(ctx context.Context, tx *sql.Tx, spec LibrarySpec, stamp string) (string, bool, error) {
	id := uuid.Must(uuid.NewV7()).String()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO libraries (id, name, content_type, enabled, created_at)
		VALUES (?, ?, ?, 1, ?)
		ON CONFLICT (name) DO NOTHING`, id, spec.Name, spec.ContentType, stamp)
	if err != nil {
		return "", false, fmt.Errorf("catalog: registering library %q: %w", spec.Name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", false, fmt.Errorf("catalog: registering library %q: %w", spec.Name, err)
	}
	if n == 1 {
		return id, true, nil
	}

	var (
		existing    string
		contentType string
	)
	if err := tx.QueryRowContext(ctx,
		`SELECT id, content_type FROM libraries WHERE name = ?`, spec.Name).
		Scan(&existing, &contentType); err != nil {
		return "", false, fmt.Errorf("catalog: re-reading library %q: %w", spec.Name, err)
	}
	// Changing a library's content type would re-identify every work under it,
	// which is a migration rather than a config edit. Refusing names it.
	if contentType != spec.ContentType {
		// A row whose type predates #227's gate is the one an operator will hit
		// while correcting the config: the new spelling is right and the row
		// still cannot follow it. Say that, and name the way out: the reconcile
		// will not retype a row by itself, but `library retype` will, and then
		// this check passes (ADR-0127).
		if !identification.IsContentType(contentType) {
			return "", false, fmt.Errorf("catalog: library %q is %q in the database, which is not a content type Heyarr "+
				"understands, and %s in the configuration — correct the row with `heyarr library retype %s %s`: "+
				"remove the library from the configuration to start, retype it, then restore it, and the existing row "+
				"and its content are kept as they are",
				spec.Name, contentType, spec.ContentType, spec.Name, spec.ContentType)
		}
		return "", false, fmt.Errorf("catalog: library %q is %s in the database and %s in the configuration — "+
			"a library's content type cannot be changed in place; create a new library instead",
			spec.Name, contentType, spec.ContentType)
	}
	return existing, false, nil
}

func getOrCreateRoot(ctx context.Context, tx *sql.Tx, libraryID, path, stamp string) (string, bool, error) {
	id := uuid.Must(uuid.NewV7()).String()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO library_roots (id, library_id, path, enabled, created_at)
		VALUES (?, ?, ?, 1, ?)
		ON CONFLICT (library_id, path) DO NOTHING`, id, libraryID, path, stamp)
	if err != nil {
		return "", false, fmt.Errorf("catalog: registering library root %s: %w", path, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", false, fmt.Errorf("catalog: registering library root %s: %w", path, err)
	}
	if n == 1 {
		return id, true, nil
	}
	var existing string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM library_roots WHERE library_id = ? AND path = ?`, libraryID, path).
		Scan(&existing); err != nil {
		return "", false, fmt.Errorf("catalog: re-reading library root %s: %w", path, err)
	}
	return existing, false, nil
}

// UnknownTypeLibrary is a library row whose content_type is not one Heyarr
// understands.
type UnknownTypeLibrary struct {
	ID          string
	Name        string
	ContentType string
}

// LibrariesWithUnknownContentType reports the libraries whose stored
// content_type identification.IsContentType refuses (#896).
//
// Every way in now refuses an unknown type, but a library created before #227
// closed the API gate, or before #896 closed the config one, keeps what it was
// given — `show`, in the one case on record. Such a library is identified as if
// it declared no type and is never an acquisition target for a wanted series,
// and nothing about it looks wrong. This is how the controller finds them to
// say so at start.
//
// Filtered here rather than in SQL so the vocabulary stays one list.
func (c *Catalog) LibrariesWithUnknownContentType(ctx context.Context) ([]UnknownTypeLibrary, error) {
	rows, err := c.db.Reader().QueryContext(ctx,
		`SELECT id, name, content_type FROM libraries ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing library content types: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []UnknownTypeLibrary
	for rows.Next() {
		var l UnknownTypeLibrary
		if err := rows.Scan(&l.ID, &l.Name, &l.ContentType); err != nil {
			return nil, fmt.Errorf("catalog: listing library content types: %w", err)
		}
		if !identification.IsContentType(l.ContentType) {
			out = append(out, l)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: listing library content types: %w", err)
	}
	return out, nil
}

// ErrNoLibrary is RetypeLibrary's answer for an id no library has.
var ErrNoLibrary = errors.New("catalog: no library with that id")

// ErrUnknownContentType is RetypeLibrary's refusal of a target type
// identification.IsContentType does not know: retyping to another unknown type
// would trade one wrong declaration for another.
var ErrUnknownContentType = errors.New("catalog: not a content type Heyarr understands")

// KnownContentTypeError is RetypeLibrary's refusal of a library whose current
// type is already a known one. Moving between two known types re-identifies
// every work in the library, which is a migration rather than a correction;
// the reconciler refuses it for the same reason.
type KnownContentTypeError struct {
	Name        string
	ContentType string
	Wanted      string
}

func (e *KnownContentTypeError) Error() string { return "catalog: " + e.Reason() }

// Reason is the refusal without the package prefix, for an API problem or a
// CLI message to carry as it is.
func (e *KnownContentTypeError) Reason() string {
	return fmt.Sprintf("library %q is already %s, a content type Heyarr understands — "+
		"a retype only corrects a type Heyarr does not understand; changing %s to %s would re-identify "+
		"every work in the library, so create a new library instead",
		e.Name, e.ContentType, e.ContentType, e.Wanted)
}

// RetypedLibrary is what RetypeLibrary did. Changed is false when the library
// already had the wanted type, which is not a transition.
type RetypedLibrary struct {
	ID                  string
	Name                string
	ContentType         string
	PreviousContentType string
	Changed             bool
}

// RetypeLibrary gives a library whose stored content_type Heyarr does not
// understand a type it does (#896, ADR-0127). It is the only in-place change
// of a library's type, and it is one-way on purpose: from unknown to known.
//
// A row created before the gates closed keeps a type like `show`, which no
// identification rule declares and no acquisition target matches. Guessing the
// right type is what createLibrary refuses to do, so the operator names it.
// From a known type the change is refused (KnownContentTypeError): that is a
// migration, not a correction. A target that is not known is refused
// (ErrUnknownContentType).
//
// Retyping to the type the library already has succeeds with Changed false and
// emits nothing, so running the command twice is safe — the second run finds a
// known type equal to the one asked for, which is not a refusal.
//
// Only the column changes. Roots, assets and their works stay as they are, and
// works already identified under the old type are not re-identified.
func (c *Catalog) RetypeLibrary(ctx context.Context, id, contentType string) (RetypedLibrary, error) {
	if !identification.IsContentType(contentType) {
		return RetypedLibrary{}, fmt.Errorf("%w: %q — use one of: %s",
			ErrUnknownContentType, contentType, strings.Join(identification.ContentTypes(), ", "))
	}

	var (
		out RetypedLibrary
		ev  events.Event
	)
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		out = RetypedLibrary{ID: id, ContentType: contentType}
		err := tx.QueryRowContext(ctx, `SELECT name, content_type FROM libraries WHERE id = ?`, id).
			Scan(&out.Name, &out.PreviousContentType)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoLibrary
		}
		if err != nil {
			return fmt.Errorf("catalog: reading library %s: %w", id, err)
		}
		if out.PreviousContentType == contentType {
			return nil
		}
		if identification.IsContentType(out.PreviousContentType) {
			return &KnownContentTypeError{Name: out.Name, ContentType: out.PreviousContentType, Wanted: contentType}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE libraries SET content_type = ? WHERE id = ?`, contentType, id); err != nil {
			return fmt.Errorf("catalog: retyping library %s: %w", id, err)
		}
		ev, err = c.events.EmitTx(ctx, tx, events.TypeLibraryRetyped, "library", id, map[string]any{
			"library_id":            id,
			"name":                  out.Name,
			"content_type":          contentType,
			"previous_content_type": out.PreviousContentType,
		})
		if err != nil {
			return err
		}
		out.Changed = true
		return nil
	})
	if err != nil {
		return RetypedLibrary{}, err
	}
	if out.Changed {
		c.events.Publish(ev)
		c.log.Info("retyped a library", "library_id", id, "library", out.Name,
			"content_type", contentType, "previous_content_type", out.PreviousContentType)
	}
	return out, nil
}
