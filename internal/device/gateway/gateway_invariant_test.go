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

// TestGatewayServesStarredAndHistoryFromDecryptedState is #387's claim, end to
// end: getStarred2, getNowPlaying and the three personal getAlbumList2 types
// (starred, recent, frequent) all return ON-DEVICE-DECRYPTED CRDT state, in the
// converged order — while the controller never sees a byte of the starred or
// played plaintext at rest (Invariant 6, §72).
func TestGatewayServesStarredAndHistoryFromDecryptedState(t *testing.T) {
	h := newHarness(t)

	// getStarred2 — most-recently-starred first: bravo was starred after alpha.
	starred := h.get("getStarred2", nil)
	if starred.Status != "ok" || starred.Starred2 == nil {
		t.Fatalf("getStarred2 status = %q (%+v)", starred.Status, starred.Error)
	}
	if got := idsOfSongs(starred.Starred2.Song); !equal(got, reverse(secretStars)) {
		t.Errorf("getStarred2 songs = %v, want %v (decrypted, newest-first)", got, reverse(secretStars))
	}
	if starred.Type != "heyarr-gateway" {
		t.Errorf("getStarred2 answered by %q, not the gateway itself", starred.Type)
	}

	// getAlbumList2?type=starred — the same set, rendered as albums.
	starAlbums := h.get("getAlbumList2", url.Values{"type": {"starred"}})
	if starAlbums.Status != "ok" || starAlbums.AlbumList2 == nil {
		t.Fatalf("getAlbumList2?type=starred status = %q (%+v)", starAlbums.Status, starAlbums.Error)
	}
	if got := idsOfAlbums(starAlbums.AlbumList2.Album); !equal(got, reverse(secretStars)) {
		t.Errorf("getAlbumList2?type=starred = %v, want %v", got, reverse(secretStars))
	}

	// getAlbumList2?type=recent — distinct items, most-recently-played first:
	// bravo (latest) then alpha.
	recent := h.get("getAlbumList2", url.Values{"type": {"recent"}})
	wantRecent := []string{"play:SECRET-bravo", "play:SECRET-alpha"}
	if got := idsOfAlbums(recent.AlbumList2.Album); !equal(got, wantRecent) {
		t.Errorf("getAlbumList2?type=recent = %v, want %v", got, wantRecent)
	}

	// getAlbumList2?type=frequent — most-played first: alpha (2) then bravo (1).
	frequent := h.get("getAlbumList2", url.Values{"type": {"frequent"}})
	wantFrequent := []string{"play:SECRET-alpha", "play:SECRET-bravo"}
	if got := idsOfAlbums(frequent.AlbumList2.Album); !equal(got, wantFrequent) {
		t.Errorf("getAlbumList2?type=frequent = %v, want %v", got, wantFrequent)
	}

	// getNowPlaying — the single most recent play: bravo.
	np := h.get("getNowPlaying", nil)
	if np.Status != "ok" || np.NowPlaying == nil {
		t.Fatalf("getNowPlaying status = %q (%+v)", np.Status, np.Error)
	}
	if len(np.NowPlaying.Entry) != 1 || np.NowPlaying.Entry[0].ID != "play:SECRET-bravo" {
		t.Errorf("getNowPlaying = %+v, want the single entry play:SECRET-bravo", np.NowPlaying.Entry)
	}

	// A catalogue getAlbumList2 type is still PROXIED, not served locally: the
	// controller must have received it.
	catalogue := h.get("getAlbumList2", url.Values{"type": {"newest"}})
	if catalogue.Status != "ok" {
		t.Fatalf("getAlbumList2?type=newest (proxied) status = %q (%+v)", catalogue.Status, catalogue.Error)
	}
	if !pathSeen(h.ctlPaths(), "/rest/getAlbumList2") {
		t.Error("a catalogue getAlbumList2 was not proxied to the controller")
	}

	// THE INVARIANT (Invariant 6, §72): the controller never held the starred or
	// played plaintext at rest, even though the gateway just served all of it. The
	// decisive check is over the CIPHERTEXT AT REST for each type's own space.
	h.assertNoPlaintextAtRest(h.starredSpaceID, secretStars)
	h.assertNoPlaintextAtRest(h.historySpaceID, distinct(secretPlays))

	// The personal-state methods were served locally, never proxied.
	for _, p := range h.ctlPaths() {
		for _, method := range []string{"/rest/getStarred2", "/rest/getNowPlaying"} {
			if strings.Contains(p, method) {
				t.Errorf("the controller received a personal-state request %q — it must be served on the device", p)
			}
		}
	}
}

// assertNoPlaintextAtRest fetches the controller's stored ciphertext for a space
// and asserts none of the secrets appear in it — the §72 claim applied to a
// non-playlist type's own space.
func (h *harness) assertNoPlaintextAtRest(spaceID string, secrets []string) {
	h.t.Helper()
	changes, err := h.api.Changes(context.Background(), spaceID)
	if err != nil {
		h.t.Fatal(err)
	}
	if len(changes) == 0 {
		h.t.Fatalf("space %s holds no changes at rest — the at-rest check would be vacuous", spaceID)
	}
	for _, ec := range changes {
		for _, secret := range secrets {
			if bytes.Contains(ec.Ciphertext, []byte(secret)) {
				h.t.Errorf("controller ciphertext for %s contains plaintext %q (Invariant 6, §72 VIOLATED)", spaceID, secret)
			}
		}
	}
}

func idsOfSongs(songs []idTitle) []string {
	out := make([]string, 0, len(songs))
	for _, s := range songs {
		out = append(out, s.ID)
	}
	return out
}

func idsOfAlbums(albums []struct {
	ID   string `json:"id"`
	Name string `json:"name"`
},
) []string {
	out := make([]string, 0, len(albums))
	for _, a := range albums {
		out = append(out, a.ID)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func reverse(xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[len(xs)-1-i] = x
	}
	return out
}

// distinct collapses a slice to its unique values, preserving first-seen order.
func distinct(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func pathSeen(paths []string, want string) bool {
	for _, p := range paths {
		if strings.Contains(p, want) {
			return true
		}
	}
	return false
}
