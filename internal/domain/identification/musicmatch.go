package identification

import (
	"strings"
	"unicode"
)

// MatchMusicTags checks whether the file's own embedded title and artist agree
// with a text request. It is metadata evidence only: tags can be wrong, and a
// match neither proves the audio nor identifies a particular master. Missing,
// conflicting, or multi-artist metadata is not accepted on a guess.
func MatchMusicTags(title, artist string, tags map[string]string) (bool, string) {
	values := make(map[string]string, len(tags))
	for k, v := range tags {
		key := strings.ToLower(strings.TrimSpace(k))
		if prev, ok := values[key]; ok && (key == "title" || key == "artist") && musicTagText(prev) != musicTagText(v) {
			return false, "conflicting embedded " + key + " tags"
		}
		values[key] = v
	}
	gotTitle, gotArtist := values["title"], values["artist"]
	if strings.TrimSpace(gotTitle) == "" || strings.TrimSpace(gotArtist) == "" {
		return false, "embedded title or artist is missing"
	}
	if musicTagText(title) == "" || musicTagText(artist) == "" {
		return false, "requested title or artist is missing"
	}
	if musicTagText(gotArtist) != musicTagText(artist) {
		return false, "embedded artist differs from request"
	}
	if musicTagText(gotTitle) != musicTagText(title) {
		return false, "embedded title differs from request"
	}
	return true, "embedded title and artist match; audio not fingerprinted"
}

func musicTagText(s string) string {
	var b strings.Builder
	space := true
	for _, r := range strings.ToLower(s) {
		if r == '\'' || r == '’' {
			continue
		}
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
			space = false
		} else if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

// MusicReleaseCouldContain is a conservative name-based discovery gate. It is
// not evidence about audio bytes: it merely avoids downloading releases whose
// title neither names the song nor indicates an artist compilation or album.
func MusicReleaseCouldContain(release, title, artist string) bool {
	release = " " + musicTagText(release) + " "
	title = musicTagText(title)
	artist = musicTagText(artist)
	if title == "" || artist == "" || !strings.Contains(release, " "+artist+" ") {
		return false
	}
	// A compilation or album can contain the song even when its name is not
	// the song title. This only admits it to download: the ingest gate still
	// checks each track's own artist and title and requires a unique match.
	if !strings.Contains(release, " "+title+" ") {
		plausible := false
		for _, marker := range []string{"album", "best of", "greatest hits", "collection", "anthology", "compilation", "essential", "singles"} {
			if strings.Contains(release, " "+marker+" ") {
				plausible = true
				break
			}
		}
		if !plausible {
			return false
		}
	}
	return !MusicAlternateVersion(release, title, artist)
}

// MusicAlternateVersion detects explicit variant terms in release or file
// names. Absence of such a term cannot establish which performance was tagged.
func MusicAlternateVersion(context, title, artist string) bool {
	context = " " + musicTagText(context) + " "
	title, artist = " "+musicTagText(title)+" ", " "+musicTagText(artist)+" "
	for _, version := range []string{"live", "karaoke", "remix", "instrumental", "cover", "tribute"} {
		if strings.Contains(context, " "+version+" ") && !strings.Contains(title, " "+version+" ") && !strings.Contains(artist, " "+version+" ") {
			return true
		}
	}
	return false
}
