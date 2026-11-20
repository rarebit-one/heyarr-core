package catalog_test

import (
	"context"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// A library's content type is checked on the reconcile path too, not only by
// POST /libraries (#896). The catalog is the write; it does not trust its
// callers to have run config.Validate first.
func TestReconcileLibrariesRefusesAnUnknownContentType(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tests := []struct {
		name        string
		contentType string
		wantErr     string
	}{
		{"known", "series", ""},
		{"legacy spelling", "show", `library "tv" has content_type "show", which is not one Heyarr understands — use one of: movie, series, music, book, document, game, rom`},
		{"unknown sentinel", "unknown", `content_type "unknown"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name := "tv"
			if tt.wantErr == "" {
				name = "tv-" + tt.contentType
			}
			before := h.eventCount(t)
			_, err := h.cat.ReconcileLibraries(ctx, []catalog.LibrarySpec{
				{Name: name, ContentType: tt.contentType, Roots: []string{"/srv/media/" + name}},
			})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("reconcile: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
			// Refused before anything was written: no row, no event.
			var n int
			if err := h.db.Reader().QueryRow(`SELECT count(*) FROM libraries WHERE name = ?`, name).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Errorf("a refused library was written: %d rows", n)
			}
			if got := h.eventCount(t); got != before {
				t.Errorf("a refused library emitted %d events", got-before)
			}
		})
	}
}

// A row that predates the gates keeps its type, and is reported (#896). Its
// operator correcting the config to the right spelling meets a refusal that
// says why the row cannot follow, not "create a new library".
func TestLibrariesWithUnknownContentType(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.cat.ReconcileLibraries(ctx, []catalog.LibrarySpec{
		{Name: "films", ContentType: "movie", Roots: []string{"/srv/media/films"}},
	}); err != nil {
		t.Fatal(err)
	}
	h.exec(t, `INSERT INTO libraries (id, name, content_type, enabled, created_at)
		VALUES ('lib-legacy', 'tv', 'show', 1, ?)`, stamp)

	got, err := h.cat.LibrariesWithUnknownContentType(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []catalog.UnknownTypeLibrary{{ID: "lib-legacy", Name: "tv", ContentType: "show"}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	_, err = h.cat.ReconcileLibraries(ctx, []catalog.LibrarySpec{
		{Name: "tv", ContentType: "series", Roots: []string{"/srv/media/tv"}},
	})
	if err == nil {
		t.Fatal("reconciling a legacy row to a new type succeeded")
	}
	for _, s := range []string{`"show" in the database`, "not a content type Heyarr understands", "#896"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("err = %v, want it to contain %q", err, s)
		}
	}
}
