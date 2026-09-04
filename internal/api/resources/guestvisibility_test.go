// The guest content boundary (ADR-0074) over the asset read surface: a
// non-guest-visible asset (a `vault` asset) is absent from a Guest's listing and
// a 404 to a Guest by id, while an enrolled reader sees it normally. The seam
// has nothing to hide in the shipped data — every asset a scan writes is
// `managed` — so this test synthesises a `vault` row to exercise it.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/auth"
)

// vaultAssetID is a synthesised encrypted/personal asset under edition1, and
// vaultBlobHash is the blob it points at (a vault asset holds bytes, so the
// asset CHECK constraint requires a blob_hash — ADR-0020/0021).
const (
	vaultAssetID  = "01990000-0000-7000-8000-0000000000av"
	vaultBlobHash = "blake3:00000000000000000000000000000000000000000000000000000000000000av"
)

// withGuest enables anonymous read-only browse on the harness. Paired with
// withAuth, a request that carries no token is admitted as a Guest.
func withGuest(hc *harnessConfig) { hc.cfg.HTTP.Guest.Enabled = true }

func seedVaultAsset(h *harness) {
	h.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, 1024, 'application/octet-stream', ?)`,
		vaultBlobHash, seedTime)
	h.exec(`INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash, source_path,
			role, filename, mime, identification_source, missing_since, created_at, updated_at) VALUES
		(?, ?, ?, 'vault', ?, NULL, 'primary', 'secret.mkv', 'video/x-matroska', 'path', NULL, ?, ?)`,
		vaultAssetID, edition1ID, libFilmsID, vaultBlobHash, seedTime, seedTime)
}

func TestGuestCannotSeeAVaultAssetByID(t *testing.T) {
	h := newHarness(t, withAuth, withGuest).seed()
	seedVaultAsset(h)

	// A Guest (no token) gets a 404 — the same answer as an unknown id, so a
	// Guest cannot even confirm the asset is there.
	if resp := h.get("/api/v1/assets/" + vaultAssetID); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("guest GET of a vault asset = %d, want 404", resp.StatusCode)
	}

	// An enrolled reader sees it: the 404 is about being a Guest, not about the
	// asset being hidden from everyone.
	tok := h.mint("reader", auth.ScopeRead)
	if resp := h.do(http.MethodGet, "/api/v1/assets/"+vaultAssetID, tok.Secret, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("read-token GET of a vault asset = %d, want 200", resp.StatusCode)
	}
}

func TestGuestListingExcludesVaultAssets(t *testing.T) {
	h := newHarness(t, withAuth, withGuest).seed()
	seedVaultAsset(h)

	// The Guest listing omits the vault asset but keeps the managed/linked ones.
	guestBody := string(h.body(h.get("/api/v1/assets")))
	if strings.Contains(guestBody, vaultAssetID) {
		t.Fatalf("guest asset listing leaked a vault asset:\n%s", guestBody)
	}
	if !strings.Contains(guestBody, asset1ID) {
		t.Fatal("guest asset listing dropped a managed asset it should show")
	}

	// The enrolled reader's listing includes it.
	tok := h.mint("reader", auth.ScopeRead)
	readerBody := string(h.body(h.do(http.MethodGet, "/api/v1/assets", tok.Secret, nil)))
	if !strings.Contains(readerBody, vaultAssetID) {
		t.Fatalf("read-token asset listing hid a vault asset:\n%s", readerBody)
	}
}

func TestGuestWorkAssetsExcludeVaultAssets(t *testing.T) {
	h := newHarness(t, withAuth, withGuest).seed()
	seedVaultAsset(h)

	// The per-work file listing applies the same boundary (the aliased-column
	// path through guestAssetFilter).
	guestBody := string(h.body(h.get("/api/v1/works/" + work1ID + "/assets")))
	if strings.Contains(guestBody, vaultAssetID) {
		t.Fatalf("guest work-assets listing leaked a vault asset:\n%s", guestBody)
	}

	tok := h.mint("reader", auth.ScopeRead)
	readerBody := string(h.body(h.do(http.MethodGet, "/api/v1/works/"+work1ID+"/assets", tok.Secret, nil)))
	if !strings.Contains(readerBody, vaultAssetID) {
		t.Fatalf("read-token work-assets listing hid a vault asset:\n%s", readerBody)
	}
}
