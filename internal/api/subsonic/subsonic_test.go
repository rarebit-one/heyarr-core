// The subsonic adapter is tested end to end over a real database, a real CAS
// and the real router — nothing is mocked. A Subsonic client's whole experience
// is HTTP in and HTTP out, so the tests drive it the same way: a request with a
// credential on the query string, a response parsed as a client would parse it.
//
//nolint:bodyclose // response bodies are closed by the t.Cleanup the harness registers
package subsonic_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/api/blobs"
	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/subsonic"
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
	token   string            // a read-scoped bearer, sent as the Subsonic password
	bytesOf map[string][]byte // track title -> its exact bytes, for stream assertions
	hashOf  map[string]string // track title -> blob hash
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

	sub, err := subsonic.New(subsonic.Options{
		DB: db, Auth: verifier, Blobs: blobHandler,
		ServerVersion: "test-server", Logger: slog.New(slog.DiscardHandler),
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
		MountPublic:        []httpapi.MountFunc{sub.Mount},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	h := &harness{
		t: t, http: ts, db: db, store: store,
		bytesOf: map[string][]byte{}, hashOf: map[string]string{},
	}
	h.token = h.mint("player", auth.ScopeRead)
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

// seed builds a small but realistic music library:
//
//	The Cartographers  (files under C — leading "The" is stripped)
//	  Contour Lines (2001)  — Datum.flac, Benchmark.mp3
//	  Field Notes (2004)    — Traverse.flac
//	Bearing  (files under B)
//	  Azimuth (2010)        — Vertex.flac
//	  Meridian (2013)       — one LINKED track (no blob): must never be listed
//	                          as a streamable song.
func (h *harness) seed(ctx context.Context, store cas.Store) {
	h.t.Helper()
	h.exec(`INSERT INTO libraries (id, name, content_type, enabled, created_at)
		VALUES ('lib-music', 'Music', 'music', 1, ?)`, stamp)
	// A non-music library, to prove the projection only ever sees music.
	h.exec(`INSERT INTO libraries (id, name, content_type, enabled, created_at)
		VALUES ('lib-film', 'Films', 'movie', 1, ?)`, stamp)

	h.album("wa", "Contour Lines", "contour lines", 2001, "The Cartographers")
	h.album("wb", "Field Notes", "field notes", 2004, "The Cartographers")
	h.album("wc", "Azimuth", "azimuth", 2010, "Bearing")
	h.album("wd", "Meridian", "meridian", 2013, "Bearing")

	h.track(ctx, store, "wa", "ea1", 1, 1, "Datum", "flac", "audio/flac", []byte("the-datum-flac-bytes-0123456789"))
	h.track(ctx, store, "wa", "ea2", 1, 2, "Benchmark", "mp3", "audio/mpeg", []byte("benchmark-mp3-bytes-abcdefghijkl"))
	h.track(ctx, store, "wb", "eb1", 1, 1, "Traverse", "flac", "audio/flac", []byte("traverse-flac-bytes"))
	h.track(ctx, store, "wc", "ec1", 1, 1, "Vertex", "flac", "audio/flac", []byte("vertex-flac-bytes"))

	// A linked edition on Meridian: an asset with NO blob (ADR-0020). It exists
	// in the catalogue but cannot be streamed, so it must be absent from every
	// song listing and count as zero.
	h.exec(`INSERT INTO editions (id, work_id, edition_key, label, edition_type, attributes, created_at)
		VALUES ('ed1', 'wd', 'ed1', 'FLAC', 'flac', '{"track":1,"track_title":"Cusp"}', ?)`, stamp)
	h.exec(`INSERT INTO assets
		(id, edition_id, library_id, source_class, blob_hash, source_path, role, filename, mime, identification_source, created_at, updated_at)
		VALUES ('ad1', 'ed1', 'lib-music', 'linked', NULL, '/music/Bearing/Meridian/01 Cusp.flac', 'primary', '01 Cusp.flac', 'audio/flac', 'scan', ?, ?)`, stamp, stamp)
}

func (h *harness) album(id, title, sortTitle string, year int, artist string) {
	h.t.Helper()
	h.exec(`INSERT INTO works
		(id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES (?, 'music', ?, ?, ?, ?, json_object('artist', ?), ?, ?)`,
		id, "music:"+id, title, sortTitle, year, artist, stamp, stamp)
}

func (h *harness) track(ctx context.Context, store cas.Store, workID, editionID string, disc, trackNo int, title, suffix, mime string, data []byte) {
	h.t.Helper()
	desc, err := store.Put(ctx, bytes.NewReader(data))
	if err != nil {
		h.t.Fatal(err)
	}
	hash := desc.Hash.String()
	h.bytesOf[title] = data
	h.hashOf[title] = hash

	h.exec(`INSERT INTO editions (id, work_id, edition_key, label, edition_type, attributes, created_at)
		VALUES (?, ?, ?, ?, ?, json_object('disc', ?, 'track', ?, 'track_title', ?), ?)`,
		editionID, workID, editionID, strings.ToUpper(suffix), suffix, disc, trackNo, title, stamp)
	h.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, ?, ?, ?)`,
		hash, len(data), mime, stamp)
	h.exec(`INSERT INTO assets
		(id, edition_id, library_id, source_class, blob_hash, role, filename, mime, identification_source, created_at, updated_at)
		VALUES (?, ?, 'lib-music', 'managed', ?, 'primary', ?, ?, 'scan', ?, ?)`,
		"a"+editionID, editionID, hash, title+"."+suffix, mime, stamp, stamp)
}

// --- request helpers --------------------------------------------------------

// creds are the auth query params a real client sends: the Heyarr bearer token
// as the Subsonic password, plus the client-name and version handshake fields.
func (h *harness) creds() url.Values {
	return url.Values{
		"u": {"player"}, "p": {h.token},
		"c": {"acceptance-client"}, "v": {"1.16.1"}, "f": {"json"},
	}
}

// get performs an authenticated JSON request and decodes the envelope.
func (h *harness) get(method string, extra url.Values) subResp {
	h.t.Helper()
	q := h.creds()
	for k, vs := range extra {
		q[k] = vs
	}
	return decode(h.t, h.raw(method, q))
}

// raw performs a request and returns the whole HTTP response, body buffered.
func (h *harness) raw(method string, q url.Values) *http.Response {
	h.t.Helper()
	u := h.http.URL + subsonic.Prefix + "/" + method + "?" + q.Encode()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func decode(t *testing.T, resp *http.Response) subResp {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		Resp subResp `json:"subsonic-response"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("response is not JSON (%d): %s", resp.StatusCode, body)
	}
	return w.Resp
}

// The client-side view of the envelope, deliberately independent of the
// adapter's own types so a rename there cannot quietly pass a test.
type subResp struct {
	Status        string `json:"status"`
	Version       string `json:"version"`
	Type          string `json:"type"`
	ServerVersion string `json:"serverVersion"`
	OpenSubsonic  bool   `json:"openSubsonic"`
	Error         *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	License *struct {
		Valid bool `json:"valid"`
	} `json:"license"`
	MusicFolders *struct {
		MusicFolder []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"musicFolder"`
	} `json:"musicFolders"`
	Artists *struct {
		IgnoredArticles string `json:"ignoredArticles"`
		Index           []struct {
			Name   string `json:"name"`
			Artist []struct {
				ID         string `json:"id"`
				Name       string `json:"name"`
				AlbumCount int    `json:"albumCount"`
			} `json:"artist"`
		} `json:"index"`
	} `json:"artists"`
	Artist *struct {
		ID         string   `json:"id"`
		Name       string   `json:"name"`
		AlbumCount int      `json:"albumCount"`
		Album      []albumT `json:"album"`
	} `json:"artist"`
	AlbumList2 *struct {
		Album []albumT `json:"album"`
	} `json:"albumList2"`
	Album                  *albumT `json:"album"`
	OpenSubsonicExtensions []struct {
		Name     string `json:"name"`
		Versions []int  `json:"versions"`
	} `json:"openSubsonicExtensions"`
}

type albumT struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Artist    string  `json:"artist"`
	ArtistID  string  `json:"artistId"`
	SongCount int     `json:"songCount"`
	Duration  int     `json:"duration"`
	Year      int     `json:"year"`
	Song      []songT `json:"song"`
}

type songT struct {
	ID          string `json:"id"`
	Parent      string `json:"parent"`
	IsDir       bool   `json:"isDir"`
	Title       string `json:"title"`
	Album       string `json:"album"`
	Artist      string `json:"artist"`
	Track       int    `json:"track"`
	DiscNumber  int    `json:"discNumber"`
	Year        int    `json:"year"`
	AlbumID     string `json:"albumId"`
	ArtistID    string `json:"artistId"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType"`
	Suffix      string `json:"suffix"`
	Type        string `json:"type"`
}

// findAlbum returns the album with the given name from a list, or fails.
func findAlbum(t *testing.T, albums []albumT, name string) albumT {
	t.Helper()
	for _, a := range albums {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("album %q not in %v", name, names(albums))
	return albumT{}
}

func names(albums []albumT) []string {
	out := make([]string, len(albums))
	for i, a := range albums {
		out[i] = a.Name
	}
	return out
}
