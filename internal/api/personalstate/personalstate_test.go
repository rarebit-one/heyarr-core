package personalstate

// White-box tests: they call the handlers directly, so they exercise the
// handler + store contract and the opacity invariant without standing up the
// authentication middleware. The scope floor on the write routes is declarative
// (Mount) and is covered where the router is assembled (the OpenAPI parity walk
// and the http foundation's own auth tests); duplicating it here would test chi,
// not this package.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	psclient "github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/statesync"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/store"
)

func newAPI(t *testing.T) *API {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(t.TempDir(), "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(store.Options{Writer: db.Writer(), Reader: db.Reader(), Events: log})
	if err != nil {
		t.Fatal(err)
	}
	api, err := New(Options{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	return api
}

// newDB opens a migrated temp database, for tests that build the store and API
// themselves (e.g. to inject a Replicator).
func newDB(t *testing.T) *sqlite.DB {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(t.TempDir(), "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db
}

// call invokes one handler with a request that carries the given URL params, so a
// {id} route can be tested without a router in front of it.
func call(t *testing.T, handler http.HandlerFunc, method, target string, body any, params map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(buf)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if len(params) > 0 {
		rctx := chi.NewRouteContext()
		for k, v := range params {
			rctx.URLParams.Add(k, v)
		}
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// TestPeerStoresCiphertextAndCannotRead is the milestone's first evidence line at
// the API layer: a device creates a space and pushes an encrypted change; the
// peer stores it; the stored bytes are CIPHERTEXT — the plaintext item never
// appears in them — a non-recipient cannot unwrap the key, and a recipient reads
// the item back.
//
// SABOTAGE (the reviewer's break): make the client push plaintext (skip
// Encrypt/statesync and store the raw item), or have the store return plaintext —
// either makes the "ciphertext at rest" assertion below fire, because the item
// bytes then appear in what the peer holds.
func TestPeerStoresCiphertextAndCannotRead(t *testing.T) {
	api := newAPI(t)

	aKey, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	bKey, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	aRecip := psclient.Recipient{ID: encryption.FormatPublicKey(aKey.PublicKey().Bytes()), Key: aKey.PublicKey()}
	bRecip := psclient.Recipient{ID: encryption.FormatPublicKey(bKey.PublicKey().Bytes()), Key: bKey.PublicKey()}

	mgr := psclient.New()
	sp, wrapped, err := mgr.Create(spaces.KindPersonal, time.Now().UTC(), []psclient.Recipient{aRecip, bRecip})
	if err != nil {
		t.Fatal(err)
	}

	create := createSpaceRequest{ID: sp.ID, Kind: string(sp.Kind)}
	for _, w := range wrapped {
		create.WrappedKeys = append(create.WrappedKeys, wrappedKeyInput{Recipient: w.Recipient, Wrapped: w.Wrapped})
	}
	if rec := call(t, api.createSpace, http.MethodPost, "/spaces", create, nil); rec.Code != http.StatusCreated {
		t.Fatalf("create space: %d %s", rec.Code, rec.Body)
	}

	// Two wrapped keys are stored.
	var keys wrappedKeysView
	rec := call(t, api.listWrappedKeys, http.MethodGet, "/spaces/"+sp.ID+"/keys", nil, map[string]string{"id": sp.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("list keys: %d %s", rec.Code, rec.Body)
	}
	mustJSON(t, rec, &keys)
	if len(keys.WrappedKeys) != 2 {
		t.Fatalf("want 2 wrapped keys, got %d", len(keys.WrappedKeys))
	}

	// A device encrypts a change and pushes it as ciphertext.
	const secret = "midnight-jazz"
	playlist := crdt.New()
	change := playlist.Add(secret)
	ec, err := statesync.Encode(mgr, sp.ID, nil, change)
	if err != nil {
		t.Fatal(err)
	}
	if rec := call(t, api.putChange, http.MethodPost, "/spaces/"+sp.ID+"/changes", ec, map[string]string{"id": sp.ID}); rec.Code != http.StatusCreated {
		t.Fatalf("put change: %d %s", rec.Code, rec.Body)
	}

	// Fetch what the peer holds.
	var got changesView
	rec = call(t, api.listChanges, http.MethodGet, "/spaces/"+sp.ID+"/changes", nil, map[string]string{"id": sp.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("list changes: %d %s", rec.Code, rec.Body)
	}
	mustJSON(t, rec, &got)
	if len(got.Changes) != 1 {
		t.Fatalf("want 1 change, got %d", len(got.Changes))
	}
	stored := got.Changes[0].Ciphertext

	// The invariant: the stored bytes are ciphertext — the plaintext item is not
	// in them.
	if bytes.Contains(stored, []byte(secret)) {
		t.Fatalf("the peer's stored change contains the plaintext %q — it is NOT opaque", secret)
	}

	// A decrypt WITHOUT the space key fails: a non-recipient cannot even unwrap.
	strangerKey, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := psclient.New().Open(sp.ID, wrapped[0].Wrapped, psclient.NewKeyUnwrapper(strangerKey)); err == nil {
		t.Fatal("a non-recipient device unwrapped a space key it was not sealed for")
	}

	// A wrap target (B) opens the space and reads the item back — proving the
	// ciphertext is real encryption, not garbage.
	bMgr := psclient.New()
	if err := bMgr.Open(sp.ID, wrapped[1].Wrapped, psclient.NewKeyUnwrapper(bKey)); err != nil {
		t.Fatalf("device B could not open its own wrapped key: %v", err)
	}
	decoded, err := statesync.DecodeAll(bMgr, got.Changes)
	if err != nil {
		t.Fatalf("device B could not decode the change: %v", err)
	}
	read := crdt.New()
	read.Apply(decoded...)
	ids := read.IDs()
	if len(ids) != 1 || ids[0] != secret {
		t.Fatalf("device B read %v, want [%q]", ids, secret)
	}
}

func TestCreateSpaceRejectsUnknownKind(t *testing.T) {
	api := newAPI(t)
	rec := call(t, api.createSpace, http.MethodPost, "/spaces", createSpaceRequest{ID: mustUUID(t), Kind: "nonsense"}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an unknown kind, got %d %s", rec.Code, rec.Body)
	}
}

func TestPutChangeRejectsForgedID(t *testing.T) {
	api := newAPI(t)
	id := mustUUID(t)
	if rec := call(t, api.createSpace, http.MethodPost, "/spaces", createSpaceRequest{ID: id, Kind: "personal"}, nil); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	forged := protocol.EncryptedChange{
		SpaceID:    id,
		ChangeID:   "blake3:deadbeef",
		Ciphertext: []byte("not really encrypted but long enough to look it"),
	}
	rec := call(t, api.putChange, http.MethodPost, "/spaces/"+id+"/changes", forged, map[string]string{"id": id})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a forged change id, got %d %s", rec.Code, rec.Body)
	}
}

func TestChangesOnUnknownSpaceIs404(t *testing.T) {
	api := newAPI(t)
	id := mustUUID(t)
	rec := call(t, api.listChanges, http.MethodGet, "/spaces/"+id+"/changes", nil, map[string]string{"id": id})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for an unknown space, got %d %s", rec.Code, rec.Body)
	}
}

func mustJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decoding response: %v (%s)", err, rec.Body)
	}
}

func mustUUID(t *testing.T) string {
	t.Helper()
	sp, err := spaces.NewSpace(spaces.KindPersonal, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return sp.ID
}

// fakeReplicator records that ReconcileAll was called and returns fixed counts.
type fakeReplicator struct {
	replicated, deferred int
	called               bool
}

func (f *fakeReplicator) ReconcileAll(context.Context) (int, int, error) {
	f.called = true
	return f.replicated, f.deferred, nil
}

// TestReplicateEndpointDrivesTheReconciler: POST /state/replicate runs the
// reconciler and returns its counts; with no reconciler it is a 503.
func TestReplicateEndpointDrivesTheReconciler(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(store.Options{Writer: db.Writer(), Reader: db.Reader(), Events: log})
	if err != nil {
		t.Fatal(err)
	}
	rep := &fakeReplicator{replicated: 3, deferred: 1}
	api, err := New(Options{Store: st, Replicator: rep})
	if err != nil {
		t.Fatal(err)
	}
	rec := call(t, api.replicate, http.MethodPost, "/state/replicate", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("replicate: %d %s", rec.Code, rec.Body)
	}
	if !rep.called {
		t.Fatal("the endpoint did not drive the reconciler")
	}
	var out replicateResult
	mustJSON(t, rec, &out)
	if out.Replicated != 3 || out.Deferred != 1 {
		t.Fatalf("counts = %+v, want {3,1}", out)
	}

	// With no reconciler wired, the route is a 503.
	bare, err := New(Options{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	rec = call(t, bare.replicate, http.MethodPost, "/state/replicate", nil, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("replicate with no reconciler = %d, want 503", rec.Code)
	}
	_ = ctx
}

// TestRewrapAndRevokeRotatesAccess is the API-layer evidence for revocation (#361):
// after a rotation the remaining recipient's copy seals the NEW key and the revoked
// recipient's copy is DELETED, so the peer holds no copy the revoked device can
// open. All opaque — the peer never reads a key.
//
// SABOTAGE (the reviewer's break): make revokeKey a no-op (skip DeleteWrappedKey),
// and the "B is gone" assertion below fires — B's stale copy would still be listed.
func TestRewrapAndRevokeRotatesAccess(t *testing.T) {
	api := newAPI(t)

	aKey, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	bKey, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	aRecip := psclient.Recipient{ID: encryption.FormatPublicKey(aKey.PublicKey().Bytes()), Key: aKey.PublicKey()}
	bRecip := psclient.Recipient{ID: encryption.FormatPublicKey(bKey.PublicKey().Bytes()), Key: bKey.PublicKey()}

	mgr := psclient.New()
	sp, wrapped, err := mgr.Create(spaces.KindPersonal, time.Now().UTC(), []psclient.Recipient{aRecip, bRecip})
	if err != nil {
		t.Fatal(err)
	}
	create := createSpaceRequest{ID: sp.ID, Kind: string(sp.Kind)}
	for _, w := range wrapped {
		create.WrappedKeys = append(create.WrappedKeys, wrappedKeyInput{Recipient: w.Recipient, Wrapped: w.Wrapped})
	}
	if rec := call(t, api.createSpace, http.MethodPost, "/spaces", create, nil); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}

	// A's wrapped copy before rotation, to prove it changes.
	aWrappedBefore := wrappedFor(t, api, sp.ID, aRecip.ID)

	// Rotate: a fresh key sealed for A only (B revoked). This is exactly what the
	// client's mgr.Rotate produces.
	rotated, err := mgr.Rotate(sp.ID, []psclient.Recipient{aRecip})
	if err != nil {
		t.Fatal(err)
	}
	body := rewrapRequest{}
	for _, w := range rotated {
		body.WrappedKeys = append(body.WrappedKeys, wrappedKeyInput{Recipient: w.Recipient, Wrapped: w.Wrapped})
	}
	if rec := call(t, api.rewrapKeys, http.MethodPost, "/spaces/"+sp.ID+"/keys", body, map[string]string{"id": sp.ID}); rec.Code != http.StatusOK {
		t.Fatalf("rewrap: %d %s", rec.Code, rec.Body)
	}

	// Revoke B: delete its stored copy.
	if rec := call(t, api.revokeKey, http.MethodDelete, "/spaces/"+sp.ID+"/keys/"+bRecip.ID, nil,
		map[string]string{"id": sp.ID, "recipient": bRecip.ID}); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke B: %d %s", rec.Code, rec.Body)
	}

	// Exactly one key remains — A's — and its bytes changed (the new key).
	var keys wrappedKeysView
	rec := call(t, api.listWrappedKeys, http.MethodGet, "/spaces/"+sp.ID+"/keys", nil, map[string]string{"id": sp.ID})
	mustJSON(t, rec, &keys)
	if len(keys.WrappedKeys) != 1 || keys.WrappedKeys[0].Recipient != aRecip.ID {
		t.Fatalf("after revocation, want only A's key, got %+v", keys.WrappedKeys)
	}
	if bytes.Equal(keys.WrappedKeys[0].Wrapped, aWrappedBefore) {
		t.Fatal("A's wrapped key did not change — the rotation did not re-key it")
	}

	// Revocation is idempotent: revoking B again is still a 204, not a 404.
	if rec := call(t, api.revokeKey, http.MethodDelete, "/spaces/"+sp.ID+"/keys/"+bRecip.ID, nil,
		map[string]string{"id": sp.ID, "recipient": bRecip.ID}); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke B again: want idempotent 204, got %d %s", rec.Code, rec.Body)
	}
}

// wrappedFor returns the wrapped bytes stored for one recipient of a space.
func wrappedFor(t *testing.T, api *API, spaceID, recipient string) []byte {
	t.Helper()
	var keys wrappedKeysView
	rec := call(t, api.listWrappedKeys, http.MethodGet, "/spaces/"+spaceID+"/keys", nil, map[string]string{"id": spaceID})
	mustJSON(t, rec, &keys)
	for _, k := range keys.WrappedKeys {
		if k.Recipient == recipient {
			return k.Wrapped
		}
	}
	t.Fatalf("no wrapped key for %q", recipient)
	return nil
}

func TestRewrapRejectsEmpty(t *testing.T) {
	api := newAPI(t)
	id := mustUUID(t)
	if rec := call(t, api.createSpace, http.MethodPost, "/spaces", createSpaceRequest{ID: id, Kind: "personal"}, nil); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	rec := call(t, api.rewrapKeys, http.MethodPost, "/spaces/"+id+"/keys", rewrapRequest{}, map[string]string{"id": id})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an empty re-wrap, got %d %s", rec.Code, rec.Body)
	}
}

func TestRevokeOnUnknownSpaceIs404(t *testing.T) {
	api := newAPI(t)
	id := mustUUID(t)
	rec := call(t, api.revokeKey, http.MethodDelete, "/spaces/"+id+"/keys/x25519:dead", nil,
		map[string]string{"id": id, "recipient": "x25519:dead"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 revoking on an unknown space, got %d %s", rec.Code, rec.Body)
	}
}
