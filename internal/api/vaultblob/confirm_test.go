package vaultblob

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rarebit-one/void-which-binds-go/hashing"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

// Provisional upload pins through the route (ADR-0096 amendment, #714),
// against a real catalogue: the upload records who uploaded and, with the flag
// on, an expiry; confirm makes the pin permanent and answers per blob.

var confirmNow = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

// realPinner is a migrated catalogue and this node's own peer id, which the
// replica an upload records must name (it references peers).
func realPinner(t *testing.T) (*catalog.Catalog, *sqlite.DB, string) {
	t.Helper()
	db := testdb.Migrated(t)
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.New(catalog.Options{DB: db, Events: log, PeerName: "test", PeerSite: "test-site"})
	if err != nil {
		t.Fatal(err)
	}
	self, err := cat.SelfPeer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return cat, db, self
}

func sumOf(b []byte) string {
	h := hashing.New()
	_, _ = h.Write(b)
	return h.Sum().String()
}

func as(r *http.Request, principal string, restricted bool) *http.Request {
	return r.WithContext(httpapi.ContextWithIdentity(r.Context(), auth.Identity{
		Principal: auth.Principal{ID: principal}, Restricted: restricted,
	}))
}

func uploadAs(t *testing.T, h *Handler, principal string, body []byte) string {
	t.Helper()
	hash := sumOf(body)
	req := httptest.NewRequest(http.MethodPut, "/vault/blobs/"+hash, bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("hash", hash)
	req = as(req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx)), principal, true)
	rec := httptest.NewRecorder()
	h.upload(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload = %d: %s", rec.Code, rec.Body.String())
	}
	return hash
}

func confirmAs(t *testing.T, h *Handler, principal string, restricted bool, body string) (int, confirmResponse) {
	t.Helper()
	req := as(httptest.NewRequest(http.MethodPost, "/vault/blobs/confirm", strings.NewReader(body)), principal, restricted)
	rec := httptest.NewRecorder()
	h.confirm(rec, req)
	var out confirmResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}
	return rec.Code, out
}

func provisionalUntil(t *testing.T, db *sqlite.DB, blob string) (string, string) {
	t.Helper()
	var until, uploader sql.NullString
	if err := db.Reader().QueryRow(
		`SELECT provisional_until, uploader FROM placement_pins WHERE blob_hash = ?`,
		blob).Scan(&until, &uploader); err != nil {
		t.Fatal(err)
	}
	return until.String, uploader.String
}

// 🔴 With the flag on, an upload's pin is provisional for a day and names its
// uploader; with it off (the rollout default) the pin is permanent, as before.
func TestAnUploadPinIsProvisionalOnlyWithTheFlagOn(t *testing.T) {
	for _, tc := range []struct {
		name        string
		provisional bool
		wantUntil   string
	}{
		{"flag on", true, sqlite.FormatTimestamp(confirmNow.Add(ProvisionalTTL))},
		{"flag off", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cat, db, self := realPinner(t)
			h, err := New(Options{
				Store: &fakeStore{}, Pinner: cat, SelfPeer: self,
				ProvisionalPins: tc.provisional, Now: func() time.Time { return confirmNow },
			})
			if err != nil {
				t.Fatal(err)
			}
			blob := uploadAs(t, h, "exec-a", []byte("ciphertext "+tc.name))
			until, uploader := provisionalUntil(t, db, blob)
			if until != tc.wantUntil {
				t.Errorf("provisional_until = %q, want %q", until, tc.wantUntil)
			}
			if uploader != "exec-a" {
				t.Errorf("uploader = %q, want exec-a", uploader)
			}
		})
	}
}

// 🔴 Confirm makes the caller's own uploads permanent and answers each blob.
func TestConfirmAnswersEachBlob(t *testing.T) {
	cat, db, self := realPinner(t)
	h, err := New(Options{
		Store: &fakeStore{}, Pinner: cat, SelfPeer: self,
		ProvisionalPins: true, Now: func() time.Time { return confirmNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	mine := uploadAs(t, h, "exec-a", []byte("mine"))
	theirs := uploadAs(t, h, "exec-b", []byte("theirs"))
	unknown := sumOf([]byte("never uploaded"))

	body, _ := json.Marshal(confirmInput{Blobs: []string{mine, theirs, unknown}})
	code, out := confirmAs(t, h, "exec-a", true, string(body))
	if code != http.StatusOK {
		t.Fatalf("confirm = %d, want 200", code)
	}
	want := []confirmResult{
		{Blob: mine, Status: catalog.ConfirmConfirmed},
		{Blob: theirs, Status: catalog.ConfirmNotUploader},
		{Blob: unknown, Status: catalog.ConfirmNotHeld},
	}
	if len(out.Results) != len(want) {
		t.Fatalf("results = %+v, want %+v", out.Results, want)
	}
	for i := range want {
		if out.Results[i] != want[i] {
			t.Errorf("result %d = %+v, want %+v", i, out.Results[i], want[i])
		}
	}
	if until, _ := provisionalUntil(t, db, mine); until != "" {
		t.Error("the confirmed upload is still provisional")
	}
	if until, _ := provisionalUntil(t, db, theirs); until == "" {
		t.Error("a restricted executor made another principal's upload permanent")
	}
}

// 🔴 Malformed requests are refused before anything is read or written.
func TestConfirmRefusesAMalformedRequest(t *testing.T) {
	cat, _, self := realPinner(t)
	h, err := New(Options{Store: &fakeStore{}, Pinner: cat, SelfPeer: self})
	if err != nil {
		t.Fatal(err)
	}
	tooMany := make([]string, maxConfirmBlobs+1)
	for i := range tooMany {
		tooMany[i] = goodHash
	}
	many, _ := json.Marshal(confirmInput{Blobs: tooMany})
	for _, tc := range []struct{ name, body string }{
		{"no blobs", `{"blobs":[]}`},
		{"not a digest", `{"blobs":["sha256:abc"]}`},
		{"not JSON", `blobs`},
		{"too many", string(many)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, _ := confirmAs(t, h, "device-1", false, tc.body); code != http.StatusBadRequest {
				t.Errorf("confirm = %d, want 400", code)
			}
		})
	}
}
