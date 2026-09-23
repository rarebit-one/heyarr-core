package vaultblob

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

type fakeStore struct {
	corrupt     bool
	gotExpected hashing.Hash
	read        int64
}

func (f *fakeStore) PutExpecting(_ context.Context, r io.Reader, expected hashing.Hash) (cas.Descriptor, error) {
	n, _ := io.Copy(io.Discard, r)
	f.read = n
	f.gotExpected = expected
	if f.corrupt {
		return cas.Descriptor{}, &cas.Corruption{Hash: expected}
	}
	return cas.Descriptor{Hash: expected, Size: n}, nil
}

type fakePinner struct {
	calls      int
	pinnedHash string
	pinnedPeer string
	err        error
}

func (f *fakePinner) PinPlacement(_ context.Context, blobHash, peerID string) error {
	f.calls++
	f.pinnedHash = blobHash
	f.pinnedPeer = peerID
	return f.err
}

const goodHash = "blake3:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func newHandler(t *testing.T, store Store, pinner Pinner) *Handler {
	t.Helper()
	h, err := New(Options{Store: store, Pinner: pinner, SelfPeer: "peer-self"})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func upload(t *testing.T, h *Handler, hash string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/vault/blobs/"+hash, bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("hash", hash)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.upload(rec, req)
	return rec
}

// TestUploadStoresAndPins: a valid ciphertext blob is stored under its declared
// id and pinned to this node, so GC keeps it.
func TestUploadStoresAndPins(t *testing.T) {
	store := &fakeStore{}
	pinner := &fakePinner{}
	h := newHandler(t, store, pinner)

	body := []byte("opaque ciphertext frames")
	rec := upload(t, h, goodHash, body)

	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if store.gotExpected.String() != goodHash {
		t.Fatalf("stored under %s, want %s", store.gotExpected, goodHash)
	}
	if pinner.calls != 1 || pinner.pinnedHash != goodHash || pinner.pinnedPeer != "peer-self" {
		t.Fatalf("want one pin of %s to peer-self, got calls=%d hash=%s peer=%s",
			goodHash, pinner.calls, pinner.pinnedHash, pinner.pinnedPeer)
	}
	var out uploadResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Hash != goodHash || out.Size != int64(len(body)) {
		t.Fatalf("result = %+v, want hash %s size %d", out, goodHash, len(body))
	}
}

// TestUploadRejectsMismatch: bytes that do not hash to the declared id (corrupted
// or truncated at the size cap) are a 400, and NOTHING is pinned.
func TestUploadRejectsMismatch(t *testing.T) {
	store := &fakeStore{corrupt: true}
	pinner := &fakePinner{}
	rec := upload(t, newHandler(t, store, pinner), goodHash, []byte("wrong bytes"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if pinner.calls != 0 {
		t.Fatal("a rejected upload must not pin anything")
	}
}

// TestUploadRejectsBadHash: a path id that is not a blake3 digest is a 400 before
// the store is touched.
func TestUploadRejectsBadHash(t *testing.T) {
	store := &fakeStore{}
	rec := upload(t, newHandler(t, store, &fakePinner{}), "not-a-digest", []byte("x"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
	if store.read != 0 {
		t.Fatal("a bad id must be refused before the body is read into the store")
	}
}

// TestUploadAcceptsPercentEncodedColon: a spec-conformant client (the JVM's
// java.net.http, used by the desktop vault-sync daemon) percent-encodes the ':'
// in a `blake3:<hex>` id to %3A. chi routes on the raw target and hands that
// encoded value to the handler; httpapi.HashParam must decode it so the id
// parses and the blob stores under its literal digest — otherwise every daemon
// upload 400s as a malformed id. A literal ':' is unchanged (TestUploadStoresAndPins).
func TestUploadAcceptsPercentEncodedColon(t *testing.T) {
	store := &fakeStore{}
	pinner := &fakePinner{}
	// What chi.URLParam returns when the client encoded the colon.
	encoded := "blake3%3A" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rec := upload(t, newHandler(t, store, pinner), encoded, []byte("opaque ciphertext frames"))

	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if store.gotExpected.String() != goodHash {
		t.Fatalf("stored under %s, want the decoded %s", store.gotExpected, goodHash)
	}
	if pinner.pinnedHash != goodHash {
		t.Fatalf("pinned %s, want the decoded %s", pinner.pinnedHash, goodHash)
	}
}

// TestUploadPinFailureIs500: if the bytes stored but the pin failed, that is a
// server error (the blob would otherwise be reclaimable) — not a silent success.
func TestUploadPinFailureIs500(t *testing.T) {
	pinner := &fakePinner{err: context.DeadlineExceeded}
	rec := upload(t, newHandler(t, &fakeStore{}, pinner), goodHash, []byte("x"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rec.Code)
	}
}

// TestNewRequiresDeps: an upload route with no pinner or no self peer is unsafe
// (the blob would be reclaimed), so construction refuses it.
func TestNewRequiresDeps(t *testing.T) {
	if _, err := New(Options{Store: &fakeStore{}, SelfPeer: "p"}); err == nil {
		t.Fatal("New must require a pinner")
	}
	if _, err := New(Options{Store: &fakeStore{}, Pinner: &fakePinner{}}); err == nil {
		t.Fatal("New must require a self peer id")
	}
	if _, err := New(Options{Pinner: &fakePinner{}, SelfPeer: "p"}); err == nil {
		t.Fatal("New must require a store")
	}
}
