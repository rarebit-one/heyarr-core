// Response bodies are closed by the t.Cleanup the harness registers in raw(),
// which bodyclose cannot see through.
//
//nolint:bodyclose // closed by the harness's t.Cleanup
package subsonic_test

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/api/subsonic"
)

// songID resolves the opaque track id a client would stream, for the named
// track on the named album.
func (h *harness) songID(t *testing.T, album, track string) string {
	t.Helper()
	r := h.get("getAlbum", url.Values{"id": {h.albumID(t, album)}})
	if r.Album == nil {
		t.Fatalf("no album %q", album)
	}
	for _, s := range r.Album.Song {
		if s.Title == track {
			return s.ID
		}
	}
	t.Fatalf("track %q not on album %q", track, album)
	return ""
}

func TestStreamReturnsExactBytes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id := h.songID(t, "Contour Lines", "Datum")

	q := h.creds()
	q.Set("id", id)
	resp := h.raw("stream", q)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}
	body := readAll(t, resp)
	if !bytes.Equal(body, h.bytesOf["Datum"]) {
		t.Fatalf("stream body = %q, want the blob bytes %q", body, h.bytesOf["Datum"])
	}
}

func TestAlbumProjectsAssetsAndKeepsOldTrackIDs(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.track(context.Background(), h.casStore, "wa", "ea3", 1, 3, "Elevation", "flac", "audio/flac", []byte("third track bytes"))
	h.exec(`UPDATE assets SET edition_id = 'ea1' WHERE id = 'aea3'`)
	h.track(context.Background(), h.casStore, "wa", "ea4", 1, 1, "Datum alternate", "mp3", "audio/mpeg", []byte("alternate datum bytes"))
	h.exec(`UPDATE assets SET attributes = json_object('disc', 1, 'track', 1, 'track_title', 'Datum') WHERE id = 'aea4'`)

	album := h.get("getAlbum", url.Values{"id": {h.albumID(t, "Contour Lines")}}).Album
	if album == nil || album.SongCount != 3 || len(album.Song) != 3 {
		t.Fatalf("album songs = %+v, want three tracks despite a shared edition and alternate format", album)
	}
	for i, title := range []string{"Datum", "Benchmark", "Elevation"} {
		if album.Song[i].Title != title {
			t.Fatalf("song %d = %q, want %q", i, album.Song[i].Title, title)
		}
	}
	if list := h.get("getAlbumList2", nil).AlbumList2; list == nil || findAlbum(t, list.Album, "Contour Lines").SongCount != 3 {
		t.Fatalf("album-list song count did not match the three visible tracks: %+v", list)
	}
	for _, id := range []string{album.Song[0].ID, "tr:ea1"} {
		q := h.creds()
		q.Set("id", id)
		resp := h.raw("stream", q)
		if resp.StatusCode != http.StatusOK || !bytes.Equal(readAll(t, resp), h.bytesOf["Datum"]) {
			t.Fatalf("track %q did not stream the original Datum bytes", id)
		}
	}
	q := h.creds()
	q.Set("id", album.Song[2].ID)
	if got := readAll(t, h.raw("stream", q)); !bytes.Equal(got, h.bytesOf["Elevation"]) {
		t.Fatalf("third track streamed %q, want Elevation bytes", got)
	}
}

func TestLegacyFilenamesDedupeAcrossFormats(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.track(context.Background(), h.casStore, "wb", "eb2", 0, 0, "Traverse alternate", "mp3", "audio/mpeg", []byte("alternate format"))
	h.exec(`UPDATE editions SET edition_key = 'flac', attributes = '{}' WHERE id = 'eb1'`)
	h.exec(`UPDATE editions SET edition_key = 'mp3', attributes = '{}' WHERE id = 'eb2'`)
	h.exec(`UPDATE assets SET attributes = '{}', filename = '01 Traverse.flac' WHERE id = 'aeb1'`)
	h.exec(`UPDATE assets SET attributes = '{}', filename = '01 Traverse.mp3' WHERE id = 'aeb2'`)

	album := h.get("getAlbum", url.Values{"id": {h.albumID(t, "Field Notes")}}).Album
	if album == nil || album.SongCount != 1 || len(album.Song) != 1 {
		t.Fatalf("legacy album = %+v, want one song for two formats", album)
	}
	list := h.get("getAlbumList2", nil).AlbumList2
	if list == nil || findAlbum(t, list.Album, "Field Notes").SongCount != 1 {
		t.Fatalf("legacy album-list count = %+v, want one", list)
	}
}

func TestDownloadReturnsExactBytes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id := h.songID(t, "Contour Lines", "Benchmark")

	q := h.creds()
	q.Set("id", id)
	resp := h.raw("download", q)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d, want 200", resp.StatusCode)
	}
	if body := readAll(t, resp); !bytes.Equal(body, h.bytesOf["Benchmark"]) {
		t.Fatalf("download body = %q, want %q", body, h.bytesOf["Benchmark"])
	}
}

// TestStreamHonoursRange proves the byte serving is delegated to the blob
// handler unchanged: a Range request gets a 206 with a Content-Range and only
// the requested bytes. The adapter writes none of that itself.
func TestStreamHonoursRange(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id := h.songID(t, "Contour Lines", "Datum")
	want := h.bytesOf["Datum"]

	q := h.creds()
	q.Set("id", id)
	u := h.http.URL + subsonic.Prefix + "/stream?" + q.Encode()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("range status = %d, want 206", resp.StatusCode)
	}
	if resp.Header.Get("Content-Range") == "" {
		t.Error("206 is missing Content-Range")
	}
	if body := readAll(t, resp); !bytes.Equal(body, want[:4]) {
		t.Fatalf("range body = %q, want first four bytes %q", body, want[:4])
	}
}

func TestStreamNotFound(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	q := h.creds()
	q.Set("id", "tr:no-such-edition")
	r := decode(t, h.raw("stream", q))
	if r.Status != "failed" || r.Error == nil || r.Error.Code != 70 {
		t.Fatalf("expected not-found envelope, got %+v / %+v", r.Status, r.Error)
	}
}

func TestStreamDoesNotExposeNonMusicAsset(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.exec(`UPDATE works SET content_type = 'movie' WHERE id = 'wa'`)
	for _, id := range []string{"tr:aea1", "tr:ea1"} {
		q := h.creds()
		q.Set("id", id)
		r := decode(t, h.raw("stream", q))
		if r.Status != "failed" || r.Error == nil || r.Error.Code != 70 {
			t.Fatalf("non-music id %q was exposed: %+v", id, r)
		}
	}
}

func TestStreamRejectsWrongKindID(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	q := h.creds()
	q.Set("id", h.albumID(t, "Contour Lines")) // an album id, not a track id
	r := decode(t, h.raw("stream", q))
	if r.Status != "failed" || r.Error == nil || r.Error.Code != 70 {
		t.Fatalf("an album id must not stream, got %+v / %+v", r.Status, r.Error)
	}
}

func TestStreamRequiresAuth(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	q := url.Values{"id": {"tr:whatever"}, "f": {"json"}} // no credential
	r := decode(t, h.raw("stream", q))
	if r.Status != "failed" || r.Error == nil {
		t.Fatalf("stream without a credential must fail, got %+v", r)
	}
}
