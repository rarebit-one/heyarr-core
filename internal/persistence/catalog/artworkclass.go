package catalog

import "strings"

// Artwork filename classes: the one definition of which `role='artwork'` file is a
// front-facing poster, which is wide decoration, and which is not a card image at
// all. The cover resolver (internal/api/resources/browse.go — the card pick, the
// backdrop pick) and the enrich predicate (enrichschedule.go — "does this Work
// already have a poster?") both read these, so the image a card shows and the
// image enrichment thinks the Work has can never disagree.
//
// They match the LOWER-CASED filename with SQLite GLOB. A release names its art the
// Kodi/Jellyfin way: `poster.jpg`, `fanart.jpg`, or `<episode>-thumb.jpg` for an
// episode still. A fetched image is named by FetchedArtwork.Kind (cover.jpg /
// backdrop.jpg) so it falls in the right class.
var (
	// posterGlobs is the front-facing image: a poster or cover. First choice
	// for a card.
	posterGlobs = []string{"poster*", "cover*", "folder*", "front*"}

	// backdropGlobs is a wide scene image — what the backdrop pick serves. It
	// is decoration for a card: a card shows it only when nothing better exists.
	backdropGlobs = []string{"fanart*", "backdrop*"}

	// decorationGlobs is every wide image a card may fall back to but that is
	// not a poster: the backdrops, plus a banner.
	decorationGlobs = append(append([]string{}, backdropGlobs...), "banner*")

	// unfitGlobs is art that is never a work's card image: an EPISODE still
	// (`<episode>-thumb.jpg`, the 16:9 frame Kodi/Jellyfin write per episode),
	// a landscape title card, and the transparent logo/clearart/disc layers.
	// A series whose only images are episode stills does NOT have a poster —
	// enrichment fetches one — and its card ranks those stills last.
	unfitGlobs = []string{
		"*-thumb.*", "thumb.*",
		"landscape*", "*-landscape.*",
		"clearlogo*", "*-clearlogo.*", "logo.*", "*-logo.*",
		"clearart*", "*-clearart.*",
		"disc.*", "*-disc.*", "discart*", "*-discart.*",
		"characterart*",
	}
)

// globAny renders "(lower(col) GLOB 'a' OR lower(col) GLOB 'b' …)". The globs
// are this file's literals, never input, so concatenation is safe.
func globAny(col string, globs []string) string {
	parts := make([]string, 0, len(globs))
	for _, g := range globs {
		parts = append(parts, "lower("+col+") GLOB '"+g+"'")
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// ArtworkIsBackdropSQL is the predicate "the artwork asset whose filename is at
// col is a backdrop" — the only images the backdrop pick may serve.
func ArtworkIsBackdropSQL(col string) string { return globAny(col, backdropGlobs) }

// ArtworkIsNotAPosterSQL is the predicate "the artwork at col is NOT a
// front-facing image": wide decoration or art that is never a card. A Work whose
// every image matches it has no poster, so enrichment fetches one.
func ArtworkIsNotAPosterSQL(col string) string {
	return "(" + globAny(col, decorationGlobs) + " OR " + globAny(col, unfitGlobs) + ")"
}

// ArtworkCardRankSQL orders a work's artwork for its card, best first: a poster
// or cover (0), an unnamed image (1), wide decoration (2), art that is never a
// card image — episode stills, logos, disc art — (3). Callers append their own
// tie-breaks.
func ArtworkCardRankSQL(col string) string {
	return "CASE WHEN " + globAny(col, posterGlobs) + " THEN 0" +
		" WHEN " + globAny(col, unfitGlobs) + " THEN 3" +
		" WHEN " + globAny(col, decorationGlobs) + " THEN 2" +
		" ELSE 1 END"
}
