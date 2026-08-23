package resources_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The client API reports §16's three states, and asking for one generates
// nothing (M5-03, ADR-0034).
//
// GET /blobs/{hash} is the single most convenient place in the system to
// violate the rule: a client asking about a blob is exactly the caller that
// would like a manifest, the handler already has the hash, and producing one
// here would look like a helpful cache warm. So the absence is asserted, from
// the outside, on the endpoint.

// blobView is the wire shape, read as JSON so the test asserts what a client
// actually receives rather than what the Go struct happens to hold.
type blobView struct {
	Hash          string `json:"hash"`
	Chunked       bool   `json:"chunked"`
	ChunkManifest string `json:"chunk_manifest"`
}

func (h *harness) blobView(t *testing.T, hash string) blobView {
	t.Helper()
	resp := h.get("/api/v1/blobs/" + hash)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET blob = %d", resp.StatusCode)
	}
	var out blobView
	if err := json.Unmarshal(h.body(resp), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTheBlobEndpointReportsAllThreeManifestStates(t *testing.T) {
	h := newHarness(t).seed()

	// blob1 gets a manifest, blob2 a recorded decision. A third is left alone.
	h.exec(`INSERT INTO chunk_manifests
			(blob_hash, algorithm, min_size, avg_size, max_size, chunk_count,
			 covered_size, digest, generated_at)
		VALUES (?, 'fastcdc', 262144, 1048576, 4194304, 0, 0, ?, ?)`,
		blob1Hash, "blake3:"+repeatHex("d"), "2026-08-01T00:00:00Z")
	h.exec(`UPDATE blobs SET chunking_exempt_reason = 'smaller than one chunk',
			chunking_exempt_at = ? WHERE hash = ?`, "2026-08-01T00:00:00Z", blob2Hash)

	present := h.blobView(t, blob1Hash)
	if present.ChunkManifest != "present" {
		t.Errorf("chunk_manifest = %q, want \"present\"", present.ChunkManifest)
	}
	if !present.Chunked {
		t.Error("the compatibility boolean is false for a blob that has a manifest")
	}

	notRequired := h.blobView(t, blob2Hash)
	if notRequired.ChunkManifest != "not_required" {
		t.Errorf("chunk_manifest = %q, want \"not_required\"", notRequired.ChunkManifest)
	}
	if notRequired.Chunked {
		t.Error("the compatibility boolean is true for a blob with no manifest")
	}

	// The two states the boolean collapsed are different on the wire, and the
	// boolean is the same for both — which is precisely why it cannot be the
	// field anyone branches on.
	if notRequired.ChunkManifest == present.ChunkManifest {
		t.Fatal("two different states came back identical")
	}
}

// 🔴 A GET is a GET.
func TestReadingABlobGeneratesNoManifest(t *testing.T) {
	h := newHarness(t).seed()

	before := h.countRows(t, `SELECT count(*) FROM chunk_manifests`)
	beforeChunks := h.countRows(t, `SELECT count(*) FROM manifest_chunks`)
	beforeJobs := h.countRows(t, `SELECT count(*) FROM jobs`)

	for range 5 {
		got := h.blobView(t, blob1Hash)
		if got.ChunkManifest != "undecided" {
			t.Fatalf("chunk_manifest = %q, want \"undecided\" — and it is a final answer",
				got.ChunkManifest)
		}
	}

	if got := h.countRows(t, `SELECT count(*) FROM chunk_manifests`); got != before {
		t.Errorf("reading a blob created %d manifest row(s)", got-before)
	}
	if got := h.countRows(t, `SELECT count(*) FROM manifest_chunks`); got != beforeChunks {
		t.Errorf("reading a blob created %d chunk row(s)", got-beforeChunks)
	}
	if got := h.countRows(t, `SELECT count(*) FROM jobs`); got != beforeJobs {
		t.Errorf("reading a blob enqueued %d job(s)", got-beforeJobs)
	}
	if got := h.countRows(t, `SELECT count(*) FROM jobs WHERE type = 'chunk_blob'`); got != 0 {
		t.Errorf("reading a blob enqueued %d chunk_blob job(s)", got)
	}
	if got := h.countRows(t,
		`SELECT count(*) FROM blobs WHERE chunking_exempt_reason IS NOT NULL`); got != 0 {
		t.Errorf("reading a blob recorded %d chunking decision(s)", got)
	}
}

func repeatHex(c string) string {
	out := ""
	for range 64 {
		out += c
	}
	return out
}
