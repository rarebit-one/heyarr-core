package catalog_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// seedLegacyShowLibrary is the case #896 was opened for: a library created
// before the gates, still `show`, holding a series' assets and no roots at all.
func (h *harness) seedLegacyShowLibrary(t *testing.T) {
	t.Helper()
	h.exec(t, `INSERT INTO libraries (id, name, content_type, enabled, created_at)
		VALUES ('lib-legacy', 'tv', 'show', 1, ?)`, stamp)
	h.exec(t, `INSERT INTO works
		(id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES ('w-show', 'series', 'series:a-show', 'A Show', 'a show', 2018, '{}', ?, ?)`, stamp, stamp)
	h.exec(t, `INSERT INTO editions (id, work_id, label, edition_type, language, attributes, created_at)
		VALUES ('e-show', 'w-show', '1080p', 'bluray', 'en', '{}', ?)`, stamp)
	for _, ep := range []string{"s01e01", "s01e02"} {
		hash := hexHash(ep)
		h.exec(t, `INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, 1000, 'video/x-matroska', ?)`,
			hash, stamp)
		h.exec(t, `INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash,
				source_path, role, filename, mime, identification_source, created_at, updated_at)
			VALUES (?, 'e-show', 'lib-legacy', 'managed', ?, ?, 'primary', ?, 'video/x-matroska', 'path', ?, ?)`,
			"a-"+ep, hash, "/srv/media/tv/"+ep+".mkv", ep+".mkv", stamp, stamp)
	}
}

func (h *harness) libraryType(t *testing.T, id string) string {
	t.Helper()
	var ct string
	if err := h.db.Reader().QueryRow(`SELECT content_type FROM libraries WHERE id = ?`, id).Scan(&ct); err != nil {
		t.Fatal(err)
	}
	return ct
}

// A non-empty library with no roots and an unknown type takes a known one in
// place (#896, ADR-0127): only the column changes, its assets stay its own, and
// the change is on the log with both types.
func TestRetypeLibraryCorrectsAnUnknownType(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedLegacyShowLibrary(t)

	got, err := h.cat.RetypeLibrary(context.Background(), "lib-legacy", "series")
	if err != nil {
		t.Fatalf("retype: %v", err)
	}
	want := catalog.RetypedLibrary{
		ID: "lib-legacy", Name: "tv", ContentType: "series", PreviousContentType: "show", Changed: true,
	}
	if got != want {
		t.Errorf("retype = %+v, want %+v", got, want)
	}
	if ct := h.libraryType(t, "lib-legacy"); ct != "series" {
		t.Errorf("the row says %q, want series", ct)
	}
	var assets int
	if err := h.db.Reader().QueryRow(`SELECT count(*) FROM assets WHERE library_id = 'lib-legacy'`).Scan(&assets); err != nil {
		t.Fatal(err)
	}
	if assets != 2 {
		t.Errorf("%d assets under the retyped library, want the seeded 2", assets)
	}

	evs, err := h.events.Since(context.Background(), 0, []string{events.TypeLibraryRetyped}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("emitted %d library.retyped events, want 1", len(evs))
	}
	payload := string(evs[0].Payload)
	for _, s := range []string{`"library_id":"lib-legacy"`, `"name":"tv"`, `"content_type":"series"`, `"previous_content_type":"show"`} {
		if !strings.Contains(payload, s) {
			t.Errorf("payload %s does not contain %s", payload, s)
		}
	}
	if libs, err := h.cat.LibrariesWithUnknownContentType(context.Background()); err != nil || len(libs) != 0 {
		t.Errorf("after the retype, unknown-type libraries = %+v, %v; want none", libs, err)
	}
}

// Every refusal leaves the row and the log alone.
func TestRetypeLibraryRefusals(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedLegacyShowLibrary(t)
	h.exec(t, `INSERT INTO libraries (id, name, content_type, enabled, created_at)
		VALUES ('lib-films', 'films', 'movie', 1, ?)`, stamp)
	ctx := context.Background()

	tests := []struct {
		name, id, to string
		check        func(error) bool
		message      string
	}{
		{
			"known to another known", "lib-films", "series",
			func(err error) bool { var k *catalog.KnownContentTypeError; return errors.As(err, &k) },
			`library "films" is already movie, a content type Heyarr understands`,
		},
		{
			"unknown to unknown", "lib-legacy", "tv",
			func(err error) bool { return errors.Is(err, catalog.ErrUnknownContentType) },
			`"tv" — use one of: movie, series`,
		},
		{
			"known to unknown", "lib-films", "show",
			func(err error) bool { return errors.Is(err, catalog.ErrUnknownContentType) },
			`"show"`,
		},
		{
			"missing library", "lib-nope", "series",
			func(err error) bool { return errors.Is(err, catalog.ErrNoLibrary) },
			"no library",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := h.cat.RetypeLibrary(ctx, tt.id, tt.to)
			if err == nil || !tt.check(err) {
				t.Fatalf("err = %v, want the %s refusal", err, tt.name)
			}
			if !strings.Contains(err.Error(), tt.message) {
				t.Errorf("err = %v, want it to contain %q", err, tt.message)
			}
		})
	}
	if ct := h.libraryType(t, "lib-legacy"); ct != "show" {
		t.Errorf("a refusal changed the legacy row to %q", ct)
	}
	if ct := h.libraryType(t, "lib-films"); ct != "movie" {
		t.Errorf("a refusal changed the films row to %q", ct)
	}
	if n := h.eventsOfType(t, events.TypeLibraryRetyped); n != 0 {
		t.Errorf("refusals emitted %d retypes, want 0", n)
	}
}

// Retyping to the type the library already has is not a transition: it
// succeeds, reports Changed false and emits nothing. So the operator's command
// can be run twice.
func TestRetypeLibraryTwiceIsANoOp(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedLegacyShowLibrary(t)
	ctx := context.Background()

	if _, err := h.cat.RetypeLibrary(ctx, "lib-legacy", "series"); err != nil {
		t.Fatal(err)
	}
	again, err := h.cat.RetypeLibrary(ctx, "lib-legacy", "series")
	if err != nil {
		t.Fatalf("second retype: %v", err)
	}
	if again.Changed || again.PreviousContentType != "series" {
		t.Errorf("second retype = %+v, want unchanged from series", again)
	}
	if n := h.eventsOfType(t, events.TypeLibraryRetyped); n != 1 {
		t.Errorf("two retypes emitted %d events, want 1", n)
	}
}

// A `libraries:` block corrected to `series` while the row still says `show`
// is refused, naming `library retype`; after the retype the same block
// reconciles onto the existing row instead of creating another, and gains its
// root there.
func TestReconcileConvergesAfterARetype(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedLegacyShowLibrary(t)
	ctx := context.Background()
	specs := []catalog.LibrarySpec{{Name: "tv", ContentType: "series", Roots: []string{"/srv/media/tv"}}}

	_, err := h.cat.ReconcileLibraries(ctx, specs)
	if err == nil || !strings.Contains(err.Error(), "`heyarr library retype tv series`") {
		t.Fatalf("reconcile before the retype = %v, want a refusal naming library retype", err)
	}

	if _, err := h.cat.RetypeLibrary(ctx, "lib-legacy", "series"); err != nil {
		t.Fatal(err)
	}
	roots, err := h.cat.ReconcileLibraries(ctx, specs)
	if err != nil {
		t.Fatalf("reconcile after the retype: %v", err)
	}
	if len(roots) != 1 || roots[0].LibraryID != "lib-legacy" || !roots[0].Created {
		t.Errorf("roots = %+v, want one new root on lib-legacy", roots)
	}
	var libs int
	if err := h.db.Reader().QueryRow(`SELECT count(*) FROM libraries WHERE name = 'tv'`).Scan(&libs); err != nil {
		t.Fatal(err)
	}
	if libs != 1 {
		t.Errorf("%d libraries named tv, want the one existing row", libs)
	}
	if n := h.eventsOfType(t, events.TypeLibraryCreated); n != 0 {
		t.Errorf("reconcile created %d libraries, want 0", n)
	}
}
