// Every HTTP response in this file is closed by the t.Cleanup that the harness
// registers, which bodyclose cannot see through — hence the file-wide
// exemption rather than a comment on each of a few dozen call sites.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

// An unknown identifier is a 404 problem document and never an empty 200.
//
// This is the failure that is invisible in a demo and expensive in a client: a
// 200 with an empty object cannot be told apart from a resource that genuinely
// has nothing in it, so a client retries forever or gives up silently
// depending on which guess its author made.
func TestUnknownIdentifiersAreNotFoundRatherThanEmpty(t *testing.T) {
	h := newHarness(t).seed()

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{"work", http.MethodGet, "/api/v1/works/01990000-0000-7000-8000-00000000dead"},
		{"edition", http.MethodGet, "/api/v1/editions/01990000-0000-7000-8000-00000000dead"},
		{"asset", http.MethodGet, "/api/v1/assets/01990000-0000-7000-8000-00000000dead"},
		{"library", http.MethodGet, "/api/v1/libraries/01990000-0000-7000-8000-00000000dead"},
		{"blob", http.MethodGet, "/api/v1/blobs/blake3:" + strings.Repeat("0", 64)},
		{"job", http.MethodGet, "/api/v1/jobs/01990000-0000-7000-8000-00000000dead"},
		{"token", http.MethodGet, "/api/v1/tokens/01990000-0000-7000-8000-00000000dead"},
		{"deleting an asset", http.MethodDelete, "/api/v1/assets/01990000-0000-7000-8000-00000000dead"},
		{"revoking a token", http.MethodDelete, "/api/v1/tokens/01990000-0000-7000-8000-00000000dead"},
		{"retrying a job", http.MethodPost, "/api/v1/jobs/01990000-0000-7000-8000-00000000dead/retry"},
		{
			"adding a root to a library", http.MethodPost,
			"/api/v1/libraries/01990000-0000-7000-8000-00000000dead/roots",
		},
		{
			"scanning a library", http.MethodPost,
			"/api/v1/libraries/01990000-0000-7000-8000-00000000dead/scan",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resp *http.Response
			if tt.method == http.MethodPost && strings.HasSuffix(tt.path, "/roots") {
				resp = h.do(tt.method, tt.path, "", strings.NewReader(`{"path":"/srv/x"}`))
			} else {
				resp = h.do(tt.method, tt.path, "", nil)
			}
			raw := h.body(resp)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body: %s", resp.StatusCode, raw)
			}
			p := decodeProblem(t, resp, raw)
			if p.Type != problem.TypeNotFound {
				t.Errorf("problem type = %q, want %q — clients branch on this", p.Type, problem.TypeNotFound)
			}
			if p.RequestID == "" {
				t.Error("the problem document carries no request id, so nobody can correlate it with the log")
			}
		})
	}
}

// A collection with no matches is an empty page, not a 404: "there are no
// science documentaries" is an answer, and the client's loop over items must
// not have to special-case it.
func TestAnEmptyCollectionIsAnEmptyPageNotAnError(t *testing.T) {
	h := newHarness(t).seed()
	for _, path := range []string{
		"/api/v1/works?content_type=nothing-has-this",
		"/api/v1/assets?library_id=01990000-0000-7000-8000-00000000dead",
		"/api/v1/jobs?state=leased",
		"/api/v1/replicas?state=missing",
	} {
		resp := h.get(path)
		raw := h.body(resp)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200: %s", path, resp.StatusCode, raw)
			continue
		}
		// `[]`, never null: a JSON null makes every client's loop a null
		// dereference.
		if !strings.Contains(string(raw), `"items":[]`) {
			t.Errorf("GET %s returned %s; an empty collection must be an empty array", path, raw)
		}
	}
}

// Scope discipline. The router applies a `read` floor, so what is worth
// asserting is that the mutating routes ask for more than that, and that the
// credential routes ask for admin.
func TestScopeEnforcementOnEveryMutatingRoute(t *testing.T) {
	h := newHarness(t, withAuth).seed()
	reader := h.mint("reader", auth.ScopeRead)
	writer := h.mint("writer", auth.ScopeWrite)
	admin := h.mint("admin", auth.ScopeAdmin)

	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		read       int
		write      int
		adminWants int
	}{
		// The admin column is 409 for the two creates that the write column has
		// already performed: the point of the table is the 403/not-403
		// boundary, and pretending the second create is fresh would be a lie
		// about what the server did.
		{
			"create a library", http.MethodPost, "/api/v1/libraries",
			`{"name":"unique-1","content_type":"movie"}`, 403, 201, 409,
		},
		{
			"add a root", http.MethodPost, "/api/v1/libraries/" + libBooksID + "/roots",
			`{"path":"/srv/unique"}`, 403, 201, 409,
		},
		{"scan", http.MethodPost, "/api/v1/libraries/" + libFilmsID + "/scan", "", 403, 202, 202},
		{"delete an asset", http.MethodDelete, "/api/v1/assets/" + asset1ID, "", 403, 204, 404},
		{"list tokens", http.MethodGet, "/api/v1/tokens", "", 403, 403, 200},
		{
			"create a token", http.MethodPost, "/api/v1/tokens",
			`{"name":"t","scopes":["read"]}`, 403, 403, 201,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, step := range []struct {
				who   string
				token string
				want  int
			}{
				{"read", reader.Secret, tt.read},
				{"write", writer.Secret, tt.write},
				{"admin", admin.Secret, tt.adminWants},
			} {
				var body *strings.Reader
				if tt.body != "" {
					body = strings.NewReader(tt.body)
				}
				var resp *http.Response
				if body == nil {
					resp = h.do(tt.method, tt.path, step.token, nil)
				} else {
					resp = h.do(tt.method, tt.path, step.token, body)
				}
				raw := h.body(resp)
				if resp.StatusCode != step.want {
					t.Errorf("%s with a %s token = %d, want %d: %s",
						tt.name, step.who, resp.StatusCode, step.want, raw)
				}
				if resp.StatusCode == http.StatusForbidden {
					if p := decodeProblem(t, resp, raw); p.Type != problem.TypeForbidden {
						t.Errorf("problem type = %q, want %q", p.Type, problem.TypeForbidden)
					}
				}
			}
		})
	}
}

// ADR-0018: deleting an asset is logical. The blob has to still be there
// afterwards, because a handler that unlinks bytes inline is the version of
// this feature where a bug is unrecoverable.
func TestDeletingAnAssetLeavesTheBlobAlone(t *testing.T) {
	h := newHarness(t).seed()

	before := h.get("/api/v1/blobs/" + blob1Hash)
	if before.StatusCode != http.StatusOK {
		t.Fatalf("the blob was not there to begin with: %d", before.StatusCode)
	}

	resp := h.do(http.MethodDelete, "/api/v1/assets/"+asset1ID, "", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204: %s", resp.StatusCode, h.body(resp))
	}

	if gone := h.get("/api/v1/assets/" + asset1ID); gone.StatusCode != http.StatusNotFound {
		t.Errorf("the asset is still readable after being deleted: %d", gone.StatusCode)
	}
	after := h.get("/api/v1/blobs/" + blob1Hash)
	if after.StatusCode != http.StatusOK {
		t.Errorf("the blob went away with the asset: %d — ADR-0018 says bytes are reclaimed by GC, never inline",
			after.StatusCode)
	}

	// Invariant 7: every state transition emits an event.
	recorded, err := h.events.Since(context.Background(), 0, []string{events.TypeAssetDeleted}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 1 {
		t.Fatalf("the deletion emitted %d events, want 1", len(recorded))
	}
	if recorded[0].SubjectID != asset1ID {
		t.Errorf("the event names %q, not the asset that was deleted", recorded[0].SubjectID)
	}
	if !strings.Contains(string(recorded[0].Payload), `"bytes_removed":false`) {
		t.Errorf("the event does not record that no bytes were removed: %s", recorded[0].Payload)
	}
}

// The event and the state change have to commit together, or invariant 7 is
// "usually" rather than "always". A rolled-back write must leave no event.
func TestAFailedWriteLeavesNoEvent(t *testing.T) {
	h := newHarness(t).seed()

	// The second create violates the unique name index, so the transaction
	// rolls back — and must take its event with it.
	first := h.do(http.MethodPost, "/api/v1/libraries", "",
		strings.NewReader(`{"name":"duplicated","content_type":"movie"}`))
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("the first create = %d: %s", first.StatusCode, h.body(first))
	}
	second := h.do(http.MethodPost, "/api/v1/libraries", "",
		strings.NewReader(`{"name":"duplicated","content_type":"movie"}`))
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("the duplicate create = %d, want 409: %s", second.StatusCode, h.body(second))
	}

	recorded, err := h.events.Since(context.Background(), 0, []string{events.TypeLibraryCreated}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 1 {
		t.Errorf("%d library.created events were recorded for one successful create — the rolled-back "+
			"write left its event behind", len(recorded))
	}
}

func TestScanEnqueuesOneJobPerEnabledRootAndIsIdempotent(t *testing.T) {
	h := newHarness(t).seed()

	// A second root, and a disabled one that must not be scanned.
	h.exec(`INSERT INTO library_roots (id, library_id, path, ingest_mode, enabled, created_at)
		VALUES ('r-second', ?, '/srv/films-2', 'reflink', 1, ?),
		       ('r-disabled', ?, '/srv/films-off', 'reflink', 0, ?)`,
		libFilmsID, seedTime, libFilmsID, seedTime)

	var first struct {
		Jobs []struct {
			ID      string          `json:"id"`
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		} `json:"jobs"`
	}
	resp := h.do(http.MethodPost, "/api/v1/libraries/"+libFilmsID+"/scan", "", nil)
	raw := h.body(resp)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("scan = %d, want 202: %s", resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Jobs) != 2 {
		t.Fatalf("the scan queued %d jobs; the library has 2 enabled roots and 1 disabled one", len(first.Jobs))
	}
	for _, j := range first.Jobs {
		if j.Type != "scan_library" {
			t.Errorf("job type = %q, want scan_library", j.Type)
		}
		if !strings.Contains(string(j.Payload), `"root_id"`) {
			t.Errorf("the payload does not carry a root_id: %s", j.Payload)
		}
	}

	// Asking twice while the first is still live must return the same jobs,
	// not queue a second walk of the same tree (ADR-0008).
	var second struct {
		Jobs []struct {
			ID string `json:"id"`
		} `json:"jobs"`
	}
	again := h.do(http.MethodPost, "/api/v1/libraries/"+libFilmsID+"/scan", "", nil)
	if err := json.Unmarshal(h.body(again), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Jobs) != len(first.Jobs) {
		t.Fatalf("the second scan queued %d jobs, the first %d", len(second.Jobs), len(first.Jobs))
	}
	for i := range first.Jobs {
		if first.Jobs[i].ID != second.Jobs[i].ID {
			t.Errorf("scanning twice produced a second job for the same root: %s then %s",
				first.Jobs[i].ID, second.Jobs[i].ID)
		}
	}
}

func TestScanningALibraryWithNoRootsIsARefusalNotAnEmptyAccept(t *testing.T) {
	h := newHarness(t).seed()
	resp := h.do(http.MethodPost, "/api/v1/libraries/"+libBooksID+"/scan", "", nil)
	raw := h.body(resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.StatusCode, raw)
	}
	if p := decodeProblem(t, resp, raw); !strings.Contains(p.Detail, "no enabled roots") {
		t.Errorf("the refusal does not say what is wrong: %q", p.Detail)
	}
}

func TestRetryDistinguishesAMissingJobFromOneThatIsStillRunning(t *testing.T) {
	h := newHarness(t).seed()

	// job1 is pending: retrying it would mean running it twice.
	resp := h.do(http.MethodPost, "/api/v1/jobs/"+job1ID+"/retry", "", nil)
	raw := h.body(resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("retrying a pending job = %d, want 409: %s", resp.StatusCode, raw)
	}
	if p := decodeProblem(t, resp, raw); p.Type != problem.TypeConflict {
		t.Errorf("problem type = %q, want %q", p.Type, problem.TypeConflict)
	}

	// job2 is dead: retrying it is the whole point of the endpoint.
	revived := h.do(http.MethodPost, "/api/v1/jobs/"+job2ID+"/retry", "", nil)
	body := h.body(revived)
	if revived.StatusCode != http.StatusOK {
		t.Fatalf("retrying a dead job = %d, want 200: %s", revived.StatusCode, body)
	}
	var job struct {
		State    string `json:"state"`
		Attempts int    `json:"attempts"`
	}
	if err := json.Unmarshal(body, &job); err != nil {
		t.Fatal(err)
	}
	if job.State != "pending" || job.Attempts != 0 {
		t.Errorf("the retried job is state=%q attempts=%d, want pending/0", job.State, job.Attempts)
	}
}

func TestTokenLifecycleNeverLeaksASecretAndRefusesADoubleRevoke(t *testing.T) {
	h := newHarness(t).seed()

	created := h.do(http.MethodPost, "/api/v1/tokens", "",
		strings.NewReader(`{"name":"jellyfin","scopes":["read","write"]}`))
	raw := h.body(created)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", created.StatusCode, raw)
	}
	var minted struct {
		Token struct {
			ID     string   `json:"id"`
			Scopes []string `json:"scopes"`
		} `json:"token"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(raw, &minted); err != nil {
		t.Fatal(err)
	}
	if minted.Secret == "" {
		t.Fatal("the mint response carries no secret, so the credential is unusable")
	}
	if len(minted.Token.Scopes) != 2 {
		t.Errorf("scopes = %v, want read and write", minted.Token.Scopes)
	}

	// The secret is returned exactly once and never again.
	fetched := h.body(h.get("/api/v1/tokens/" + minted.Token.ID))
	if strings.Contains(string(fetched), minted.Secret) {
		t.Error("reading a token back returns its secret")
	}
	listed := h.body(h.get("/api/v1/tokens"))
	if strings.Contains(string(listed), minted.Secret) {
		t.Error("listing tokens returns secrets")
	}

	first := h.do(http.MethodDelete, "/api/v1/tokens/"+minted.Token.ID, "", nil)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("revoke = %d, want 200: %s", first.StatusCode, h.body(first))
	}
	second := h.do(http.MethodDelete, "/api/v1/tokens/"+minted.Token.ID, "", nil)
	if second.StatusCode != http.StatusConflict {
		t.Errorf("revoking twice = %d, want 409 — a script must not read the second one as success",
			second.StatusCode)
	}
}

func TestCreateValidation(t *testing.T) {
	h := newHarness(t).seed()

	tests := []struct {
		name string
		path string
		body string
	}{
		{"no body at all", "/api/v1/libraries", ""},
		{"not JSON", "/api/v1/libraries", `not json`},
		{"two documents", "/api/v1/libraries", `{"name":"a","content_type":"movie"}{"name":"b"}`},
		{"an empty name", "/api/v1/libraries", `{"name":"   ","content_type":"movie"}`},
		{
			"an unknown ingest mode", "/api/v1/libraries/" + libFilmsID + "/roots",
			`{"path":"/srv/x","ingest_mode":"teleport"}`,
		},
		{"a token with no scopes", "/api/v1/tokens", `{"name":"x","scopes":[]}`},
		{"a token with an unknown scope", "/api/v1/tokens", `{"name":"x","scopes":["superuser"]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := h.do(http.MethodPost, tt.path, "", strings.NewReader(tt.body))
			raw := h.body(resp)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", resp.StatusCode, raw)
			}
			decodeProblem(t, resp, raw)
		})
	}
}
