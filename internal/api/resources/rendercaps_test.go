package resources_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/render"
	"github.com/rarebit-one/heyarr-core/internal/auth"
)

// seedTrackReplicas records this node holding the music tracks (and the poster,
// for the media-type refusal), as a scan would:
// a capability is minted only for bytes routing can find.
func (h *harness) seedTrackReplicas() *harness {
	h.t.Helper()
	for _, hash := range []string{track1BlobHash, track2BlobHash, track3BlobHash, posterBlobHash} {
		h.exec(`INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, verified_at, reported_at, updated_at)
			VALUES (?, ?, 'present', 1024, ?, ?, ?)`, hash, peerID, seedTime, seedTime, seedTime)
	}
	return h
}

var testRenderSecret = []byte("0123456789abcdef0123456789abcdef")

const (
	rendererOrigin = "http://192.0.2.10:8098"
	publicOrigin   = "https://heyarr.example"
)

type renderCapabilities struct {
	Capabilities []struct {
		AssetID           string `json:"asset_id"`
		RenderURL         string `json:"render_url"`
		RenderUnavailable string `json:"render_unavailable"`
		ExpiresAt         string `json:"expires_at"`
	} `json:"capabilities"`
}

// mintCapabilities returns the status, the raw body (for failure messages) and the
// parsed answer when it is a 200. The body is always read, which closes it.
func (h *harness) mintCapabilities(t *testing.T, body string) (int, string, renderCapabilities) {
	t.Helper()
	resp := h.do(http.MethodPost, "/api/v1/render/capabilities", "", strings.NewReader(body))
	defer func() { _ = resp.Body.Close() }()
	raw := h.body(resp)
	var out renderCapabilities
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, string(raw), out
}

// A phone queueing an album gets one capability per track, at the PUBLIC
// origin it reaches the node through — not the LAN render listener a
// television is pointed at — and each verifies for exactly its own blob.
func TestRenderCapabilitiesMintPerTrackAtThePublicOrigin(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withRender(testRenderSecret, rendererOrigin, publicOrigin)).seed().seedBrowse().seedTrackReplicas()

	status, raw, got := h.mintCapabilities(t, `{"asset_ids":["`+track1AssetID+`","`+track2AssetID+`"]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}
	if len(got.Capabilities) != 2 {
		t.Fatalf("got %d capabilities, want 2", len(got.Capabilities))
	}
	for i, want := range []struct{ asset, blob string }{{track1AssetID, track1BlobHash}, {track2AssetID, track2BlobHash}} {
		c := got.Capabilities[i]
		if c.AssetID != want.asset {
			t.Errorf("[%d] asset_id = %q, want %q (request order)", i, c.AssetID, want.asset)
		}
		if c.RenderUnavailable != "" {
			t.Fatalf("[%d] unavailable: %s", i, c.RenderUnavailable)
		}
		if !strings.HasPrefix(c.RenderURL, publicOrigin+"/render/") {
			t.Errorf("[%d] render_url = %q, want it at the public origin", i, c.RenderURL)
		}
		token := strings.TrimPrefix(c.RenderURL, publicOrigin+"/render/")
		token, _, _ = strings.Cut(token, "/")
		capability, err := render.Verify(testRenderSecret, token, fixedTime)
		if err != nil {
			t.Fatalf("[%d] capability does not verify: %v", i, err)
		}
		if capability.BlobHash != want.blob || capability.MIME != "audio/flac" {
			t.Errorf("[%d] capability = %+v, want blob %s audio/flac", i, capability, want.blob)
		}
		if want := fixedTime.Add(6 * time.Hour).UTC().Format(time.RFC3339); c.ExpiresAt != want {
			t.Errorf("[%d] expires_at = %q, want %q", i, c.ExpiresAt, want)
		}
	}
}

// Minting is not playing: no session, no token, no event. POST /playback
// would have opened one session per track.
func TestRenderCapabilitiesOpenNoSession(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withRender(testRenderSecret, rendererOrigin, publicOrigin)).seed().seedBrowse().seedTrackReplicas()
	count := func(table string) int {
		var n int
		if err := h.db.Reader().QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	sessions, events := count("consumption_sessions"), count("events")

	if status, raw, _ := h.mintCapabilities(t, `{"asset_ids":["`+track1AssetID+`"]}`); status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}
	if got := count("consumption_sessions"); got != sessions {
		t.Errorf("consumption_sessions %d -> %d: minting opened a session", sessions, got)
	}
	if got := count("events"); got != events {
		t.Errorf("events %d -> %d: minting emitted an event", events, got)
	}
}

// Every refusal is a reason the client can act on, per asset, never silence.
func TestRenderCapabilitiesExplainEachRefusal(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withRender(testRenderSecret, rendererOrigin, publicOrigin)).seed().seedBrowse().seedTrackReplicas()

	_, _, got := h.mintCapabilities(t, `{"asset_ids":["`+posterAssetID+`","no-such-asset",""]}`)
	if len(got.Capabilities) != 3 {
		t.Fatalf("got %d capabilities, want 3", len(got.Capabilities))
	}
	for i, wantSubstr := range []string{"image/jpeg", "no asset", "empty asset id"} {
		c := got.Capabilities[i]
		if c.RenderURL != "" || c.ExpiresAt != "" {
			t.Errorf("[%d] minted %q for an asset that should be refused", i, c.RenderURL)
		}
		if !strings.Contains(c.RenderUnavailable, wantSubstr) {
			t.Errorf("[%d] render_unavailable = %q, want it to mention %q", i, c.RenderUnavailable, wantSubstr)
		}
	}
}

// A node with no render secret says so for every asset rather than answering
// with neither a URL nor a reason.
func TestRenderCapabilitiesOnAnUnconfiguredNodeSaySo(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedBrowse()

	_, _, got := h.mintCapabilities(t, `{"asset_ids":["`+track1AssetID+`"]}`)
	if len(got.Capabilities) != 1 || got.Capabilities[0].RenderURL != "" ||
		!strings.Contains(got.Capabilities[0].RenderUnavailable, "mints no capabilities") {
		t.Errorf("capabilities = %+v, want one refusal naming the missing configuration", got.Capabilities)
	}
}

func TestRenderCapabilitiesRequestBounds(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withRender(testRenderSecret, rendererOrigin, publicOrigin)).seed()

	many := `{"asset_ids":[` + strings.TrimSuffix(strings.Repeat(`"a",`, 201), ",") + `]}`
	for name, body := range map[string]string{"empty": `{"asset_ids":[]}`, "missing": `{}`, "too many": many} {
		if status, _, _ := h.mintCapabilities(t, body); status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, status)
		}
	}
}

// A capability is an unauthenticated URL, so a Guest gets one only for what a
// Guest may see (ADR-0074): a vault asset is "no asset", exactly as GET
// /assets/{id} answers, while an enrolled reader is minted one.
func TestRenderCapabilitiesKeepTheGuestBoundary(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withAuth, withGuest, withRender(testRenderSecret, rendererOrigin, publicOrigin)).seed()
	seedVaultAsset(h)
	h.exec(`INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, verified_at, reported_at, updated_at)
		VALUES (?, ?, 'present', 1024, ?, ?, ?)`, vaultBlobHash, peerID, seedTime, seedTime, seedTime)
	body := `{"asset_ids":["` + vaultAssetID + `"]}`

	resp := h.do(http.MethodPost, "/api/v1/render/capabilities", "", strings.NewReader(body))
	defer func() { _ = resp.Body.Close() }()
	var asGuest renderCapabilities
	if err := json.Unmarshal(h.body(resp), &asGuest); err != nil || len(asGuest.Capabilities) != 1 {
		t.Fatalf("guest answer: status %d, err %v, %+v", resp.StatusCode, err, asGuest)
	}
	if c := asGuest.Capabilities[0]; c.RenderURL != "" || !strings.Contains(c.RenderUnavailable, "no asset") {
		t.Errorf("guest was answered %+v, want the unknown-asset refusal", c)
	}

	tok := h.mint("reader", auth.ScopeRead)
	resp2 := h.do(http.MethodPost, "/api/v1/render/capabilities", tok.Secret, strings.NewReader(body))
	defer func() { _ = resp2.Body.Close() }()
	var asReader renderCapabilities
	if err := json.Unmarshal(h.body(resp2), &asReader); err != nil || len(asReader.Capabilities) != 1 {
		t.Fatalf("reader answer: status %d, err %v", resp2.StatusCode, err)
	}
	// The vault asset is a video/x-matroska blob this node holds: a reader is minted one.
	if c := asReader.Capabilities[0]; c.RenderURL == "" {
		t.Errorf("reader was refused: %s", c.RenderUnavailable)
	}
}

// Bytes routing cannot find get a reason, not a capability that fetches nothing.
func TestRenderCapabilitiesRefuseUnroutedBytes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withRender(testRenderSecret, rendererOrigin, publicOrigin)).seed().seedBrowse()

	_, _, got := h.mintCapabilities(t, `{"asset_ids":["`+track1AssetID+`"]}`)
	if len(got.Capabilities) != 1 || got.Capabilities[0].RenderURL != "" ||
		!strings.Contains(got.Capabilities[0].RenderUnavailable, "peer") {
		t.Errorf("capabilities = %+v, want a routing refusal", got.Capabilities)
	}
}
