// Every HTTP response in this file is closed by the t.Cleanup that the harness
// registers, which bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/testutil"
)

func goldenPath(name string) string { return filepath.Join("testdata", name) }

// Every response shape is a golden file. Determinism is what makes that
// possible: the clock, the identifiers and the request id are all injected or
// supplied, so a golden file changes when the shape changes and at no other
// time (ADR-0017).
//
// A golden file regenerated from the code under test proves only that the
// output has not changed, so the assertions that carry meaning — status codes,
// scope enforcement, 404s, pagination stability — are written out separately
// rather than left to the goldens.
func TestResponseShapes(t *testing.T) {
	h := newHarness(t).seed()

	tests := []struct {
		name   string
		path   string
		golden string
	}{
		{"works", "/api/v1/works", "works_list.json"},
		{"works filtered by content type", "/api/v1/works?content_type=movie", "works_by_content_type.json"},
		{"works filtered by library", "/api/v1/works?library_id=" + libBooksID, "works_by_library.json"},
		{"works searched", "/api/v1/works?q=RUNNER", "works_search.json"},
		{"works first page", "/api/v1/works?limit=1", "works_first_page.json"},
		{"one work", "/api/v1/works/" + work1ID, "work.json"},
		{"one edition", "/api/v1/editions/" + edition1ID, "edition.json"},
		{"assets", "/api/v1/assets", "assets_list.json"},
		{"assets in a library", "/api/v1/assets?library_id=" + libFilmsID, "assets_by_library.json"},
		{"missing assets", "/api/v1/assets?state=missing", "assets_missing.json"},
		{"one asset", "/api/v1/assets/" + asset3ID, "asset_linked.json"},
		{"libraries", "/api/v1/libraries", "libraries_list.json"},
		{"one library", "/api/v1/libraries/" + libFilmsID, "library.json"},
		{"one blob", "/api/v1/blobs/" + blob1Hash, "blob.json"},
		{"peers", "/api/v1/peers", "peers_list.json"},
		{"replicas", "/api/v1/replicas", "replicas_list.json"},
		{"corrupt replicas", "/api/v1/replicas?state=corrupt", "replicas_corrupt.json"},
		{"publications", "/api/v1/publications", "publications_list.json"},
		{"publications by format", "/api/v1/publications?format=epub", "publications_epub.json"},
		{"consumption sessions", "/api/v1/consumption/sessions", "sessions_list.json"},
		{"resumable sessions", "/api/v1/consumption/sessions?state=resumable", "sessions_resumable.json"},
		{"one session", "/api/v1/consumption/sessions/" + session1ID, "session.json"},
		{"devices", "/api/v1/devices", "devices_list.json"},
		{"one device", "/api/v1/devices/" + device1ID, "device.json"},
		{"quality profiles", "/api/v1/quality-profiles", "quality_profiles_list.json"},
		{"one quality profile", "/api/v1/quality-profiles/" + profile1ID, "quality_profile.json"},
		{
			"a profile that is never finished", "/api/v1/quality-profiles/" + profile2ID,
			"quality_profile_open_ended.json",
		},
		{"jobs", "/api/v1/jobs", "jobs_list.json"},
		{"one job", "/api/v1/jobs/" + job2ID, "job_dead.json"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := h.doStable(http.MethodGet, tt.path, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			testutil.Golden(t, goldenPath(tt.golden), h.indent(resp))
		})
	}
}

// The problem documents are golden-tested too. The `type` URI is the contract
// clients branch on, so a change to one has to show up in a reviewable diff
// rather than in a client's error handling six months later.
func TestProblemShapes(t *testing.T) {
	h := newHarness(t).seed()

	tests := []struct {
		name   string
		method string
		path   string
		body   string
		status int
		golden string
	}{
		{"unknown work", http.MethodGet, "/api/v1/works/nope", "", 404, "problem_unknown_work.json"},
		{"unknown blob", http.MethodGet, "/api/v1/blobs/blake3:deadbeef", "", 404, "problem_unknown_blob.json"},
		{
			"a cursor from another collection", http.MethodGet,
			"/api/v1/jobs?cursor=" + worksCursor(t, h), "", 400, "problem_foreign_cursor.json",
		},
		{"an unknown state", http.MethodGet, "/api/v1/jobs?state=finished", "", 400, "problem_unknown_state.json"},
		{
			"a library with a missing field", http.MethodPost, "/api/v1/libraries",
			`{"name":"films"}`, 400, "problem_missing_field.json",
		},
		{
			"a library with an unknown field", http.MethodPost, "/api/v1/libraries",
			`{"name":"x","content_type":"movie","content_typo":"movie"}`, 400, "problem_unknown_field.json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body *strings.Reader
			if tt.body != "" {
				body = strings.NewReader(tt.body)
			}
			var resp *http.Response
			if body == nil {
				resp = h.doStable(tt.method, tt.path, nil)
			} else {
				resp = h.doStable(tt.method, tt.path, body)
			}
			if resp.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.status)
			}
			raw := h.indent(resp)
			testutil.Golden(t, goldenPath(tt.golden), raw)
			if !strings.Contains(string(raw), problem.TypeBase) {
				t.Error("the problem document carries no type URI, which is the only thing clients may branch on")
			}
		})
	}
}

// worksCursor returns a real cursor from /works, for the test that hands it to
// a different collection.
func worksCursor(t *testing.T, h *harness) string {
	t.Helper()
	var page struct {
		NextCursor string `json:"next_cursor"`
	}
	decode(t, h, "/api/v1/works?limit=1", &page)
	if page.NextCursor == "" {
		t.Fatal("the first page of works returned no cursor, so this test proves nothing")
	}
	return page.NextCursor
}

// The write endpoints' shapes. Two identifiers here are minted inside packages
// this test does not own — the job queue's and the token store's — so they are
// redacted and nothing else is.
func TestWriteShapes(t *testing.T) {
	h := newHarness(t).seed()

	t.Run("a created library", func(t *testing.T) {
		resp := h.doStable(http.MethodPost, "/api/v1/libraries",
			strings.NewReader(`{"name":"music","content_type":"album"}`))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want 201", resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/api/v1/libraries/") {
			t.Errorf("Location = %q, which a client cannot follow", loc)
		}
		testutil.Golden(t, goldenPath("library_created.json"), h.indent(resp))
	})

	t.Run("a created root", func(t *testing.T) {
		resp := h.doStable(http.MethodPost, "/api/v1/libraries/"+libBooksID+"/roots",
			strings.NewReader(`{"path":"/srv/books","ingest_mode":"link"}`))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want 201", resp.StatusCode)
		}
		testutil.Golden(t, goldenPath("root_created.json"), h.indent(resp))
	})

	t.Run("a queued scan", func(t *testing.T) {
		resp := h.doStable(http.MethodPost, "/api/v1/libraries/"+libFilmsID+"/scan", nil)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", resp.StatusCode)
		}
		testutil.Golden(t, goldenPath("scan_accepted.json"), h.redact(resp, "id"))
	})

	t.Run("a minted token", func(t *testing.T) {
		resp := h.doStable(http.MethodPost, "/api/v1/tokens",
			strings.NewReader(`{"name":"jellyfin","scopes":["read"]}`))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want 201", resp.StatusCode)
		}
		testutil.Golden(t, goldenPath("token_created.json"), h.redact(resp, "id", "secret"))
	})
}
