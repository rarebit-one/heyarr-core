package subsonic

import (
	"encoding/base64"
	"strings"
)

// Subsonic identifiers are opaque strings a client round-trips verbatim: it
// reads an id from one response and hands it back to another endpoint. So every
// id this adapter emits must decode, offline, back to the catalogue query that
// produced it — there is no id table.
//
// Three shapes, each self-describing by a two-letter tag:
//
//	al:<work-id>     an album is a music Work; the id is the Work's UUID.
//	tr:<edition-id>  a track is an Edition; the id is the Edition's UUID.
//	ar:<b64(name)>   an artist is NOT an entity — it is a projection over
//	                 works.attributes.artist — so the id carries the name
//	                 itself, base64url-encoded so an arbitrary name (slashes,
//	                 spaces, unicode) survives a URL round-trip.
//
// A malformed or wrong-kind id decodes to ("", false); callers turn that into a
// Subsonic "not found" rather than a query on garbage.
const (
	tagAlbum  = "al:"
	tagTrack  = "tr:"
	tagArtist = "ar:"
)

func albumID(workID string) string    { return tagAlbum + workID }
func trackID(editionID string) string { return tagTrack + editionID }

func artistID(name string) string {
	return tagArtist + base64.RawURLEncoding.EncodeToString([]byte(name))
}

// decodeAlbumID recovers a Work id from an album id.
func decodeAlbumID(id string) (string, bool) {
	return strings.TrimPrefix(id, tagAlbum), strings.HasPrefix(id, tagAlbum) && len(id) > len(tagAlbum)
}

// decodeTrackID recovers an Edition id from a track id.
func decodeTrackID(id string) (string, bool) {
	return strings.TrimPrefix(id, tagTrack), strings.HasPrefix(id, tagTrack) && len(id) > len(tagTrack)
}

// decodeArtistID recovers an artist name from an artist id.
func decodeArtistID(id string) (string, bool) {
	if !strings.HasPrefix(id, tagArtist) {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(id[len(tagArtist):])
	if err != nil || len(raw) == 0 {
		return "", false
	}
	return string(raw), true
}

// indexKey is the single upper-case letter a name is filed under in the artist
// index. It matches how the catalogue normalises for sort — leading articles
// are already stripped in sort_title, so this only needs the first rune. A name
// that does not start with a letter files under "#", the Subsonic convention.
func indexKey(sortName string) string {
	s := strings.TrimSpace(sortName)
	if s == "" {
		return "#"
	}
	r := []rune(strings.ToUpper(s))[0]
	if r < 'A' || r > 'Z' {
		return "#"
	}
	return string(r)
}
