package subsonic

import (
	"encoding/hex"
	"path"
	"strings"
)

// decodeHex reverses Subsonic's enc:<hex> password encoding, which a client
// uses so a password never appears verbatim in a URL. It is hex of the raw
// password bytes, nothing more.
func decodeHex(s string) (string, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// articles are the words a Subsonic client is told to ignore when sorting, and
// which the adapter therefore strips before it files a name in the index. The
// set matches ignoredArticles, echoed to the client so the two agree.
var articles = strings.Fields(ignoredArticles)

// stripArticle removes a single leading article so "The Cartographers" files
// and sorts under C. Only the first word is considered, and only as a whole
// word — "Theremin" keeps its T.
func stripArticle(name string) string {
	trimmed := strings.TrimSpace(name)
	for _, a := range articles {
		if len(trimmed) > len(a)+1 &&
			strings.EqualFold(trimmed[:len(a)], a) &&
			trimmed[len(a)] == ' ' {
			return strings.TrimSpace(trimmed[len(a)+1:])
		}
	}
	return trimmed
}

// suffixOf is the file-type suffix a Subsonic client shows and uses to guess a
// player: "flac", "mp3". The filename extension is the authority because it is
// what the bytes were ingested as; the edition type is a fallback for a rare
// asset with no filename.
func suffixOf(filename, editionType string) string {
	if ext := strings.TrimPrefix(path.Ext(filename), "."); ext != "" {
		return strings.ToLower(ext)
	}
	return strings.ToLower(editionType)
}
