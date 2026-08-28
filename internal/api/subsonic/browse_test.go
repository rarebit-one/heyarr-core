package subsonic_test

import (
	"net/url"
	"testing"
)

func TestGetArtists(t *testing.T) {
	h := newHarness(t)
	r := h.get("getArtists", nil)
	if r.Artists == nil {
		t.Fatal("no artists payload")
	}
	if r.Artists.IgnoredArticles == "" {
		t.Error("ignoredArticles should be echoed so the client sorts the same way")
	}

	// Two buckets: Bearing under B, "The Cartographers" under C (article
	// stripped), in that order.
	var letters []string
	for _, idx := range r.Artists.Index {
		letters = append(letters, idx.Name)
	}
	if len(letters) != 2 || letters[0] != "B" || letters[1] != "C" {
		t.Fatalf("index letters = %v, want [B C]", letters)
	}

	cart := r.Artists.Index[1].Artist[0]
	if cart.Name != "The Cartographers" {
		t.Fatalf("C bucket artist = %q, want The Cartographers", cart.Name)
	}
	if cart.AlbumCount != 2 {
		t.Errorf("Cartographers albumCount = %d, want 2", cart.AlbumCount)
	}

	// The opaque id round-trips: hand it back to getArtist and get the same
	// artist. This is what proves the id is usable by a real client, not just
	// well-formed.
	back := h.get("getArtist", url.Values{"id": {cart.ID}})
	if back.Artist == nil || back.Artist.Name != "The Cartographers" {
		t.Fatalf("getArtist(%q) did not return the artist: %+v", cart.ID, back.Artist)
	}
}

func TestGetArtist(t *testing.T) {
	h := newHarness(t)
	artists := h.get("getArtists", nil)
	id := artists.Artists.Index[1].Artist[0].ID // The Cartographers

	r := h.get("getArtist", url.Values{"id": {id}})
	if r.Artist == nil {
		t.Fatalf("no artist payload: %+v", r.Error)
	}
	if r.Artist.AlbumCount != 2 || len(r.Artist.Album) != 2 {
		t.Fatalf("album count = %d / %d, want 2", r.Artist.AlbumCount, len(r.Artist.Album))
	}
	// Ordered by year: Contour Lines (2001) before Field Notes (2004).
	if r.Artist.Album[0].Name != "Contour Lines" || r.Artist.Album[1].Name != "Field Notes" {
		t.Fatalf("album order = %v, want [Contour Lines, Field Notes]", names(r.Artist.Album))
	}
	if got := findAlbum(t, r.Artist.Album, "Contour Lines").SongCount; got != 2 {
		t.Errorf("Contour Lines songCount = %d, want 2", got)
	}
}

func TestGetArtistNotFound(t *testing.T) {
	h := newHarness(t)
	// A well-formed but wrong-kind id must not be mistaken for an artist.
	r := h.get("getArtist", url.Values{"id": {"tr:something"}})
	if r.Status != "failed" || r.Error == nil || r.Error.Code != 70 {
		t.Fatalf("expected not-found, got %+v / %+v", r.Status, r.Error)
	}
}

func TestGetAlbumList2Alphabetical(t *testing.T) {
	h := newHarness(t)
	r := h.get("getAlbumList2", url.Values{"type": {"alphabeticalByName"}})
	if r.AlbumList2 == nil {
		t.Fatal("no albumList2 payload")
	}
	got := names(r.AlbumList2.Album)
	want := []string{"Azimuth", "Contour Lines", "Field Notes", "Meridian"}
	if len(got) != len(want) {
		t.Fatalf("albums = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("albums = %v, want %v", got, want)
		}
	}
	// Meridian's only track is linked (no blob), so it is a real album with
	// zero streamable songs — not omitted, not fabricated.
	if m := findAlbum(t, r.AlbumList2.Album, "Meridian"); m.SongCount != 0 {
		t.Errorf("Meridian songCount = %d, want 0 (its only track is linked)", m.SongCount)
	}
}

func TestGetAlbumList2SizeAndOffset(t *testing.T) {
	h := newHarness(t)
	first := h.get("getAlbumList2", url.Values{"type": {"alphabeticalByName"}, "size": {"2"}})
	if len(first.AlbumList2.Album) != 2 {
		t.Fatalf("size=2 returned %d albums", len(first.AlbumList2.Album))
	}
	second := h.get("getAlbumList2", url.Values{"type": {"alphabeticalByName"}, "size": {"2"}, "offset": {"2"}})
	if len(second.AlbumList2.Album) != 2 {
		t.Fatalf("offset page returned %d albums", len(second.AlbumList2.Album))
	}
	if first.AlbumList2.Album[0].ID == second.AlbumList2.Album[0].ID {
		t.Error("offset did not advance the page")
	}
}

// TestGetAlbumList2PersonalTypesEmpty proves the history/starred list types
// return an empty list, not a fabricated one: that state is personal and
// encrypted, and the controller cannot read it (§72).
func TestGetAlbumList2PersonalTypesEmpty(t *testing.T) {
	h := newHarness(t)
	for _, typ := range []string{"starred", "recent", "frequent"} {
		r := h.get("getAlbumList2", url.Values{"type": {typ}})
		if r.Status != "ok" {
			t.Fatalf("type=%s status %q", typ, r.Status)
		}
		if r.AlbumList2 == nil || len(r.AlbumList2.Album) != 0 {
			t.Fatalf("type=%s should be empty, got %+v", typ, r.AlbumList2)
		}
	}
}

func TestGetAlbum(t *testing.T) {
	h := newHarness(t)
	id := h.albumID(t, "Contour Lines")

	r := h.get("getAlbum", url.Values{"id": {id}})
	if r.Album == nil {
		t.Fatalf("no album payload: %+v", r.Error)
	}
	if r.Album.Name != "Contour Lines" || r.Album.Artist != "The Cartographers" || r.Album.Year != 2001 {
		t.Fatalf("album header wrong: %+v", r.Album)
	}
	if r.Album.SongCount != 2 || len(r.Album.Song) != 2 {
		t.Fatalf("song count = %d / %d, want 2", r.Album.SongCount, len(r.Album.Song))
	}

	// Ordered by disc then track: Datum (1), Benchmark (2).
	datum, bench := r.Album.Song[0], r.Album.Song[1]
	if datum.Title != "Datum" || datum.Track != 1 || datum.Suffix != "flac" {
		t.Errorf("track 1 = %+v", datum)
	}
	if bench.Title != "Benchmark" || bench.Track != 2 || bench.Suffix != "mp3" {
		t.Errorf("track 2 = %+v", bench)
	}
	if datum.ContentType != "audio/flac" || datum.Type != "music" {
		t.Errorf("track content-type/type = %q/%q", datum.ContentType, datum.Type)
	}
	if datum.Size == 0 {
		t.Error("track size should be the blob size")
	}
	// Cross-references a client follows: a song points back at its album and
	// artist by the same opaque ids those endpoints accept.
	if datum.AlbumID != id || datum.Parent != id {
		t.Errorf("song albumId/parent = %q/%q, want %q", datum.AlbumID, datum.Parent, id)
	}
	if datum.ArtistID == "" {
		t.Error("song should carry an artistId")
	}
}

func TestGetAlbumLinkedTrackExcluded(t *testing.T) {
	h := newHarness(t)
	id := h.albumID(t, "Meridian")
	r := h.get("getAlbum", url.Values{"id": {id}})
	if r.Album == nil {
		t.Fatalf("no album payload: %+v", r.Error)
	}
	if r.Album.SongCount != 0 || len(r.Album.Song) != 0 {
		t.Fatalf("Meridian should list no streamable songs, got %+v", r.Album.Song)
	}
}

func TestGetAlbumNotFound(t *testing.T) {
	h := newHarness(t)
	r := h.get("getAlbum", url.Values{"id": {"al:no-such-work"}})
	if r.Status != "failed" || r.Error == nil || r.Error.Code != 70 {
		t.Fatalf("expected not-found, got %+v / %+v", r.Status, r.Error)
	}
}

// albumID fetches the opaque id a client would use for the named album.
func (h *harness) albumID(t *testing.T, name string) string {
	t.Helper()
	list := h.get("getAlbumList2", url.Values{"type": {"alphabeticalByName"}, "size": {"500"}})
	return findAlbum(t, list.AlbumList2.Album, name).ID
}
