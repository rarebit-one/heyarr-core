package subsonic_test

import (
	"net/url"
	"testing"
)

func TestRecordingSongsAndStreamsRequireMatchingBlobEvidence(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	trackID := h.songID(t, "Contour Lines", "Datum")
	albumID := h.albumID(t, "Contour Lines")
	h.exec(`UPDATE works SET work_key='music-recording:11111111-1111-4111-8111-111111111111' WHERE id='wa'`)
	check := func(wantVisible bool) {
		t.Helper()
		a := h.get("getAlbum", url.Values{"id": {albumID}}).Album
		if a == nil || (len(a.Song) > 0) != wantVisible {
			t.Fatalf("album = %+v, want visible %v", a, wantVisible)
		}
		for _, id := range []string{trackID, "tr:ea1"} {
			q := h.creds()
			q.Set("id", id)
			resp := h.raw("stream", q)
			if wantVisible {
				if got := string(readAll(t, resp)); got != string(h.bytesOf["Datum"]) {
					t.Fatalf("stream %s = %q", id, got)
				}
			} else {
				r := decode(t, resp)
				if r.Status != "failed" || r.Error == nil || r.Error.Code != 70 {
					t.Fatalf("stream %s = %+v", id, r)
				}
			}
		}
	}
	check(false)
	h.exec(`UPDATE works SET attributes=json_set(attributes, '$.metadata_matched_blob', ?) WHERE id='wa'`, h.hashOf["Datum"])
	check(true)
	h.exec(`UPDATE works SET attributes=json_set(attributes, '$.metadata_matched_blob', ?) WHERE id='wa'`, "blake3:different")
	check(false)
}
