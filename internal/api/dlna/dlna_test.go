// The DLNA adapter is tested end to end over a real database, a real CAS and
// the real router, with the render route mounted beside it — because the whole
// point of a res URL is that a device can fetch the bytes from it, and a test
// that stopped at "a URL was emitted" would prove the mechanism-with-no-caller
// defect rather than refute it. So the tests Browse, read the res URL out of the
// DIDL-Lite, and fetch it.
//
//nolint:bodyclose // response bodies are closed by the t.Cleanup the harness registers
package dlna_test

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/api/blobs"
	"github.com/rarebit-one/heyarr-core/internal/api/dlna"
	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/render"
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
	bytesOf map[string][]byte // asset title -> its exact bytes
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

	secret, err := render.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	renderHandler, err := render.New(render.Options{Blobs: blobHandler, Secret: secret, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	dlnaHandler, err := dlna.New(dlna.Options{
		DB: db, RenderSecret: secret, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.HTTP.Auth.Enabled = false
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.UnixSocket = ""

	srv, err := httpapi.New(httpapi.Options{
		Config: cfg, Logger: slog.New(slog.DiscardHandler), DB: db, Events: eventLog,
		Build:              buildinfo.Info{Version: "test", Commit: "abc", Date: stamp},
		SchemaVersion:      4,
		KnownSchemaVersion: 4,
		MountPublic:        []httpapi.MountFunc{dlnaHandler.Mount, renderHandler.Mount},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	h := &harness{t: t, http: ts, db: db, bytesOf: map[string][]byte{}}
	h.seed(ctx, casStore)
	return h
}

func (h *harness) exec(q string, args ...any) {
	h.t.Helper()
	if _, err := h.db.Writer().Exec(q, args...); err != nil {
		h.t.Fatalf("seeding (%s): %v", q, err)
	}
}

// seed builds a tiny library across content types:
//
//	Movies:  Arrival (2016)  — .mkv, video/x-matroska (render-servable)
//	Music:   Datum           — .flac, audio/flac (render-servable)
//	Books:   The Survey       — .epub, application/epub+zip (NOT servable → omitted)
func (h *harness) seed(ctx context.Context, store cas.Store) {
	h.t.Helper()
	h.asset(ctx, store, "movie", "wm", "em", "am", "Arrival", 2016, "mkv", "video/x-matroska", []byte("arrival-mkv-bytes-0123456789abcdef"))
	h.asset(ctx, store, "music", "wu", "eu", "au", "Datum", 2001, "flac", "audio/flac", []byte("datum-flac-bytes-fedcba9876543210"))
	h.asset(ctx, store, "book", "wb", "eb", "ab", "The Survey", 2019, "epub", "application/epub+zip", []byte("the-survey-epub-bytes"))
}

func (h *harness) asset(ctx context.Context, store cas.Store, ct, workID, editionID, assetID, title string, year int, suffix, mime string, data []byte) {
	h.t.Helper()
	desc, err := store.Put(ctx, bytes.NewReader(data))
	if err != nil {
		h.t.Fatal(err)
	}
	hash := desc.Hash.String()
	h.bytesOf[title] = data

	h.exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, '{}', ?, ?)`, workID, ct, ct+":"+workID, title, strings.ToLower(title), year, stamp, stamp)
	h.exec(`INSERT INTO editions (id, work_id, edition_key, label, edition_type, attributes, created_at)
		VALUES (?, ?, ?, ?, ?, '{}', ?)`, editionID, workID, editionID, strings.ToUpper(suffix), suffix, stamp)
	h.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, ?, ?, ?)`, hash, len(data), mime, stamp)
	h.exec(`INSERT INTO assets
		(id, edition_id, source_class, blob_hash, role, filename, mime, identification_source, created_at, updated_at)
		VALUES (?, ?, 'managed', ?, 'primary', ?, ?, 'scan', ?, ?)`,
		assetID, editionID, hash, title+"."+suffix, mime, stamp, stamp)
}

// browse issues a SOAP Browse and returns the decoded envelope.
func (h *harness) browse(objectID, flag string) browseResult {
	h.t.Helper()
	body := `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` +
		`<u:Browse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1">` +
		`<ObjectID>` + objectID + `</ObjectID><BrowseFlag>` + flag + `</BrowseFlag>` +
		`<Filter>*</Filter><StartingIndex>0</StartingIndex><RequestedCount>0</RequestedCount>` +
		`<SortCriteria></SortCriteria></u:Browse></s:Body></s:Envelope>`
	return h.decodeBrowse(h.post("/dlna/control/ContentDirectory", body))
}

func (h *harness) post(path, body string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, h.http.URL+path, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:ContentDirectory:1#Browse"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (h *harness) get(path string) *http.Response {
	h.t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, h.http.URL+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// --- decoding -----------------------------------------------------------------

type browseResult struct {
	status         int
	fault          bool
	faultCode      int
	numberReturned int
	totalMatches   int
	didl           didl
}

type didl struct {
	XMLName    xml.Name    `xml:"DIDL-Lite"`
	Containers []container `xml:"container"`
	Items      []item      `xml:"item"`
}

type container struct {
	ID         string `xml:"id,attr"`
	ParentID   string `xml:"parentID,attr"`
	ChildCount int    `xml:"childCount,attr"`
	Title      string `xml:"title"`
	Class      string `xml:"class"`
}

type item struct {
	ID       string `xml:"id,attr"`
	ParentID string `xml:"parentID,attr"`
	Title    string `xml:"title"`
	Class    string `xml:"class"`
	Res      struct {
		ProtocolInfo string `xml:"protocolInfo,attr"`
		Size         int64  `xml:"size,attr"`
		URL          string `xml:",chardata"`
	} `xml:"res"`
}

// decodeBrowse parses a SOAP reply by unmarshalling the whole envelope, which
// lets encoding/xml unescape the Result element's DIDL-Lite into a string for
// us — exactly the double-decoding a real control point does — rather than
// scraping it with a regex.
func (h *harness) decodeBrowse(resp *http.Response) browseResult {
	h.t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	var env struct {
		Body struct {
			BrowseResponse struct {
				Result         string `xml:"Result"`
				NumberReturned int    `xml:"NumberReturned"`
				TotalMatches   int    `xml:"TotalMatches"`
			} `xml:"BrowseResponse"`
			Fault struct {
				Detail struct {
					UPnPError struct {
						ErrorCode int `xml:"errorCode"`
					} `xml:"UPnPError"`
				} `xml:"detail"`
			} `xml:"Fault"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(raw, &env); err != nil {
		h.t.Fatalf("response is not a SOAP envelope: %v\n%s", err, raw)
	}
	out := browseResult{status: resp.StatusCode}
	if env.Body.Fault.Detail.UPnPError.ErrorCode != 0 {
		out.fault = true
		out.faultCode = env.Body.Fault.Detail.UPnPError.ErrorCode
		return out
	}
	out.numberReturned = env.Body.BrowseResponse.NumberReturned
	out.totalMatches = env.Body.BrowseResponse.TotalMatches
	if r := env.Body.BrowseResponse.Result; r != "" {
		if err := xml.Unmarshal([]byte(r), &out.didl); err != nil {
			h.t.Fatalf("Result is not DIDL-Lite: %v\n%s", err, r)
		}
	}
	return out
}

func (r browseResult) itemByTitle(t *testing.T, title string) item {
	t.Helper()
	for _, it := range r.didl.Items {
		if it.Title == title {
			return it
		}
	}
	t.Fatalf("item %q not in browse result (%d items)", title, len(r.didl.Items))
	return item{}
}

func (r browseResult) containerByID(t *testing.T, id string) container {
	t.Helper()
	for _, c := range r.didl.Containers {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("container %q not in browse result", id)
	return container{}
}
