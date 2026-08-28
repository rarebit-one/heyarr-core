// The OPDS adapter is tested end to end over a real database, a real CAS and
// the real router — nothing is mocked. An OPDS reader's whole experience is HTTP
// in and XML/bytes out, so the tests drive it the same way.
//
//nolint:bodyclose // response bodies are closed by the t.Cleanup the harness registers
package opds_test

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/api/blobs"
	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/opds"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

const stamp = "2026-08-01T00:00:00Z"

type harness struct {
	t       *testing.T
	http    *httptest.Server
	db      *sqlite.DB
	store   *auth.Store
	token   string            // a read-scoped bearer, sent as the Basic password
	bytesOf map[string][]byte // edition id -> its exact bytes
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(dir, "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	store, err := auth.NewStore(auth.StoreOptions{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := auth.NewVerifier(auth.VerifierOptions{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}

	casStore, err := cas.OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blobHandler, err := blobs.New(blobs.Options{Store: casStore, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}

	handler, err := opds.New(opds.Options{
		DB: db, Auth: verifier, Blobs: blobHandler, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.HTTP.Auth.Enabled = true
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.UnixSocket = ""

	srv, err := httpapi.New(httpapi.Options{
		Config: cfg, Logger: slog.New(slog.DiscardHandler), DB: db, Verifier: verifier, Events: eventLog,
		Build:              buildinfo.Info{Version: "test", Commit: "abc", Date: stamp},
		SchemaVersion:      4,
		KnownSchemaVersion: 4,
		MountPublic:        []httpapi.MountFunc{handler.Mount},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	h := &harness{t: t, http: ts, db: db, store: store, bytesOf: map[string][]byte{}}
	h.token = h.mint("reader", auth.ScopeRead)
	h.seed(ctx, casStore)
	return h
}

func (h *harness) mint(name string, scopes ...auth.Scope) string {
	h.t.Helper()
	created, err := h.store.Create(context.Background(), name, scopes, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	return created.Secret
}

func (h *harness) exec(query string, args ...any) {
	h.t.Helper()
	if _, err := h.db.Writer().Exec(query, args...); err != nil {
		h.t.Fatalf("seeding (%s): %v", query, err)
	}
}

// seed builds a small publication library:
//
//	Ada Prentice — The Long Survey   epub + cbz  (both streamable)
//	Bex Coombs   — Marginalia         one LINKED format only (no blob):
//	                                   must never be offered as acquirable.
func (h *harness) seed(ctx context.Context, store cas.Store) {
	h.t.Helper()
	h.exec(`INSERT INTO libraries (id, name, content_type, enabled, created_at)
		VALUES ('lib-books', 'Books', 'book', 1, ?)`, stamp)

	h.book("wa", "The Long Survey", "the long survey", 2019, "Ada Prentice")
	h.book("wb", "Marginalia", "marginalia", 2022, "Bex Coombs")
	// A non-book work, to prove the acquisition feed only ever sees books.
	h.exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES ('wm', 'movie', 'movie:wm', 'Arrival', 'arrival', 2016, '{}', ?, ?)`, stamp, stamp)

	h.edition(ctx, store, "wa", "ea1", "epub", "application/epub+zip", []byte("epub-bytes-of-the-long-survey-0123456789"))
	h.edition(ctx, store, "wa", "ea2", "cbz", "application/x-cbz", []byte("cbz-bytes-of-the-long-survey"))

	// A linked edition on Marginalia: an asset with NO blob (ADR-0020). It is in
	// the catalogue but cannot be downloaded, so it must have no entry at all.
	h.exec(`INSERT INTO editions (id, work_id, edition_key, label, edition_type, attributes, created_at)
		VALUES ('eb1', 'wb', 'epub', 'EPUB', 'epub', '{}', ?)`, stamp)
	h.exec(`INSERT INTO assets
		(id, edition_id, library_id, source_class, blob_hash, source_path, role, filename, mime, identification_source, created_at, updated_at)
		VALUES ('ab1', 'eb1', 'lib-books', 'linked', NULL, '/books/Bex Coombs/Marginalia.epub', 'primary', 'Marginalia.epub', 'application/epub+zip', 'scan', ?, ?)`, stamp, stamp)
}

func (h *harness) book(id, title, sortTitle string, year int, authorName string) {
	h.t.Helper()
	h.exec(`INSERT INTO works
		(id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES (?, 'book', ?, ?, ?, ?, json_object('author', ?), ?, ?)`,
		id, "book:"+id, title, sortTitle, year, authorName, stamp, stamp)
}

func (h *harness) edition(ctx context.Context, store cas.Store, workID, editionID, format, mime string, data []byte) {
	h.t.Helper()
	desc, err := store.Put(ctx, bytes.NewReader(data))
	if err != nil {
		h.t.Fatal(err)
	}
	hash := desc.Hash.String()
	h.bytesOf[editionID] = data

	h.exec(`INSERT INTO editions (id, work_id, edition_key, label, edition_type, attributes, created_at)
		VALUES (?, ?, ?, ?, ?, '{}', ?)`, editionID, workID, format, format, format, stamp)
	h.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, ?, ?, ?)`,
		hash, len(data), mime, stamp)
	h.exec(`INSERT INTO assets
		(id, edition_id, library_id, source_class, blob_hash, role, filename, mime, identification_source, created_at, updated_at)
		VALUES (?, ?, 'lib-books', 'managed', ?, 'primary', ?, ?, 'scan', ?, ?)`,
		"a"+editionID, editionID, hash, format, mime, stamp, stamp)
}

// --- request helpers --------------------------------------------------------

func (h *harness) get(path string, auth bool) *http.Response {
	h.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, h.http.URL+path, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	if auth {
		req.SetBasicAuth("reader", h.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func body(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The client-side view of a feed, independent of the adapter's own types so a
// rename there cannot quietly pass a test.
type feedT struct {
	XMLName xml.Name `xml:"feed"`
	Title   string   `xml:"title"`
	Links   []struct {
		Rel  string `xml:"rel,attr"`
		Href string `xml:"href,attr"`
		Type string `xml:"type,attr"`
	} `xml:"link"`
	Entries []struct {
		Title   string `xml:"title"`
		ID      string `xml:"id"`
		Authors []struct {
			Name string `xml:"name"`
		} `xml:"author"`
		Links []struct {
			Rel  string `xml:"rel,attr"`
			Href string `xml:"href,attr"`
			Type string `xml:"type,attr"`
		} `xml:"link"`
	} `xml:"entry"`
}

func parseFeed(t *testing.T, resp *http.Response) feedT {
	t.Helper()
	var f feedT
	if err := xml.Unmarshal(body(t, resp), &f); err != nil {
		t.Fatalf("response is not an Atom feed: %v", err)
	}
	return f
}
