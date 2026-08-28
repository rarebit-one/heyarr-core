//nolint:bodyclose // response bodies are closed by the t.Cleanup the harness registers
package gateway_test

import (
	"bytes"
	"context"
	"io"
	"net/url"
	"strings"
	"testing"
)

// TestGatewayServesDecryptedPlaylistsAndProxiesLibrary is the milestone's claim,
// end to end: a stock-Subsonic-shaped request to the gateway returns the
// playlist from ON-DEVICE-DECRYPTED CRDT state, AND the artist list proxied from
// the controller — while the controller never sees a byte of the playlist
// plaintext. A proxied stream is byte-identical to the controller's own bytes.
func TestGatewayServesDecryptedPlaylistsAndProxiesLibrary(t *testing.T) {
	h := newHarness(t)

	// 1. Personal state, served locally from decrypted CRDT.
	pls := h.get("getPlaylists", nil)
	if pls.Status != "ok" {
		t.Fatalf("getPlaylists status = %q (%+v)", pls.Status, pls.Error)
	}
	if pls.Type != "heyarr-gateway" {
		t.Errorf("getPlaylists was answered by %q, not the gateway itself", pls.Type)
	}
	if pls.Playlists == nil || len(pls.Playlists.Playlist) != 1 {
		t.Fatalf("want exactly one decryptable playlist, got %+v", pls.Playlists)
	}
	if got := pls.Playlists.Playlist[0].SongCount; got != len(secretItems) {
		t.Errorf("playlist songCount = %d, want %d", got, len(secretItems))
	}
	if id := pls.Playlists.Playlist[0].ID; id != h.spaceID {
		t.Errorf("playlist id = %q, want the space id %q", id, h.spaceID)
	}

	// 2. The playlist's entries are the decrypted items, in converged order.
	one := h.get("getPlaylist", url.Values{"id": {h.spaceID}})
	if one.Status != "ok" || one.Playlist == nil {
		t.Fatalf("getPlaylist status = %q (%+v)", one.Status, one.Error)
	}
	var gotItems []string
	for _, e := range one.Playlist.Entry {
		gotItems = append(gotItems, e.ID)
	}
	if strings.Join(gotItems, ",") != strings.Join(secretItems, ",") {
		t.Errorf("playlist entries = %v, want %v (decrypted, in Lamport order)", gotItems, secretItems)
	}

	// 3. Library, proxied to the controller.
	artists := h.get("getArtists", nil)
	if artists.Status != "ok" || artists.Artists == nil {
		t.Fatalf("getArtists (proxied) status = %q (%+v)", artists.Status, artists.Error)
	}
	if !artistPresent(artists, "The Cartographers") {
		t.Errorf("proxied getArtists did not carry the seeded artist: %+v", artists.Artists)
	}

	// 4. THE INVARIANT (Invariant 6, §72, ADR-0051): the controller never saw the
	//    playlist plaintext, even though the gateway just served it.
	//
	// The decisive check is over the CIPHERTEXT AT REST — the exact bytes the
	// controller stores and serves for this space's changes, base64-decoded back
	// to the bytes on the wire. This is the M9 claim ("the plaintext item is
	// nowhere in the bytes at rest") applied to the gateway's own space: the
	// controller answered the gateway's change fetch, so it demonstrably held
	// these bytes, and the plaintext the gateway just served is in none of them.
	changes, err := h.api.Changes(context.Background(), h.spaceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != len(secretItems) {
		t.Fatalf("the controller holds %d changes, want %d — the at-rest check would be vacuous otherwise", len(changes), len(secretItems))
	}
	for _, ec := range changes {
		for _, secret := range secretItems {
			if bytes.Contains(ec.Ciphertext, []byte(secret)) {
				t.Errorf("the controller's at-rest ciphertext contains the plaintext item %q — personal-state plaintext reached the controller (Invariant 6, §72 VIOLATED)", secret)
			}
		}
	}
	// And nowhere in ANY response the controller served — a second, coarser net
	// over every captured body. (On its own this is not sufficient: ciphertext
	// rides the wire base64-encoded, so a raw-substring scan cannot see through
	// it — which is exactly why the decoded at-rest check above is the decisive
	// one.)
	bodies := h.ctlBodies()
	for _, secret := range secretItems {
		if bytes.Contains(bodies, []byte(secret)) {
			t.Errorf("the controller served the plaintext item %q in a response body (Invariant 6, §72 VIOLATED)", secret)
		}
	}
	// The negatives are not vacuous: the controller DID serve the proxied
	// library, and that traffic was captured — so the absences are real.
	if !bytes.Contains(bodies, []byte("Cartographers")) {
		t.Fatal("the recorder captured no proxied library traffic, so the plaintext-absence assertion proves nothing")
	}
	// And the personal-state methods were served locally, never proxied: the
	// controller received no getPlaylists/getPlaylist request at all.
	for _, p := range h.ctlPaths() {
		if strings.Contains(p, "/rest/getPlaylist") {
			t.Errorf("the controller received a personal-state request %q — it must be served on the device", p)
		}
	}

	// 5. A proxied stream is byte-identical to the controller's own bytes.
	resp := h.raw("stream", url.Values{"id": {"tr:ea1"}})
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != trackFLAC {
		t.Errorf("streamed bytes are not byte-identical to the blob:\n got %q\nwant %q", got, trackFLAC)
	}
}

// TestTheAppAndControllerCredentialsAreDistinct is the two-credential design made
// a test: the app authenticates to the DEVICE with its own password, and the
// controller bearer is NOT that password. A stock app can never present, guess or
// leak the controller token, because the device never accepts it.
func TestTheAppAndControllerCredentialsAreDistinct(t *testing.T) {
	h := newHarness(t)

	// The right device password works (already exercised above, asserted here so
	// the negatives below are not vacuously passing against a server that refuses
	// everything).
	if ok := h.get("getPlaylists", nil); ok.Status != "ok" {
		t.Fatalf("the correct device password was refused: %+v", ok.Error)
	}

	cases := []struct {
		name string
		q    url.Values
		code int
	}{
		{"wrong password", url.Values{"u": {appUser}, "p": {"not-the-password"}, "f": {"json"}}, 40},
		{"missing password", url.Values{"u": {appUser}, "f": {"json"}}, 10},
		{"salted token is refused", url.Values{"u": {appUser}, "t": {"deadbeef"}, "s": {"salt"}, "f": {"json"}}, 40},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := decodeResp(t, h.rawQuery("getPlaylists", tc.q))
			if resp.Error == nil || resp.Error.Code != tc.code {
				t.Fatalf("want error code %d, got %+v", tc.code, resp.Error)
			}
		})
	}
}

// TestAnUnsupportedMethodIsRefusedInTheEnvelope: an unknown method returns a
// Subsonic error the client can parse, not an HTTP 404 it chokes on.
func TestAnUnsupportedMethodIsRefusedInTheEnvelope(t *testing.T) {
	h := newHarness(t)
	resp := h.get("getPodcasts", nil)
	if resp.Error == nil {
		t.Fatalf("an unsupported method was not refused: %+v", resp)
	}
	if !strings.Contains(resp.Error.Message, "unsupported method") {
		t.Errorf("refusal should name the problem; got %q", resp.Error.Message)
	}
}

func artistPresent(r subResp, name string) bool {
	for _, idx := range r.Artists.Index {
		for _, a := range idx.Artist {
			if a.Name == name {
				return true
			}
		}
	}
	return false
}
