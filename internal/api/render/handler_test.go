package render

import (
	"bytes"
	"context"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rarebit-one/heyarr-core/internal/api/blobs"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// seeded builds a router with the renderer route mounted over a real CAS,
// exactly as the controller wires it: one blob handler, reached two ways.
func seeded(t *testing.T, size int64) (http.Handler, []byte, hashing.Hash) {
	t.Helper()

	data := make([]byte, size)
	rng := rand.New(rand.NewSource(20260824)) //nolint:gosec // deterministic fixture, not a credential
	if _, err := rng.Read(data); err != nil {
		t.Fatal(err)
	}
	store, err := cas.OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	desc, err := store.Put(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	blobHandler, err := blobs.New(blobs.Options{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Options{
		Blobs:  blobHandler,
		Secret: testSecret,
		Now:    func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	h.Mount(r)
	return r, data, desc.Hash
}

func token(t *testing.T, hash hashing.Hash, mime string) string {
	t.Helper()
	return mustSign(t, Capability{
		BlobHash: hash.String(), ExpiresAt: testNow.Add(time.Hour), MIME: mime,
	}, testSecret)
}

// TestServesBytesToACredentiallessClient is the whole point of ADR-0039: a GET
// with no Authorization header, which is all a television can manage.
func TestServesBytesToACredentiallessClient(t *testing.T) {
	t.Parallel()

	router, data, hash := seeded(t, 64<<10)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, Path(token(t, hash, "video/mp4")), nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// The reason the blob endpoint alone cannot serve a renderer: it answers
	// application/octet-stream, which a DLNA device refuses.
	if got := rec.Header().Get("Content-Type"); got != "video/mp4" {
		t.Errorf("Content-Type = %q, want video/mp4", got)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes — a renderer cannot seek without it", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), data) {
		t.Error("the body is not the blob")
	}
}

// TestRangesStillWork guards the reuse. The renderer route wraps the blob
// handler rather than reimplementing it, and a wrapper that broke ranges would
// break seeking on every device.
func TestRangesStillWork(t *testing.T) {
	t.Parallel()

	router, data, hash := seeded(t, 64<<10)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, Path(token(t, hash, "video/mp4")), nil)
	req.Header.Set("Range", "bytes=100-199")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 100-199/65536" {
		t.Errorf("Content-Range = %q", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), data[100:200]) {
		t.Error("the wrong bytes came back")
	}
}

// TestRefusalsAreIndistinguishable pins the decision not to leak which failure
// occurred. Every one is a 404 with the same body, so the endpoint cannot be
// used as an oracle for guessing at tokens.
func TestRefusalsAreIndistinguishable(t *testing.T) {
	t.Parallel()

	router, _, hash := seeded(t, 4<<10)
	expired := mustSign(t, Capability{
		BlobHash: hash.String(), ExpiresAt: testNow.Add(-time.Hour), MIME: "video/mp4",
	}, testSecret)
	foreign := mustSign(t, Capability{
		BlobHash: hash.String(), ExpiresAt: testNow.Add(time.Hour), MIME: "video/mp4",
	}, otherSecret)

	tests := []struct {
		name  string
		token string
	}{
		{name: "expired", token: expired},
		{name: "signed by another peer", token: foreign},
		{name: "gibberish", token: "not-a-capability"},
	}

	var bodies []string
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Path(tc.token), nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			bodies = append(bodies, rec.Body.String())
		})
	}
	for _, b := range bodies {
		if b != bodies[0] {
			t.Error("two refusals produced different bodies, which tells a caller which one it was")
		}
	}
}

// TestAValidCapabilityForAnAbsentBlobIs404 separates the two 404s in the
// implementation: this one comes from the CAS, not from verification.
func TestAValidCapabilityForAnAbsentBlobIs404(t *testing.T) {
	t.Parallel()

	router, _, _ := seeded(t, 4<<10)
	absent, err := hashing.Parse("blake3:" + "11111111111111111111111111111111" + "11111111111111111111111111111111")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Path(token(t, absent, "video/mp4")), nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestTrailingNameIsAccepted covers the cosmetic segment some renderers need
// before they will issue a GET at all.
func TestTrailingNameIsAccepted(t *testing.T) {
	t.Parallel()

	router, data, hash := seeded(t, 4<<10)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, Path(token(t, hash, "video/mp4"))+"/stream.mp4", nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(rec.Body.Bytes()) != len(data) {
		t.Error("the trailing name changed what was served")
	}
}

// TestHeadIsAnswered matters because renderers routinely HEAD before they GET,
// and one that gets a 405 gives up without playing anything.
func TestHeadIsAnswered(t *testing.T) {
	t.Parallel()

	router, _, hash := seeded(t, 4<<10)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, Path(token(t, hash, "audio/mpeg")), nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "audio/mpeg" {
		t.Errorf("Content-Type = %q, want audio/mpeg", got)
	}
}

func TestEnsureSecret(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first, err := EnsureSecret(dir)
	if err != nil {
		t.Fatalf("EnsureSecret: %v", err)
	}
	if len(first) != SecretLen {
		t.Fatalf("len = %d, want %d", len(first), SecretLen)
	}
	// Stable across calls, or every restart would invalidate every outstanding
	// capability.
	again, err := EnsureSecret(dir)
	if err != nil {
		t.Fatalf("EnsureSecret again: %v", err)
	}
	if string(first) != string(again) {
		t.Error("the secret changed between calls")
	}

	info, err := os.Stat(filepath.Join(dir, SecretFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

func TestEnsureSecretRefusesATruncatedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, SecretFile), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Regenerating would silently invalidate outstanding capabilities and hide
	// whatever damaged the file.
	if _, err := EnsureSecret(dir); err == nil {
		t.Fatal("want an error for a truncated secret")
	}
}
