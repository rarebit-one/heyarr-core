package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/google/uuid"

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
