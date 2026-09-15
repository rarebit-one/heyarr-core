package vaultplacement

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakePinner struct {
	pins   [][2]string
	unpins [][2]string
	pinErr error
	unErr  error
}

func (f *fakePinner) PinPlacement(_ context.Context, blobHash, peerID string) error {
	f.pins = append(f.pins, [2]string{blobHash, peerID})
	return f.pinErr
}

func (f *fakePinner) UnpinPlacement(_ context.Context, blobHash, peerID string) error {
	f.unpins = append(f.unpins, [2]string{blobHash, peerID})
	return f.unErr
}

const goodHash = "blake3:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func newHandler(t *testing.T, p Pinner) *Handler {
	t.Helper()
	h, err := New(Options{Pinner: p})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func post(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/vault/placements", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.pin(rec, req)
	return rec
}

func del(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/vault/placements", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.unpin(rec, req)
	return rec
}

// TestPinRecordsPlacement: a valid (blob, peer) pair records the pin and echoes it
// back.
func TestPinRecordsPlacement(t *testing.T) {
	p := &fakePinner{}
	rec := post(t, newHandler(t, p), `{"blob_hash":"`+goodHash+`","peer_id":"site-b"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(p.pins) != 1 || p.pins[0] != [2]string{goodHash, "site-b"} {
		t.Fatalf("want one pin of %s to site-b, got %v", goodHash, p.pins)
	}
	var out placementResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.BlobHash != goodHash || out.PeerID != "site-b" {
		t.Fatalf("result = %+v", out)
	}
}

// TestUnpinRemovesPlacement: a delete removes the pin and answers 204.
func TestUnpinRemovesPlacement(t *testing.T) {
	p := &fakePinner{}
	rec := del(t, newHandler(t, p), `{"blob_hash":"`+goodHash+`","peer_id":"site-b"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(p.unpins) != 1 || p.unpins[0] != [2]string{goodHash, "site-b"} {
		t.Fatalf("want one unpin, got %v", p.unpins)
	}
}

// TestPinRejectsBadHash: a blob id that is not a blake3 digest is a 400 before the
// pinner is touched.
func TestPinRejectsBadHash(t *testing.T) {
	p := &fakePinner{}
	rec := post(t, newHandler(t, p), `{"blob_hash":"not-a-digest","peer_id":"site-b"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
	if len(p.pins) != 0 {
		t.Fatal("a bad hash must be refused before anything is pinned")
	}
}

// TestPinRejectsEmptyPeer: a pin needs a peer to name.
func TestPinRejectsEmptyPeer(t *testing.T) {
	p := &fakePinner{}
	rec := post(t, newHandler(t, p), `{"blob_hash":"`+goodHash+`","peer_id":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
	if len(p.pins) != 0 {
		t.Fatal("a missing peer must be refused before anything is pinned")
	}
}

// TestPinRejectsUnknownField: DisallowUnknownFields catches a client that mistyped
// a field rather than silently recording the wrong thing.
func TestPinRejectsUnknownField(t *testing.T) {
	rec := post(t, newHandler(t, &fakePinner{}),
		`{"blob_hash":"`+goodHash+`","peer_id":"site-b","space":"oops"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}

// TestNewRequiresPinner: a route with no pinner is not constructible.
func TestNewRequiresPinner(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New must require a pinner")
	}
}
